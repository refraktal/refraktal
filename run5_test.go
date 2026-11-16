package refraktal

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"refraktal.dev/refraktal/internal/acp"
	"refraktal.dev/refraktal/internal/acp/acptest"
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

// forgeCostEnv makes the test binary, run as the fake agent, replace the
// cost in its usage_update with this raw JSON and add a child Refraktal's
// report to its prompt response: a nested report with a malformed cost, as
// it reaches the wire.
const forgeCostEnv = "REFRAKTAL_TEST_FORGE_COST"

// runForgedAgent serves the fake agent through the forging writer when
// forgeCostEnv is set (called from TestMain).
func runForgedAgent() {
	raw := os.Getenv(forgeCostEnv)
	if raw == "" || os.Getenv("ACPTEST_MODE") == "" {
		return
	}
	w := &costForger{cost: json.RawMessage(raw)}
	acptest.Serve(os.Getenv("ACPTEST_MODE"), os.Stdin, w)
	os.Exit(0)
}

type costForger struct {
	cost json.RawMessage
	buf  []byte
}

func (f *costForger) Write(b []byte) (int, error) {
	f.buf = append(f.buf, b...)
	for {
		i := bytes.IndexByte(f.buf, '\n')
		if i < 0 {
			return len(b), nil
		}
		line := f.buf[:i]
		var m map[string]any
		if json.Unmarshal(line, &m) == nil {
			if p, ok := m["params"].(map[string]any); ok {
				if u, ok := p["update"].(map[string]any); ok && u["sessionUpdate"] == "usage_update" {
					u["cost"] = f.cost
				}
			}
			if r, ok := m["result"].(map[string]any); ok && r["stopReason"] != nil {
				r["_meta"] = map[string]any{"refraktal": map[string]any{"turns": 0, "retained_usd": 0, "outcome": "applied", "exit_code": 0}}
			}
			line, _ = json.Marshal(m)
		}
		if _, err := os.Stdout.Write(append(line, '\n')); err != nil {
			return 0, err
		}
		f.buf = f.buf[i+1:]
	}
}

// Run-5 #4: a nested cost that is present but malformed (negative, not a
// number, not an object, no amount) is an untrusted report, not an absent
// one: the child is charged its whole grant, exit 2, nothing applied.
func TestMalformedNestedCostForfeitsTheGrant(t *testing.T) {
	for _, cost := range []string{`{"amount":-1,"currency":"USD"}`, `{"amount":"malformed","currency":"USD"}`, `"malformed"`, `{"currency":"USD"}`} {
		t.Run(cost, func(t *testing.T) {
			r := newRunRig(t)
			res := r.run(t, writer("claude", "out.txt=work", forgeCostEnv+"="+cost), nil)
			if res.Outcome != RunExhausted || res.ExitCode() != ExitBudget || exists(filepath.Join(r.repo, "out.txt")) {
				t.Errorf("outcome %s exit %d applied %t, want %s exit %d, nothing applied", res.Outcome, res.ExitCode(), exists(filepath.Join(r.repo, "out.txt")), RunExhausted, ExitBudget)
			}
			if !strings.Contains(strings.Join(res.Notices, " "), "untrusted report (a malformed cost)") {
				t.Errorf("no untrusted-report notice: %q", res.Notices)
			}
			if res.Budget.Spent < 5 || res.Branch == "" {
				t.Errorf("the grant was not forfeited, or the work not kept: spent %.4f branch %q", res.Budget.Spent, res.Branch)
			}
		})
	}
	// An absent cost is still priced from tokens, and applies.
	r := newRunRig(t)
	if res := r.run(t, writer("claude", "out.txt=work", "ACPTEST_NOCOST=1"), nil); res.Outcome != RunApplied {
		t.Errorf("an absent cost: outcome %s (%s)", res.Outcome, res.Error)
	}
}
