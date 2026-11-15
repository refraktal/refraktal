package refraktal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"refraktal.dev/refraktal/internal/acp"
	"refraktal.dev/refraktal/internal/budget"
	"refraktal.dev/refraktal/internal/config"
	"refraktal.dev/refraktal/internal/harness"
	"refraktal.dev/refraktal/internal/price"
)

// fakeCheckArg makes the test binary a check command (see TestMain).
const fakeCheckArg = "refraktal-fake-check"

func fakeCheck(args []string) int {
	switch args[0] {
	case "pass-if": // pass-if <file> <text>: passes when the file holds the text
		b, _ := os.ReadFile(args[1])
		if strings.Contains(string(b), args[2]) {
			return 0
		}
		return 1
	case "env": // reports leaked variables, then leaves a detached sleeper behind
		for _, kv := range os.Environ() {
			name, _, _ := strings.Cut(kv, "=")
			if strings.HasPrefix(name, "GIT_") || strings.HasSuffix(name, "_API_KEY") {
				fmt.Println("leaked", name)
			}
		}
		kid := exec.Command("sleep", "30")
		kid.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if kid.Start() == nil {
			fmt.Println("detached", kid.Process.Pid)
		}
		return 0
	case "hang":
		time.Sleep(time.Hour)
	}
	return 2
}

