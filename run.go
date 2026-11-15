package refraktal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"refraktal.dev/refraktal/internal/acp"
	"refraktal.dev/refraktal/internal/config"
	"refraktal.dev/refraktal/internal/harness"
	"refraktal.dev/refraktal/internal/policy"
	"refraktal.dev/refraktal/internal/proc"
	"refraktal.dev/refraktal/internal/runlog"
	"refraktal.dev/refraktal/internal/worktree"
)

// DefaultLimit is the wall-clock limit of a run until the --mins budget
// arrives (M4). It is never unlimited.
const DefaultLimit = 30 * time.Minute

// Run outcomes.
const (
	RunApplied     = "applied"      // the child's changes were applied to the working tree
	RunKept        = "kept"         // --no-apply: the changes are on the worktree's branch
	RunNoChanges   = "no_changes"   // the turn ended and nothing changed
	RunApplyFailed = "apply_failed" // the changes did not apply cleanly; they stay on the branch
	RunFailed      = "failed"       // the turn did not end normally
	RunTimedOut    = "timed_out"    // the time limit ran out
	RunUnusable    = "unusable"     // the harness could not be started or logged in
)

// RunOptions configure Run.
type RunOptions struct {
	Task    string
	Harness string // claude, codex or agy; routing arrives in M5
	NoApply bool   // leave the result on the worktree's branch

	// Dir is a folder inside the repository. Default: the current folder.
	// Default folders (this one and WorktreeDir) are refused when under
	// $TMPDIR or /tmp.
	Dir string
	// Limit is the wall-clock limit. Default DefaultLimit.
	Limit time.Duration
	// LiveDir, ConfigPath and WorktreeDir default to the user's folders:
	// os.UserConfigDir()/refraktal/{live,config.json} and
	// os.UserCacheDir()/refraktal/worktrees.
	LiveDir, ConfigPath, WorktreeDir string
	// Harnesses to choose from. Default harness.Defaults.
	Harnesses []harness.Harness
	// OnNotice receives each notice as it is made, such as hooks in the
	// repository or an inherited MCP setup. Optional.
	OnNotice func(string)
}

// RunResult is what Run did.
type RunResult struct {
	Run        string     `json:"run"` // run folder, relative to the repository
	Harness    string     `json:"harness"`
	Outcome    string     `json:"outcome"`
	StopReason string     `json:"stop_reason,omitempty"`
	Reply      string     `json:"reply,omitempty"`
	Files      []string   `json:"files,omitempty"`    // changed paths
	Branch     string     `json:"branch,omitempty"`   // when the worktree is kept
	Worktree   string     `json:"worktree,omitempty"` // when the worktree is kept
	MCP        string     `json:"mcp,omitempty"`
	Allowed    int        `json:"permissions_allowed"`
	Denied     int        `json:"permissions_denied"`
	Usage      *acp.Usage `json:"usage,omitempty"`
	Cost       *acp.Cost  `json:"cost,omitempty"`
	Notices    []string   `json:"notices,omitempty"`
	Error      string     `json:"error,omitempty"`
}

// ExitCode maps the outcome to the exit codes in docs/DESIGN.md.
func (r RunResult) ExitCode() int {
	switch r.Outcome {
	case RunApplied, RunKept, RunNoChanges:
		return ExitOK
	case RunTimedOut:
		return ExitBudget
	case RunUnusable:
		return ExitNothingUsable
	default:
		return ExitNoCandidate
	}
}

