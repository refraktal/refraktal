package refraktal

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"refraktal.dev/refraktal/internal/acp"
	"refraktal.dev/refraktal/internal/budget"
	"refraktal.dev/refraktal/internal/harness"
	"refraktal.dev/refraktal/internal/proc"
)

// slowDiscovery wraps git so repository discovery (rev-parse
// --show-toplevel) takes delay.
func slowDiscovery(t *testing.T, delay time.Duration) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := "#!/bin/sh\ncase \" $* \" in *' --show-toplevel '*) /bin/sleep " + strconv.FormatFloat(delay.Seconds(), 'f', -1, 64) + ";; esac\n" +
		"exec '" + strings.ReplaceAll(realGit, "'", "'\\''") + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// Run-7 #4: --mins covers the whole run, setup included. Repository
// discovery and config loading ran before the deadline was set, so a slow
// discovery did not count: a 30 ms dry run that took 320 ms exited 0.
func TestSetupCountsAgainstMins(t *testing.T) {
	t.Run("dry run (Astra's reproduction)", func(t *testing.T) {
		r := newRunRig(t)
		slowDiscovery(t, 200*time.Millisecond)
		start := time.Now()
		res := r.run(t, fake{name: "claude", agentMode: "turn"}, func(o *RunOptions) {
			o.Dry = true
			o.Budget = &budget.Limits{USD: 5, Turns: 5, Wall: 30 * time.Millisecond, Depth: 2}
		})
		if res.Outcome != RunExhausted || res.ExitCode() != ExitBudget || !strings.Contains(res.Error, "repository discovery") {
			t.Errorf("after %s: outcome %s exit %d (%s), want %s exit %d during repository discovery",
				time.Since(start).Round(time.Millisecond), res.Outcome, res.ExitCode(), res.Error, RunExhausted, ExitBudget)
		}
	})
	t.Run("a run never starts the harness", func(t *testing.T) {
		r := newRunRig(t)
		slowDiscovery(t, 200*time.Millisecond)
		res := r.run(t, writer("claude", "out.txt=work"), func(o *RunOptions) {
			o.Budget = &budget.Limits{USD: 5, Turns: 5, Wall: 100 * time.Millisecond, Depth: 2}
		})
		if res.Outcome != RunExhausted || res.ExitCode() != ExitBudget || !strings.Contains(res.Error, "repository discovery") ||
			exists(filepath.Join(r.repo, "out.txt")) || exists(r.prompts) {
			t.Errorf("outcome %s exit %d (%s), applied %t, prompted %t: want %s exit %d during repository discovery, no prompt, nothing applied",
				res.Outcome, res.ExitCode(), res.Error, exists(filepath.Join(r.repo, "out.txt")), exists(r.prompts), RunExhausted, ExitBudget)
		}
	})
	t.Run("control: a slow discovery within --mins", func(t *testing.T) {
		r := newRunRig(t)
		slowDiscovery(t, 200*time.Millisecond)
		res := r.run(t, writer("claude", "out.txt=work"), nil)
		if res.Outcome != RunApplied {
			t.Errorf("outcome %s (%s), want %s", res.Outcome, res.Error, RunApplied)
		}
	})
}

// unconfirmed makes the named children's shutdown report a process whose
// exit could not be confirmed (or, with left, still running; with errText,
// a cleanup error). The seam edits the report; the real kill still runs.
func unconfirmed(t *testing.T, edit func(*proc.ShutdownReport), names ...string) {
	t.Helper()
	testChildShutdown = func(name string, sr *proc.ShutdownReport) {
		if slices.Contains(names, name) {
			edit(sr)
		}
	}
	t.Cleanup(func() { testChildShutdown = nil })
}

func unknownProc(sr *proc.ShutdownReport) {
	sr.Kill.Unknown = append(sr.Kill.Unknown, proc.Proc{PID: 1 << 30, Start: "x"})
}

// collected reports whether anything was committed on a kept branch.
func (r runRig) collected(t *testing.T, branch string) bool {
	t.Helper()
	return strings.TrimSpace(r.git(t, "rev-list", "--count", "main.."+branch)) != "0"
}

// wantUnconfirmed checks a run stopped by unconfirmed cleanup: the outcome,
// nothing applied, a notice naming the children.
func wantUnconfirmed(t *testing.T, r runRig, res RunResult, outcome string, exit int, names ...string) {
	t.Helper()
	if res.Outcome != outcome || res.ExitCode() != exit {
		t.Errorf("outcome %s exit %d (%s), want %s exit %d", res.Outcome, res.ExitCode(), res.Error, outcome, exit)
	}
	for _, f := range []string{"out.txt", "a.txt", "b.txt"} {
		if exists(filepath.Join(r.repo, f)) {
			t.Errorf("%s was applied after unconfirmed cleanup", f)
		}
	}
	notices := strings.Join(res.Notices, "\n")
	for _, n := range names {
		if !strings.Contains(notices, "cleanup was not confirmed for") || !strings.Contains(notices, n) {
			t.Errorf("no notice naming %s: %q", n, res.Notices)
		}
	}
}

