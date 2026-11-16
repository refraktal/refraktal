package refraktal

import (
	"path/filepath"
	"strings"
	"testing"

	"refraktal.dev/refraktal/internal/budget"
)

// Run-6 #1: a child report that is present but cannot be decoded (a number
// outside its field's range, a wrong type, a report that is not an object)
// is an untrusted report, exactly like an out-of-range one: the child is
// charged its whole grant, exit 2, nothing applied. The same holds for the
// response's usage, the other untrusted numbers a child sends there.
func TestUndecodableChildReportForfeitsTheGrant(t *testing.T) {
	for _, tc := range []struct{ result, why string }{
		// Astra's reproduction: one past MaxInt64.
		{`{"_meta":{"refraktal":{"turns":9223372036854775808,"outcome":"applied","exit":0}}}`, "an undecodable report"},
		{`{"_meta":{"refraktal":{"turns":-9223372036854775809}}}`, "an undecodable report"},
		{`{"_meta":{"refraktal":{"turns":1.5}}}`, "an undecodable report"},
		{`{"_meta":{"refraktal":{"turns":"1"}}}`, "an undecodable report"},
		{`{"_meta":{"refraktal":{"turns":0,"exit":9223372036854775808}}}`, "an undecodable report"},
		{`{"_meta":{"refraktal":{"turns":0,"retained_usd":1e999}}}`, "an undecodable report"},
		{`{"_meta":{"refraktal":{"turns":0,"retained_usd":"0"}}}`, "an undecodable report"},
		{`{"_meta":{"refraktal":{"turns":0,"outcome":7}}}`, "an undecodable report"},
		{`{"_meta":{"refraktal":"malformed"}}`, "an undecodable report"},
		{`{"_meta":{"refraktal":[0]}}`, "an undecodable report"},
		{`{"usage":{"inputTokens":9223372036854775808}}`, "malformed usage"},
		{`{"usage":{"outputTokens":"12"}}`, "malformed usage"},
		{`{"usage":"malformed"}`, "malformed usage"},
	} {
		t.Run(tc.result, func(t *testing.T) {
			r := newRunRig(t)
			res := r.run(t, writer("claude", "out.txt=work", forgeResultEnv+"="+tc.result), nil)
			forfeited := false
			for _, ch := range res.Budget.Charges {
				forfeited = forfeited || ch.Source == budget.Forfeited
			}
			if res.Outcome != RunExhausted || res.ExitCode() != ExitBudget || exists(filepath.Join(r.repo, "out.txt")) {
				t.Errorf("outcome %s exit %d applied %t (%s), want %s exit %d, nothing applied",
					res.Outcome, res.ExitCode(), exists(filepath.Join(r.repo, "out.txt")), res.Error, RunExhausted, ExitBudget)
			}
			if !forfeited || res.Budget.Spent < 5 || res.Branch == "" {
				t.Errorf("the grant was not forfeited, or the work not kept: forfeited %t spent %.4f turns %d branch %q",
					forfeited, res.Budget.Spent, res.Budget.Turns, res.Branch)
			}
			if !strings.Contains(strings.Join(res.Notices, " "), "untrusted report ("+tc.why+")") {
				t.Errorf("no %q notice: %q", tc.why, res.Notices)
			}
		})
	}
}

// A null report is an absent one (a plain harness), and a plain harness's
// undecodable usage still fails its turn as before: neither forfeits. A
// negative token count decodes: it is validated as before (unpriced, and
// unused beside a valid cost), so it does not forfeit either.
func TestChildResponseControls(t *testing.T) {
	r := newRunRig(t)
	if res := r.run(t, writer("claude", "out.txt=work", forgeResultEnv+`={"_meta":{"refraktal":null}}`), nil); res.Outcome != RunApplied {
		t.Errorf("a null report: outcome %s (%s), want applied", res.Outcome, res.Error)
	}
	r = newRunRig(t)
	res := r.run(t, writer("claude", "out.txt=work", forgeResultEnv+`={"_meta":null,"usage":{"inputTokens":"12"}}`), nil)
	if res.Outcome != RunFailed || res.ExitCode() != ExitNoCandidate || strings.Contains(strings.Join(res.Notices, " "), "untrusted") {
		t.Errorf("a plain harness's undecodable usage: outcome %s exit %d notices %q, want failed exit 1, no forfeit", res.Outcome, res.ExitCode(), res.Notices)
	}
	r = newRunRig(t)
	if res := r.run(t, writer("claude", "out.txt=work", forgeResultEnv+`={"usage":{"inputTokens":-5,"outputTokens":1,"totalTokens":1}}`), nil); res.Outcome != RunApplied {
		t.Errorf("a negative token count beside a valid cost: outcome %s (%s), want applied", res.Outcome, res.Error)
	}
}