func self(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

// checkConfig writes a config whose check passes when out.txt holds "good".
func (r runRig) checkConfig(t *testing.T, extra string) {
	t.Helper()
	argv, _ := json.Marshal([]string{self(t), fakeCheckArg, "pass-if", "out.txt", "good"})
	os.WriteFile(r.config, []byte(`{"checks": {"`+r.repo+`": {"test": `+string(argv)+`}}`+extra+`}`), 0o600)
}

func writer(name, write string, env ...string) fake {
	return fake{name: name, agentMode: "turn", env: append([]string{"ACPTEST_WRITE=" + write}, env...)}
}

// fan runs a fan-out over the given fakes, pinned with --harness.
func (r runRig) fan(t *testing.T, fakes []harness.Harness, mut func(*RunOptions)) RunResult {
	t.Helper()
	var names []string
	for _, h := range fakes {
		names = append(names, h.Name())
	}
	return r.run(t, fake{}, func(o *RunOptions) {
		o.Harness, o.Harnesses = strings.Join(names, ","), fakes
		o.Getenv = func(string) string { return "" }
		if mut != nil {
			mut(o)
		}
	})
}

func cand(res RunResult, name string) Candidate {
	for _, c := range res.Candidates {
		if c.Harness == name {
			return c
		}
	}
	return Candidate{}
}

func (r runRig) noWorktreesLeft(t *testing.T) {
	t.Helper()
	if entries, _ := os.ReadDir(r.wtDir); len(entries) != 0 {
		t.Errorf("%d worktrees left", len(entries))
	}
	if out := r.git(t, "branch", "--list", "refraktal/*"); strings.TrimSpace(out) != "" {
		t.Errorf("branches left: %s", out)
	}
}

// Several fake harnesses in parallel; the checks separate them: the one
// that passes wins and is applied, everything else is removed. Each child
// reserved a third of --usd.
func TestFanOutClearWinnerOnChecks(t *testing.T) {
	r := newRunRig(t)
	r.checkConfig(t, "")
	res := r.fan(t, []harness.Harness{writer("claude", "out.txt=good-1"), writer("codex", "out.txt=bad"), fake{name: "agy", agentMode: "turn"}}, nil)
	if res.Outcome != RunApplied || res.ExitCode() != ExitOK || res.Harness != "claude" || res.Verdict == nil ||
		res.Verdict.Method != "checks" || res.Verdict.Checks != "config" || res.Judge != nil {
		t.Fatalf("%+v verdict %+v", res, res.Verdict)
	}
	if b, _ := os.ReadFile(filepath.Join(r.repo, "out.txt")); string(b) != "good-1\n" {
		t.Fatalf("out.txt = %q", b)
	}
	if c := cand(res, "codex"); len(c.Checks) != 1 || c.Checks[0].Status != CheckFailed {
		t.Fatalf("codex %+v", c)
	}
	if c := cand(res, "agy"); c.Outcome != candNoChanges {
		t.Fatalf("agy %+v", c)
	}
	for _, c := range res.Candidates {
		if c.Settlement == nil || !near(c.Settlement.Spent+c.Settlement.Returned, 5.0/3) {
			t.Errorf("%s: grant %+v, want a third of $5", c.Harness, c.Settlement)
		}
	}
	if res.Budget.Turns != 3 {
		t.Errorf("turns %d", res.Budget.Turns)
	}
	r.noWorktreesLeft(t)
	if !exists(filepath.Join(r.repo, res.Run, "claude", "check-test.log")) {
		t.Error("the check's output is not in the run folder")
	}
}

// --no-apply leaves every candidate in its worktree, the winner marked.
func TestFanOutNoApplyKeepsEveryCandidate(t *testing.T) {
	r := newRunRig(t)
	r.checkConfig(t, "")
	res := r.fan(t, []harness.Harness{writer("claude", "out.txt=good-1"), writer("codex", "out.txt=bad")}, func(o *RunOptions) { o.NoApply = true })
	if res.Outcome != RunKept || res.Harness != "claude" || res.Branch == "" || !exists(filepath.Join(res.Worktree, "out.txt")) {
		t.Fatalf("%+v", res)
	}
	for _, c := range res.Candidates {
		if c.Branch == "" || !exists(c.Worktree) {
			t.Errorf("%s not kept: %+v", c.Harness, c)
		}
	}
	if exists(filepath.Join(r.repo, "out.txt")) {
		t.Fatal("--no-apply changed the working tree")
	}
}

// A close call without a pre-score goes to the judge: a read-only harness
// in its own worktree, given the diffs under letters. Its answer picks the
// winner, its attempt to write is denied, and its cost counts.
func TestFanOutCloseCallGoesToTheJudge(t *testing.T) {
	r := newRunRig(t)
	r.checkConfig(t, "")
	judgeEnv := []string{"ACPTEST_JUDGE=Both work; B is cleaner.\nWINNER: B"}
	res := r.fan(t, []harness.Harness{writer("claude", "out.txt=good-1", judgeEnv...), writer("codex", "out.txt=good-2")}, nil)
	if res.Outcome != RunApplied || res.Harness != "codex" || res.Verdict.Method != "judge" || res.Verdict.Judge != "claude" ||
		strings.Join(res.Verdict.CloseCall, ",") != "claude,codex" {
		t.Fatalf("%+v verdict %+v", res, res.Verdict)
	}
	if b, _ := os.ReadFile(filepath.Join(r.repo, "out.txt")); string(b) != "good-2\n" {
		t.Fatalf("out.txt = %q", b)
	}
	if !res.Verdict.JudgedOwn {
		t.Fatal("claude judged a close call it was in: the verdict must say so")
	}
	if j := res.Judge; j == nil || j.Denied != 1 || j.Allowed != 0 || j.Files != nil {
		t.Fatalf("judge %+v: its write must be denied and nothing collected", j)
	}
	if res.Budget.Turns != 3 || len(res.Budget.Charges) != 3 {
		t.Fatalf("the judge's turn and cost must count: %+v", res.Budget)
	}
	r.noWorktreesLeft(t)
}

// The judge can find no candidate correct: no winner, exit 1, candidates kept.
func TestFanOutJudgeSaysNone(t *testing.T) {
	r := newRunRig(t)
	res := r.fan(t, []harness.Harness{writer("claude", "a.txt=1", "ACPTEST_JUDGE=WINNER: none"), writer("codex", "b.txt=2")}, nil)
	if res.Outcome != RunNoWinner || res.ExitCode() != ExitNoCandidate || res.Verdict.Checks != "none" {
		t.Fatalf("%+v verdict %+v", res, res.Verdict)
	}
	for _, c := range res.Candidates {
		if c.Branch == "" {
			t.Errorf("%s not kept", c.Harness)
		}
	}
}

// One child failing does not stop the others.
func TestFanOutOneChildFails(t *testing.T) {
	r := newRunRig(t)
	res := r.fan(t, []harness.Harness{fake{name: "claude", agentMode: "noauth"}, writer("codex", "out.txt=good")}, nil)
	if res.Outcome != RunApplied || res.Harness != "codex" || cand(res, "claude").Outcome != candUnusable {
		t.Fatalf("%+v", res)
	}
	r.noWorktreesLeft(t)
}

// A budget spent mid-fan-out: --turns 2 lets two children run; the third,
// and then the judge, cannot start. Exit 2, candidates kept.
func TestFanOutBudgetExhaustedMidway(t *testing.T) {
	r := newRunRig(t)
	res := r.fan(t, []harness.Harness{writer("claude", "a.txt=1"), writer("codex", "b.txt=2"), writer("agy", "c.txt=3")},
		func(o *RunOptions) { o.Budget.Turns = 2 })
	n := 0
	for _, c := range res.Candidates {
		if c.Outcome == candExhausted {
			n++
		}
	}
	if n != 1 || res.Outcome != RunExhausted || res.ExitCode() != ExitBudget || !strings.Contains(res.Error, "judge") {
		t.Fatalf("exhausted children %d: %+v", n, res)
	}
	if res.Budget.Turns != 2 {
		t.Fatalf("turns %d", res.Budget.Turns)
	}
}

// --mins running out while the judge decides: timed out, exit 2, the
// candidates kept and the judge's worktree removed.
func TestFanOutMinsDuringJudging(t *testing.T) {
	r := newRunRig(t)
	start := time.Now()
	res := r.fan(t, []harness.Harness{writer("claude", "a.txt=1", "ACPTEST_JUDGE=hang"), writer("codex", "b.txt=2")},
		func(o *RunOptions) { o.Budget.Wall = 4 * time.Second })
	if res.Outcome != RunTimedOut || res.ExitCode() != ExitBudget || res.Judge == nil || res.Judge.Outcome != candTimedOut {
		t.Fatalf("%+v judge %+v", res, res.Judge)
	}
	if time.Since(start) > 20*time.Second {
		t.Fatalf("took %s", time.Since(start))
	}
	entries, _ := os.ReadDir(r.wtDir)
	if len(entries) != 2 {
		t.Fatalf("want the two candidates' worktrees, have %d", len(entries))
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "-judge-") {
			t.Fatal("the judge's worktree was kept")
		}
	}
}

