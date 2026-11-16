package refraktal

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"refraktal.dev/refraktal/internal/budget"
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
