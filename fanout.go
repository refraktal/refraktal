package refraktal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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
	"refraktal.dev/refraktal/internal/route"
	"refraktal.dev/refraktal/internal/runlog"
	"refraktal.dev/refraktal/internal/safegit"
	"refraktal.dev/refraktal/internal/worktree"
)

// Candidate outcomes: how one child's turn ended.
const (
	candChanges   = "changes"
	candNoChanges = "no_changes"
	candFailed    = "failed"
	candTimedOut  = "timed_out"
	candExhausted = "budget_exhausted"
	candUnusable  = "unusable"
)

// Candidate is one harness's attempt at the task (or the judge's turn).
type Candidate struct {
	Harness        string             `json:"harness"`
	Label          string             `json:"label,omitempty"` // the anonymous letter the judge saw
	Outcome        string             `json:"outcome"`
	StopReason     string             `json:"stop_reason,omitempty"`
	Reply          string             `json:"reply,omitempty"`
	Files          []string           `json:"files,omitempty"`
	Branch         string             `json:"branch,omitempty"`
	Worktree       string             `json:"worktree,omitempty"`
	MCP            string             `json:"mcp,omitempty"`
	Allowed        int                `json:"permissions_allowed"`
	Denied         int                `json:"permissions_denied"`
	Usage          *acp.Usage         `json:"usage,omitempty"`
	Cost           *acp.Cost          `json:"cost,omitempty"`
	Model          string             `json:"model,omitempty"`
	Settlement     *budget.Settlement `json:"settlement,omitempty"`
	Applies        *bool              `json:"applies,omitempty"` // the diff applies cleanly to the working tree
	Checks         []CheckResult      `json:"checks,omitempty"`
	Flagged        string             `json:"flagged,omitempty"` // why it cannot win on checks alone
	Score          *float64           `json:"pre_score,omitempty"`
	ScoreTruncated bool               `json:"pre_score_truncated,omitempty"`
	Error          string             `json:"error,omitempty"`
	LeftRunning    int                `json:"left_running"` // processes still running after shutdown
	Unknown        int                `json:"unknown"`      // processes whose cleanup could not be confirmed

	wt      *worktree.Worktree
	pending bool // stopped, not collected yet
	// collectFailed: the collect failed, so the worktree is kept whatever
	// else happens (its work is never deleted).
	collectFailed    bool
	id, suffix, name string
	root             *os.Root // the candidate's scratch folder in the run folder
	result           worktree.Result
	timedOut         bool
	swept            int
}

// runEnv is what every child of one run shares.
type runEnv struct {
	ctx    context.Context
	o      RunOptions
	root   string
	cfg    config.Config
	ledger *budget.Ledger
	prices price.Table
	run    runlog.Run
	log    *runlog.Log
	base   string
	notice func(string)
	// rawNotice: a notice with a raw error or path, and its safe wording.
	rawNotice func(raw, safe string)
	subtree   string // Confine: the session's folder in the repository, or "" for all of it
	// nestCeiling is the deepest REFRAKTAL_NEST any descendant may run at,
	// passed down unchanged (REFRAKTAL_NEST_CEILING).
	nestCeiling int

	amu      sync.Mutex // this run's own figures (the ledger may be shared)
	charged  float64
	retained float64
	turns    int
}

// account adds to this run's own figures.
func (e *runEnv) account(charged, retained float64, turns int) {
	e.amu.Lock()
	defer e.amu.Unlock()
	e.charged += charged
	e.retained += retained
	e.turns += turns
}

// figures copies this run's own figures into the result.
func (e *runEnv) figures(res *RunResult) {
	e.amu.Lock()
	defer e.amu.Unlock()
	res.Charged, res.Retained, res.RunTurns = e.charged, e.retained, e.turns
}

// childSpec says what kind of child to run.
type childSpec struct {
	suffix string // worktree, branch and scratch suffix; "" for a lone candidate
	share  int    // children still to start, this one included
	// reserve, if set, takes the child's grant instead of ReserveShare(share):
	// parallel children count and reserve in one step.
	reserve func() (budget.Grant, error)
	prompt  string
	judge   bool // read-only, and its worktree is never collected
}