// Run-7 #1: cleanup confirmation is a precondition for collect and apply,
// run-wide. Collect and apply run git in the shared repository, so they
// must only run once no process of ours can still be changing it. A child
// (harness, check or judge) whose shutdown left a process running, could
// not confirm one, or reported a cleanup error stops the run there: nothing
// more is collected, nothing is applied, every worktree is kept, the live
// records stay, and the run ends failed (exit 1), unless --mins or a
// budget ended it first (exit 2).
func TestUnconfirmedCleanupBlocksCollectAndApply(t *testing.T) {
	t.Run("process inspection fails (Astra's reproduction)", func(t *testing.T) {
		r := newRunRig(t)
		bin := t.TempDir()
		if err := os.WriteFile(filepath.Join(bin, "ps"), []byte("#!/bin/sh\necho 'fixture: process inspection unavailable' >&2\nexit 1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		res := r.run(t, writer("claude", "out.txt=changed"), nil)
		if res.Unknown == 0 {
			t.Fatalf("the fixture did not make cleanup unconfirmed: %+v", res)
		}
		wantUnconfirmed(t, r, res, RunFailed, ExitNoCandidate, "claude")
		if res.Branch == "" || res.Worktree == "" || !exists(res.Worktree) {
			t.Errorf("the worktree was not kept: branch %q worktree %q", res.Branch, res.Worktree)
		} else if r.collected(t, res.Branch) || len(res.Files) > 0 {
			t.Errorf("the candidate was collected after unconfirmed cleanup: files %v", res.Files)
		}
	})
	for _, tc := range []struct {
		name string
		edit func(*proc.ShutdownReport)
	}{
		{"unknown", unknownProc},
		{"left running", func(sr *proc.ShutdownReport) {
			sr.Swept.Alive = append(sr.Swept.Alive, proc.Proc{PID: 1 << 30, Start: "x"})
		}},
		{"cleanup error", func(sr *proc.ShutdownReport) { sr.Err = "fixture: inventory failed" }},
	} {
		t.Run("one candidate, "+tc.name, func(t *testing.T) {
			r := newRunRig(t)
			unconfirmed(t, tc.edit, "claude")
			res := r.run(t, writer("claude", "out.txt=changed"), nil)
			wantUnconfirmed(t, r, res, RunFailed, ExitNoCandidate, "claude")
			if res.Worktree == "" || r.collected(t, res.Branch) {
				t.Errorf("worktree %q not kept, or collected", res.Worktree)
			}
		})
	}
	t.Run("fan-out: one candidate unconfirmed, its sibling not collected or applied", func(t *testing.T) {
		r := newRunRig(t)
		unconfirmed(t, unknownProc, "claude")
		res := r.fan(t, []harness.Harness{writer("claude", "a.txt=1"), writer("codex", "b.txt=2")}, nil)
		wantUnconfirmed(t, r, res, RunFailed, ExitNoCandidate, "claude")
		for _, name := range []string{"claude", "codex"} {
			if c := cand(res, name); c.Worktree == "" || r.collected(t, c.Branch) {
				t.Errorf("%s: worktree %q not kept, or collected", name, c.Worktree)
			}
		}
	})
	t.Run("fan-out: a check unconfirmed, nothing applied", func(t *testing.T) {
		r := newRunRig(t)
		r.checkConfig(t, "")
		testCheckShutdown = unknownProc
		defer func() { testCheckShutdown = nil }()
		res := r.fan(t, []harness.Harness{writer("claude", "out.txt=good"), writer("codex", "out.txt=bad")}, nil)
		wantUnconfirmed(t, r, res, RunFailed, ExitNoCandidate, "check test")
		for _, name := range []string{"claude", "codex"} {
			if c := cand(res, name); c.Worktree == "" {
				t.Errorf("%s: worktree not kept", name)
			}
		}
	})
	t.Run("fan-out: the judge unconfirmed, its answer not used", func(t *testing.T) {
		r := newRunRig(t)
		r.checkConfig(t, "")
		unconfirmed(t, unknownProc, "judge-claude")
		res := r.fan(t, []harness.Harness{writer("claude", "out.txt=good-1", "ACPTEST_JUDGE=WINNER: B"), writer("codex", "out.txt=good-2")}, nil)
		wantUnconfirmed(t, r, res, RunFailed, ExitNoCandidate, "judge-claude")
		if res.Harness != "" {
			t.Errorf("the judge's answer was used: winner %q", res.Harness)
		}
		if j := res.Judge; j == nil || j.Worktree == "" || !exists(j.Worktree) {
			t.Errorf("the judge's worktree was not kept: %+v", j)
		}
		for _, name := range []string{"claude", "codex"} {
			if c := cand(res, name); c.Worktree == "" {
				t.Errorf("%s: worktree not kept", name)
			}
		}
	})
	t.Run("--mins keeps exit 2", func(t *testing.T) {
		r := newRunRig(t)
		unconfirmed(t, unknownProc, "claude")
		res := r.run(t, fake{name: "claude", agentMode: "hang"}, func(o *RunOptions) {
			o.Budget = &budget.Limits{USD: 5, Turns: 5, Wall: 2 * time.Second, Depth: 2}
		})
		wantUnconfirmed(t, r, res, RunTimedOut, ExitBudget, "claude")
		if res.Worktree == "" {
			t.Error("the worktree was not kept")
		}
	})
	t.Run("budget exhaustion keeps exit 2", func(t *testing.T) {
		r := newRunRig(t)
		unconfirmed(t, unknownProc, "claude")
		testChildTurn = func(h string, t *acp.Turn) { t.Child = &acp.ChildReport{Turns: 1000} }
		defer func() { testChildTurn = nil }()
		res := r.run(t, writer("claude", "out.txt=changed"), nil)
		wantUnconfirmed(t, r, res, RunExhausted, ExitBudget, "claude")
		if res.Worktree == "" || r.collected(t, res.Branch) {
			t.Errorf("worktree %q not kept, or collected", res.Worktree)
		}
	})
}
