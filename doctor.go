package refraktal

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"refraktal.dev/refraktal/internal/budget"
	"refraktal.dev/refraktal/internal/config"
	"refraktal.dev/refraktal/internal/harness"
	"refraktal.dev/refraktal/internal/proc"
	"refraktal.dev/refraktal/internal/runlog"
)

// Exit codes, from docs/DESIGN.md.
const (
	ExitOK            = 0 // winner applied, or kept with --no-apply; doctor: something is usable
	ExitNoCandidate   = 1 // no candidate passed the judge
	ExitBudget        = 2 // a budget ran out
	ExitNothingUsable = 3 // nothing usable
)

// Harness check states.
const (
	StatusOK          = "ok"          // logged in: the handshake reached session/new
	StatusLoggedOut   = "logged_out"  // session/new refused for authentication
	StatusUnavailable = "unavailable" // not installed, or its adapter could not be prepared
	StatusError       = "error"       // started, but the handshake failed
)

// DoctorOptions configure Doctor. The zero value checks the default harnesses.
type DoctorOptions struct {
	// Root is the folder that holds .refraktal/runs. Default: the git
	// repository containing the current directory, else the current
	// directory. It must not be under $TMPDIR or /tmp.
	Root string
	// LiveDir is the shared folder of live process records. Default
	// os.UserConfigDir()/refraktal/live.
	LiveDir string
	// Harnesses to check. Default harness.Defaults, with adapters cached
	// under os.UserCacheDir()/refraktal/adapters.
	Harnesses []harness.Harness
	// Getenv reads the environment. Default os.Getenv.
	Getenv func(string) string
	// Timeout bounds each harness's handshake. Default 90 s.
	Timeout time.Duration
	// ConfigPath is the config file. Default os.UserConfigDir()/refraktal/config.json.
	ConfigPath string
	// ShellPrefix: as RunOptions.ShellPrefix.
	ShellPrefix string
	// Neutral is where each handshake gets an empty session folder, outside
	// any repository, so a repository's own harness settings and hooks never
	// run during doctor. Default os.UserCacheDir()/refraktal/doctor.
	Neutral string
}

// DoctorReport is what Doctor found.
type DoctorReport struct {
	Run       string         `json:"run"` // run folder, relative to the root
	Harnesses []HarnessCheck `json:"harnesses"`
	Decider   DeciderCheck   `json:"decider"`
	Cleanup   CleanupCheck   `json:"cleanup"`
	Usable    []string       `json:"usable"` // harnesses with status ok
}

// ExitCode is 0 when at least one harness is usable, else ExitNothingUsable.
func (r DoctorReport) ExitCode() int {
	if len(r.Usable) == 0 {
		return ExitNothingUsable
	}
	return ExitOK
}

// HarnessCheck is one harness's result. It never holds account details.
type HarnessCheck struct {
	Name             string `json:"name"`
	Status           string `json:"status"`
	Reason           string `json:"reason,omitempty"`
	Adapter          string `json:"adapter,omitempty"`               // pinned package@version
	AdapterInstalled bool   `json:"adapter_installed_now,omitempty"` // installed into the cache by this run
	Bundled          string `json:"bundled_version,omitempty"`       // the harness binary the adapter runs
	Installed        string `json:"installed_version,omitempty"`     // the CLI on PATH
	Agent            string `json:"agent,omitempty"`                 // name and version from initialize
	Mode             string `json:"mode"`                            // the permission mode Refraktal sets
	ModeOK           bool   `json:"mode_ok"`                         // session/set_mode to it succeeded
	MCP              string `json:"mcp"`                             // isolated or inherit
	HandshakeMs      int64  `json:"handshake_ms,omitempty"`
	LeftRunning      int    `json:"left_running"` // processes still running after shutdown
}

