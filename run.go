package refraktal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"refraktal.dev/refraktal/internal/acp"
	"refraktal.dev/refraktal/internal/budget"
	"refraktal.dev/refraktal/internal/config"
	"refraktal.dev/refraktal/internal/harness"
	"refraktal.dev/refraktal/internal/price"
	"refraktal.dev/refraktal/internal/proc"
	"refraktal.dev/refraktal/internal/route"
	"refraktal.dev/refraktal/internal/runlog"
	"refraktal.dev/refraktal/internal/safegit"
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
	RunPlanned     = "planned"          // --dry: routed and planned, nothing run
	RunUnusable    = "unusable"         // the harness could not be started or logged in
	RunNoWinner    = "no_winner"        // fan-out: no candidate passed the judge
)

// RunOptions configure Run.
type RunOptions struct {
	Task    string
	Harness string // claude, codex or agy, or a comma-separated set to fan out to; "" lets the router choose
	NoApply bool   // leave every candidate on its worktree's branch
	Dry     bool   // plan and route only: no worktree, no harness
	Fan     int    // caps the router's width; 0: no cap

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
	// Harnesses to choose from. Default harness.Defaults, plus the refraktal
	// harness when Self is set.
	Harnesses []harness.Harness
	// Self is the path to this refraktal binary, for the refraktal harness (a
	// child Refraktal over ACP). The CLI passes its own path. Empty: none.
	Self string
	// Confine keeps the run to Dir's subtree of the repository (the acp
	// server sets it for a session's folder): each harness works in that
	// subfolder of its worktree, its edits are allowed only there, and a
	// result that changes anything outside it is refused. Without it, the
	// run covers the whole repository whatever Dir is.
	Confine bool
	// Ledger, if set, is a ledger shared with other runs (an acp server's,
	// seeded from the parent's grant): Budget is then only its limits.
	Ledger *budget.Ledger
	// ParentRun, set when this run is a child Refraktal's, is the folder in
	// the parent's run folder where this run's folder is made (so child runs
	// nest inside the parent's). The acp server takes it from
	// REFRAKTAL_PARENT_RUN.
	ParentRun string
	// ShellPrefix is a binary that calls proc.MaybeShellPrefix first in its
	// main. It is set as Claude's shell prefix so the sessions its tool
	// commands run in are recorded and swept. The CLI passes its own path.
	// Empty means none: those sessions are then not swept.
	ShellPrefix string
	// Getenv reads the environment (GEMINI_API_KEY). Default os.Getenv.
	Getenv func(string) string
	// RouterEndpoint and RouteCache override the Decisions API endpoint and
	// the decision cache folder ("-" disables it), for tests.
	RouterEndpoint, RouteCache string
	// OnNotice receives each notice as it is made, such as hooks in the
	// repository or an inherited MCP setup, in full. Optional.
	OnNotice func(string)
	// OnSafeNotice receives each notice in a form safe to send to another
	// program (the acp server's client): a notice carrying a raw OS error,
	// URL or path arrives as fixed wording instead. Optional.
	OnSafeNotice func(string)
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
	Route      *route.Result      `json:"route,omitempty"` // how the harness was chosen, when the router chose it
	Plan       []string           `json:"plan,omitempty"`  // --dry: the harnesses a run would start
	Candidates []Candidate        `json:"candidates,omitempty"`
	Verdict    *Verdict           `json:"verdict,omitempty"`
	Judge      *Candidate         `json:"judge,omitempty"`
	Notices    []string           `json:"notices,omitempty"`
	Error      string             `json:"error,omitempty"`
	// Charged, Retained and RunTurns are this run's own figures (on a
	// shared ledger, Budget is the whole ledger's): the actual cost charged
	// (reported and computed), the reservations its cancelled children kept
	// beyond that, and the prompts it sent.
	Charged  float64 `json:"charged_usd"`
	Retained float64 `json:"retained_usd,omitempty"`
	RunTurns int     `json:"run_turns"`
	// LeftRunning and Unknown count processes still running after shutdown,
	// and processes whose cleanup could not be confirmed, over every child.
	LeftRunning int `json:"left_running"`
	Unknown     int `json:"unknown"`
}