// child runs one harness on the prompt in a worktree of its own: reserve a
// share of the budget, create the worktree, open the session, run the turn,
// charge it and collect what changed.
func (e *runEnv) child(h harness.Harness, sp childSpec) *Candidate {
	c := &Candidate{Harness: h.Name(), Outcome: candUnusable}
	sctx, cancel := context.WithDeadline(e.ctx, e.ledger.Deadline())
	defer cancel()
	expired := func() bool { return errors.Is(sctx.Err(), context.DeadlineExceeded) && e.ctx.Err() == nil }
	if sp.reserve == nil {
		sp.reserve = func() (budget.Grant, error) { return e.ledger.ReserveShare(sp.share) }
	}
	grant, err := sp.reserve()
	if err != nil {
		c.Outcome, c.Error = candExhausted, err.Error()
		return c
	}
	settle := func(cancelled bool) {
		st := e.ledger.Settle(grant, cancelled)
		c.Settlement = &st
	}
	exhausted := func(step string) *Candidate {
		settle(false)
		c.Outcome, c.Error = candExhausted, "budget exhausted: --mins ran out during "+step
		return c
	}
	id, name := e.run.ID, h.Name()
	if sp.suffix != "" {
		id += "-" + sp.suffix
		name = sp.suffix
	}
	wt, err := worktree.CreateAt(sctx, e.root, filepath.Join(e.o.WorktreeDir, id), "refraktal/"+id, e.base)
	if err != nil {
		if expired() {
			return exhausted("worktree creation")
		}
		settle(false)
		c.Error = err.Error()
		return c
	}
	c.wt = &wt
	removeWT := func() {
		if err := e.remove(wt); err != nil {
			e.log.Event("worktree_remove_failed", map[string]any{"harness": name, "error": runlog.ErrorField("worktree_remove_failed", err.Error())})
		}
		c.wt = nil
	}
	e.log.Event("worktree", map[string]any{"harness": name, "branch": wt.Branch, "base": wt.Base})
	hcfg := e.cfg.For(h.Name())
	e.log.Event("harness", map[string]any{"harness": h.Name(), "mcp": hcfg.MCP, "judge": sp.judge})
	if hooks := harness.ProjectHooks(wt.Dir, h.Name()); len(hooks) > 0 {
		e.notice(fmt.Sprintf("this repository configures %s hooks (%s); they run inside the child, outside Refraktal's permission policy",
			h.Name(), strings.Join(hooks, ", ")))
	}
	scratch, tmp := filepath.Join(e.run.Dir, name), filepath.Join(e.run.Dir, "tmp")
	if sp.suffix != "" {
		tmp += "-" + sp.suffix
	}
	var scratchRoot *os.Root
	for _, d := range []string{filepath.Base(scratch), filepath.Base(tmp)} {
		if err = e.run.Mkdir(d, 0o700); err != nil {
			break
		}
	}
	if err == nil {
		scratchRoot, err = e.run.OpenRoot(filepath.Base(scratch))
	}
	if err != nil {
		settle(false)
		removeWT()
		c.Error = err.Error()
		return c
	}
	c.root = scratchRoot
	// Durable events carry an error's code and digest only; harness error
	// text can hold account details, so the raw text goes to the gitignored
	// stderr capture beside the harness's own.
	errEvent := func(code, raw string) map[string]any {
		runlog.AppendRaw(scratchRoot, "stderr", code, raw)
		return runlog.ErrorField(code, raw)
	}
	// Confined: the harness works in the session's subfolder of its
	// worktree, and its edits are allowed only there.
	cwd := wt.Dir
	if e.subtree != "" {
		cwd = filepath.Join(wt.Dir, filepath.FromSlash(e.subtree))
		if err := os.MkdirAll(cwd, 0o755); err != nil {
			settle(false)
			removeWT()
			c.Error = err.Error()
			return c
		}
	}
	pol := policy.Policy{Worktree: cwd, ReadOnly: sp.judge}
	var mu sync.Mutex
	var reply strings.Builder
	// The prompt about to be sent counts as one of the turns: a child
	// Refraktal has the rest.
	childBudget, _ := json.Marshal(map[string]any{"usd": grant.USD, "turns": max(0, grant.Turns-1),
		"deadline": grant.Deadline.UTC().Format(time.RFC3339), "depth": grant.Depth})
	s, err := openSession(sctx, sessionSpec{h: h, hcfg: hcfg, scratch: scratch, scratchRoot: scratchRoot, cwd: cwd, tmpDir: tmp,
		liveDir: e.o.LiveDir, labels: map[string]string{"run": e.run.ID, "harness": name}, log: e.log, timeout: 2 * time.Minute,
		readOnly: sp.judge,
		// REFRAKTAL_RUN tags the child's processes for logs only (the session
		// sweep finds leftovers). REFRAKTAL_BUDGET is what a child Refraktal
		// may use.
		// REFRAKTAL_PARENT_RUN: a child Refraktal makes its run folder here,
		// inside this run's.
		env:       append(childEnv(e.root, e.o, hcfg, h.Name(), e.nestCeiling, e.run.ID+"-"+name, string(childBudget)), ParentRunEnv+"="+scratch),
		prefixExe: e.o.ShellPrefix,
		onPermission: func(_ context.Context, req acp.PermissionRequest) string {
			d := pol.Decide(req)
			mu.Lock()
			if d.Allow {
				c.Allowed++
			} else {
				c.Denied++
			}
			mu.Unlock()
			e.log.Event("permission", map[string]any{"harness": name, "decision": d})
			return d.OptionID
		},
		onUpdate: func(u acp.Update) {
			if u.Kind == "agent_message_chunk" {
				mu.Lock()
				reply.WriteString(chunkText(u))
				mu.Unlock()
			}
		}})
	c.MCP = s.launch.MCP
	closeSession := func(grace time.Duration) proc.ShutdownReport {
		sr := s.close(e.log, name, grace)
		c.LeftRunning, c.Unknown = shutdownCounts(sr)
		if c.LeftRunning > 0 || c.Unknown > 0 {
			e.notice(fmt.Sprintf("%s: shutdown incomplete: %d still running, %d unknown; live record kept for retry", name, c.LeftRunning, c.Unknown))
		}
		return sr
	}
	if c.MCP == harness.MCPInherit {
		e.notice(h.Name() + " uses your own MCP servers and plugins (mcp: inherit in the config)")
	}
	if err != nil {
		closeSession(5 * time.Second)
		c.Error = fmt.Sprintf("%s: %v", statusOf(err), err)
		code := statusOf(err)
		if expired() {
			// --mins ran out while the harness was starting: a spent budget.
			c.Outcome, c.Error = candExhausted, "budget exhausted: --mins ran out while starting the harness ("+c.Error+")"
			code = RunExhausted
		}
		settle(false)
		removeWT()
		e.log.Event("child_end", map[string]any{"harness": name, "outcome": c.Outcome, "error": errEvent(code, c.Error)})
		return c
	}
	if err := e.ledger.BeginTurn(grant); err != nil {
		closeSession(5 * time.Second)
		removeWT()
		settle(false)
		c.Outcome, c.Error = candExhausted, err.Error()
		e.log.Event("child_end", map[string]any{"harness": name, "outcome": c.Outcome, "error": errEvent(RunExhausted, c.Error)})
		return c
	}
	e.account(0, 0, 1)
	tctx, tcancel := context.WithDeadline(e.ctx, grant.Deadline)
	turn, perr := s.client.Prompt(tctx, s.id, sp.prompt)
	c.timedOut = errors.Is(tctx.Err(), context.DeadlineExceeded)
	tcancel()
	sr := closeSession(10 * time.Second)
	c.swept = len(sr.Swept.Killed)
	c.Model = cmpOr(turn.Model, s.model)
	// A turn is cancelled if we sent session/cancel or the harness ended it
	// as cancelled on its own (Codex does after a denial).
	turn.Cancelled = turn.Cancelled || turn.StopReason == "cancelled"
	var cost sessionCost // a fresh session: its first report is the whole cost
	charged, childRetained := 0.0, 0.0
	for _, ch := range cost.charges(h.Name(), c.Model, turn, e.prices) {
		e.ledger.Charge(grant, ch)
		if ch.Source != budget.Estimated {
			charged += ch.USD
		}
		e.log.Event("charge", map[string]any{"harness": name, "charge": ch})
		if ch.Source == budget.Unpriced {
			e.notice(fmt.Sprintf("warning: no price for model %q; its tokens count as $0 against --usd", ch.Model))
		}
	}
	if ch := turn.Child; ch != nil {
		// A child Refraktal's own prompts count against --turns too, and
		// the reservations its cancelled children kept are held here, not
		// reported as spent.
		e.ledger.AddTurns(grant, ch.Turns)
		e.account(0, 0, ch.Turns)
		if ch.Retained > 0 && !math.IsNaN(ch.Retained) && !math.IsInf(ch.Retained, 0) {
			e.ledger.Charge(grant, budget.Charge{USD: ch.Retained, Source: budget.Retained, Note: "kept by the child Refraktal's cancelled children"})
			e.account(0, ch.Retained, 0)
			childRetained = ch.Retained
		}
		e.log.Event("child_run", map[string]any{"harness": name, "turns": ch.Turns, "outcome": ch.Outcome, "exit": ch.Exit})
	}
	settle(turn.Cancelled)
	e.account(charged, 0, 0)
	if kept := c.Settlement.Spent - charged - childRetained; turn.Cancelled && kept > 0 {
		e.account(0, kept, 0) // the reservation a cancelled child keeps
	}
	if st := c.Settlement; st.Overrun > 0 {
		e.notice(fmt.Sprintf("the turn cost $%.4f, $%.4f beyond the --usd grant: --usd is checked before a turn, never inside one", st.Spent, st.Overrun))
	}
	mu.Lock()
	c.Reply = strings.TrimSpace(reply.String())
	mu.Unlock()
	c.StopReason, c.Usage, c.Cost = turn.StopReason, turn.Usage, turn.Cost
	e.log.Event("turn", map[string]any{"harness": name, "stop_reason": turn.StopReason, "cancelled": turn.Cancelled, "wall_ms": turn.Wall.Milliseconds(),
		"usage": turn.Usage, "cost": turn.Cost, "last_used": turn.LastUsed, "error": errEvent(turnErrorCode(perr), errString(perr)),
		"left_running": c.LeftRunning, "unknown": c.Unknown, "exited_on_its_own": sr.ExitedOnItsOwn})
	switch {
	case c.timedOut:
		c.Outcome = candTimedOut
	case perr != nil || turn.StopReason != "end_turn":
		c.Outcome, c.Error = candFailed, errString(perr)
	default:
		c.Outcome = candNoChanges
	}
	if sp.judge { // the judge's worktree is never collected, but must be unchanged
		sctx, scancel := e.opCtx()
		changed, err := wt.Changed(sctx)
		scancel()
		if err != nil || len(changed) > 0 {
			c.Files = changed
			c.Outcome = candFailed
			c.Error = "the judge changed its worktree, so its answer is not used"
			if err != nil {
				c.Error = "the judge's worktree could not be checked, so its answer is not used"
			}
			e.notice(fmt.Sprintf("%s: %s (%d paths)", name, c.Error, len(changed)))
		}
		removeWT()
		e.log.Event("child_end", map[string]any{"harness": name, "outcome": c.Outcome, "judge": true, "swept": c.swept,
			"changed": len(c.Files), "allowed": c.Allowed, "denied": c.Denied})
		return c
	}
	// Collected later (collect), once every child of the run has stopped.
	c.id, c.suffix, c.name = id, sp.suffix, name
	c.pending = true
	return c
}