// DeciderCheck says which routing decider a run would use.
type DeciderCheck struct {
	Kind   string `json:"kind"` // gemini or fallback
	Model  string `json:"model,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// CleanupCheck counts what was cleaned up from dead runs.
type CleanupCheck struct {
	Records int      `json:"records"` // live records found
	Live    int      `json:"live"`    // owned by a running Refraktal: left alone
	Dead    int      `json:"dead"`    // owned by a dead run: cleaned up
	Killed  int      `json:"killed"`  // processes killed
	Gone    int      `json:"gone"`    // processes that had already exited
	Changed int      `json:"changed"` // PIDs now used by other processes: left alone
	Alive   int      `json:"alive"`   // killed but still running
	Errors  []string `json:"errors"`
}

// Doctor cleans up after dead runs, then checks each harness with a
// zero-token handshake: initialize, session/new and session/set_mode, then
// shutdown. No prompt is ever sent.
func Doctor(ctx context.Context, o DoctorOptions) (DoctorReport, error) {
	var rep DoctorReport
	if n := proc.Nest(); n > budget.Defaults.Depth+1 {
		return rep, fmt.Errorf("refusing to start: nested %d levels deep (%s)", n, proc.NestEnv)
	}
	if err := o.defaults(); err != nil {
		return rep, err
	}
	run, err := runlog.NewRun(o.Root)
	if err != nil {
		return rep, err
	}
	rep.Run = run.Rel
	log, err := runlog.Create(filepath.Join(run.Dir, "events.jsonl"))
	if err != nil {
		return rep, err
	}
	defer log.Close()

	cr, err := proc.CleanLeftovers(o.LiveDir)
	rep.Cleanup = CleanupCheck{Records: cr.Records, Live: cr.Live, Dead: cr.Dead, Killed: len(cr.Kill.Killed),
		Gone: len(cr.Kill.Gone), Changed: len(cr.Kill.Changed), Alive: len(cr.Kill.Alive), Errors: cr.Errors}
	if err != nil {
		rep.Cleanup.Errors = append(rep.Cleanup.Errors, err.Error())
	}
	log.Procs("cleanup_killed", cr.Kill.Killed)

	rep.Decider = DeciderCheck{Kind: "fallback", Reason: "GEMINI_API_KEY not set"}
	if o.Getenv("GEMINI_API_KEY") != "" {
		rep.Decider = DeciderCheck{Kind: "gemini", Model: "gemini-decision-1"}
	}

	cfg, err := config.Load(o.ConfigPath)
	if err != nil {
		return rep, err
	}
	rep.Harnesses = make([]HarnessCheck, len(o.Harnesses))
	var wg sync.WaitGroup
	for i, h := range o.Harnesses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rep.Harnesses[i] = checkHarness(ctx, h, cfg.For(h.Name()), o, run, log)
		}()
	}
	wg.Wait()
	rep.Usable = []string{}
	for _, c := range rep.Harnesses {
		if c.Status == StatusOK {
			rep.Usable = append(rep.Usable, c.Name)
		}
		log.Event("harness_check", map[string]any{"check": c})
	}
	log.Event("doctor", map[string]any{"usable": rep.Usable, "decider": rep.Decider, "cleanup": rep.Cleanup})
	return rep, nil
}

func (o *DoctorOptions) defaults() error {
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	o.Timeout = cmp.Or(o.Timeout, 90*time.Second)
	var err error
	if o.Root == "" {
		if o.Root, err = defaultRoot(); err != nil {
			return err
		}
		if err := runlog.CheckNotTemp(o.Root); err != nil {
			return err
		}
	}
	if o.LiveDir == "" {
		if o.LiveDir, err = proc.DefaultLiveDir(); err != nil {
			return err
		}
	}
	if o.ConfigPath == "" {
		if o.ConfigPath, err = config.DefaultPath(); err != nil {
			return err
		}
	}
	if o.Neutral == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return err
		}
		o.Neutral = filepath.Join(cache, "refraktal", "doctor")
	}
	if o.Harnesses == nil {
		cache, err := harness.DefaultCacheDir()
		if err != nil {
			return err
		}
		o.Harnesses = harness.Defaults(cache)
	}
	return nil
}

// defaultRoot is the top of the git repository containing the current
// directory, or the current directory itself.
func defaultRoot() (string, error) {
	if out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output(); err == nil {
		if top := strings.TrimSpace(string(out)); top != "" {
			return top, nil
		}
	}
	return os.Getwd()
}

func checkHarness(ctx context.Context, h harness.Harness, hcfg config.Harness, o DoctorOptions, run runlog.Run, log *runlog.Log) HarnessCheck {
	c := HarnessCheck{Name: h.Name(), Mode: h.Mode(), MCP: hcfg.MCP}
	scratch := filepath.Join(run.Dir, h.Name())
	// An empty folder outside the repository: harnesses load project settings
	// (and Claude runs project hooks) from the session folder, and doctor must
	// not run a repository's code.
	cwd := filepath.Join(o.Neutral, run.ID+"-"+h.Name())
	for _, d := range []string{scratch, cwd, filepath.Join(scratch, "tmp")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			c.Status, c.Reason = StatusError, err.Error()
			return c
		}
	}
	defer os.RemoveAll(cwd)

	start := time.Now()
	s, err := openSession(ctx, sessionSpec{h: h, hcfg: hcfg, scratch: scratch, cwd: cwd, tmpDir: filepath.Join(scratch, "tmp"),
		liveDir: o.LiveDir, labels: map[string]string{"run": run.ID, "harness": h.Name()}, log: log, timeout: o.Timeout,
		env: []string{"REFRAKTAL_RUN=" + run.ID + "-" + h.Name()}, prefixExe: o.ShellPrefix})
	l := s.launch
	c.Adapter, c.AdapterInstalled, c.Bundled, c.Installed = l.Adapter, l.AdapterInstalled, l.Bundled, l.Installed
	if l.MCP != "" {
		c.MCP = l.MCP
	}
	c.Agent = s.agent
	c.Status, c.ModeOK = StatusOK, err == nil
	if err != nil {
		c.Status, c.Reason = statusOf(err), err.Error()
	}
	if s.child != nil {
		c.HandshakeMs = time.Since(start).Milliseconds()
		c.LeftRunning = len(s.close(log, h.Name(), 5*time.Second).Kill.Alive)
	}
	return c
}

// WriteDoctorJSON writes the report as JSONL: one line per harness, then the
// decider, the cleanup and a summary.
func WriteDoctorJSON(w io.Writer, r DoctorReport) error {
	enc := json.NewEncoder(w)
	for _, c := range r.Harnesses {
		if err := enc.Encode(struct {
			Type string `json:"type"`
			HarnessCheck
		}{"harness", c}); err != nil {
			return err
		}
	}
	lines := []any{
		struct {
			Type string `json:"type"`
			DeciderCheck
		}{"decider", r.Decider},
		struct {
			Type string `json:"type"`
			CleanupCheck
		}{"cleanup", r.Cleanup},
		map[string]any{"type": "summary", "usable": r.Usable, "run": r.Run, "exit": r.ExitCode()},
	}
	for _, l := range lines {
		if err := enc.Encode(l); err != nil {
			return err
		}
	}
	return nil
}

// WriteDoctorText writes the report for a person.
func WriteDoctorText(w io.Writer, r DoctorReport) {
	p := func(format string, a ...any) { fmt.Fprintf(w, format, a...) }
	for _, c := range r.Harnesses {
		p("%-8s %-11s", c.Name, c.Status)
		var parts []string
		if c.Adapter != "" {
			parts = append(parts, "adapter "+c.Adapter)
		}
		if c.Bundled != "" {
			parts = append(parts, "bundled "+c.Bundled)
		}
		parts = append(parts, "installed "+cmp.Or(c.Installed, "no"))
		if c.Status == StatusOK {
			parts = append(parts, "mode "+c.Mode, "mcp "+c.MCP)
		}
		p("%s\n", strings.Join(parts, ", "))
		if c.Reason != "" {
			p("%-20s%s\n", "", c.Reason)
		}
		if c.MCP == harness.MCPInherit {
			p("%-20s%s\n", "", "mcp inherit (config): this harness uses your own MCP servers and plugins")
		}
		if c.LeftRunning > 0 {
			p("%-20s%d processes still running after shutdown\n", "", c.LeftRunning)
		}
	}
	if r.Decider.Kind == "gemini" {
		p("%-8s %-11s%s\n", "decider", "gemini", "GEMINI_API_KEY set, model "+r.Decider.Model)
	} else {
		p("%-8s %-11s%s\n", "decider", "fallback", r.Decider.Reason)
	}
	cl := r.Cleanup
	switch {
	case cl.Dead == 0:
		p("%-8s %-11s%s\n", "cleanup", "ok", "no leftovers from dead runs")
	default:
		p("%-8s %-11s%d dead runs: %d processes killed, %d already gone, %d PIDs reused (left alone)\n",
			"cleanup", "done", cl.Dead, cl.Killed, cl.Gone, cl.Changed)
	}
	for _, e := range cl.Errors {
		p("%-20s%s\n", "", e)
	}
	if len(r.Usable) == 0 {
		p("\nnothing usable\n")
	} else {
		p("\nusable: %s\n", strings.Join(slices.Clone(r.Usable), ", "))
	}
	p("run folder: %s\n", r.Run)
}
