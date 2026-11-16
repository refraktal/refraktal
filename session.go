package refraktal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"refraktal.dev/refraktal/internal/acp"
	"refraktal.dev/refraktal/internal/config"
	"refraktal.dev/refraktal/internal/harness"
	"refraktal.dev/refraktal/internal/proc"
	"refraktal.dev/refraktal/internal/runlog"
)

// sessionSpec says how to open a session with one harness.
type sessionSpec struct {
	h           harness.Harness
	hcfg        config.Harness
	scratch     string   // inside the run folder
	scratchRoot *os.Root // required: pinned scratch folder for all writes
	cwd         string   // the session folder
	tmpDir      string   // a private TMPDIR, used by Codex
	liveDir     string
	labels      map[string]string
	env         []string // extra variables for the child, such as REFRAKTAL_RUN
	// prefixExe is a binary that calls proc.MaybeShellPrefix first in main,
	// used as the harness's shell prefix; "" means none. It is never guessed
	// from os.Executable(): in a test or a library host that is a different
	// program, and Claude would run it for every tool command.
	prefixExe     string
	log           *runlog.Log
	timeout       time.Duration // for initialize, session/new and set_mode
	probeVersions bool          // doctor reads installed and bundled versions
	// readOnly sets the harness's own read-only mode instead of its usual
	// one (a judge); a harness that does not offer it fails to open.
	readOnly bool

	onPermission func(context.Context, acp.PermissionRequest) string
	onUpdate     func(acp.Update)
}

// session is an open session with a harness.
type session struct {
	launch harness.Launch
	child  *proc.Child
	client *acp.Client
	id     string
	agent  string // name and version from initialize
	model  string // the model session/new reported, if any
}

// openError says how far opening got: status is one of the Status constants.
type openError struct {
	status string
	err    error
}

func (e *openError) Error() string { return e.err.Error() }

// openSession prepares the harness, starts it under process control and
// performs the handshake: initialize, session/new, session/set_mode. It sends
// no prompt. On error the returned session may hold a child to shut down.
func openSession(ctx context.Context, sp sessionSpec) (*session, error) {
	s := &session{}
	if sp.scratchRoot == nil {
		return s, &openError{StatusError, errors.New("session: scratch root required")}
	}
	var err error
	s.launch, err = sp.h.Prepare(ctx, harness.Options{Scratch: sp.scratch, ScratchRoot: sp.scratchRoot, Cwd: sp.cwd, MCP: sp.hcfg.MCP,
		Executable: sp.hcfg.Executable, TmpDir: sp.tmpDir, ReadOnly: sp.readOnly, ProbeVersions: sp.probeVersions})
	if err != nil {
		return s, &openError{StatusUnavailable, err}
	}
	stderr, err := sp.scratchRoot.OpenFile("stderr", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return s, &openError{StatusError, err}
	}
	defer stderr.Close() // the child holds its own copy of the descriptor
	env := append(append([]string{}, s.launch.Env...), sp.env...)
	var sessions string
	if v := s.launch.ShellPrefixVar; v != "" && sp.prefixExe != "" {
		if err := checkShellPrefix(sp.prefixExe); err != nil {
			return s, &openError{StatusUnavailable, err}
		}
		// The harness's tool commands run in sessions of their own. Route each
		// one through the shell-prefix helper, which records the session for
		// the shutdown sweep. Environment only: no settings file is touched. A
		// prefix the user already set is chained, unless it is the helper.
		sessions = filepath.Join(sp.scratch, "sessions")
		env = append(env, v+"="+sp.prefixExe, proc.HelperEnv+"=shell-prefix", proc.SessionsEnv+"="+sessions,
			proc.PrefixVarEnv+"="+v)
		if orig := os.Getenv(v); orig != "" && !proc.SameFile(orig, sp.prefixExe) {
			env = append(env, proc.NextEnv+"="+orig)
		}
	}
	s.child, err = proc.Start(proc.Spec{Argv: s.launch.Argv, Env: env, Dir: sp.cwd, Stderr: stderr,
		LiveDir: sp.liveDir, Labels: sp.labels, SessionsFile: sessions})
	if err != nil {
		return s, &openError{StatusUnavailable, err}
	}
	name := sp.h.Name()
	s.client = acp.New(s.child.Stdout, s.child.Stdin, acp.Options{ClientName: "refraktal", ClientVersion: Version,
		OnUpdate: func(u acp.Update) {
			sp.log.Update(name, u.SessionID, u.Kind, u.Raw)
			if sp.onUpdate != nil {
				sp.onUpdate(u)
			}
		},
		OnNotification: func(m string, p json.RawMessage) { sp.log.Notification(name, m, p) },
		OnPermission:   sp.onPermission,
		CancelGrace:    15 * time.Second,
	})
	hctx, cancel := context.WithTimeout(ctx, sp.timeout)
	defer cancel()
	a, err := s.client.Initialize(hctx)
	if err != nil {
		return s, &openError{StatusError, err}
	}
	s.agent = strings.TrimSpace(a.Info.Name + " " + a.Info.Version)
	ses, err := s.client.NewSession(hctx, sp.cwd, s.launch.Meta)
	if err != nil {
		var e *acp.Error
		if errors.As(err, &e) && (e.Code == -32000 || strings.Contains(strings.ToLower(e.Message), "auth")) {
			return s, &openError{StatusLoggedOut, errors.New(e.Message)}
		}
		return s, &openError{StatusError, err}
	}
	s.id, s.model = ses.ID, ses.Model()
	mode := sp.h.Mode()
	if sp.readOnly {
		mode = sp.h.ReadOnlyMode()
		if !offers(ses, mode) {
			return s, &openError{StatusUnavailable, fmt.Errorf("%s does not offer its read-only mode %q", sp.h.Name(), mode)}
		}
	}
	if err := s.client.SetMode(hctx, s.id, mode); err != nil {
		return s, &openError{StatusError, fmt.Errorf("mode %s: %w", mode, err)}
	}
	return s, nil
}

