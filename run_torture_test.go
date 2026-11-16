package refraktal

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"refraktal.dev/refraktal/internal/acp/acptest"
	"refraktal.dev/refraktal/internal/budget"
	"refraktal.dev/refraktal/internal/harness"
	"refraktal.dev/refraktal/internal/proc"
)

var torture = flag.Bool("torture", false, "repeat the bounded fake-agent timeout torture cases")

type tortureHarness struct {
	spec acptest.TortureSpec
	role string
}

func (tortureHarness) Name() string         { return "agy" }
func (tortureHarness) Mode() string         { return "default" }
func (tortureHarness) ReadOnlyMode() string { return "plan" }
func (h tortureHarness) Prepare(context.Context, harness.Options) (harness.Launch, error) {
	argv, env, err := acptest.TortureCommand(h.spec, h.role)
	return harness.Launch{Argv: argv, Env: env, MCP: harness.MCPIsolated}, err
}

type tortureRunReply struct {
	result RunResult
	err    error
}

func startTortureRun(t *testing.T, r runRig, s acptest.TortureSpec, role string) (<-chan tortureRunReply, <-chan struct{}) {
	t.Helper()
	// This is the duration represented by --mins, not a parent cancellation.
	lim := budget.Limits{USD: 5, Turns: 5, Wall: 6 * time.Second, Depth: 2}
	o := RunOptions{Task: "exercise timeout cleanup", Harness: "agy", Dir: r.repo, Budget: &lim,
		PriceURL: r.priceURL, PriceCache: r.priceCache, LiveDir: r.live, ConfigPath: r.config, WorktreeDir: r.wtDir,
		Harnesses: []harness.Harness{tortureHarness{spec: s, role: role}}}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	replies := make(chan tortureRunReply, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		res, err := Run(ctx, o)
		replies <- tortureRunReply{result: res, err: err}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("Run did not return during bounded test cleanup; fake expires independently")
		}
	})
	return replies, finished
}

func tortureRunResult(t *testing.T, replies <-chan tortureRunReply) RunResult {
	t.Helper()
	select {
	case reply := <-replies:
		if reply.err != nil {
			t.Fatal(reply.err)
		}
		return reply.result
	case <-time.After(25 * time.Second):
		t.Fatal("Run hung beyond the budget, cancel grace and shutdown grace")
		return RunResult{}
	}
}

func rootTortureGone(t *testing.T, r runRig, s acptest.TortureSpec) {
	t.Helper()
	var quietSince time.Time
	var survivors []int
	defer func() {
		if len(survivors) > 0 {
			t.Logf("surviving fixture PIDs: %v", survivors)
		}
	}()
	acptest.WaitFor(t, 3*time.Second, "zero surviving fake or test Git processes", func() bool {
		all, err := proc.List()
		if err != nil {
			t.Fatal(err)
		}
		survivors = survivors[:0]
		for _, p := range all {
			// Read-only assertions, never inputs to a signal or session sweep.
			if acptest.TortureMatches(s, p.Cmd) || strings.Contains(p.Cmd, r.repo) || strings.Contains(p.Cmd, r.wtDir) {
				survivors = append(survivors, p.PID)
			}
		}
		if len(survivors) > 0 {
			quietSince = time.Time{}
			return false
		}
		if quietSince.IsZero() {
			quietSince = time.Now()
		}
		return time.Since(quietSince) >= 100*time.Millisecond
	})
	if err := acptest.TortureError(s); err != nil {
		t.Fatal(err)
	}
	acptest.TortureSlots(t, s)
	if entries, err := os.ReadDir(r.live); err != nil || len(entries) != 0 {
		t.Errorf("live records left after cleanup: %d, error %v", len(entries), err)
	}
}

func tortureEvents(t *testing.T, path string) []map[string]json.RawMessage {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var events []map[string]json.RawMessage
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e map[string]json.RawMessage
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		events = append(events, e)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}