// A candidate that changes the check's own definition cannot win on checks:
// only the judge can pick it.
func TestFlaggedCandidateNeedsTheJudge(t *testing.T) {
	r := newRunRig(t)
	r.checkConfig(t, "")
	res := r.fan(t, []harness.Harness{writer("claude", "out.txt=good|.github/workflows/ci.yml=x"), writer("codex", "out.txt=bad", "ACPTEST_JUDGE=WINNER: A")}, nil)
	if c := cand(res, "claude"); !strings.Contains(c.Flagged, "CI files") {
		t.Fatalf("not flagged: %+v", c)
	}
	if res.Outcome != RunApplied || res.Harness != "claude" || res.Verdict.Method != "judge" || res.Verdict.Judge != "codex" {
		t.Fatalf("%+v verdict %+v", res, res.Verdict)
	}
}

// Checks run like a harness child: no GIT_* variables or API keys, output
// only in the run folder, and a process they leave behind is swept.
func TestChecksRunSandboxed(t *testing.T) {
	r := newRunRig(t)
	t.Setenv("GIT_REFRAKTAL_PROBE", "1")
	t.Setenv("GEMINI_API_KEY", "should-not-leak")
	argv, _ := json.Marshal([]string{self(t), fakeCheckArg, "env"})
	os.WriteFile(r.config, []byte(`{"checks": {"`+r.repo+`": {"test": `+string(argv)+`}}}`), 0o600)
	res := r.fan(t, []harness.Harness{writer("claude", "a.txt=1", "ACPTEST_JUDGE=WINNER: A"), writer("codex", "b.txt=2")}, nil)
	for _, name := range []string{"claude", "codex"} {
		b, err := os.ReadFile(filepath.Join(r.repo, res.Run, name, "check-test.log"))
		if err != nil || strings.Contains(string(b), "leaked") {
			t.Fatalf("%s check log %q %v", name, b, err)
		}
		_, after, ok := strings.Cut(string(b), "detached ")
		pid, _ := strconv.Atoi(strings.TrimSpace(after))
		if !ok || pid <= 0 {
			t.Fatalf("no sleeper pid in %q", b)
		}
		dead := false
		for i := 0; i < 40 && !dead; i++ {
			dead = errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
			time.Sleep(50 * time.Millisecond)
		}
		if !dead {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("%s: the check's detached sleeper survived", name)
		}
	}
	if res.Outcome != RunApplied {
		t.Fatalf("%+v", res)
	}
}

