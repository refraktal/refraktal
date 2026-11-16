package refraktal

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"refraktal.dev/refraktal/internal/budget"
	"refraktal.dev/refraktal/internal/harness"
)

// lockIndex makes the next collect in dir fail, as a stale index.lock does:
// the worktree's git directory is named by its .git file.
func lockIndex(t *testing.T, dir string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	gitdir := strings.TrimSpace(strings.TrimPrefix(string(b), "gitdir:"))
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(dir, gitdir)
	}
	if err := os.WriteFile(filepath.Join(gitdir, "index.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Astra P1 (a): a failed collect never deletes the candidate's work: the
// worktree and branch are kept and reported, for one candidate and in a
// fan-out whose sibling made no changes.
func TestFailedCollectKeepsTheWork(t *testing.T) {
	r := newRunRig(t)
	testBeforeCollect = func(dir string) { lockIndex(t, dir) }
	defer func() { testBeforeCollect = nil }()
	res := r.run(t, writer("claude", "out.txt=work"), nil)
	if res.Outcome != RunFailed || res.Branch == "" || !exists(filepath.Join(res.Worktree, "out.txt")) {
		t.Fatalf("a failed collect lost the work: %+v", res)
	}
	r = newRunRig(t)
	testBeforeCollect = func(dir string) {
		if exists(filepath.Join(dir, "out.txt")) { // only the writer's worktree
			lockIndex(t, dir)
		}
	}
	res = r.fan(t, []harness.Harness{writer("claude", "out.txt=work"), fake{name: "codex", agentMode: "turn"}}, nil)
	if c := cand(res, "claude"); c.Branch == "" || !exists(filepath.Join(c.Worktree, "out.txt")) {
		t.Fatalf("a fan-out whose sibling made no changes deleted the failed candidate's work: %+v (outcome %s)", c, res.Outcome)
	}
}

// Astra P1 (b): a cancel that arrives after the turns (during judging or
// collection) stops before apply: nothing reaches the working tree and the
// result stays on its branch.
func TestCancelStopsBeforeApply(t *testing.T) {
	for _, fan := range []bool{false, true} {
		r := newRunRig(t)
		ctx, cancel := context.WithCancel(context.Background())
		testBeforeApply = func(time.Time) { cancel() }
		lim := budget.Limits{USD: 5, Turns: 5, Wall: 20 * time.Second, Depth: 2}
		o := RunOptions{Task: "x", Harness: "claude", Dir: r.repo, Budget: &lim, PriceURL: r.priceURL, PriceCache: r.priceCache,
			LiveDir: r.live, ConfigPath: r.config, WorktreeDir: r.wtDir, Getenv: func(string) string { return "" },
			Harnesses: []harness.Harness{writer("claude", "out.txt=good")}}
		if fan {
			r.checkConfig(t, "")
			o.Harness, o.Harnesses = "claude,codex", []harness.Harness{writer("claude", "out.txt=good"), writer("codex", "out.txt=bad")}
		}
		res, err := Run(ctx, o)
		testBeforeApply = nil
		cancel()
		if err != nil || res.Outcome == RunApplied || exists(filepath.Join(r.repo, "out.txt")) || res.Branch == "" {
			t.Errorf("fan=%v: applied after a cancel: %+v %v", fan, res, err)
		}
	}
}

// Astra P1 (c): no candidate is collected until every child of the run has
// stopped and been swept: a still-running sibling shares .git/config and
// could add a filter between driver discovery and the collect.
func TestCollectWaitsForEveryChild(t *testing.T) {
	r := newRunRig(t)
	res := r.fan(t, []harness.Harness{writer("claude", "out.txt=good"), fake{name: "codex", agentMode: "slowstart"}}, nil)
	events, _ := os.ReadFile(filepath.Join(r.repo, res.Run, "events.jsonl"))
	collected, codexDone := -1, -1
	for i, l := range strings.Split(string(events), "\n") {
		var e struct{ Event, Harness string }
		json.Unmarshal([]byte(l), &e)
		switch {
		case e.Event == "collected" && e.Harness == "claude" && collected < 0:
			collected = i
		case e.Event == "turn" && e.Harness == "codex" && codexDone < 0:
			codexDone = i
		}
	}
	if collected < 0 || codexDone < 0 || collected < codexDone {
		t.Fatalf("claude was collected (event %d) before codex stopped (event %d)", collected, codexDone)
	}
}

// Astra P1 (e): REFRAKTAL_PASS_GEMINI_KEY=1 passes the routing key only to a
// nested refraktal child, never to an ordinary harness.
func TestPassGeminiKeyOnlyToARefraktalChild(t *testing.T) {
	env := map[string]string{PassKeyEnv: "1", "GEMINI_API_KEY": "k"}
	getenv := func(k string) string { return env[k] }
	r := newRunRig(t)
	r.run(t, fake{name: "codex", agentMode: "turn"}, func(o *RunOptions) { o.Getenv = getenv })
	if b, _ := os.ReadFile(r.prompts + ".env"); strings.Contains(string(b), "GEMINI_API_KEY") {
		t.Fatalf("an ordinary harness got the routing key: %q", b)
	}
	r.run(t, fake{name: "refraktal", agentMode: "turn"}, func(o *RunOptions) { o.Getenv = getenv })
	if b, _ := os.ReadFile(r.prompts + ".env"); !strings.Contains(string(b), "GEMINI_API_KEY") {
		t.Fatalf("the nested refraktal child did not get the routing key it was opted in to: %q", b)
	}
}

// Astra P1 (f): no test may fall back to the real harnesses: in a test
// binary, Doctor and Run refuse a nil harness list unless REFRAKTAL_LIVE=1.
// (PATH and HOME are emptied so this test cannot reach a real harness even
// if the guard is missing.)
func TestTestsNeverFallBackToRealHarnesses(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	r := newRig(t)
	os.MkdirAll(r.root, 0o755)
	_, err := Doctor(context.Background(), DoctorOptions{Root: r.root, LiveDir: r.live, Neutral: r.neutral,
		Getenv: func(string) string { return "" }, Timeout: time.Second, ConfigPath: filepath.Join(r.root, "none.json")})
	if err == nil || !strings.Contains(err.Error(), "harnesses") {
		t.Fatalf("doctor in a test fell back to the real harnesses: %v", err)
	}
}

// Astra P2: a fan-out where one planned candidate could not start for budget
// exits 2 with the candidates kept, and applies nothing, even if another
// candidate made changes.
func TestPartialExhaustionNeverApplies(t *testing.T) {
	r := newRunRig(t)
	res := r.fan(t, []harness.Harness{writer("claude", "out.txt=a"), writer("codex", "out.txt=b")}, func(o *RunOptions) { o.Budget.Turns = 1 })
	if res.Outcome != RunExhausted || res.ExitCode() != ExitBudget || exists(filepath.Join(r.repo, "out.txt")) {
		t.Fatalf("partial exhaustion applied or succeeded: %+v", res)
	}
	kept := 0
	for _, c := range res.Candidates {
		if c.Branch != "" {
			kept++
		}
	}
	if kept == 0 {
		t.Fatal("the candidate with changes was not kept")
	}
}

// Astra P2: --depth 2 allows a grandchild: the nesting guard tracks the
// absolute ceiling the top run set, not the remaining depth.
func TestDepthTwoAllowsAGrandchild(t *testing.T) {
	r := newRunRig(t)
	grand := r.acpChildSpec("claude", fakeSpec{Name: "claude", Env: []string{"ACPTEST_WRITE=out.txt=deep"}})
	child := r.refraktal(r.acpChildSpec("refraktal", fakeSpec{Name: "refraktal", Child: &grand}))
	res := r.run(t, fake{}, func(o *RunOptions) {
		o.Harness, o.Harnesses = "refraktal", []harness.Harness{child}
		o.Budget.Depth = 2
	})
	if res.Outcome != RunApplied {
		t.Fatalf("--depth 2 refused a valid grandchild: %+v", res)
	}
	if b, _ := os.ReadFile(filepath.Join(r.repo, "out.txt")); string(b) != "deep\n" {
		t.Fatalf("out.txt = %q", b)
	}
}

// Astra P2: an edit to a known test-definition file is flagged whatever the
// check command is (make test may run npm test, pytest or cargo test).
func TestWrappedCheckDefinitionsAreFlagged(t *testing.T) {
	p := checkPlan{Steps: []checkStep{{"test", []string{"make", "test"}}}, Source: "config"}
	for _, f := range []string{"package.json", "pyproject.toml", "setup.cfg", "tox.ini", "pytest.ini", "Cargo.toml", "go.mod"} {
		diff := []byte("diff --git a/go.mod b/go.mod\n--- a/go.mod\n+++ b/go.mod\n@@ -1 +1 @@\n-go 1.24\n+go 1.26\n")
		if why := p.touchesDefinition([]string{f}, diff); why == "" {
			t.Errorf("an edit to %s was not flagged under make test", f)
		}
	}
}

// Keeps #18's coverage now that partial exhaustion ends a run before the
// judge: with a tiny --usd both candidates run (a turn is never cut for
// cost), the checks tie, and the judge is refused for budget before it
// starts: exit 2, and the verdict names no judge.
func TestJudgeRefusedForBudgetIsNotNamed(t *testing.T) {
	r := newRunRig(t)
	res := r.fan(t, []harness.Harness{writer("claude", "a.txt=1", "ACPTEST_JUDGE=WINNER: A"), writer("codex", "b.txt=2")},
		func(o *RunOptions) { o.Budget.USD = 0.001 })
	if res.Outcome != RunExhausted || res.Verdict == nil || res.Verdict.Judge != "" || res.Harness != "" || !strings.Contains(res.Error, "judge") {
		t.Fatalf("%+v verdict %+v", res, res.Verdict)
	}
}

// A test-definition file in a subfolder (a monorepo's web/package.json, a
// make -C sub test) is flagged too: matched by its base name.
func TestNestedTestDefinitionsAreFlagged(t *testing.T) {
	p := checkPlan{Steps: []checkStep{{"test", []string{"make", "test"}}}, Source: "config"}
	for _, f := range []string{"web/package.json", "services/api/pyproject.toml", "crates/x/Cargo.toml", "sub/Makefile", "a/b/GNUmakefile"} {
		if why := p.touchesDefinition([]string{f}, nil); why == "" {
			t.Errorf("an edit to %s was not flagged", f)
		}
	}
	nested := []byte("diff --git a/tools/go.mod b/tools/go.mod\n--- a/tools/go.mod\n+++ b/tools/go.mod\n@@ -1 +1 @@\n-go 1.24\n+go 1.26\n")
	if why := p.touchesDefinition([]string{"tools/go.mod"}, nested); why == "" {
		t.Error("a nested go.mod's go line change was not flagged")
	}
}
