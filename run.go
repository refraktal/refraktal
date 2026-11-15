package refraktal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"refraktal.dev/refraktal/internal/acp"
	"refraktal.dev/refraktal/internal/budget"
	"refraktal.dev/refraktal/internal/config"
	"refraktal.dev/refraktal/internal/harness"
	"refraktal.dev/refraktal/internal/policy"
	"refraktal.dev/refraktal/internal/price"
	"refraktal.dev/refraktal/internal/proc"
	"refraktal.dev/refraktal/internal/runlog"
	"refraktal.dev/refraktal/internal/worktree"
)

// Run outcomes.
const (
	RunApplied     = "applied"          // the child's changes were applied to the working tree
	RunKept        = "kept"             // --no-apply: the changes are on the worktree's branch
	RunNoChanges   = "no_changes"       // the turn ended and nothing changed
	RunApplyFailed = "apply_failed"     // the changes did not apply cleanly; they stay on the branch
	RunFailed      = "failed"           // the turn did not end normally
	RunTimedOut    = "timed_out"        // --mins ran out during the turn
	RunExhausted   = "budget_exhausted" // a budget was spent before the turn could start
	RunUnusable    = "unusable"         // the harness could not be started or logged in
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
	// Budget holds --usd, --turns, --mins and --depth. Default budget.Defaults.
	Budget *budget.Limits
	// PriceURL and PriceCache say where the LiteLLM price table comes from.
	// Defaults: price.DefaultURL and os.UserCacheDir()/refraktal/prices.json.
	PriceURL, PriceCache string
	// LiveDir, ConfigPath and WorktreeDir default to the user's folders:
	// os.UserConfigDir()/refraktal/{live,config.json} and
	// os.UserCacheDir()/refraktal/worktrees.
	LiveDir, ConfigPath, WorktreeDir string
	// Harnesses to choose from. Default harness.Defaults.
	Harnesses []harness.Harness
	// ShellPrefix is a binary that calls proc.MaybeShellPrefix first in its
	// main. It is set as Claude's shell prefix so the sessions its tool
	// commands run in are recorded and swept. The CLI passes its own path.
	// Empty means none: those sessions are then not swept.
	ShellPrefix string
	// OnNotice receives each notice as it is made, such as hooks in the
	// repository or an inherited MCP setup. Optional.
	OnNotice func(string)
}

// RunResult is what Run did.
type RunResult struct {
	Run        string             `json:"run"` // run folder, relative to the repository
	Harness    string             `json:"harness"`
	Outcome    string             `json:"outcome"`
	StopReason string             `json:"stop_reason,omitempty"`
	Reply      string             `json:"reply,omitempty"`
	Files      []string           `json:"files,omitempty"`    // changed paths
	Branch     string             `json:"branch,omitempty"`   // when the worktree is kept
	Worktree   string             `json:"worktree,omitempty"` // when the worktree is kept
	MCP        string             `json:"mcp,omitempty"`
	Allowed    int                `json:"permissions_allowed"`
	Denied     int                `json:"permissions_denied"`
	Usage      *acp.Usage         `json:"usage,omitempty"`
	Cost       *acp.Cost          `json:"cost,omitempty"` // as the harness reported it (Claude: cumulative for the session)
	Model      string             `json:"model,omitempty"`
	Budget     budget.Summary     `json:"budget"`
	Settlement *budget.Settlement `json:"settlement,omitempty"`
	Notices    []string           `json:"notices,omitempty"`
	Error      string             `json:"error,omitempty"`
}

