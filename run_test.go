package refraktal

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"refraktal.dev/refraktal/internal/acp/acptest"
	"refraktal.dev/refraktal/internal/budget"
	"refraktal.dev/refraktal/internal/harness"
)

type runRig struct {
	repo, wtDir, live, config, prompts, priceURL, priceCache string
}

// prices is a small LiteLLM-style table: fake-model costs $1 per million
// input tokens and $2 per million output tokens.
const prices = `{"fake-model": {"input_cost_per_token": 1e-06, "output_cost_per_token": 2e-06}}`

func newRunRig(t *testing.T) runRig {
	t.Helper()
	d := t.TempDir()
	r := runRig{repo: filepath.Join(d, "repo"), wtDir: filepath.Join(d, "worktrees"), live: filepath.Join(d, "live"),
		config: filepath.Join(d, "config.json"), prompts: filepath.Join(d, "prompts")}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(prices)) }))
	t.Cleanup(srv.Close)
	r.priceURL, r.priceCache = srv.URL, filepath.Join(d, "prices.json")
	os.MkdirAll(r.repo, 0o755)
	r.git(t, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(r.repo, "README"), []byte("x\n"), 0o644)
	r.git(t, "add", "-A")
	r.git(t, "commit", "-q", "-m", "init")
	return r
}

func (r runRig) git(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", r.repo}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return string(out)
}