// collect commits what a candidate changed and classifies it. It runs only
// once every child of the run has stopped and been swept: a still-running
// harness shares the repository's .git and could change its configuration
// (a filter, say) between driver discovery and the collect. A failed collect
// keeps the candidate's worktree and branch: its work is never deleted.
func (e *runEnv) collect(c *Candidate) {
	if !c.pending || c.wt == nil {
		return
	}
	c.pending = false
	errEvent := func(code, raw string) map[string]any {
		if c.root != nil {
			runlog.AppendRaw(c.root, "stderr", code, raw)
		}
		return runlog.ErrorField(code, raw)
	}
	if testBeforeCollect != nil {
		testBeforeCollect(c.wt.Dir)
	}
	// Retain the result even after cancellation, with a bounded collection.
	collectCtx, cancelCollect := e.opCtx()
	r, cerr := c.wt.Collect(collectCtx, "refraktal "+c.id+": "+firstLine(e.o.Task))
	cancelCollect()
	c.result, c.Files = r, r.Files
	if len(r.Diff) > 0 {
		e.run.WriteFile(resultName(c.suffix), r.Diff, 0o600)
	}
	switch {
	case cerr != nil:
		c.Outcome, c.Error, c.collectFailed = candFailed, cerr.Error(), true
	case len(outside(e.subtree, r.Files)) > 0:
		// Confined to the session's folder: a change outside it is refused.
		c.Outcome = candFailed
		c.Error = fmt.Sprintf("the result changes %d files outside the session folder %s: refused", len(outside(e.subtree, r.Files)), e.subtree)
	case c.Outcome == candNoChanges && len(r.Files) > 0:
		c.Outcome = candChanges
	}
	e.log.Event("collected", map[string]any{"harness": c.name, "files": len(c.Files), "failed": c.collectFailed})
	e.log.Event("child_end", map[string]any{"harness": c.name, "outcome": c.Outcome, "files": c.Files, "allowed": c.Allowed,
		"denied": c.Denied, "error": errEvent(c.Outcome, c.Error), "settlement": c.Settlement, "swept": c.swept})
}