// A check that hangs is stopped at its own time limit and fails.
func TestCheckTimeLimit(t *testing.T) {
	r := newRunRig(t)
	argv, _ := json.Marshal([]string{self(t), fakeCheckArg, "hang"})
	os.WriteFile(r.config, []byte(`{"checks": {"`+r.repo+`": {"test": `+string(argv)+`, "timeout_mins": 0.01}}}`), 0o600)
	res := r.fan(t, []harness.Harness{writer("claude", "a.txt=1"), writer("codex", "b.txt=2")}, func(o *RunOptions) { o.Budget.Wall = 30 * time.Second })
	if res.Outcome != RunNoWinner || cand(res, "claude").Checks[0].Status != CheckTimedOut {
		t.Fatalf("%+v %+v", res, cand(res, "claude"))
	}
}

// fanGemini answers routing with a fan-out to claude and codex, and each
// pre-score with score(diff).
func fanGemini(t *testing.T, score func(diff string) float64) *httptest.Server {
	route := `{"answers": {
	 "task_kind": {"type": "choice", "choice": "feature", "confidence": 0.8, "probabilities": {"bugfix": 0.05, "feature": 0.8, "refactor": 0.05, "test": 0.04, "docs": 0.03, "investigation": 0.03}},
	 "harness": {"type": "choice", "choice": "claude-code", "confidence": 0.6, "probabilities": {"claude-code": 0.6, "codex": 0.3, "antigravity": 0.05, "refraktal": 0.05}},
	 "fan_out": {"type": "noul", "noul": 0.9},
	 "fan_out_width": {"type": "score", "score": 0.1, "confidence": 0.8, "probabilities": {"0": 0.8, "1": 0.15, "2": 0.05}},
	 "needs_decomposition": {"type": "noul", "noul": 0.1}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req struct {
			Input     string                     `json:"input"`
			Questions map[string]json.RawMessage `json:"questions"`
		}
		json.Unmarshal(b, &req)
		if _, ok := req.Questions["correct"]; !ok {
			w.Write([]byte(route))
			return
		}
		var in struct{ Diff string }
		json.Unmarshal([]byte(req.Input), &in)
		fmt.Fprintf(w, `{"answers": {"correct": {"type": "noul", "noul": %v}}}`, score(in.Diff))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func routed(srv *httptest.Server, fakes []harness.Harness) func(*RunOptions) {
	return func(o *RunOptions) {
		o.Harness, o.Harnesses, o.RouteCache, o.RouterEndpoint = "", fakes, "-", srv.URL
		o.Getenv = func(k string) string { return map[string]string{"GEMINI_API_KEY": "test-key"}[k] }
	}
}

// With a key, the router's fan-out runs, and the pre-score separates the
// candidates by more than the margin: no judge.
func TestFanOutPreScore(t *testing.T) {
	r := newRunRig(t)
	srv := fanGemini(t, func(d string) float64 {
		if strings.Contains(d, "good-1") {
			return 0.9
		}
		return 0.5
	})
	fakes := []harness.Harness{writer("claude", "out.txt=good-1"), writer("codex", "out.txt=good-2"), writer("agy", "out.txt=never")}
	res := r.run(t, fake{}, routed(srv, fakes))
	if res.Outcome != RunApplied || res.Harness != "claude" || res.Verdict.Method != "pre-score" || len(res.Candidates) != 2 || res.Judge != nil {
		t.Fatalf("%+v verdict %+v", res, res.Verdict)
	}
	if c := cand(res, "codex"); c.Score == nil || *c.Score != 0.5 {
		t.Fatalf("codex %+v", c)
	}
	// --fan caps the router's width.
	os.Remove(filepath.Join(r.repo, "out.txt"))
	res = r.run(t, fake{}, func(o *RunOptions) { routed(srv, fakes)(o); o.Fan = 1 })
	if len(res.Candidates) != 0 || res.Harness != "claude" || !strings.Contains(strings.Join(res.Notices, "\n"), "--fan 1 caps it") {
		t.Fatalf("--fan 1: %+v", res)
	}
}

// Pre-scores within the margin are a close call for the judge; the margin
// comes from the config.
func TestFanOutPreScoreCloseCall(t *testing.T) {
	r := newRunRig(t)
	srv := fanGemini(t, func(d string) float64 {
		if strings.Contains(d, "good-1") {
			return 0.80
		}
		return 0.75
	})
	fakes := []harness.Harness{writer("claude", "out.txt=good-1", "ACPTEST_JUDGE=WINNER: B"), writer("codex", "out.txt=good-2")}
	res := r.run(t, fake{}, routed(srv, fakes))
	if res.Harness != "codex" || res.Verdict.Method != "judge" {
		t.Fatalf("margin 0.10: %+v", res.Verdict)
	}
	os.WriteFile(r.config, []byte(`{"judge": {"margin": 0.01}}`), 0o600)
	os.Remove(filepath.Join(r.repo, "out.txt")) // the first run applied it
	res = r.run(t, fake{}, routed(srv, fakes))
	if res.Harness != "claude" || res.Verdict.Method != "pre-score" || res.Verdict.Margin != 0.01 {
		t.Fatalf("margin 0.01: %+v", res.Verdict)
	}
}

// Check commands are detected from the base commit only; ambiguity means
// no check.
func TestPlanChecks(t *testing.T) {
	r := newRunRig(t)
	ctx := context.Background()
	commit := func(files map[string]string) string {
		for f, c := range files {
			os.WriteFile(filepath.Join(r.repo, f), []byte(c), 0o644)
		}
		r.git(t, "add", "-A")
		r.git(t, "commit", "-q", "-m", "x")
		return strings.TrimSpace(r.git(t, "rev-parse", "HEAD"))
	}
	none := strings.TrimSpace(r.git(t, "rev-parse", "HEAD"))
	if p := planChecks(ctx, r.repo, none, config.Config{}, r.config); p.Source != "none" || len(p.Steps) != 0 || p.Note == "" {
		t.Fatalf("nothing to detect: %+v", p)
	}
	npm := commit(map[string]string{"package.json": `{"scripts": {"test": "echo \"Error: no test specified\" && exit 1"}}`})
	if p := planChecks(ctx, r.repo, npm, config.Config{}, r.config); p.Source != "none" {
		t.Fatalf("npm's placeholder counted: %+v", p)
	}
	npm = commit(map[string]string{"package.json": `{"scripts": {"test": "vitest run"}}`})
	if p := planChecks(ctx, r.repo, npm, config.Config{}, r.config); p.Source != "npm" || p.Steps[0].Argv[0] != "npm" {
		t.Fatalf("npm: %+v", p)
	}
	both := commit(map[string]string{"go.mod": "module x\n\ngo 1.24\n"})
	if p := planChecks(ctx, r.repo, both, config.Config{}, r.config); p.Source != "none" || !strings.Contains(p.Note, "more than one") {
		t.Fatalf("ambiguous: %+v", p)
	}
	// The base commit decides, not the working tree.
	if p := planChecks(ctx, r.repo, none, config.Config{}, r.config); p.Source != "none" || strings.Contains(p.Note, "more than one") {
		t.Fatalf("read the working tree: %+v", p)
	}
	cfg := config.Config{Checks: map[string]config.Check{r.repo: {Build: []string{"b"}, Test: []string{"t"}, TimeoutMins: 2}}}
	if p := planChecks(ctx, r.repo, both, cfg, r.config); p.Source != "config" || len(p.Steps) != 2 || p.Timeout != 2*time.Minute {
		t.Fatalf("config: %+v", p)
	}
	os.Remove(filepath.Join(r.repo, "package.json"))
	r.git(t, "add", "-A")
	r.git(t, "commit", "-q", "-m", "x")
	gomod := strings.TrimSpace(r.git(t, "rev-parse", "HEAD"))
	p := planChecks(ctx, r.repo, gomod, config.Config{}, r.config)
	if p.Source != "go" || len(p.Steps) != 2 {
		t.Fatalf("go: %+v", p)
	}
	tool := []byte("diff --git a/go.mod b/go.mod\n--- a/go.mod\n+++ b/go.mod\n@@ -1,3 +1,3 @@\n module x\n-go 1.24\n+go 1.26\n")
	if why := p.touchesDefinition([]string{"go.mod"}, tool); !strings.Contains(why, "toolchain") {
		t.Fatalf("go line change not flagged: %q", why)
	}
	req := []byte("diff --git a/go.mod b/go.mod\n--- a/go.mod\n+++ b/go.mod\n@@ -3,1 +3,2 @@\n go 1.24\n+require y v1.0.0\n")
	if why := p.touchesDefinition([]string{"go.mod"}, req); why != "" {
		t.Fatalf("a require line flagged: %q", why)
	}
}

func pf(v float64) *float64 { return &v }

func TestChoose(t *testing.T) {
	yes := true
	plan := checkPlan{Steps: []checkStep{{"test", []string{"x"}}}, Source: "config"}
	pass := []CheckResult{{Name: "test", Status: CheckPassed}}
	mk := func(name string, score *float64, checks []CheckResult, flagged string) *Candidate {
		return &Candidate{Harness: name, Outcome: candChanges, Applies: &yes, Checks: checks, Score: score, Flagged: flagged}
	}
	// A passing candidate beats a higher pre-score with no passing check.
	w, j, v := choose([]*Candidate{mk("a", pf(0.9), []CheckResult{{Status: CheckFailed}}, ""), mk("b", pf(0.2), pass, "")}, plan, true, 0.1)
	if w == nil || w.Harness != "b" || j != nil || v.Method != "checks" {
		t.Fatalf("checks first: %v %v %+v", w, j, v)
	}
	// Within the top rank, the pre-score decides beyond the margin.
	w, _, v = choose([]*Candidate{mk("a", pf(0.5), pass, ""), mk("b", pf(0.7), pass, "")}, plan, true, 0.1)
	if w == nil || w.Harness != "b" || v.Method != "pre-score" {
		t.Fatalf("pre-score: %v %+v", w, v)
	}
	// Exactly at the margin is a close call.
	_, j, _ = choose([]*Candidate{mk("a", pf(0.6), pass, ""), mk("b", pf(0.7), pass, "")}, plan, true, 0.1)
	if len(j) != 2 {
		t.Fatalf("at the margin: %v", j)
	}
	// No pre-score: a tie in the top rank is a close call.
	_, j, _ = choose([]*Candidate{mk("a", nil, pass, ""), mk("b", nil, pass, "")}, plan, false, 0.1)
	if len(j) != 2 {
		t.Fatalf("unscored tie: %v", j)
	}
	// A flagged candidate ranks as unchecked; alone at the top it needs the judge.
	_, j, _ = choose([]*Candidate{mk("a", nil, pass, "changes CI files"), mk("b", nil, []CheckResult{{Status: CheckFailed}}, "")}, plan, false, 0.1)
	if len(j) != 1 || j[0].Harness != "a" {
		t.Fatalf("flagged: %v", j)
	}
	// Nothing applies: no winner.
	no := false
	if w, j, v := choose([]*Candidate{{Harness: "a", Outcome: candChanges, Applies: &no}}, plan, false, 0.1); w != nil || j != nil || v.Method != "none" {
		t.Fatalf("none: %v %v %+v", w, j, v)
	}
}

func TestParseWinner(t *testing.T) {
	labels := []string{"A", "B"}
	// Only the final line, in the exact format, counts.
	for _, tc := range [][2]string{
		{"B is better.\nWINNER: B", "B"},
		{"B is better.\nWINNER: B\n\n", "B"},
		{"WINNER: A\nOn reflection:\nWINNER: none", "none"},
		{"WINNER: B\nThat is my answer.", ""},
		{"I considered writing WINNER: A but\nWINNER: B", "B"},
		{"Not WINNER: A", ""},
		{"winner: a", ""},
		{"**WINNER: B**", ""},
		{"WINNER: B.", ""},
		{"WINNER: C", ""},
		{"I pick B", ""},
	} {
		if got := parseWinner(tc[0], labels); got != tc[1] {
			t.Errorf("%q: got %q, want %q", tc[0], got, tc[1])
		}
	}
}

// (d) Claude's cost is cumulative per session: a second prompt in the same
// session is charged the difference from the previous report.
func TestSessionCostChargesTheDelta(t *testing.T) {
	var s sessionCost
	turn := func(amount float64) acp.Turn { return acp.Turn{Cost: &acp.Cost{Amount: amount, Currency: "USD"}} }
	first := s.charges("claude", "m", turn(0.01), price.Table{})
	second := s.charges("claude", "m", turn(0.025), price.Table{})
	if len(first) != 1 || !near(first[0].USD, 0.01) || len(second) != 1 || !near(second[0].USD, 0.015) || second[0].Source != budget.Reported {
		t.Fatalf("first %+v second %+v", first, second)
	}
}

// The judge comes from a usable harness outside the close call when one is
// available, even one the fan-out did not run; the verdict records it.
func TestJudgeIsPickedOutsideTheCloseCall(t *testing.T) {
	r := newRunRig(t)
	outsider := fake{name: "agy", agentMode: "turn", env: []string{"ACPTEST_JUDGE=WINNER: B"}}
	res := r.fan(t, []harness.Harness{writer("claude", "a.txt=1", "ACPTEST_JUDGE=WINNER: A"), writer("codex", "b.txt=2")},
		func(o *RunOptions) { o.Harnesses = append(o.Harnesses, outsider) })
	if v := res.Verdict; v == nil || v.Judge != "agy" || v.JudgedOwn || v.Winner != "codex" || len(res.Candidates) != 2 {
		t.Fatalf("verdict %+v", res.Verdict)
	}
	// An outsider that cannot be started (logged out) is skipped: a
	// candidate judges, and the verdict says it judged its own candidate.
	os.Remove(filepath.Join(r.repo, "b.txt"))
	res = r.fan(t, []harness.Harness{writer("claude", "a.txt=1", "ACPTEST_JUDGE=WINNER: A"), writer("codex", "b.txt=2")},
		func(o *RunOptions) { o.Harnesses = append(o.Harnesses, fake{name: "agy", agentMode: "noauth"}) })
	if v := res.Verdict; v == nil || v.Judge != "claude" || !v.JudgedOwn || v.Winner != "claude" || len(v.JudgesTried) != 1 || v.JudgesTried[0] != "agy" {
		t.Fatalf("fallback verdict %+v", res.Verdict)
	}
	// show (M7) renders the run log: the verdict there names the judge.
	events, _ := os.ReadFile(filepath.Join(r.repo, res.Run, "events.jsonl"))
	if !strings.Contains(string(events), `"judge":"claude","judged_own":true`) {
		t.Fatal("the run log's verdict does not record that the judge judged its own candidate")
	}
}

// (5) make reads GNUmakefile, then makefile, then Makefile: detect all three
// from the base, run the one make would use explicitly, and flag changes to
// any of them.
func TestMakeVariants(t *testing.T) {
	r := newRunRig(t)
	ctx := context.Background()
	commit := func(files map[string]string) string {
		for f, c := range files {
			os.WriteFile(filepath.Join(r.repo, f), []byte(c), 0o644)
		}
		r.git(t, "add", "-A")
		r.git(t, "commit", "-q", "-m", "x")
		return strings.TrimSpace(r.git(t, "rev-parse", "HEAD"))
	}
	base := commit(map[string]string{"makefile": "test:\n\ttrue\n"})
	p := planChecks(ctx, r.repo, base, config.Config{}, r.config)
	if p.Source != "make" || strings.Join(p.Steps[0].Argv, " ") != "make -f makefile test" {
		t.Fatalf("lowercase makefile: %+v", p)
	}
	base = commit(map[string]string{"GNUmakefile": "test:\n\ttrue\n", "Makefile": "build:\n\ttrue\n"})
	p = planChecks(ctx, r.repo, base, config.Config{}, r.config)
	if p.Source != "make" || strings.Join(p.Steps[0].Argv, " ") != "make -f GNUmakefile test" {
		t.Fatalf("GNUmakefile first: %+v", p)
	}
	for _, f := range []string{"GNUmakefile", "makefile", "Makefile"} {
		if why := p.touchesDefinition([]string{f}, nil); why == "" {
			t.Errorf("a change to %s is not flagged", f)
		}
	}
	// make uses the first file present: here GNUmakefile, which has no test
	// target, so there is no make check even though Makefile has one.
	base = commit(map[string]string{"GNUmakefile": "build:\n\ttrue\n", "Makefile": "test:\n\ttrue\n"})
	if p := planChecks(ctx, r.repo, base, config.Config{}, r.config); p.Source == "make" {
		t.Fatalf("detected a target make would not use: %+v", p)
	}
}

// (4) A candidate that edits or deletes tests cannot win on checks alone.
func TestTestFileChangesAreFlagged(t *testing.T) {
	p := checkPlan{Steps: []checkStep{{"test", []string{"go", "test", "./..."}}}, Source: "go"}
	for _, f := range []string{"pkg/a_test.go", "test/x.sh", "src/tests/y.py", "web/__tests__/z.js", "a.test.ts", "b.spec.js",
		"test_mod.py", "pkg/mod_test.py"} {
		if why := p.touchesDefinition([]string{f}, nil); !strings.Contains(why, "test files") {
			t.Errorf("%s not flagged: %q", f, why)
		}
	}
	for _, f := range []string{"main.go", "testing.md", "contest/a.go", "latest.go", "attestation.go"} {
		if why := p.touchesDefinition([]string{f}, nil); why != "" {
			t.Errorf("%s flagged: %q", f, why)
		}
	}
	// Without a check there is nothing to win on checks: nothing is flagged.
	if why := (checkPlan{Source: "none"}).touchesDefinition([]string{"a_test.go", ".github/x.yml"}, nil); why != "" {
		t.Errorf("flagged with no check: %q", why)
	}
}
