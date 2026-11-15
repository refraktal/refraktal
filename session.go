package refraktal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"refraktal.dev/refraktal/internal/acp"
	"refraktal.dev/refraktal/internal/config"
	"refraktal.dev/refraktal/internal/harness"
	"refraktal.dev/refraktal/internal/proc"
	"refraktal.dev/refraktal/internal/runlog"
)

// sessionSpec says how to open a session with one harness.
type sessionSpec struct {
	h       harness.Harness
	hcfg    config.Harness
	scratch string // inside the run folder
	cwd     string // the session folder
	tmpDir  string // a private TMPDIR, used by Codex
	liveDir string
	labels  map[string]string
	log     *runlog.Log
	timeout time.Duration // for initialize, session/new and set_mode

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
	var err error
	s.launch, err = sp.h.Prepare(ctx, harness.Options{Scratch: sp.scratch, Cwd: sp.cwd, MCP: sp.hcfg.MCP,
		Executable: sp.hcfg.Executable, TmpDir: sp.tmpDir})
	if err != nil {
		return s, &openError{StatusUnavailable, err}
	}
	stderr, err := os.OpenFile(filepath.Join(sp.scratch, "stderr"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return s, &openError{StatusError, err}
	}
	defer stderr.Close() // the child holds its own copy of the descriptor
	s.child, err = proc.Start(proc.Spec{Argv: s.launch.Argv, Env: s.launch.Env, Dir: sp.cwd, Stderr: stderr,
		LiveDir: sp.liveDir, Labels: sp.labels})
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
	s.id = ses.ID
	if err := s.client.SetMode(hctx, s.id, sp.h.Mode()); err != nil {
		return s, &openError{StatusError, fmt.Errorf("mode %s: %w", sp.h.Mode(), err)}
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
	return r
}

func statusOf(err error) string {
	var oe *openError
	if errors.As(err, &oe) {
		return oe.status
	}
	return StatusError
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