func (r runRig) run(t *testing.T, h fake, mut func(*RunOptions)) RunResult {
	t.Helper()
	h.prompts = r.prompts
	lim := budget.Limits{USD: 5, Turns: 5, Wall: 10 * time.Second, Depth: 2}
	o := RunOptions{Task: "do the thing", Harness: h.name, Dir: r.repo, Budget: &lim, PriceURL: r.priceURL, PriceCache: r.priceCache,
		LiveDir: r.live, ConfigPath: r.config, WorktreeDir: r.wtDir, Harnesses: []harness.Harness{h}}
	if mut != nil {
		mut(&o)
	}
	res, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

func TestRunAppliesWhatThePolicyAllows(t *testing.T) {
	r := newRunRig(t)
	res := r.run(t, fake{name: "claude", agentMode: "tools"}, nil)
	if res.Outcome != RunApplied || res.ExitCode() != ExitOK || res.Allowed != 1 || res.Denied != 2 {
		t.Fatalf("result %+v", res)
	}
	if b, _ := os.ReadFile(filepath.Join(r.repo, "hello.txt")); string(b) != "hello\n" {
		t.Fatalf("hello.txt in the working tree = %q", b)
	}
	if exists(filepath.Join(r.wtDir, "outside.txt")) || exists(filepath.Join(r.repo, "pushed.txt")) {
		t.Fatal("a denied action happened")
	}
	if strings.Join(res.Files, ",") != "hello.txt" || res.Branch != "" || res.Worktree != "" {
		t.Fatalf("files %v branch %q worktree %q", res.Files, res.Branch, res.Worktree)
	}
	if entries, _ := os.ReadDir(r.wtDir); len(entries) != 0 {
		t.Fatal("worktree not removed after applying")
	}
	if out := r.git(t, "branch", "--list", "refraktal/*"); strings.TrimSpace(out) != "" {
		t.Fatalf("branch left: %s", out)
	}
	if res.Cost == nil || res.Usage == nil || res.Usage.TotalTokens != 900 {
		t.Fatalf("usage %+v cost %+v", res.Usage, res.Cost)
	}
	events, _ := os.ReadFile(filepath.Join(r.repo, res.Run, "events.jsonl"))
	if strings.Count(string(events), `"event":"permission"`) != 3 || !strings.Contains(string(events), "deny-list: git push") {
		t.Fatalf("permission decisions not logged:\n%s", events)
	}
	if !exists(filepath.Join(r.repo, res.Run, "result.diff")) {
		t.Fatal("no result.diff in the run folder")
	}
	if out := r.git(t, "status", "--porcelain"); strings.Contains(out, ".refraktal") {
		t.Fatalf("run folder visible to git: %s", out)
	}
}

func TestRunNoApplyKeepsTheBranch(t *testing.T) {
	r := newRunRig(t)
	res := r.run(t, fake{name: "claude", agentMode: "tools"}, func(o *RunOptions) { o.NoApply = true })
	if res.Outcome != RunKept || res.ExitCode() != ExitOK || res.Branch == "" || res.Worktree == "" {
		t.Fatalf("result %+v", res)
	}
	if exists(filepath.Join(r.repo, "hello.txt")) {
		t.Fatal("--no-apply changed the working tree")
	}
	if !exists(filepath.Join(res.Worktree, "hello.txt")) {
		t.Fatal("the kept worktree lacks the change")
	}
	if out := r.git(t, "log", "--oneline", res.Branch, "-1"); !strings.Contains(out, "do the thing") {
		t.Fatalf("branch head: %s", out)
	}
}

func TestRunWithNoChanges(t *testing.T) {
	r := newRunRig(t)
	res := r.run(t, fake{name: "codex", agentMode: "turn"}, nil)
	if res.Outcome != RunNoChanges || res.ExitCode() != ExitOK || res.StopReason != "end_turn" || !strings.Contains(res.Reply, "Hello") {
		t.Fatalf("result %+v", res)
	}
	if entries, _ := os.ReadDir(r.wtDir); len(entries) != 0 {
		t.Fatal("worktree not removed")
	}
}

func TestRunTimesOut(t *testing.T) {
	r := newRunRig(t)
	res := r.run(t, fake{name: "agy", agentMode: "hang"}, func(o *RunOptions) { o.Budget.Wall = 2 * time.Second })
	if res.Outcome != RunTimedOut || res.ExitCode() != ExitBudget || res.StopReason != "cancelled" {
		t.Fatalf("result %+v", res)
	}
	// The cancelled child keeps its whole reservation for enforcement. Its
	// finished call (200 in, 100 out) is computed; the call the cancel cut off
	// is estimated from the last context fill (5000) priced as input.
	if res.Settlement == nil || !near(res.Settlement.Spent, 5) || res.Settlement.Returned != 0 {
		t.Fatalf("settlement %+v", res.Settlement)
	}
	if !near(res.Budget.Reported, 200*1e-6+100*2e-6) || !near(res.Budget.Estimated, 5000*1e-6) || !near(res.Budget.Spent, 5) {
		t.Fatalf("budget %+v", res.Budget)
	}
}

func TestRunWithALoggedOutHarness(t *testing.T) {
	r := newRunRig(t)
	res := r.run(t, fake{name: "claude", agentMode: "noauth"}, nil)
	if res.Outcome != RunUnusable || res.ExitCode() != ExitNothingUsable || !strings.Contains(res.Error, StatusLoggedOut) {
		t.Fatalf("result %+v", res)
	}
	if _, err := os.Stat(filepath.Join(r.prompts)); err == nil {
		t.Fatal("prompt sent to a harness that failed the handshake")
	}
	if entries, _ := os.ReadDir(r.wtDir); len(entries) != 0 {
		t.Fatal("worktree not removed")
	}
}

func TestRunNotices(t *testing.T) {
	r := newRunRig(t)
	os.MkdirAll(filepath.Join(r.repo, ".claude"), 0o755)
	os.WriteFile(filepath.Join(r.repo, ".claude", "settings.json"), []byte(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"true"}]}]}}`), 0o644)
	r.git(t, "add", "-A")
	r.git(t, "commit", "-q", "-m", "hooks")
	var seen []string
	res := r.run(t, fake{name: "claude", agentMode: "turn"}, func(o *RunOptions) { o.OnNotice = func(s string) { seen = append(seen, s) } })
	if len(res.Notices) != 1 || len(seen) != 1 || !strings.Contains(seen[0], ".claude/settings.json") {
		t.Fatalf("notices %v", res.Notices)
	}

	os.WriteFile(r.config, []byte(`{"harnesses":{"codex":{"mcp":"inherit"}}}`), 0o600)
	res = r.run(t, fake{name: "codex", agentMode: "turn"}, nil)
	if res.MCP != harness.MCPInherit || len(res.Notices) != 1 || !strings.Contains(res.Notices[0], "your own MCP servers") {
		t.Fatalf("mcp %q notices %v", res.MCP, res.Notices)
	}
}

func TestRunNeedsATaskAndAKnownHarness(t *testing.T) {
	r := newRunRig(t)
	if _, err := Run(context.Background(), RunOptions{Harness: "claude", Dir: r.repo}); err == nil {
		t.Fatal("want an error for an empty task")
	}
	_, err := Run(context.Background(), RunOptions{Task: "x", Harness: "nope", Dir: r.repo, LiveDir: r.live,
		ConfigPath: r.config, WorktreeDir: r.wtDir, Harnesses: []harness.Harness{fake{name: "claude"}}})
	if err == nil || !strings.Contains(err.Error(), "unknown harness") {
		t.Fatalf("got %v", err)
	}
}

func TestRunNoApplyKeepsTheWorktreeEvenWithoutChanges(t *testing.T) {
	r := newRunRig(t)
	res := r.run(t, fake{name: "claude", agentMode: "turn"}, func(o *RunOptions) { o.NoApply = true })
	if res.Outcome != RunNoChanges || res.Worktree == "" || !exists(res.Worktree) {
		t.Fatalf("result %+v", res)
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestRunRefusesWhenABudgetIsSpent(t *testing.T) {
	for name, set := range map[string]func(*budget.Limits){
		"usd":   func(l *budget.Limits) { l.USD = 0 },
		"turns": func(l *budget.Limits) { l.Turns = 0 },
		"depth": func(l *budget.Limits) { l.Depth = -1 },
	} {
		r := newRunRig(t)
		res := r.run(t, fake{name: "claude", agentMode: "turn"}, func(o *RunOptions) { set(o.Budget) })
		if res.Outcome != RunExhausted || res.ExitCode() != ExitBudget || !strings.Contains(res.Error, "--"+name) {
			t.Errorf("%s: %+v", name, res)
		}
		if exists(r.prompts) {
			t.Errorf("%s: a prompt was sent", name)
		}
		if entries, _ := os.ReadDir(r.wtDir); len(entries) != 0 {
			t.Errorf("%s: worktree created", name)
		}
	}
}

func TestRunCostSources(t *testing.T) {
	r := newRunRig(t)
	// Reported: the harness reports a cost (as Claude does).
	res := r.run(t, fake{name: "claude", agentMode: "turn"}, nil)
	if len(res.Budget.Charges) != 1 || res.Budget.Charges[0].Source != budget.Reported || !near(res.Budget.Spent, 0.0123) ||
		res.Settlement == nil || !near(res.Settlement.Returned, 5-0.0123) || res.Model != "fake-model" {
		t.Fatalf("reported: %+v %+v", res.Budget, res.Settlement)
	}
	// Computed: tokens only, priced from the table. Usage: 1400 uncached
	// input, 1000 cached reads (no cache price: priced as input), 100 output.
	res = r.run(t, fake{name: "codex", agentMode: "turn", env: []string{"ACPTEST_NOCOST=1"}}, nil)
	want := 1400*1e-6 + 1000*1e-6 + 100*2e-6
	if len(res.Budget.Charges) != 1 || res.Budget.Charges[0].Source != budget.Computed || !near(res.Budget.Spent, want) {
		t.Fatalf("computed: %+v", res.Budget)
	}
	// Unpriced: a model the table does not know counts as $0, with a warning.
	res = r.run(t, fake{name: "agy", agentMode: "turn", env: []string{"ACPTEST_NOCOST=1", "ACPTEST_MODEL=mystery-model"}}, nil)
	if len(res.Budget.Charges) != 1 || res.Budget.Charges[0].Source != budget.Unpriced || res.Budget.Spent != 0 ||
		len(res.Budget.Unpriced) != 1 || !strings.Contains(strings.Join(res.Notices, "\n"), `no price for model "mystery-model"`) {
		t.Fatalf("unpriced: %+v notices %v", res.Budget, res.Notices)
	}
	// An override in the config prices the unknown model.
	os.WriteFile(r.config, []byte(`{"prices":{"mystery-model":{"input":10,"output":20}}}`), 0o600)
	res = r.run(t, fake{name: "agy", agentMode: "turn", env: []string{"ACPTEST_NOCOST=1", "ACPTEST_MODEL=mystery-model"}}, nil)
	want = (1400+1000)*10e-6 + 100*20e-6
	if res.Budget.Charges[0].Source != budget.Computed || !near(res.Budget.Spent, want) {
		t.Fatalf("override: %+v", res.Budget)
	}
}

func TestRunReportsAnOverrun(t *testing.T) {
	r := newRunRig(t)
	res := r.run(t, fake{name: "claude", agentMode: "turn"}, func(o *RunOptions) { o.Budget.USD = 0.01 })
	if res.Outcome != RunNoChanges || res.Settlement == nil || !near(res.Settlement.Overrun, 0.0023) ||
		!strings.Contains(strings.Join(res.Notices, "\n"), "beyond the --usd grant") {
		t.Fatalf("%+v notices %v", res.Settlement, res.Notices)
	}
}

// 9. --mins running out while the harness starts is a spent budget (exit 2),
// not an unusable harness, and the budget summary is filled in.
func TestRunStartupDeadlineIsBudgetExhausted(t *testing.T) {
	r := newRunRig(t)
	res := r.run(t, fake{name: "claude", agentMode: "slowstart"}, func(o *RunOptions) { o.Budget.Wall = time.Second })
	if res.Outcome != RunExhausted || res.ExitCode() != ExitBudget || res.Budget.Limits.USD != 5 {
		t.Fatalf("%+v", res)
	}
}

// The shell prefix is never guessed from os.Executable(): in a test or a
// library host that is a different program, and Claude would run it for
// every tool command. Without RunOptions.ShellPrefix there is no prefix.
func TestShellPrefixIsOptIn(t *testing.T) {
	r := newRunRig(t)
	h := fake{name: "claude", agentMode: "turn", prefixVar: "CLAUDE_CODE_SHELL_PREFIX"}
	r.run(t, h, nil)
	if b, _ := os.ReadFile(r.prompts + ".env"); !strings.HasPrefix(strings.TrimSpace(string(b)), "prefix= helper= ") {
		t.Fatalf("without ShellPrefix the harness got %q", b)
	}
	self, _ := os.Executable() // its TestMain calls proc.MaybeShellPrefix first
	r.run(t, h, func(o *RunOptions) { o.ShellPrefix = self })
	if b, _ := os.ReadFile(r.prompts + ".env"); !strings.HasPrefix(strings.TrimSpace(string(b)), "prefix="+self+" helper=shell-prefix") {
		t.Fatalf("with ShellPrefix the harness got %q", b)
	}
}

// B. In a test, the shell prefix may only be the test binary itself (whose
// TestMain calls proc.MaybeShellPrefix first), never another program.
func TestForeignShellPrefixIsRefusedInATest(t *testing.T) {
	r := newRunRig(t)
	res := r.run(t, fake{name: "claude", agentMode: "turn", prefixVar: "CLAUDE_CODE_SHELL_PREFIX"},
		func(o *RunOptions) { o.ShellPrefix = "/bin/echo" })
	if res.Outcome != RunUnusable || !strings.Contains(res.Error, "shell prefix") || exists(r.prompts) {
		t.Fatalf("%+v", res)
	}
}

// C. A hard nesting guard, independent of budgets: a run refuses to start
// past depth + 1 levels of REFRAKTAL_NEST, and its children get one more.
func TestNestingGuard(t *testing.T) {
	r := newRunRig(t)
	t.Setenv("REFRAKTAL_NEST", "4") // depth 2 allows up to 3
	res := r.run(t, fake{name: "claude", agentMode: "turn"}, nil)
	if res.Outcome != RunExhausted || res.ExitCode() != ExitBudget || !strings.Contains(res.Error, "REFRAKTAL_NEST") || exists(r.prompts) {
		t.Fatalf("%+v", res)
	}
	t.Setenv("REFRAKTAL_NEST", "1")
	r.run(t, fake{name: "claude", agentMode: "turn"}, nil)
	if b, _ := os.ReadFile(r.prompts + ".env"); !strings.Contains(string(b), "nest=2") {
		t.Fatalf("child env: %q", b)
	}
}

// 14. A turn the harness ended as cancelled on its own counts as cancelled:
// the child keeps its whole reservation, and the cut-off call is estimated.
func TestHarnessCancelledTurnKeepsItsReservation(t *testing.T) {
	r := newRunRig(t)
	res := r.run(t, fake{name: "codex", agentMode: "selfcancel"}, nil)
	if res.Settlement == nil || !near(res.Settlement.Spent, 5) || !near(res.Budget.Estimated, 4000*1e-6) {
		t.Fatalf("settlement %+v budget %+v", res.Settlement, res.Budget)
	}
}

// 15. The grant's deadline covers every setup step, the price fetch
// included: a slow fetch must not outlive --mins.
func TestSlowPriceFetchHonoursTheDeadline(t *testing.T) {
	r := newRunRig(t)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		select {
		case <-time.After(4 * time.Second):
		case <-req.Context().Done():
		}
		w.Write([]byte(prices))
	}))
	defer slow.Close()
	start := time.Now()
	res := r.run(t, fake{name: "claude", agentMode: "turn"}, func(o *RunOptions) { o.Budget.Wall = time.Second; o.PriceURL = slow.URL })
	if res.Outcome != RunExhausted || res.ExitCode() != ExitBudget || time.Since(start) > 3*time.Second {
		t.Fatalf("after %s: %+v", time.Since(start), res)
	}
}

// Hardening 4. Durable run events never carry raw harness error text: a code
// and a short digest only; the raw text goes to the gitignored stderr capture.
func TestRunEventsCarryNoRawHarnessErrors(t *testing.T) {
	r := newRunRig(t)
	res := r.run(t, fake{name: "claude", agentMode: "noauth"}, nil)
	events, _ := os.ReadFile(filepath.Join(r.repo, res.Run, "events.jsonl"))
	if strings.Contains(string(events), acptest.AuthEmail) || strings.Contains(string(events), "Authentication required") {
		t.Fatalf("raw harness error text in the run events:\n%s", events)
	}
	if !strings.Contains(string(events), `"code":"logged_out"`) {
		t.Fatalf("no error code in the run events:\n%s", events)
	}
	stderr, _ := os.ReadFile(filepath.Join(r.repo, res.Run, "claude", "stderr"))
	if !strings.Contains(string(stderr), "Authentication required") {
		t.Fatal("the raw text is not in the stderr capture")
	}
}

func TestDoctorEventsCarryNoRawHarnessErrors(t *testing.T) {
	r := newRig(t)
	rep := r.doctor(t, fake{name: "claude", agentMode: "noauth", prompts: r.prompts})
	events, _ := os.ReadFile(filepath.Join(r.root, rep.Run, "events.jsonl"))
	if strings.Contains(string(events), acptest.AuthEmail) || !strings.Contains(string(events), `"code":"logged_out"`) {
		t.Fatalf("doctor events:\n%s", events)
	}
}

// geminiServer answers like the Decisions API, with one of the spike's
// recorded responses (the bugfix sample, routed to codex).
func geminiServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var fx []struct {
		Raw json.RawMessage `json:"raw_response"`
	}
	b, err := os.ReadFile(filepath.Join("internal", "route", "testdata", "spike_live.json"))
	if err != nil || json.Unmarshal(b, &fx) != nil {
		t.Fatal(err)
	}
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("x-goog-api-key") == "" {
			http.Error(w, "no key", http.StatusUnauthorized)
			return
		}
		w.Write(fx[0].Raw)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func allFakes() []harness.Harness {
	return []harness.Harness{fake{name: "claude", agentMode: "turn"}, fake{name: "codex", agentMode: "turn"}, fake{name: "agy", agentMode: "turn"}}
}

// Without --harness the router chooses: with no key, the deterministic
// fallback; the decision is logged and the routed harness runs.
func TestRunRoutesWithoutAHarness(t *testing.T) {
	r := newRunRig(t)
	res := r.run(t, fake{}, func(o *RunOptions) {
		o.Harness, o.Harnesses, o.RouteCache = "", allFakes(), "-"
		o.Task = "Fix the off-by-one error in pagination: the last page of results is dropped."
		o.Getenv = func(string) string { return "" }
	})
	if res.Route == nil || res.Route.Decision.Decider != "fallback" || res.Harness != "codex" || res.Outcome != RunNoChanges {
		t.Fatalf("%+v", res)
	}
	if !strings.Contains(strings.Join(res.Notices, "\n"), "routed to codex by fallback") {
		t.Fatalf("notices %v", res.Notices)
	}
	if b, err := os.ReadFile(filepath.Join(r.repo, ".refraktal", "decisions.jsonl")); err != nil || !strings.Contains(string(b), `"thresholds"`) {
		t.Fatalf("decision log: %v", err)
	}
	events, _ := os.ReadFile(filepath.Join(r.repo, res.Run, "events.jsonl"))
	if !strings.Contains(string(events), `"event":"route"`) {
		t.Fatal("no route event")
	}
}

// --dry plans and routes, with or without a key, and runs nothing.
func TestRunDry(t *testing.T) {
	r := newRunRig(t)
	res := r.run(t, fake{}, func(o *RunOptions) {
		o.Harness, o.Harnesses, o.Dry, o.RouteCache = "", allFakes(), true, "-"
		o.Getenv = func(string) string { return "" }
	})
	if res.Outcome != RunPlanned || res.ExitCode() != ExitOK || res.Route == nil || res.Route.Decision.Decider != "fallback" || exists(r.prompts) {
		t.Fatalf("without a key: %+v", res)
	}
	if entries, _ := os.ReadDir(r.wtDir); len(entries) != 0 {
		t.Fatal("--dry created a worktree")
	}
	srv, hits := geminiServer(t)
	res = r.run(t, fake{}, func(o *RunOptions) {
		o.Harness, o.Harnesses, o.Dry, o.RouteCache, o.RouterEndpoint = "", allFakes(), true, "-", srv.URL
		o.Getenv = func(k string) string { return map[string]string{"GEMINI_API_KEY": "test-key"}[k] }
	})
	if res.Outcome != RunPlanned || res.Route == nil || res.Route.Decision.Decider != "gemini" || res.Harness != "codex" || hits.Load() != 1 {
		t.Fatalf("with a key: %+v (hits %d)", res, hits.Load())
	}
}

func TestRunFanAndHarnessOverride(t *testing.T) {
	r := newRunRig(t)
	if _, err := Run(context.Background(), RunOptions{Task: "x", Fan: 2, Dir: r.repo}); err == nil || !strings.Contains(err.Error(), "M6") {
		t.Fatalf("--fan 2: %v", err)
	}
	srv, hits := geminiServer(t)
	res := r.run(t, fake{name: "agy", agentMode: "turn"}, func(o *RunOptions) {
		o.RouterEndpoint = srv.URL
		o.Getenv = func(k string) string { return map[string]string{"GEMINI_API_KEY": "test-key"}[k] }
	})
	if res.Route != nil || hits.Load() != 0 || res.Harness != "agy" {
		t.Fatalf("--harness must override the router: %+v (hits %d)", res, hits.Load())
	}
}