// ExitCode maps the outcome to the exit codes in docs/DESIGN.md.
func (r RunResult) ExitCode() int {
	switch r.Outcome {
	case RunApplied, RunKept, RunNoChanges, RunPlanned:
		return ExitOK
	case RunTimedOut, RunExhausted:
		return ExitBudget
	case RunUnusable:
		return ExitNothingUsable
	default:
		return ExitNoCandidate
	}
}

func (o *RunOptions) defaults(ctx context.Context) (root string, err error) {
	gitCtx, cancel := context.WithTimeout(ctx, safegit.Timeout)
	defer cancel()
	defaultDir := o.Dir == ""
	if defaultDir {
		if o.Dir, err = os.Getwd(); err != nil {
			return "", err
		}
	}
	if root, err = worktree.Root(gitCtx, o.Dir); err != nil {
		return "", err
	}
	if defaultDir { // folders a caller chose explicitly are its own business
		if err := runlog.CheckNotTemp(root); err != nil {
			return "", err
		}
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
		if err := noRealHarnessesInTests(); err != nil {
			return "", err
		}
		cache, err := harness.DefaultCacheDir()
		if err != nil {
			return "", err
		}
		o.Harnesses = harness.Defaults(cache)
		if o.Self != "" {
			o.Harnesses = append(o.Harnesses, harness.Refraktal(o.Self))
		}
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
	if o.Fan < 0 {
		return res, errors.New("--fan must not be negative")
	}
	// --mins covers the whole run, setup included: the ledger, and with it
	// the deadline, starts before any setup step, and every setup step
	// (repository discovery, the config, the price fetch, the worktree,
	// starting the harness) runs under it.
	if o.Budget == nil {
		d := budget.Defaults
		o.Budget = &d
	}
	ledger, err := o.Ledger, error(nil)
	if ledger == nil {
		ledger, err = budget.New(*o.Budget, nil)
	}
	if err != nil {
		var ex *budget.Exhausted
		if errors.As(err, &ex) {
			res.Outcome, res.Error = RunExhausted, err.Error()
			return res, nil
		}
		return res, err
	}
	setupCtx, setupCancel := context.WithDeadline(ctx, ledger.Deadline())
	defer setupCancel()
	setupExpired := func() bool { return errors.Is(setupCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil }
	exhausted := func(step string) (RunResult, error) {
		res.Outcome, res.Budget = RunExhausted, ledger.Summary()
		res.Error = "budget exhausted: --mins ran out during " + step
		return res, nil
	}
	root, err := o.defaults(setupCtx)
	if setupExpired() {
		return exhausted("repository discovery")
	}
	if err != nil {
		return res, err
	}
	find := func(name string) harness.Harness {
		for _, c := range o.Harnesses {
			if c.Name() == name {
				return c
			}
		}
		return nil
	}
	// --harness a,b pins the set; without it the router chooses.
	var hs []harness.Harness
	if o.Harness != "" {
		for _, n := range strings.Split(o.Harness, ",") {
			n = strings.TrimSpace(n)
			h := find(n)
			if h == nil {
				return res, fmt.Errorf("unknown harness %q", n)
			}
			if slices.ContainsFunc(hs, func(x harness.Harness) bool { return x.Name() == n }) {
				return res, fmt.Errorf("harness %q named twice", n)
			}
			hs = append(hs, h)
		}
		if o.Fan > 0 && o.Fan < len(hs) {
			return res, fmt.Errorf("--fan %d is fewer than the %d harnesses --harness names", o.Fan, len(hs))
		}
		if len(hs) == 1 {
			res.Harness = hs[0].Name()
		}
	}
	cfg, err := config.Load(o.ConfigPath)
	if setupExpired() {
		return exhausted("loading the config")
	}
	if err != nil {
		return res, err
	}
	ceiling, err := nestCeiling(proc.Nest(), o.Budget.Depth)
	if err != nil {
		res.Outcome, res.Error = RunExhausted, err.Error()
		return res, nil
	}
	if err := ledger.Check(); err != nil {
		res.Outcome, res.Error, res.Budget = RunExhausted, err.Error(), ledger.Summary()
		return res, nil
	}
	var nmu sync.Mutex
	// rawNotice is a notice whose text may carry a raw OS error, URL or path:
	// in full for the CLI and the result, as safe for OnSafeNotice.
	rawNotice := func(raw, safe string) {
		nmu.Lock()
		defer nmu.Unlock()
		res.Notices = append(res.Notices, raw)
		if o.OnNotice != nil {
			o.OnNotice(raw)
		}
		if o.OnSafeNotice != nil {
			o.OnSafeNotice(safe)
		}
	}
	notice := func(s string) { rawNotice(s, s) }

	cleanup, cerr := proc.CleanLeftovers(o.LiveDir)
	newRun := func() (runlog.Run, error) { return runlog.NewRun(root) }
	if o.ParentRun != "" {
		newRun = func() (runlog.Run, error) { return runlog.NewRunUnder(o.ParentRun) }
	}
	run, err := newRun()
	if err != nil {
		return res, err
	}
	defer run.Close()
	res.Run = run.Rel
	log, err := run.Create("events.jsonl")
	if err != nil {
		return res, err
	}
	defer log.Close()
	log.Event("run_start", map[string]any{"harness": o.Harness, "task": o.Task, "no_apply": o.NoApply, "dry": o.Dry,
		"budget": o.Budget, "mins": o.Budget.Wall.Minutes(), "fan": o.Fan})

	// Without --harness the router chooses; its call runs alongside the rest
	// of the setup.
	type routed struct {
		r   route.Result
		err error
	}
	var routeCh chan routed
	scoreKey := "" // the pre-score runs only when the router ran with a key
	if hs == nil {
		key, source := route.LoadKey(root, o.Getenv)
		if source == ".env" {
			notice("routing with GEMINI_API_KEY from this repository's untracked .env")
		}
		scoreKey = key
		rt := route.New(route.Options{APIKey: key, Endpoint: o.RouterEndpoint, CacheDir: o.RouteCache,
			LogPath: filepath.Join(root, ".refraktal", "decisions.jsonl"), RunID: run.ID})
		routeCh = make(chan routed, 1)
		go func() {
			r, err := rt.Route(setupCtx, RepoInput(root, o.Task))
			routeCh <- routed{r, err}
		}()
	}
	joinRoute := func() error {
		if routeCh == nil {
			return nil
		}
		rr := <-routeCh
		routeCh = nil
		if rr.err != nil {
			return rr.err
		}
		r := rr.r
		res.Route = &r
		d := r.Decision
		log.Event("route", map[string]any{"decider": d.Decider, "model": d.Model, "label": d.Harness.Value, "harness": r.Harness,
			"harnesses": r.Run, "confidence": d.Harness.Confidence, "probabilities": d.Harness.Probabilities, "action": r.Outcome.Action,
			"width": r.Outcome.Width, "reason": r.Outcome.Reason, "cached": r.Cached, "latency_ms": d.LatencyMS,
			"fallback": runlog.ErrorField("fallback", d.FallbackReason)})
		names := r.Run
		if o.Fan > 0 && len(names) > o.Fan {
			names = names[:o.Fan]
		}
		how := fmt.Sprintf("%s (confidence %.2f, %.0f ms)", d.Decider, d.Harness.Confidence, d.LatencyMS)
		if r.Cached {
			how = fmt.Sprintf("%s, cached (confidence %.2f)", d.Decider, d.Harness.Confidence)
		}
		if d.Decider == "fallback" {
			how += "; " + d.FallbackReason
		}
		notice(fmt.Sprintf("routed to %s by %s", strings.Join(names, ", "), how))
		for _, n := range r.Notes {
			if strings.HasPrefix(n, "decision log not written") {
				rawNotice(n, "the decision log was not written (see the run's notices)")
			} else {
				notice(n)
			}
		}
		if len(names) < len(r.Run) {
			notice(fmt.Sprintf("the router advised %d harnesses; --fan %d caps it", len(r.Run), o.Fan))
		}
		for _, n := range names {
			h := find(n)
			if h == nil {
				return fmt.Errorf("the router chose %q, which is not available", n)
			}
			hs = append(hs, h)
		}
		if len(hs) == 0 {
			return route.ErrNoHarness
		}
		res.Harness = hs[0].Name()
		return nil
	}
	if o.Dry {
		err := joinRoute()
		if setupExpired() { // the router fell back because --mins ran out: not a plan
			return exhausted("routing")
		}
		res.Budget = ledger.Summary()
		if err != nil {
			res.Error = err.Error()
			return res, nil
		}
		res.Outcome = RunPlanned
		for _, h := range hs {
			res.Plan = append(res.Plan, h.Name())
		}
		log.Event("run_end", map[string]any{"outcome": res.Outcome, "harness": res.Harness, "plan": res.Plan})
		return res, nil
	}
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
		rawNotice("warning: "+perr0.Error(), "warning: the price table could not be fetched or read; models without a price count as $0")
	}
	log.Event("prices", map[string]any{"source": prices.Source, "warning": errString(perr0)})
	if cerr != nil || cleanup.Dead > 0 {
		log.Event("cleanup", map[string]any{"dead": cleanup.Dead, "killed": len(cleanup.Kill.Killed),
			"alive": len(cleanup.Kill.Alive), "unknown": len(cleanup.Kill.Unknown), "errors": cleanup.Errors, "error": fmt.Sprint(cerr)})
		if len(cleanup.Kill.Alive) > 0 || len(cleanup.Kill.Unknown) > 0 {
			notice(fmt.Sprintf("cleanup incomplete: %d still running, %d unknown; live records kept for retry",
				len(cleanup.Kill.Alive), len(cleanup.Kill.Unknown)))
		}
	}
	if err := os.MkdirAll(o.WorktreeDir, 0o755); err != nil {
		return res, err
	}
	headCtx, headCancel := context.WithTimeout(setupCtx, safegit.Timeout)
	base, err := worktree.Head(headCtx, root)
	headCancel()
	if err != nil {
		if setupExpired() {
			return exhausted("worktree creation")
		}
		return res, err
	}
	if err := joinRoute(); err != nil || setupExpired() {
		if setupExpired() {
			return exhausted("routing")
		}
		res.Error = err.Error()
		return res, nil
	}
	env := &runEnv{ctx: ctx, o: o, root: root, cfg: cfg, ledger: ledger, prices: prices, run: run, log: log, base: base, notice: notice,
		rawNotice: rawNotice, nestCeiling: ceiling}
	if o.Confine {
		if env.subtree, err = subtreeOf(root, o.Dir); err != nil {
			res.Error = err.Error()
			return res, nil
		}
	}
	if len(hs) > 1 {
		env.fanOut(hs, scoreKey, &res)
		res.Budget = ledger.Summary()
		env.figures(&res)
		log.Event("run_end", map[string]any{"outcome": res.Outcome, "winner": res.Harness, "files": res.Files, "branch": res.Branch,
			"verdict": res.Verdict, "error": runlog.ErrorField(res.Outcome, res.Error), "budget": res.Budget})
		return res, nil
	}

	// One candidate: nothing to choose, so it is applied as in M3.
	c := env.child(hs[0], childSpec{prompt: o.Task})
	env.collect(c) // the only child, and it has stopped
	res.Harness, res.MCP, res.Allowed, res.Denied = c.Harness, c.MCP, c.Allowed, c.Denied
	res.Model, res.Reply, res.StopReason, res.Usage, res.Cost = c.Model, c.Reply, c.StopReason, c.Usage, c.Cost
	res.Settlement, res.Files, res.Error = c.Settlement, c.Files, c.Error
	res.LeftRunning, res.Unknown = c.LeftRunning, c.Unknown
	switch c.Outcome {
	case candUnusable:
		res.Outcome = RunUnusable
	case candExhausted:
		res.Outcome = RunExhausted
	case candTimedOut:
		res.Outcome = RunTimedOut
	case candFailed:
		res.Outcome = RunFailed
	case candNoChanges:
		res.Outcome = RunNoChanges
	case candChanges:
		switch {
		case o.NoApply:
			res.Outcome = RunKept
		default:
			if n, err := env.apply(c); errors.Is(err, errMinsExpired) {
				res.Outcome, res.Error = RunTimedOut, err.Error()
			} else if errors.Is(err, errCancelled) {
				res.Outcome, res.Error = RunFailed, err.Error()
			} else if err != nil {
				res.Outcome, res.Error = RunApplyFailed, err.Error()
			} else {
				res.Outcome = RunApplied
				if n != "" {
					rawNotice(n, "some LFS-tracked files were left as LFS pointers")
				}
			}
		}
	}
	// --no-apply always leaves the worktree; so do a failed apply and a
	// failed or timed-out turn that changed something.
	if c.wt != nil && (o.NoApply || c.collectFailed || (len(c.Files) > 0 && res.Outcome != RunApplied)) {
		c.keep()
		res.Branch, res.Worktree = c.Branch, c.Worktree
	}
	env.drop(c)
	res.Budget = ledger.Summary()
	env.figures(&res)
	log.Event("run_end", map[string]any{"outcome": res.Outcome, "harness": res.Harness, "files": res.Files, "branch": res.Branch,
		"allowed": res.Allowed, "denied": res.Denied, "error": runlog.ErrorField(res.Outcome, res.Error), "budget": res.Budget,
		"settlement": res.Settlement, "swept": c.swept, "left_running": res.LeftRunning, "unknown": res.Unknown})
	return res, nil
}

// subtreeOf is dir relative to the repository root, slash-separated, or ""
// for the root itself.
func subtreeOf(root, dir string) (string, error) {
	r, err1 := filepath.EvalSymlinks(root)
	d, err2 := filepath.EvalSymlinks(dir)
	if err1 != nil || err2 != nil {
		return "", errors.New("cannot resolve the session folder inside the repository")
	}
	rel, err := filepath.Rel(r, d)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("the session folder is not inside the repository")
	}
	if rel == "." {
		return "", nil
	}
	return filepath.ToSlash(rel), nil
}

