package refraktal

import (
	"os"
	"path/filepath"
	"testing"

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