func (o *RunOptions) defaults() (root string, err error) {
	defaultDir := o.Dir == ""
	if defaultDir {
		if o.Dir, err = os.Getwd(); err != nil {
			return "", err
		}
	}
	if root, err = worktree.Root(context.Background(), o.Dir); err != nil {
		return "", err
	}
	if defaultDir { // folders a caller chose explicitly are its own business
		if err := runlog.CheckNotTemp(root); err != nil {
			return "", err
		}
	}
	if o.Limit <= 0 {
		o.Limit = DefaultLimit
	}
	if o.LiveDir == "" {
		if o.LiveDir, err = proc.DefaultLiveDir(); err != nil {
			return "", err
		}
	}
	if o.ConfigPath == "" {
		if o.ConfigPath, err = config.DefaultPath(); err != nil {
			return "", err
		}
	}
	if o.WorktreeDir == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return "", err
		}
		o.WorktreeDir = filepath.Join(cache, "refraktal", "worktrees")
		if err := runlog.CheckNotTemp(o.WorktreeDir); err != nil {
			return "", err
		}
	}
	if o.Harnesses == nil {
		cache, err := harness.DefaultCacheDir()
		if err != nil {
			return "", err
		}
		o.Harnesses = harness.Defaults(cache)
	}
	return root, nil
}

