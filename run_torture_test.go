package refraktal

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	name string
}

func (h tortureHarness) Name() string {
	if h.name != "" {
		return h.name
	}
	return "agy"
}
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

func startTortureRun(t *testing.T, r runRig, s acptest.TortureSpec, role string, muts ...func(*RunOptions)) (<-chan tortureRunReply, <-chan struct{}) {
	t.Helper()
	// This is the duration represented by --mins, not a parent cancellation.
	lim := budget.Limits{USD: 5, Turns: 5, Wall: 6 * time.Second, Depth: 2}
	o := RunOptions{Task: "exercise timeout cleanup", Harness: "agy", Dir: r.repo, Budget: &lim,
		PriceURL: r.priceURL, PriceCache: r.priceCache, LiveDir: r.live, ConfigPath: r.config, WorktreeDir: r.wtDir,
		Harnesses: []harness.Harness{tortureHarness{spec: s, role: role}}}
	for _, mut := range muts {
		mut(&o)
	}
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

func rootTortureGone(t *testing.T, r runRig, s acptest.TortureSpec, observed ...proc.Proc) {
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
			known := slices.ContainsFunc(observed, func(was proc.Proc) bool { return p.Same(was) })
			if known || acptest.TortureMatches(s, p.Cmd) || strings.Contains(p.Cmd, r.repo) || strings.Contains(p.Cmd, r.wtDir) {
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
}

func startTortureFan(t *testing.T, r runRig, s acptest.TortureSpec) (<-chan tortureRunReply, <-chan struct{}) {
	t.Helper()
	return startTortureRun(t, r, s, "", func(o *RunOptions) {
		o.Budget.Wall = 12 * time.Second
		o.Harness = "claude,codex"
		o.Getenv = func(string) string { return "" }
		o.Harnesses = []harness.Harness{
			tortureHarness{spec: s, name: "claude", role: "agent-edit-tree"},
			tortureHarness{spec: s, name: "codex", role: "agent-edit-tree"},
			tortureHarness{spec: s, name: "agy", role: "agent-judge"},
		}
	})
}

// Snapshot all user files, including binary untracked bytes, modes and empty
// directories. Git administration and the run's own logs are the only exclusions.
func tortureTree(t *testing.T, repo string) map[string]struct {
	mode os.FileMode
	data string
} {
	t.Helper()
	out := map[string]struct {
		mode os.FileMode
		data string
	}{}
	err := filepath.WalkDir(repo, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(repo, path)
		if err != nil {
			return err
		}
		if rel == ".git" || rel == ".refraktal" {
			return filepath.SkipDir
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		var data []byte
		if !d.IsDir() {
			data, err = os.ReadFile(path)
			if err != nil {
				return err
			}
		}
		out[rel] = struct {
			mode os.FileMode
			data string
		}{info.Mode(), string(data)}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func preserveTortureTree(t *testing.T, r runRig) func() {
	t.Helper()
	write := func(name, data string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(r.repo, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("user.txt", "base\n")
	r.git(t, "add", "user.txt")
	r.git(t, "commit", "-q", "-m", "user fixture")
	write("user.txt", "staged user bytes\n")
	r.git(t, "add", "user.txt")
	write("user.txt", "unstaged user bytes\x00\xff\n")
	write("local.txt", "untracked user bytes\x00\xfe\n")
	before := tortureTree(t, r.repo)
	head := r.git(t, "rev-parse", "HEAD")
	index := r.git(t, "diff", "--cached", "--binary")
	status := r.git(t, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	return func() {
		t.Helper()
		if !maps.Equal(before, tortureTree(t, r.repo)) {
			t.Error("exhausted run changed the user's file bytes, modes or paths")
		}
		if r.git(t, "rev-parse", "HEAD") != head || r.git(t, "diff", "--cached", "--binary") != index ||
			r.git(t, "status", "--porcelain=v1", "-z", "--untracked-files=all") != status {
			t.Error("exhausted run changed the user's HEAD, index or working-tree status")
		}
	}
}

// Remember birth identities as well as argv tags, so a check's make parent
// and short-lived polling children are covered without matching their names.
// These observations are only assertions, never signal targets.
func observeTortureTree(t *testing.T, r runRig, s acptest.TortureSpec, roles map[string]int, ready func() bool, finished <-chan struct{}) []proc.Proc {
	t.Helper()
	var observed []proc.Proc
	acptest.WaitFor(t, 18*time.Second, "the gated phase and its live descendants", func() bool {
		select {
		case <-finished:
			t.Fatal("Run finished before the gated phase became active")
		default:
		}
		if err := acptest.TortureError(s); err != nil {
			t.Fatal(err)
		}
		if !ready() {
			return false
		}
		all, err := proc.List()
		if err != nil {
			t.Fatal(err)
		}
		liveRoles := map[string]int{}
		selected := map[int]bool{}
		for _, p := range all {
			if acptest.TortureMatches(s, p.Cmd) || strings.Contains(p.Cmd, r.repo) || strings.Contains(p.Cmd, r.wtDir) {
				selected[p.PID] = true
			}
		}
		for _, p := range acptest.TortureInventory(s) {
			if selected[p.PID] {
				liveRoles[p.Role]++
			}
		}
		for role, want := range roles {
			if liveRoles[role] != want {
				return false
			}
		}
		for changed := true; changed; {
			changed = false
			for _, p := range all {
				if selected[p.PID] && p.PPID > 1 && p.PPID != os.Getpid() && !selected[p.PPID] {
					selected[p.PPID], changed = true, true
				}
				if selected[p.PPID] && !selected[p.PID] {
					selected[p.PID], changed = true, true
				}
			}
		}
		for pid := range selected {
			if p, ok := all[pid]; ok {
				observed = append(observed, p)
			}
		}
		return true
	})
	return observed
}

func assertTortureCandidatesKept(t *testing.T, r runRig, res RunResult) {
	t.Helper()
	if res.Outcome != RunTimedOut || res.ExitCode() != ExitBudget || len(res.Candidates) != 2 || res.LeftRunning != 0 || res.Unknown != 0 {
		t.Fatalf("fan-out exhaustion: outcome %q, exit %d, candidates %d, alive/unknown %d/%d", res.Outcome, res.ExitCode(), len(res.Candidates), res.LeftRunning, res.Unknown)
	}
	var branches, dirs []string
	worktrees := []string{tortureCanonicalPath(t, r.repo)}
	for _, name := range []string{"claude", "codex"} {
		c := cand(res, name)
		id := filepath.Base(res.Run) + "-" + name
		wantDir, wantBranch := filepath.Join(r.wtDir, id), "refraktal/"+id
		if c.Outcome != candChanges || c.StopReason != "end_turn" || c.Worktree != wantDir || c.Branch != wantBranch ||
			c.LeftRunning != 0 || c.Unknown != 0 || c.Applies == nil || !*c.Applies {
			t.Fatalf("%s was not checked and retained normally: %+v", name, c)
		}
		for file, want := range map[string]string{"README": "agent result\n", "tool-output.txt": "complete result\n"} {
			b, err := os.ReadFile(filepath.Join(c.Worktree, file))
			if err != nil || string(b) != want || r.git(t, "show", c.Branch+":"+file) != want {
				t.Errorf("%s lost collected %s in its branch or worktree", name, file)
			}
		}
		if b, err := os.ReadFile(filepath.Join(r.repo, res.Run, resultName(name))); err != nil || len(b) == 0 {
			t.Errorf("%s has no retained result patch", name)
		}
		branches, dirs, worktrees = append(branches, wantBranch), append(dirs, id), append(worktrees, tortureCanonicalPath(t, wantDir))
	}
	gotBranches := strings.Fields(r.git(t, "for-each-ref", "--format=%(refname:short)", "refs/heads/refraktal/"))
	entries, err := os.ReadDir(r.wtDir)
	if err != nil {
		t.Fatal(err)
	}
	var gotDirs, gotWorktrees []string
	for _, e := range entries {
		gotDirs = append(gotDirs, e.Name())
	}
	for _, line := range strings.Split(r.git(t, "worktree", "list", "--porcelain"), "\n") {
		if path, ok := strings.CutPrefix(line, "worktree "); ok {
			gotWorktrees = append(gotWorktrees, path)
		}
	}
	for _, pair := range [][2][]string{{branches, gotBranches}, {dirs, gotDirs}, {worktrees, gotWorktrees}} {
		slices.Sort(pair[0])
		slices.Sort(pair[1])
		if !slices.Equal(pair[0], pair[1]) {
			t.Errorf("wrong retained branches or worktrees: want %v, got %v", pair[0], pair[1])
		}
	}
}

func tortureCanonicalPath(t *testing.T, path string) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(path) // macOS: /var may be /private/var
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func assertTortureFanEvents(t *testing.T, r runRig, res RunResult, judging bool) {
	t.Helper()
	seen := map[string]int{}
	checked, ended, shutdown := map[string]bool{}, map[string]bool{}, map[string]bool{}
	str := func(e map[string]json.RawMessage, key string) string {
		var s string
		json.Unmarshal(e[key], &s)
		return s
	}
	for _, e := range tortureEvents(t, filepath.Join(r.repo, res.Run, "events.jsonl")) {
		name, h := str(e, "event"), str(e, "harness")
		seen[name]++
		switch name {
		case "checks":
			if judging {
				if str(e, "source") != "none" {
					t.Error("judge fixture did not reach an unchecked tie")
				}
			} else {
				var steps []checkStep
				if json.Unmarshal(e["steps"], &steps) != nil || str(e, "source") != "make" || len(steps) != 1 ||
					!slices.Equal(steps[0].Argv, []string{"make", "-f", "Makefile", "test"}) {
					t.Error("check was not detected from the committed Makefile")
				}
			}
		case "run_end":
			var detail struct{ Code string }
			if json.Unmarshal(e["error"], &detail) != nil || detail.Code != RunTimedOut || str(e, "outcome") != RunTimedOut {
				t.Error("run_end does not durably record --mins exhaustion")
			}
		case "checked":
			var checks []CheckResult
			if err := json.Unmarshal(e["checks"], &checks); err != nil {
				t.Fatal(err)
			}
			if !judging && (len(checks) != 1 || checks[0].Status != CheckTimedOut) {
				t.Errorf("%s event lacks its in-flight check timeout: %+v", h, checks)
			}
			checked[h] = true
		case "child_end":
			ended[h] = true
		case "shutdown":
			shutdown[h] = true
		case "judge_start":
			var close []string
			if json.Unmarshal(e["close_call"], &close) != nil || !slices.Equal(close, []string{"claude", "codex"}) {
				t.Error("judge did not receive the two-candidate close call")
			}
		case "judge_end":
			if str(e, "outcome") != candTimedOut {
				t.Error("judge_end does not record the judge timeout")
			}
		}
	}
	for _, event := range []string{"run_start", "checks", "run_end"} {
		if seen[event] != 1 {
			t.Errorf("expected one durable %s event, got %d", event, seen[event])
		}
	}
	for _, h := range []string{"claude", "codex"} {
		if !checked[h] || !ended[h] || !shutdown[h] {
			t.Errorf("%s lacks its checked, child_end or shutdown event", h)
		}
	}
	if judging {
		if seen["judge_start"] != 1 || seen["judge_end"] != 1 || !ended["judge-agy"] || !shutdown["judge-agy"] {
			t.Error("judge lifecycle was not recorded")
		}
	} else if seen["judge_start"] != 0 {
		t.Error("judge started after the check exhausted --mins")
	}
	if seen["worktree_remove_failed"] != 0 {
		t.Error("worktree removal failed")
	}
}

func TestTortureMinsExpiresMidCheck(t *testing.T) {
	repeats := 1
	if *torture {
		repeats = 5
	}
	for trial := 0; trial < repeats; trial++ {
		t.Run(fmt.Sprint(trial), func(t *testing.T) {
			checkSentinel := acptest.StartSentinel(t)
			r := newRunRig(t)
			s := acptest.NewTortureSpec(t, *torture)
			// Permit only this test's shell/make fixture through the test-only
			// proc launch guard. Harnesses stay pinned to acptest fakes.
			t.Setenv(proc.LiveEnv, "1")
			for name, data := range map[string]string{
				"torture-check.sh": acptest.TortureCheckScript(s),
				"Makefile":         "test:\n\texec /bin/sh ./torture-check.sh '" + strings.ReplaceAll(s.Dir, "'", `'\''`) + "'\n",
			} {
				if err := os.WriteFile(filepath.Join(r.repo, name), []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			r.git(t, "add", "Makefile", "torture-check.sh")
			r.git(t, "commit", "-q", "-m", "base-commit check gate")
			// Detection must read HEAD, even when the user removes test: locally.
			if err := os.WriteFile(filepath.Join(r.repo, "Makefile"), []byte("# user's uncommitted makefile\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			unchanged := preserveTortureTree(t, r)
			defer acptest.TortureMark(t, s, "release-check")
			replies, finished := startTortureFan(t, r, s)
			observed := observeTortureTree(t, r, s, map[string]int{"check": 2, "check-child": 2}, func() bool {
				paths, _ := filepath.Glob(filepath.Join(s.Dir, "check-entered-*"))
				if len(paths) != 2 {
					return false
				}
				for _, path := range paths {
					if b, err := os.ReadFile(path); err != nil || len(b) == 0 {
						return false
					}
				}
				return true
			}, finished)
			deadline := tortureGrantDeadline(t, r)
			if !time.Now().Before(deadline) {
				t.Fatal("fixture reached the checks after its grant expired")
			}
			acptest.WaitFor(t, 14*time.Second, "--mins to expire inside both checks", func() bool { return !time.Now().Before(deadline) })
			// Keep the gate closed through shutdown, so a check cannot mask a
			// missing kill by exiting voluntarily when the test releases it.
			res := tortureRunResult(t, replies)
			assertTortureCandidatesKept(t, r, res)
			checkDirs := map[string]bool{}
			for _, c := range res.Candidates {
				checkDirs[tortureCanonicalPath(t, c.Worktree)] = true
				if len(c.Checks) != 1 || c.Checks[0].Status != CheckTimedOut {
					t.Errorf("%s was not interrupted in its deterministic check: %+v", c.Harness, c.Checks)
				}
			}
			paths, _ := filepath.Glob(filepath.Join(s.Dir, "check-entered-*"))
			for _, path := range paths {
				b, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				dir := tortureCanonicalPath(t, strings.TrimSpace(string(b)))
				if !checkDirs[dir] {
					t.Error("check did not run in a distinct candidate worktree")
				}
				delete(checkDirs, dir)
			}
			if len(paths) != 2 || len(checkDirs) != 0 {
				t.Error("a candidate worktree never entered its deterministic check")
			}
			if res.Judge != nil {
				t.Error("exhausted checks reached the judge")
			}
			unchanged()
			assertTortureFanEvents(t, r, res, false)
			rootTortureGone(t, r, s, observed...)
			if n := acptest.TortureSlots(t, s); n != 10 {
				t.Errorf("two candidate trees and two shell check trees used %d slots, want 10", n)
			}
			acptest.TortureMark(t, s, "release-check")
			checkSentinel()
		})
	}
}

func TestTortureMinsExpiresMidJudge(t *testing.T) {
	repeats := 1
	if *torture {
		repeats = 5
	}
	for trial := 0; trial < repeats; trial++ {
		t.Run(fmt.Sprint(trial), func(t *testing.T) {
			checkSentinel := acptest.StartSentinel(t)
			r := newRunRig(t)
			s := acptest.NewTortureSpec(t, *torture)
			unchanged := preserveTortureTree(t, r)
			defer acptest.TortureMark(t, s, "release-judge")
			replies, finished := startTortureFan(t, r, s)
			observed := observeTortureTree(t, r, s, map[string]int{"agent-judge": 1, "cycle": 1, "leaf": 1},
				func() bool { return acptest.TortureHas(s, "judge-started") }, finished)
			deadline := tortureGrantDeadline(t, r)
			if !time.Now().Before(deadline) {
				t.Fatal("fixture reached the judge after its grant expired")
			}
			acptest.WaitFor(t, 14*time.Second, "--mins and session/cancel inside the judge", func() bool {
				return !time.Now().Before(deadline) && acptest.TortureHas(s, "judge-cancelled")
			})
			acptest.TortureMark(t, s, "release-judge")
			res := tortureRunResult(t, replies)
			assertTortureCandidatesKept(t, r, res)
			if j := res.Judge; j == nil || j.Harness != "agy" || j.Outcome != candTimedOut || j.StopReason != "cancelled" ||
				j.Branch != "" || j.Worktree != "" || len(j.Files) != 0 || j.LeftRunning != 0 || j.Unknown != 0 {
				t.Fatalf("judge timeout did not cancel and discard its unchanged worktree: %+v", j)
			}
			if v := res.Verdict; v == nil || v.Judge != "agy" || v.JudgedOwn || v.Winner != "" ||
				v.Checks != "none" || !slices.Equal(v.CloseCall, []string{"claude", "codex"}) {
				t.Fatalf("did not time out in an unscored close call: %+v", v)
			}
			unchanged()
			assertTortureFanEvents(t, r, res, true)
			rootTortureGone(t, r, s, observed...)
			if n := acptest.TortureSlots(t, s); n != 9 {
				t.Errorf("two candidate trees and the judge tree used %d slots, want 9", n)
			}
			checkSentinel()
		})
	}
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