func resultName(suffix string) string {
	if suffix == "" {
		return "result.diff"
	}
	return "result-" + suffix + ".diff"
}

// opCtx bounds a git operation that must finish once started (collect,
// apply, remove), even after the run is cancelled.
func (e *runEnv) opCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(e.ctx), safegit.OperationTimeout)
}

func (e *runEnv) remove(wt worktree.Worktree) error {
	ctx, cancel := e.opCtx()
	defer cancel()
	return wt.Remove(ctx)
}

// apply applies a candidate to the working tree: checked first, never
// stopped halfway. It returns a notice about LFS pointers, if any.
func (e *runEnv) apply(c *Candidate) (string, error) {
	if testBeforeApply != nil {
		testBeforeApply(e.ledger.Deadline())
	}
	// Never apply once --mins has expired: the apply itself is quick and
	// atomic, so this is the last point to stop.
	if !time.Now().Before(e.ledger.Deadline()) {
		return "", errMinsExpired
	}
	// A cancel (Ctrl-C, or the ACP client's session/cancel) that arrived
	// during judging or collection stops here: nothing reaches the tree.
	if e.ctx.Err() != nil {
		return "", errCancelled
	}
	if out := outside(e.subtree, c.result.Files); len(out) > 0 {
		return "", fmt.Errorf("the result changes %d files outside the session folder %s: refused", len(out), e.subtree)
	}
	ctx, cancel := e.opCtx()
	defer cancel()
	return c.wt.Apply(ctx, c.result)
}