// outside returns the files not under the subtree ("" is the whole tree).
func outside(subtree string, files []string) []string {
	if subtree == "" {
		return nil
	}
	var out []string
	for _, f := range files {
		if !strings.HasPrefix(f, subtree+"/") {
			out = append(out, f)
		}
	}
	return out
}

func chunkText(u acp.Update) string {
	var c struct {
		Content struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(u.Raw, &c) == nil && c.Content.Type == "text" {
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
	case validCost(t.Cost):
		// Claude's cost is cumulative for the session; Run sends one prompt
		// per session, so the previous figure is 0.
		out = append(out, budget.Charge{USD: t.Cost.Amount, Source: budget.Reported, Model: model})
	case t.Usage != nil && !validUsage(t.Usage):
		out = append(out, budget.Charge{Source: budget.Unpriced, Model: model, Note: "invalid usage reported (a negative token count): not priced"})
	case t.Usage != nil && priced:
		out = append(out, budget.Charge{USD: p.Cost(*t.Usage), Source: budget.Computed, Model: model, Note: "priced as " + key})
	case t.Usage != nil:
		out = append(out, budget.Charge{Source: budget.Unpriced, Model: model})
	}
	if t.Cancelled && t.LastUsed != nil && *t.LastUsed >= 0 {
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

// validCost reports whether a harness's reported cost can be charged: US
// dollars, finite and not negative. Otherwise the tokens are priced.
func validCost(c *acp.Cost) bool {
	return c != nil && strings.EqualFold(c.Currency, "USD") && !math.IsNaN(c.Amount) && !math.IsInf(c.Amount, 0) && c.Amount >= 0
}

// validUsage reports whether every token count is non-negative.
func validUsage(u *acp.Usage) bool {
	for _, p := range []*int64{u.ThoughtTokens, u.CachedReadTokens, u.CachedWriteTokens} {
		if p != nil && *p < 0 {
			return false
		}
	}
	return u.TotalTokens >= 0 && u.InputTokens >= 0 && u.OutputTokens >= 0
}

// turnErrorCode is the structured code for a failed prompt.
func turnErrorCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, acp.ErrCancelIgnored):
		return "cancel_ignored"
	case errors.Is(err, acp.ErrFrameTooLarge):
		return "frame_too_large"
	case errors.Is(err, acp.ErrAgentExited):
		return "agent_exited"
	}
	return "prompt_failed"
}

