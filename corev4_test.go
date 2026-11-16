package refraktal

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"refraktal.dev/refraktal/internal/acp"
	"refraktal.dev/refraktal/internal/budget"
	"refraktal.dev/refraktal/internal/harness"
)

// Run-4 #3: a candidate that fails after making changes (max_tokens here) is
// kept, branch and worktree, and a fan-out with no applicable winner in
// which a candidate failed with changes ends failed, exit 1: both when the
// other candidate made no changes and when it made changes that lose.
func TestFailedCandidateWithChangesIsKept(t *testing.T) {
	partial := writer("claude", "partial.txt=valuable partial work", "ACPTEST_JUDGE=stop=max_tokens\npartial")
	for _, tc := range []struct {
		name  string
		other harness.Harness
		check bool
	}{
		{"other made no changes", fake{name: "codex", agentMode: "turn"}, false},
		{"other's changes fail the checks", writer("codex", "out.txt=bad"), true},
	} {
		r := newRunRig(t)
		if tc.check {
			r.checkConfig(t, "")
		}
		// The fake ends with ACPTEST_JUDGE's stop reason only for a prompt
		// that starts "You are judging"; the other candidates answer as usual.
		res := r.fan(t, []harness.Harness{partial, tc.other}, func(o *RunOptions) { o.Task = "You are judging this change" })
		c := cand(res, "claude")
		if res.Outcome != RunFailed || res.ExitCode() != ExitNoCandidate {
			t.Errorf("%s: outcome %s exit %d, want failed exit %d", tc.name, res.Outcome, res.ExitCode(), ExitNoCandidate)
		}
		if c.Branch == "" || !exists(filepath.Join(c.Worktree, "partial.txt")) {
			t.Errorf("%s: the failed candidate's work was not kept: %+v", tc.name, c)
		}
		if b, _ := os.ReadFile(filepath.Join(r.repo, "partial.txt")); len(b) > 0 {
			t.Errorf("%s: a failed candidate was applied", tc.name)
		}
	}
}

// V3 (run-4 #4): a child report that cannot be trusted (a turn count outside
// 0..the child's remaining allowance, a cost or retained amount that is not
// finite and non-negative, or retained beyond the grant) charges the child
// its whole grant, marks it exhausted with a notice, and the run follows the
// exhaustion rule: exit 2, candidates kept, nothing applied. Nothing wraps.
func TestUntrustedChildReportForfeitsTheGrant(t *testing.T) {
	forged := map[string]func(*acp.Turn){
		"turns MaxInt":   func(t *acp.Turn) { t.Child = &acp.ChildReport{Turns: math.MaxInt} },
		"turns negative": func(t *acp.Turn) { t.Child = &acp.ChildReport{Turns: -1} },
		"turns past":     func(t *acp.Turn) { t.Child = &acp.ChildReport{Turns: 1000} },
		"cost negative":  func(t *acp.Turn) { t.Child, t.Cost = &acp.ChildReport{}, &acp.Cost{Amount: -3, Currency: "USD"} },
		"cost infinite": func(t *acp.Turn) {
			t.Child, t.Cost = &acp.ChildReport{}, &acp.Cost{Amount: math.Inf(1), Currency: "USD"}
		},
		"retained NaN":   func(t *acp.Turn) { t.Child = &acp.ChildReport{Retained: math.NaN()} },
		"retained > usd": func(t *acp.Turn) { t.Child = &acp.ChildReport{Retained: 1e9} },
		// A real cost beyond the grant is still charged in full: forfeiting
		// adds the unspent rest of a grant, never caps an overrun.
		"turns past, overrun": func(t *acp.Turn) {
			t.Child, t.Cost = &acp.ChildReport{Turns: 1000}, &acp.Cost{Amount: 8, Currency: "USD"}
		},
	}
	for name, forge := range forged {
		for _, fan := range []bool{false, true} {
			testChildTurn = func(h string, t *acp.Turn) {
				if h == "claude" {
					forge(t)
				}
			}
			r := newRunRig(t)
			var safe []string
			mut := func(o *RunOptions) { o.OnSafeNotice = func(n string) { safe = append(safe, n) } }
			var res RunResult
			if fan {
				res = r.fan(t, []harness.Harness{writer("claude", "out.txt=a"), writer("codex", "b.txt=b")}, mut)
			} else {
				res = r.run(t, writer("claude", "out.txt=a"), mut)
			}
			testChildTurn = nil
			where := fmt.Sprintf("%s (fan-out %v)", name, fan)
			if res.Outcome != RunExhausted || res.ExitCode() != ExitBudget {
				t.Errorf("%s: outcome %s exit %d, want budget_exhausted exit 2", where, res.Outcome, res.ExitCode())
			}
			if exists(filepath.Join(r.repo, "out.txt")) || exists(filepath.Join(r.repo, "b.txt")) {
				t.Errorf("%s: applied", where)
			}
			branch := res.Branch
			if fan {
				branch = cand(res, "claude").Branch
			}
			if branch == "" {
				t.Errorf("%s: the candidate was not kept", where)
			}
			if b := res.Budget; b.Turns < 0 || b.Spent < 0 || math.IsNaN(b.Spent) || math.IsInf(b.Spent, 0) || res.RunTurns < 0 {
				t.Errorf("%s: accounting wrapped or was poisoned: %+v, run turns %d", where, b, res.RunTurns)
			}
			// The run's own figure (what a parent is told) matches the ledger.
			if math.Abs(res.Charged-res.Budget.Spent) > 1e-9 {
				t.Errorf("%s: the run reports $%v charged, the ledger $%v spent", where, res.Charged, res.Budget.Spent)
			}
			if strings.Contains(name, "overrun") && res.Budget.Spent < 8 {
				t.Errorf("%s: a real $8 cost was capped at $%v", where, res.Budget.Spent)
			}
			forfeited := false
			for _, ch := range res.Budget.Charges {
				forfeited = forfeited || ch.Source == budget.Forfeited
			}
			// With an overrun nothing of the grant is left to forfeit.
			if forfeited == strings.Contains(name, "overrun") || !strings.Contains(strings.Join(safe, "\n"), "untrusted report") {
				t.Errorf("%s: no forfeit charge or notice: %+v %q", where, res.Budget.Charges, safe)
			}
		}
	}
}
