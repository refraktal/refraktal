package refraktal

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"refraktal.dev/refraktal/internal/acp"
	"refraktal.dev/refraktal/internal/budget"
	"refraktal.dev/refraktal/internal/harness"
)

// Run-5 #3: budget exhaustion and --mins expiry win over a collect failure.
// A collect failure never upgrades an exhausted or timed-out run to applied
// and never downgrades exit 2 to exit 1; every candidate is kept.
func TestCollectFailureNeverOverridesExhaustion(t *testing.T) {
	t.Run("timed out, one candidate", func(t *testing.T) {
		r := newRunRig(t)
		testBeforeCollect = func(dir string) { lockIndex(t, dir) }
		defer func() { testBeforeCollect = nil }()
		res := r.run(t, fake{name: "claude", agentMode: "hang"}, func(o *RunOptions) {
			o.Budget = &budget.Limits{USD: 5, Turns: 5, Wall: 2 * time.Second, Depth: 2}
		})
		if res.Outcome != RunTimedOut || res.ExitCode() != ExitBudget {
			t.Errorf("outcome %s exit %d, want %s exit %d (error: %s)", res.Outcome, res.ExitCode(), RunTimedOut, ExitBudget, res.Error)
		}
		if res.Branch == "" || !strings.Contains(res.Error, "index.lock") {
			t.Errorf("the candidate was not kept, or the collect error was lost: branch %q error %q", res.Branch, res.Error)
		}
	})
	t.Run("untrusted report, fan-out", func(t *testing.T) {
		r := newRunRig(t)
		testChildTurn = func(h string, t *acp.Turn) {
			if h == "claude" {
				t.Child = &acp.ChildReport{Turns: 1000}
			}
		}
		testBeforeCollect = func(dir string) {
			if strings.HasSuffix(dir, "-claude") {
				lockIndex(t, dir)
			}
		}
		defer func() { testChildTurn, testBeforeCollect = nil, nil }()
		res := r.fan(t, []harness.Harness{writer("claude", "a.txt=work"), writer("codex", "out.txt=work")}, nil)
		if res.Outcome != RunExhausted || res.ExitCode() != ExitBudget {
			t.Errorf("outcome %s exit %d, want %s exit %d (error: %s)", res.Outcome, res.ExitCode(), RunExhausted, ExitBudget, res.Error)
		}
		if exists(filepath.Join(r.repo, "out.txt")) {
			t.Error("the sibling was applied after the untrusted report")
		}
		for _, name := range []string{"claude", "codex"} {
			if c := cand(res, name); c.Branch == "" {
				t.Errorf("%s was not kept: %+v", name, c)
			}
		}
		if c := cand(res, "claude"); c.Outcome != candExhausted || !strings.Contains(c.Error, "untrusted report") || !strings.Contains(c.Error, "index.lock") {
			t.Errorf("claude: outcome %s error %q, want exhausted with both the forfeit and the collect error", c.Outcome, c.Error)
		}
	})
}
