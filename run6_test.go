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

// keptWhole reports whether the run kept a child's whole grant: a forfeited
// charge, every dollar of the $5 grant and every one of its 5 turns.
func keptWhole(res RunResult) bool {
	forfeited := false
	for _, ch := range res.Budget.Charges {
		forfeited = forfeited || ch.Source == budget.Forfeited
	}
	return forfeited && res.Budget.Spent >= 5 && res.Budget.Turns == 5
}

// A refraktal child whose turn ends without a valid report may have spent
// its whole grant on its own harnesses without telling us: the parent keeps
// that grant, dollars and turns, whatever the reason. A turn that otherwise
// ended normally (end_turn) makes the missing report untrusted: forfeit,
// exhausted, exit 2, nothing applied.
func TestRefraktalChildWithoutReportForfeits(t *testing.T) {
	for _, result := range []string{
		`{"_meta":5}`,                     // _meta present, not an object
		`{"_meta":null}`,                  // no _meta
		`{"_meta":{}}`,                    // no _meta.refraktal
		`{"_meta":{"refraktal":null}}`,    // a null report
		`{"_meta":{"other":{"turns":0}}}`, // someone else's metadata only
	} {
		t.Run(result, func(t *testing.T) {
			r := newRunRig(t)
			res := r.run(t, writer("refraktal", "out.txt=work", forgeResultEnv+"="+result), nil)
			if res.Outcome != RunExhausted || res.ExitCode() != ExitBudget || exists(filepath.Join(r.repo, "out.txt")) {
				t.Errorf("outcome %s exit %d applied %t (%s), want %s exit %d, nothing applied",
					res.Outcome, res.ExitCode(), exists(filepath.Join(r.repo, "out.txt")), res.Error, RunExhausted, ExitBudget)
			}
			if !keptWhole(res) || res.Branch == "" {
				t.Errorf("the grant was not kept whole, or the work not kept: spent %.4f turns %d branch %q", res.Budget.Spent, res.Budget.Turns, res.Branch)
			}
			if !strings.Contains(strings.Join(res.Notices, " "), "untrusted report (no report)") {
				t.Errorf("no untrusted-report notice: %q", res.Notices)
			}
		})
	}
}

// A refraktal child whose turn failed (an ACP error, a crash) stays failed,
// but its whole grant is still kept, with a notice.
func TestFailedRefraktalChildKeepsItsGrant(t *testing.T) {
	for _, mode := range []string{"error", "crash"} {
		t.Run(mode, func(t *testing.T) {
			r := newRunRig(t)
			res := r.run(t, writer("refraktal", "out.txt=work", forgePromptEnv+"="+mode), nil)
			if res.Outcome != RunFailed || res.ExitCode() != ExitNoCandidate || exists(filepath.Join(r.repo, "out.txt")) {
				t.Errorf("outcome %s exit %d applied %t (%s), want %s exit %d", res.Outcome, res.ExitCode(), exists(filepath.Join(r.repo, "out.txt")), res.Error, RunFailed, ExitNoCandidate)
			}
			if !keptWhole(res) {
				t.Errorf("the grant was not kept whole: spent %.4f turns %d charges %+v", res.Budget.Spent, res.Budget.Turns, res.Budget.Charges)
			}
			if !strings.Contains(strings.Join(res.Notices, " "), "ended without a report") {
				t.Errorf("no kept-grant notice: %q", res.Notices)
			}
		})
	}
	// An ordinary harness is unchanged: a failed turn returns its unused
	// grant, and a missing report is no report at all.
	for _, mode := range []string{"error", "crash"} {
		r := newRunRig(t)
		res := r.run(t, writer("claude", "out.txt=work", forgePromptEnv+"="+mode), nil)
		if res.Outcome != RunFailed || res.Budget.Spent >= 5 || strings.Contains(strings.Join(res.Notices, " "), "without a report") {
			t.Errorf("claude %s: outcome %s spent %.4f notices %q, want failed with the unused grant returned", mode, res.Outcome, res.Budget.Spent, res.Notices)
		}
	}
	r := newRunRig(t)
	if res := r.run(t, writer("claude", "out.txt=work", forgeResultEnv+`={"_meta":{}}`), nil); res.Outcome != RunApplied {
		t.Errorf("claude without a report: outcome %s (%s), want applied", res.Outcome, res.Error)
	}
}