// errMinsExpired: --mins ran out before the winner could be applied.
var errMinsExpired = errors.New("--mins ran out before the result could be applied; it is kept on its branch")

// testBeforeCollect, set by tests, runs just before a candidate is collected
// (with its worktree folder).
var testBeforeCollect func(dir string)

// errCancelled: the run was cancelled before the result could be applied.
var errCancelled = errors.New("cancelled before the result could be applied; it is kept on its branch")

// testBeforeApply, set by tests, runs just before the apply's deadline check.
var testBeforeApply func(deadline time.Time)

// keep marks a candidate's worktree as kept for the user.
func (c *Candidate) keep() {
	if c.wt != nil {
		c.Branch, c.Worktree = c.wt.Branch, c.wt.Dir
	}
}

// drop removes a candidate's worktree and branch unless it is kept.
func (e *runEnv) drop(c *Candidate) {
	if c.root != nil {
		c.root.Close()
		c.root = nil
	}
	if c.wt == nil || c.Worktree != "" {
		return
	}
	if err := e.remove(*c.wt); err != nil {
		e.log.Event("worktree_remove_failed", map[string]any{"harness": c.Harness, "error": runlog.ErrorField("worktree_remove_failed", err.Error())})
	}
	c.wt = nil
}

// sessionCost turns a session's reported costs into per-turn charges.
// Claude reports a cumulative cost for the session, so each turn is charged
// the difference from the previous report.
type sessionCost struct{ prev float64 }