// languages maps file extensions to the language names the router is told.
var languages = map[string]string{".go": "go", ".py": "python", ".ts": "typescript", ".tsx": "typescript", ".js": "javascript",
	".jsx": "javascript", ".rs": "rust", ".java": "java", ".kt": "kotlin", ".swift": "swift", ".rb": "ruby", ".c": "c",
	".h": "c", ".cpp": "cpp", ".cc": "cpp", ".cs": "csharp", ".php": "php", ".scala": "scala", ".m": "objective-c"}

// RepoInput describes the repository for the router: its three most common
// languages, its tracked file count and whether it has tests.
func RepoInput(root, task string) route.Input {
	ctx, cancel := context.WithTimeout(context.Background(), safegit.Timeout)
	defer cancel()
	in := route.Input{Task: task, Repo: route.RepoContext{Languages: []string{}}}
	out, err := safegit.Command(ctx, root, "ls-files", "-z").Output()
	if err != nil {
		return in
	}
	counts := map[string]int{}
	for _, f := range strings.Split(string(out), "\x00") {
		if f == "" {
			continue
		}
		in.Repo.Files++
		if l, ok := languages[strings.ToLower(filepath.Ext(f))]; ok {
			counts[l]++
		}
		base := filepath.Base(f)
		if strings.HasSuffix(base, "_test.go") || strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.py") ||
			strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") ||
			strings.Contains("/"+f, "/test/") || strings.Contains("/"+f, "/tests/") {
			in.Repo.HasTests = true
		}
	}
	for l := range counts {
		in.Repo.Languages = append(in.Repo.Languages, l)
	}
	sort.Slice(in.Repo.Languages, func(i, j int) bool {
		a, b := in.Repo.Languages[i], in.Repo.Languages[j]
		if counts[a] != counts[b] {
			return counts[a] > counts[b]
		}
		return a < b
	})
	if len(in.Repo.Languages) > 3 {
		in.Repo.Languages = in.Repo.Languages[:3]
	}
	return in
}