// Run sends one task to one harness in a worktree of its own, then applies
// the result to the working tree (or, with NoApply, leaves it on the
// worktree's branch).
func Run(ctx context.Context, o RunOptions) (RunResult, error) {
	res := RunResult{Harness: o.Harness, Outcome: RunUnusable}
	if strings.TrimSpace(o.Task) == "" {
		return res, errors.New("no task: give it as an argument or on stdin")
	}
	root, err := o.defaults()
	if err != nil {
		return res, err
	}
	var h harness.Harness
	for _, c := range o.Harnesses {
		if c.Name() == o.Harness {
			h = c
		}
	}
	if h == nil {
		return res, fmt.Errorf("unknown harness %q", o.Harness)
	}
	cfg, err := config.Load(o.ConfigPath)
	if err != nil {
		return res, err
	}
	hcfg := cfg.For(h.Name())
	notice := func(s string) {
		res.Notices = append(res.Notices, s)
		if o.OnNotice != nil {
			o.OnNotice(s)
		}
	}

	cleanup, cerr := proc.CleanLeftovers(o.LiveDir)
	run, err := runlog.NewRun(root)
	if err != nil {
		return res, err
	}
	res.Run = run.Rel
	log, err := runlog.Create(filepath.Join(run.Dir, "events.jsonl"))
	if err != nil {
		return res, err
	}
	defer log.Close()
	log.Event("run_start", map[string]any{"harness": h.Name(), "task": o.Task, "no_apply": o.NoApply,
		"limit_s": o.Limit.Seconds(), "mcp": hcfg.MCP})
	if cerr != nil || cleanup.Dead > 0 {
		log.Event("cleanup", map[string]any{"dead": cleanup.Dead, "killed": len(cleanup.Kill.Killed), "error": fmt.Sprint(cerr)})
	}

	if err := os.MkdirAll(o.WorktreeDir, 0o755); err != nil {
		return res, err
	}
	wt, err := worktree.Create(ctx, root, filepath.Join(o.WorktreeDir, run.ID), "refraktal/"+run.ID)
	if err != nil {
		return res, err
	}
	log.Event("worktree", map[string]any{"branch": wt.Branch, "base": wt.Base})
	if hooks := harness.ProjectHooks(wt.Dir, h.Name()); len(hooks) > 0 {
		notice(fmt.Sprintf("this repository configures %s hooks (%s); they run inside the child, outside Refraktal's permission policy",
			h.Name(), strings.Join(hooks, ", ")))
	}

	scratch, tmp := filepath.Join(run.Dir, h.Name()), filepath.Join(run.Dir, "tmp")
	for _, d := range []string{scratch, tmp} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return res, err
		}
	}
	pol := policy.Policy{Worktree: wt.Dir}
	var mu sync.Mutex
	var reply strings.Builder
	s, err := openSession(ctx, sessionSpec{h: h, hcfg: hcfg, scratch: scratch, cwd: wt.Dir, tmpDir: tmp,
		liveDir: o.LiveDir, labels: map[string]string{"run": run.ID, "harness": h.Name()}, log: log, timeout: 2 * time.Minute,
		onPermission: func(_ context.Context, req acp.PermissionRequest) string {
			d := pol.Decide(req)
			mu.Lock()
			if d.Allow {
				res.Allowed++
			} else {
				res.Denied++
			}
			mu.Unlock()
			log.Event("permission", map[string]any{"harness": h.Name(), "decision": d})
			return d.OptionID
		},
		onUpdate: func(u acp.Update) {
			if u.Kind == "agent_message_chunk" {
				mu.Lock()
				reply.WriteString(chunkText(u))
				mu.Unlock()
			}
		}})
	res.MCP = s.launch.MCP
	if res.MCP == harness.MCPInherit {
		notice(h.Name() + " uses your own MCP servers and plugins (mcp: inherit in the config)")
	}
	if err != nil {
		s.close(log, h.Name(), 5*time.Second)
		res.Error = fmt.Sprintf("%s: %v", statusOf(err), err)
		wt.Remove(context.Background())
		log.Event("run_end", map[string]any{"outcome": res.Outcome, "error": res.Error})
		return res, nil
	}

	tctx, cancel := context.WithTimeout(ctx, o.Limit)
	turn, perr := s.client.Prompt(tctx, s.id, o.Task)
	timedOut := errors.Is(tctx.Err(), context.DeadlineExceeded)
	cancel()
	sr := s.close(log, h.Name(), 10*time.Second)
	mu.Lock()
	res.Reply = strings.TrimSpace(reply.String())
	mu.Unlock()
	res.StopReason, res.Usage, res.Cost = turn.StopReason, turn.Usage, turn.Cost
	log.Event("turn", map[string]any{"stop_reason": turn.StopReason, "cancelled": turn.Cancelled, "wall_ms": turn.Wall.Milliseconds(),
		"usage": turn.Usage, "cost": turn.Cost, "last_used": turn.LastUsed, "error": errString(perr),
		"left_running": len(sr.Kill.Alive), "exited_on_its_own": sr.ExitedOnItsOwn})

	r, cerr2 := wt.Collect(context.Background(), "refraktal "+run.ID+": "+firstLine(o.Task))
	res.Files = r.Files
	if len(r.Diff) > 0 {
		os.WriteFile(filepath.Join(run.Dir, "result.diff"), r.Diff, 0o600)
	}
	keep := func(outcome string) {
		res.Outcome, res.Branch, res.Worktree = outcome, wt.Branch, wt.Dir
	}
	switch {
	case cerr2 != nil:
		keep(RunFailed)
		res.Error = cerr2.Error()
	case timedOut:
		keep(RunTimedOut)
	case perr != nil || turn.StopReason != "end_turn":
		keep(RunFailed)
		res.Error = errString(perr)
	case len(r.Files) == 0 && o.NoApply:
		keep(RunNoChanges) // --no-apply always leaves the worktree
	case len(r.Files) == 0:
		res.Outcome = RunNoChanges
	case o.NoApply:
		keep(RunKept)
	default:
		if err := wt.Apply(context.Background(), r); err != nil {
			keep(RunApplyFailed)
			res.Error = err.Error()
		} else {
			res.Outcome = RunApplied
		}
	}
	if res.Worktree == "" {
		if err := wt.Remove(context.Background()); err != nil {
			log.Event("worktree_remove_failed", map[string]any{"error": err.Error()})
		}
	} else if len(r.Files) == 0 && !o.NoApply {
		// Nothing to keep: a failed or timed-out turn that changed nothing.
		wt.Remove(context.Background())
		res.Branch, res.Worktree = "", ""
	}
	log.Event("run_end", map[string]any{"outcome": res.Outcome, "files": res.Files, "branch": res.Branch,
		"allowed": res.Allowed, "denied": res.Denied, "error": res.Error})
	return res, nil
}

func chunkText(u acp.Update) string {
	var c struct {
		Content struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if jsonUnmarshal(u.Raw, &c) == nil && c.Content.Type == "text" {
		return c.Content.Text
	}
	return ""
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 72 {
		s = s[:72]
	}
	return s
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