// close shuts the child down and logs what was tracked and killed.
func (s *session) close(log *runlog.Log, name string, grace time.Duration) proc.ShutdownReport {
	if s == nil || s.child == nil {
		return proc.ShutdownReport{}
	}
	r := s.child.Shutdown(grace)
	log.Procs("tracked_"+name, r.Tracked)
	log.Procs("killed_"+name, r.Kill.Killed)
	alive, unknown := shutdownCounts(r)
	log.Event("shutdown", map[string]any{"harness": name, "left_running": alive, "unknown": unknown})
	return r
}

// shutdownCounts counts each PID once across the tree kill and session sweeps.
func shutdownCounts(r proc.ShutdownReport) (alive, unknown int) {
	a, u := map[int]bool{}, map[int]bool{}
	for _, kr := range []proc.KillReport{r.Kill, r.Swept} {
		for _, p := range kr.Alive {
			a[p.PID] = true
		}
		for _, p := range kr.Unknown {
			u[p.PID] = true
		}
	}
	return len(a), len(u)
}

// offers reports whether session/new listed the mode.
func offers(ses acp.Session, mode string) bool {
	if mode == "" || ses.Modes == nil {
		return false
	}
	for _, m := range ses.Modes.Available {
		if m.ID == mode {
			return true
		}
	}
	return false
}

func statusOf(err error) string {
	var oe *openError
	if errors.As(err, &oe) {
		return oe.status
	}
	return StatusError
}

// checkShellPrefix makes sure the shell prefix is an executable file and,
// inside a test binary, the test binary itself: its TestMain must call
// proc.MaybeShellPrefix first. Any other program would be run by the harness
// for every tool command.
func checkShellPrefix(exe string) error {
	if fi, err := os.Stat(exe); err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 || !filepath.IsAbs(exe) {
		return fmt.Errorf("shell prefix %q is not an absolute path to an executable", exe)
	}
	if testing.Testing() {
		if self, err := os.Executable(); err != nil || !proc.SameFile(exe, self) {
			return fmt.Errorf("shell prefix %q refused inside a test: only the test binary, whose TestMain calls proc.MaybeShellPrefix, may be used", exe)
		}
	}
	return nil
}