func (s *sessionCost) charges(harnessName, model string, t acp.Turn, prices price.Table) []budget.Charge {
	out := charges(harnessName, model, t, prices)
	for i := range out {
		if out[i].Source == budget.Reported {
			total := out[i].USD
			out[i].USD = max(0, total-s.prev)
			s.prev = total
		}
	}
	return out
}

// fanOut runs every harness in parallel, judges the candidates and applies
// (or keeps) the winner.
func (e *runEnv) fanOut(hs []harness.Harness, scoreKey string, res *RunResult) {
	cands := make([]*Candidate, len(hs))
	var wg sync.WaitGroup
	var started sync.Mutex
	left := len(hs)
	for i, h := range hs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each child reserves what is left divided by the children still
			// to start, so parallel children split --usd. Counting and
			// reserving are one step: a child counted later must not reserve
			// first and take the others' shares.
			reserve := func() (budget.Grant, error) {
				started.Lock()
				defer started.Unlock()
				n := left
				left--
				return e.ledger.ReserveShare(n)
			}
			cands[i] = e.child(h, childSpec{suffix: h.Name(), reserve: reserve, prompt: e.o.Task})
		}()
	}
	wg.Wait()
	// Every child has stopped and been swept: only now is anything collected.
	for _, c := range cands {
		e.collect(c)
	}
	for i, c := range cands {
		c.Label = string(rune('A' + i))
	}
	res.Candidates = nil
	res.Harness = "" // set only for a winner, never the pinned list
	finish := func(outcome, why string, keepAll bool) {
		res.Outcome = outcome
		if why != "" {
			res.Error = why
		}
		for _, c := range cands {
			if c.collectFailed || keepAll && (len(c.Files) > 0 || e.o.NoApply) {
				c.keep()
			}
			e.drop(c)
			res.Allowed += c.Allowed
			res.LeftRunning += c.LeftRunning
			res.Unknown += c.Unknown
			res.Denied += c.Denied
			res.Candidates = append(res.Candidates, *c)
		}
	}
	var changed, unusable, timedOut, exhausted, nochange int
	for _, c := range cands {
		switch c.Outcome {
		case candChanges:
			changed++
		case candUnusable:
			unusable++
		case candTimedOut:
			timedOut++
		case candExhausted:
			exhausted++
		case candNoChanges:
			nochange++
		}
	}
	switch {
	case timedOut > 0:
		finish(RunTimedOut, "--mins ran out during the candidates' turns", true)
		return
	case changed == 0 && unusable == len(cands):
		finish(RunUnusable, "no harness could be started", false)
		return
	case exhausted > 0:
		// A planned candidate could not run for budget: the run did not get
		// to try everything it planned, so nothing is chosen or applied, even
		// from the candidates that ran. Exit 2, candidates kept.
		finish(RunExhausted, "budget exhausted before every candidate could run", true)
		return
	case changed == 0 && nochange > 0:
		finish(RunNoChanges, "", e.o.NoApply)
		return
	case changed == 0:
		finish(RunFailed, "no candidate finished its turn", true)
		return
	}

	// 1. Deterministic checks, decided from the base commit.
	plan := planChecks(e.ctx, e.root, e.base, e.cfg, e.o.ConfigPath)
	if plan.Note != "" {
		e.notice(plan.Note)
	}
	e.log.Event("checks", map[string]any{"source": plan.Source, "steps": plan.Steps, "timeout_mins": plan.Timeout.Minutes()})
	cctx, ccancel := context.WithDeadline(e.ctx, e.ledger.Deadline())
	defer ccancel()
	var cw sync.WaitGroup
	for _, c := range cands {
		if c.Outcome != candChanges {
			continue
		}
		cw.Add(1)
		go func() {
			defer cw.Done()
			actx, acancel := context.WithTimeout(cctx, safegit.OperationTimeout)
			ok := c.wt.CheckApply(actx, c.result) == nil
			acancel()
			c.Applies = &ok
			c.Flagged = plan.touchesDefinition(c.Files, c.result.Diff)
			if !ok {
				return
			}
			if err := c.root.Mkdir("check-tmp", 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				c.Checks = append(c.Checks, CheckResult{Name: "setup", Status: CheckError, Exit: -1})
				return
			}
			env := checkEnv(filepath.Join(e.run.Dir, c.Harness, "check-tmp"))
			for _, s := range plan.Steps {
				r := runCheck(cctx, c.wt.Dir, env, c.root, "check-"+s.Name+".log", e.o.LiveDir,
					map[string]string{"run": e.run.ID, "harness": c.Harness, "check": s.Name}, s, plan.Timeout)
				c.Checks = append(c.Checks, r)
				if raw, safe, ok := r.cleanupNotice(c.Harness); ok {
					e.rawNotice(raw, safe)
				}
				if r.Status != CheckPassed {
					break
				}
			}
		}()
	}
	cw.Wait()
	for _, c := range cands {
		if c.Outcome == candChanges {
			e.log.Event("checked", map[string]any{"harness": c.Harness, "applies": c.Applies, "checks": c.Checks, "flagged": c.Flagged})
		}
	}
	if !time.Now().Before(e.ledger.Deadline()) {
		finish(RunTimedOut, "--mins ran out during the checks", true)
		return
	}

	// 2. The System 1 pre-score, only when the router ran with a key.
	scored := scoreKey != ""
	if scored {
		var sw sync.WaitGroup
		var failed sync.Once
		for _, c := range cands {
			if c.Outcome != candChanges || c.rank(plan) == rankOut {
				continue
			}
			sw.Add(1)
			go func() {
				defer sw.Done()
				p, trunc, err := route.Gemini{APIKey: scoreKey, Endpoint: e.o.RouterEndpoint}.Score(cctx, e.o.Task, c.result.Diff)
				if err != nil {
					failed.Do(func() {
						e.rawNotice("pre-score unavailable ("+err.Error()+"); the checks decide, and ties go to the judge",
							"pre-score unavailable; the checks decide, and ties go to the judge")
					})
					e.log.Event("pre_score", map[string]any{"harness": c.Harness, "error": runlog.ErrorField("pre_score_failed", err.Error())})
					return
				}
				c.Score, c.ScoreTruncated = &p, trunc
				e.log.Event("pre_score", map[string]any{"harness": c.Harness, "p": p, "truncated": trunc})
			}()
		}
		sw.Wait()
		for _, c := range cands {
			if c.Outcome == candChanges && c.rank(plan) > rankOut && c.Score == nil {
				scored = false
			}
		}
	}

	// 3. The harness judge, only for a close call.
	winner, close, v := choose(cands, plan, scored, e.cfg.MarginOr())
	if close != nil {
		e.log.Event("judge_start", map[string]any{"close_call": v.CloseCall, "reason": v.Reason})
		var j *Candidate
		prompt := judgePrompt(e.o.Task, close)
		for _, jh := range judgeOrder(e.o.Harnesses, hs, cands, close) {
			j = e.child(jh, childSpec{suffix: "judge-" + jh.Name(), share: 1, prompt: prompt, judge: true})
			if j.Outcome == candExhausted {
				break // refused before it started: no judge to name
			}
			if j.Outcome != candUnusable {
				v.Judge = jh.Name()
				break
			}
			// Could not be started or logged in: no turn was spent; try the next.
			v.JudgesTried = append(v.JudgesTried, jh.Name())
			e.log.Event("judge_unusable", map[string]any{"judge": jh.Name()})
		}
		for _, c := range close {
			if c.Harness == v.Judge {
				v.JudgedOwn = true
			}
		}
		if v.JudgedOwn {
			e.notice(fmt.Sprintf("the judge, %s, is one of the close-call candidates: no other harness was available and usable", v.Judge))
		}
		res.Judge = j
		if j != nil {
			res.LeftRunning += j.LeftRunning
			res.Unknown += j.Unknown
		}
		var labels []string
		for _, c := range close {
			labels = append(labels, c.Label)
		}
		w := parseWinner(j.Reply, labels)
		e.log.Event("judge_end", map[string]any{"judge": v.Judge, "judged_own": v.JudgedOwn, "tried": v.JudgesTried, "outcome": j.Outcome, "answer": w})
		switch {
		case j.Outcome == candTimedOut:
			res.Verdict = &v
			finish(RunTimedOut, "--mins ran out while the judge was deciding a close call", true)
			return
		case j.Outcome == candExhausted:
			res.Verdict = &v
			finish(RunExhausted, "budget exhausted before the judge could decide a close call ("+j.Error+")", true)
			return
		case j.Outcome == candFailed:
			// Cancelled, max_tokens or an ACP error: its answer does not
			// count. The checks ranking decides; a flagged candidate cannot
			// win that way.
			why := "the judge did not finish (" + cmpOr(j.StopReason, "error") + ")"
			if len(j.Files) > 0 || strings.HasPrefix(j.Error, "the judge") {
				why = "the judge's answer is not used (it changed its worktree, or it could not be checked)"
			}
			for _, c := range close {
				if c.Flagged == "" {
					winner = c
					break
				}
			}
			if winner == nil {
				v.Method, v.Reason = "none", why+" and no candidate in the close call can win on checks"
				res.Verdict = &v
				e.notice(v.Reason)
				finish(RunNoWinner, v.Reason, true)
				return
			}
			v.Winner, v.Method = winner.Harness, "checks"
			v.Reason = why + "; the checks ranking decides"
			e.notice(fmt.Sprintf("%s: falling back to the checks ranking, which puts %s first", why, winner.Harness))
		case w == "" || w == "none":
			v.Method = "judge"
			if w == "none" {
				v.Reason = "the judge found no candidate correct"
			} else {
				v.Reason = "the judge gave no valid answer (" + j.Outcome + ")"
			}
			res.Verdict = &v
			finish(RunNoWinner, v.Reason, true)
			return
		default:
			for _, c := range close {
				if c.Label == w {
					winner = c
				}
			}
			v.Winner, v.Method, v.Reason = winner.Harness, "judge", "the judge chose candidate "+w+" in a close call"
		}
	}
	res.Verdict = &v
	e.log.Event("verdict", map[string]any{"verdict": v})
	if winner == nil {
		finish(RunNoWinner, v.Reason, true)
		return
	}
	res.Harness, res.Files, res.Reply, res.Model = winner.Harness, winner.Files, winner.Reply, winner.Model
	switch {
	case e.o.NoApply:
		finish(RunKept, "", true)
		res.Branch, res.Worktree = winner.wtKept(res.Candidates)
	default:
		n, err := e.apply(winner)
		if errors.Is(err, errMinsExpired) {
			finish(RunTimedOut, err.Error(), true)
			res.Branch, res.Worktree = winner.wtKept(res.Candidates)
			return
		}
		if errors.Is(err, errCancelled) {
			finish(RunFailed, err.Error(), true)
			res.Branch, res.Worktree = winner.wtKept(res.Candidates)
			return
		}
		if err != nil {
			finish(RunApplyFailed, err.Error(), true)
			res.Branch, res.Worktree = winner.wtKept(res.Candidates)
			return
		}
		if n != "" {
			e.rawNotice(n, "some LFS-tracked files were left as LFS pointers")
		}
		finish(RunApplied, "", false)
	}
}