func assertTortureEvents(t *testing.T, r runRig, res RunResult, cancelled bool) {
	t.Helper()
	if res.LeftRunning != 0 || res.Unknown != 0 {
		t.Errorf("run reported incomplete cleanup: alive %d, unknown %d", res.LeftRunning, res.Unknown)
	}
	seen := map[string]bool{}
	var finalOutcome string
	var turnCancelled, toolInProgress bool
	for _, e := range tortureEvents(t, filepath.Join(r.repo, res.Run, "events.jsonl")) {
		var name string
		json.Unmarshal(e["event"], &name)
		seen[name] = true
		if name == "run_end" {
			json.Unmarshal(e["outcome"], &finalOutcome)
		}
		if name == "update" && string(e["kind"]) == `"tool_call_update"` {
			var u struct {
				Status string `json:"status"`
			}
			if json.Unmarshal(e["update"], &u) == nil && u.Status == "in_progress" {
				toolInProgress = true
			}
		}
		if name == "turn" {
			json.Unmarshal(e["cancelled"], &turnCancelled)
		}
	}
	for _, name := range []string{"run_start", "worktree", "shutdown", "turn", "run_end"} {
		if !seen[name] {
			t.Errorf("missing durable %s event", name)
		}
	}
	if cancelled && (!seen["permission"] || !toolInProgress) {
		t.Error("in-flight tool and its permission decision were not logged")
	}
	if finalOutcome != res.Outcome || turnCancelled != cancelled {
		t.Errorf("events disagree with result: outcome %q, turn cancelled %v", finalOutcome, turnCancelled)
	}
}