// ExitCode maps the outcome to the exit codes in docs/DESIGN.md.
func (r RunResult) ExitCode() int {
	switch r.Outcome {
	case RunApplied, RunKept, RunNoChanges:
		return ExitOK
	case RunTimedOut, RunExhausted:
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
	if o.Budget == nil {
		d := budget.Defaults
		o.Budget = &d
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
	if n := proc.Nest(); n > o.Budget.Depth+1 {
		res.Outcome = RunExhausted
		res.Error = fmt.Sprintf("refusing to start: nested %d levels deep (%s), past --depth %d + 1", n, proc.NestEnv, o.Budget.Depth)
		return res, nil
	}
	ledger, err := budget.New(*o.Budget, nil)
	if err != nil {
		var ex *budget.Exhausted
		if errors.As(err, &ex) {
			res.Outcome, res.Error = RunExhausted, err.Error()
			return res, nil
		}
		return res, err
	}
	grant, err := ledger.Reserve()
	if err != nil {
		res.Outcome, res.Error, res.Budget = RunExhausted, err.Error(), ledger.Summary()
		return res, nil
	}
	notice := func(s string) {
		res.Notices = append(res.Notices, s)
		if o.OnNotice != nil {
			o.OnNotice(s)
		}
	}
	// --mins covers every setup step too: the price fetch, the worktree and
	// starting the harness all run under the grant's deadline.
	setupCtx, setupCancel := context.WithDeadline(ctx, grant.Deadline)
	defer setupCancel()
	setupExpired := func() bool { return errors.Is(setupCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil }
	exhausted := func(step string) (RunResult, error) {
		st := ledger.Settle(grant, false)
		res.Outcome, res.Settlement, res.Budget = RunExhausted, &st, ledger.Summary()
		res.Error = "budget exhausted: --mins ran out during " + step
		return res, nil
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
		"budget": o.Budget, "mins": o.Budget.Wall.Minutes(), "grant": grant, "mcp": hcfg.MCP})
	prices, perr0 := price.Load(setupCtx, price.LoadOptions{CachePath: o.PriceCache, URL: o.PriceURL})
	if setupExpired() {
		log.Event("run_end", map[string]any{"outcome": RunExhausted, "step": "price fetch"})
		return exhausted("the price fetch")
	}
	for model, p := range cfg.Prices {
		pp := price.Price{Input: p.Input / 1e6, Output: p.Output / 1e6}
		if p.CacheRead != nil {
			pp.CacheRead, pp.HasCacheRead = *p.CacheRead/1e6, true
		}
		if p.CacheWrite != nil {
			pp.CacheWrite, pp.HasCacheWrite = *p.CacheWrite/1e6, true
		}
		prices.Override(model, pp)
	}
	if perr0 != nil {
		notice("warning: " + perr0.Error())
	}
	log.Event("prices", map[string]any{"source": prices.Source, "warning": errString(perr0)})
	if cerr != nil || cleanup.Dead > 0 {
		log.Event("cleanup", map[string]any{"dead": cleanup.Dead, "killed": len(cleanup.Kill.Killed), "error": fmt.Sprint(cerr)})
	}

	if err := os.MkdirAll(o.WorktreeDir, 0o755); err != nil {
		return res, err
	}
	wt, err := worktree.Create(setupCtx, root, filepath.Join(o.WorktreeDir, run.ID), "refraktal/"+run.ID)
	if err != nil {
		if setupExpired() {
			return exhausted("worktree creation")
		}
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
	childBudget, _ := json.Marshal(map[string]any{"usd": grant.USD, "turns": grant.Turns,
		"deadline": grant.Deadline.UTC().Format(time.RFC3339), "depth": grant.Depth})
	s, err := openSession(setupCtx, sessionSpec{h: h, hcfg: hcfg, scratch: scratch, cwd: wt.Dir, tmpDir: tmp,
		liveDir: o.LiveDir, labels: map[string]string{"run": run.ID, "harness": h.Name()}, log: log, timeout: 2 * time.Minute,
		// REFRAKTAL_RUN tags the child's processes for logs only (the session
		// sweep finds leftovers). REFRAKTAL_BUDGET is what a child Refraktal
		// may use.
		env:       []string{"REFRAKTAL_RUN=" + run.ID + "-" + h.Name(), "REFRAKTAL_BUDGET=" + string(childBudget)},
		prefixExe: o.ShellPrefix,
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
		if setupExpired() {
			// --mins ran out while the harness was starting: a spent budget.
			res.Outcome, res.Error = RunExhausted, "budget exhausted: --mins ran out while starting the harness ("+res.Error+")"
		}
		st := ledger.Settle(grant, false)
		res.Settlement, res.Budget = &st, ledger.Summary()
		wt.Remove(context.Background())
		log.Event("run_end", map[string]any{"outcome": res.Outcome, "error": res.Error, "budget": res.Budget})
		return res, nil
	}

	if err := ledger.BeginTurn(grant); err != nil {
		s.close(log, h.Name(), 5*time.Second)
		wt.Remove(context.Background())
		ledger.Settle(grant, false)
		res.Outcome, res.Error, res.Budget = RunExhausted, err.Error(), ledger.Summary()
		log.Event("run_end", map[string]any{"outcome": res.Outcome, "error": res.Error})
		return res, nil
	}
	tctx, cancel := context.WithDeadline(ctx, grant.Deadline)
	turn, perr := s.client.Prompt(tctx, s.id, o.Task)
	timedOut := errors.Is(tctx.Err(), context.DeadlineExceeded)
	cancel()
	sr := s.close(log, h.Name(), 10*time.Second)
	res.Model = cmpOr(turn.Model, s.model)
	// A turn is cancelled if we sent session/cancel or the harness ended it
	// as cancelled on its own (Codex does after a denial).
	turn.Cancelled = turn.Cancelled || turn.StopReason == "cancelled"
	for _, c := range charges(h.Name(), res.Model, turn, prices) {
		ledger.Charge(grant, c)
		log.Event("charge", map[string]any{"harness": h.Name(), "charge": c})
		if c.Source == budget.Unpriced {
			notice(fmt.Sprintf("warning: no price for model %q; its tokens count as $0 against --usd", c.Model))
		}
	}
	st := ledger.Settle(grant, turn.Cancelled)
	res.Settlement, res.Budget = &st, ledger.Summary()
	if st.Overrun > 0 {
		notice(fmt.Sprintf("the turn cost $%.4f, $%.4f beyond the --usd grant: --usd is checked before a turn, never inside one", st.Spent, st.Overrun))
	}
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
		"allowed": res.Allowed, "denied": res.Denied, "error": res.Error, "budget": res.Budget, "settlement": res.Settlement,
		"swept": len(sr.Swept.Killed)})
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

// charges turns a turn's figures into budget charges, as docs/DESIGN.md says:
// a reported cost wins; otherwise tokens are priced from the table, or
// counted as $0 and flagged unpriced; a cancelled turn adds an estimate for
// the call the cancel cut off (its last context fill priced as input).
func charges(harnessName, model string, t acp.Turn, prices price.Table) []budget.Charge {
	var out []budget.Charge
	p, key, priced := prices.Lookup(harness.PriceKeys(harnessName, model)...)
	switch {
	case t.Cost != nil && strings.EqualFold(t.Cost.Currency, "USD"):
		// Claude's cost is cumulative for the session; Run sends one prompt
		// per session, so the previous figure is 0.
		out = append(out, budget.Charge{USD: t.Cost.Amount, Source: budget.Reported, Model: model})
	case t.Usage != nil && priced:
		out = append(out, budget.Charge{USD: p.Cost(*t.Usage), Source: budget.Computed, Model: model, Note: "priced as " + key})
	case t.Usage != nil:
		out = append(out, budget.Charge{Source: budget.Unpriced, Model: model})
	}
	if t.Cancelled && t.LastUsed != nil {
		c := budget.Charge{Source: budget.Estimated, Model: model, Note: "last context fill priced as input"}
		if priced {
			c.USD = float64(*t.LastUsed) * p.Input
		} else {
			c.Note += "; no price, counted as $0"
		}
		out = append(out, c)
	}
	return out
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