// NestCeilingEnv is the deepest REFRAKTAL_NEST any run below may start at:
// set by the top run (its own nest plus --depth) and only ever lowered.
const NestCeilingEnv = "REFRAKTAL_NEST_CEILING"

// nestCeiling applies the nesting guard and returns the ceiling to pass
// down. With an inherited ceiling, a run past it refuses to start, whatever
// its remaining --depth (a grandchild with depth 0 left is still allowed: it
// may run harnesses, not another Refraktal). Without one (a top run, or a
// Refraktal started by hand inside another), the old bound applies: refuse
// past --depth + 1 levels.
func nestCeiling(nest, depth int) (int, error) {
	own := nest + depth
	if c, err := strconv.Atoi(os.Getenv(NestCeilingEnv)); err == nil {
		if nest > c {
			return 0, fmt.Errorf("refusing to start: nested %d levels deep (%s), past the ceiling %d the top run set (%s)", nest, proc.NestEnv, c, NestCeilingEnv)
		}
		return min(own, c), nil
	}
	if nest > depth+1 {
		return 0, fmt.Errorf("refusing to start: nested %d levels deep (%s), past --depth %d + 1", nest, proc.NestEnv, depth)
	}
	return own, nil
}

// noRealHarnessesInTests refuses, inside a test binary, to fall back to the
// real harnesses (preparing them can install adapters and run their CLIs),
// unless REFRAKTAL_LIVE=1: a test must name its harnesses.
func noRealHarnessesInTests() error {
	if testing.Testing() && os.Getenv("REFRAKTAL_LIVE") != "1" {
		return errors.New("a test must name its harnesses: the real ones are never used in go test (REFRAKTAL_LIVE=1 allows them)")
	}
	return nil
}

// PassKeyEnv, set to 1 by the user, opts in to giving harness children the
// routing key, for a nested Refraktal that should route with Gemini. Without
// it no child gets an API key (proc.ScrubEnv).
const PassKeyEnv = "REFRAKTAL_PASS_GEMINI_KEY"

func childEnv(root string, o RunOptions, hcfg config.Harness, harnessName string, ceiling int, tag, budgetJSON string) []string {
	env := []string{"REFRAKTAL_RUN=" + tag, "REFRAKTAL_BUDGET=" + budgetJSON, NestCeilingEnv + "=" + strconv.Itoa(ceiling)}
	// Provider keys are scrubbed from every child (proc.ScrubEnv); the
	// config's pass_env opts one harness in to a named key.
	for _, k := range hcfg.PassEnv {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	getenv := o.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	// The opt-in passes the routing key to a nested Refraktal only: never to
	// an ordinary harness (pass_env is the way to give one a key).
	if getenv(PassKeyEnv) == "1" && harnessName == "refraktal" {
		if key, _ := route.LoadKey(root, o.Getenv); key != "" {
			env = append(env, route.KeyEnv+"="+key)
		}
	}
	return env
}