func TestTortureMinsExpiresMidTool(t *testing.T) {
	repeats := 1
	if *torture {
		repeats = 5
	}
	for trial := 0; trial < repeats; trial++ {
		for _, changed := range []bool{false, true} {
			t.Run(fmt.Sprintf("trial-%d/changed-%v", trial, changed), func(t *testing.T) {
				checkSentinel := acptest.StartSentinel(t)
				r := newRunRig(t)
				s := acptest.NewTortureSpec(t, *torture)
				// Preserve a user's uncommitted edit even if the cancelled agent
				// has a different version of this same file in its worktree.
				if err := os.WriteFile(filepath.Join(r.repo, "README"), []byte("user edit\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				role := "agent-tool"
				if changed {
					role = "agent-tool-change"
				}
				replies, finished := startTortureRun(t, r, s, role)
				acptest.WaitFor(t, 12*time.Second, "an in-flight tool with live descendants", func() bool {
					if acptest.TortureHas(s, "tool-started") && len(acptest.TortureInventory(s)) == 3 {
						return true
					}
					select {
					case <-finished:
						t.Fatal("Run finished before the fake tool became active")
					default:
					}
					return false
				})
				res := tortureRunResult(t, replies)
				if res.Outcome != RunTimedOut || res.ExitCode() != ExitBudget || res.StopReason != "cancelled" || res.Allowed != 1 {
					t.Fatalf("mid-tool timeout: outcome %q, exit %d, stop %q, allowed %d", res.Outcome, res.ExitCode(), res.StopReason, res.Allowed)
				}
				if !acptest.TortureHas(s, "cancelled") || res.LeftRunning != 0 || res.Unknown != 0 {
					t.Error("cancel was not delivered or process cleanup was incomplete")
				}
				if b, err := os.ReadFile(filepath.Join(r.repo, "README")); err != nil || string(b) != "user edit\n" || exists(filepath.Join(r.repo, "tool-output.txt")) {
					t.Error("a timed-out tool changed the user's tree")
				}
				if changed {
					if res.Branch == "" || res.Worktree == "" || !exists(res.Worktree) || !exists(filepath.Join(r.repo, res.Run, "result.diff")) {
						t.Fatal("cancelled result was not retained as a patch, branch and worktree")
					}
					if b, err := os.ReadFile(filepath.Join(res.Worktree, "README")); err != nil || string(b) != "agent result\n" {
						t.Fatal("kept worktree lost the agent's partial result")
					}
					if strings.TrimSpace(r.git(t, "show", res.Branch+":tool-output.txt")) != "complete result" {
						t.Fatal("retained branch lacks the collected tool output")
					}
				} else {
					if res.Worktree != "" || res.Branch != "" {
						t.Error("empty timed-out worktree was kept")
					}
					if entries, err := os.ReadDir(r.wtDir); err != nil || len(entries) != 0 {
						t.Error("empty worktree was not removed")
					}
					if strings.TrimSpace(r.git(t, "branch", "--list", "refraktal/*")) != "" {
						t.Error("empty timed-out branch was not removed")
					}
				}
				assertTortureEvents(t, r, res, true)
				rootTortureGone(t, r, s)
				checkSentinel()
			})
		}
	}
	// TODO(M6): --mins expiring mid-judge and mid-check, including fan-out
	// cancellation and cleanup of every sibling candidate's process tree.
}

// A test-only Git wrapper gates the mutating apply command. It is a shell
// script, not a re-exec of the test binary: Collect and Apply make dozens of
// git calls, and starting the race-instrumented test binary for each one made
// the run slower than the test's waits. It holds only a mutating apply (no
// --check) until release-apply exists, gives up at the fixture deadline, and
// otherwise execs real git at once.
func gateTortureApply(t *testing.T, s acptest.TortureSpec) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(s.Dir, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	q := func(p string) string { return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'" }
	script := fmt.Sprintf(`#!/bin/sh
deadline=%d
mut=0; seen=0
for a in "$@"; do
  if [ $seen = 1 ] && [ "$a" = "--check" ]; then mut=0; fi
  if [ $seen = 0 ] && [ "$a" = "apply" ]; then seen=1; mut=1; fi
done
if [ $mut = 1 ]; then
  : > %s || exit 125
  while [ ! -e %s ]; do
    [ "$(date +%%s)" -ge $deadline ] && exit 124
    sleep 0.05
  done
fi
exec %s "$@"
`, s.Deadline.Unix(), q(filepath.Join(s.Dir, "apply-entered")), q(filepath.Join(s.Dir, "release-apply")), q(realGit))
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func tortureGrantDeadline(t *testing.T, r runRig) time.Time {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(r.repo, ".refraktal", "runs", "*", "events.jsonl"))
	if err != nil || len(paths) != 1 {
		t.Fatal("expected one run event log")
	}
	for _, e := range tortureEvents(t, paths[0]) {
		if string(e["event"]) == `"run_start"` {
			// The ledger's deadline is its start plus --mins; the ledger is
			// created just before run_start is logged, so the event time plus
			// mins is the deadline or a few milliseconds after it. Waiting
			// for this time therefore always waits past the real deadline.
			var at time.Time
			var mins float64
			if json.Unmarshal(e["t"], &at) != nil || json.Unmarshal(e["mins"], &mins) != nil || at.IsZero() || mins <= 0 {
				t.Fatal("run_start lacks its time or mins")
			}
			return at.Add(time.Duration(mins * float64(time.Minute)))
		}
	}
	t.Fatal("missing run_start")
	return time.Time{}
}

func TestTortureMinsExpiresDuringApply(t *testing.T) {
	repeats := 1
	if *torture {
		repeats = 5
	}
	for trial := 0; trial < repeats; trial++ {
		t.Run(fmt.Sprint(trial), func(t *testing.T) {
			checkSentinel := acptest.StartSentinel(t)
			r := newRunRig(t)
			s := acptest.NewTortureSpec(t, *torture)
			gateTortureApply(t, s)
			defer acptest.TortureMark(t, s, "release-apply")
			if err := os.WriteFile(filepath.Join(r.repo, "local.txt"), []byte("user file\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			replies, finished := startTortureRun(t, r, s, "agent-edit")
			acptest.WaitFor(t, 12*time.Second, "mutating git apply entered", func() bool {
				if acptest.TortureHas(s, "apply-entered") {
					return true
				}
				select {
				case <-finished:
					t.Fatal("Run finished before apply")
				default:
				}
				return false
			})
			deadline := tortureGrantDeadline(t, r)
			if !time.Now().Before(deadline) {
				t.Fatal("fixture reached apply after its grant expired")
			}
			acptest.WaitFor(t, 8*time.Second, "the actual --mins deadline to expire inside apply", func() bool {
				return !time.Now().Before(deadline)
			})
			if b, err := os.ReadFile(filepath.Join(r.repo, "README")); err != nil || string(b) != "x\n" || exists(filepath.Join(r.repo, "tool-output.txt")) {
				t.Fatal("user tree changed before the gated apply ran")
			}
			acptest.TortureMark(t, s, "release-apply")
			res := tortureRunResult(t, replies)
			// DESIGN.md bounds --mins through the end of the turn. Once apply
			// starts, run.go deliberately lets its checked operation complete.
			if res.Outcome != RunApplied || res.ExitCode() != ExitOK || res.Worktree != "" || res.Branch != "" {
				t.Errorf("deadline during atomic apply: outcome %q, exit %d, worktree kept %v", res.Outcome, res.ExitCode(), res.Worktree != "")
			}
			for name, want := range map[string]string{"README": "agent result\n", "tool-output.txt": "complete result\n", "local.txt": "user file\n"} {
				if b, err := os.ReadFile(filepath.Join(r.repo, name)); err != nil || string(b) != want {
					t.Errorf("incomplete apply or lost user file: %s", name)
				}
			}
			if entries, err := os.ReadDir(r.wtDir); err != nil || len(entries) != 0 || strings.TrimSpace(r.git(t, "branch", "--list", "refraktal/*")) != "" {
				t.Error("successful apply did not remove the worktree and branch")
			}
			if strings.TrimSpace(r.git(t, "diff", "--cached", "--name-only")) != "" {
				t.Error("apply staged changes in the user's index")
			}
			assertTortureEvents(t, r, res, false)
			rootTortureGone(t, r, s)
			checkSentinel()
		})
	}
}