// wtKept returns the winner's kept branch and worktree from the final list.
func (c *Candidate) wtKept(all []Candidate) (string, string) {
	for _, k := range all {
		if k.Harness == c.Harness {
			return k.Branch, k.Worktree
		}
	}
	return "", ""
}

// judgeOrder is the order judges are tried in: harnesses outside the close
// call first (this run's that could be started, then the other configured
// ones), then the close-call candidates. A judge that cannot be started is
// skipped for the next.
func judgeOrder(all, run []harness.Harness, cands []*Candidate, close []*Candidate) []harness.Harness {
	inClose, ran := map[string]bool{}, map[string]bool{}
	for _, c := range close {
		inClose[c.Harness] = true
	}
	var out []harness.Harness
	for i, h := range run {
		ran[h.Name()] = true
		if !inClose[h.Name()] && cands[i].Outcome != candUnusable && h.ReadOnlyMode() != "" {
			out = append(out, h)
		}
	}
	for _, h := range all {
		if !ran[h.Name()] && h.ReadOnlyMode() != "" { // a harness with no read-only mode never judges
			out = append(out, h)
		}
	}
	for _, c := range close {
		for _, h := range run {
			if h.Name() == c.Harness && h.ReadOnlyMode() != "" {
				out = append(out, h)
			}
		}
	}
	return out
}
