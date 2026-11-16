package refraktal

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"refraktal.dev/refraktal/internal/acp"
	"refraktal.dev/refraktal/internal/acp/acptest"
	"refraktal.dev/refraktal/internal/budget"
	"refraktal.dev/refraktal/internal/harness"
)

// childACPArg makes the test binary a child Refraktal ACP server (see
// TestMain), configured by a JSON acpChildSpec.
const childACPArg = "refraktal-acp-child"

type fakeSpec struct {
	Name  string        `json:"name"`
	Mode  string        `json:"mode,omitempty"`
	Env   []string      `json:"env,omitempty"`
	Child *acpChildSpec `json:"child,omitempty"` // a refraktal harness: a grandchild
}

type acpChildSpec struct {
	Pin        string     `json:"pin"`
	Fakes      []fakeSpec `json:"fakes"`
	Live       string     `json:"live"`
	Config     string     `json:"config"`
	WT         string     `json:"wt"`
	Price      string     `json:"price"`
	PriceCache string     `json:"price_cache"`
	Flags      *budget.Limits
}

func (s acpChildSpec) harnesses() []harness.Harness {
	var hs []harness.Harness
	self, _ := os.Executable()
	for _, f := range s.Fakes {
		if f.Child != nil {
			b, _ := json.Marshal(f.Child)
			hs = append(hs, harness.Refraktal(self, childACPArg, string(b)))
			continue
		}
		hs = append(hs, fake{name: f.Name, agentMode: cmpOrS(f.Mode, "turn"), env: f.Env})
	}
	return hs
}

// childACP serves ACP on stdio as a child Refraktal would with `acp`.
func childACP(spec string) int {
	var s acpChildSpec
	if err := json.Unmarshal([]byte(spec), &s); err != nil {
		return 3
	}
	lim := budget.Defaults
	if s.Flags != nil {
		lim = *s.Flags
	}
	o := ServeOptions{Run: RunOptions{Harness: s.Pin, Harnesses: s.harnesses(), Budget: &lim, LiveDir: s.Live, ConfigPath: s.Config,
		WorktreeDir: s.WT, PriceURL: s.Price, PriceCache: s.PriceCache, Getenv: func(string) string { return "" }}}
	if err := ServeACP(context.Background(), os.Stdin, os.Stdout, o); err != nil {
		return 3
	}
	return 0
}

func (r runRig) acpChildSpec(pin string, fakes ...fakeSpec) acpChildSpec {
	return acpChildSpec{Pin: pin, Fakes: fakes, Live: r.live, Config: r.config, WT: r.wtDir, Price: r.priceURL, PriceCache: r.priceCache}
}

func (r runRig) refraktal(s acpChildSpec) harness.Harness {
	self, _ := os.Executable()
	b, _ := json.Marshal(s)
	return harness.Refraktal(self, childACPArg, string(b))
}

// serve runs ServeACP in process and returns an ACP client connected to it.
func serve(t *testing.T, o ServeOptions, onUpdate func(acp.Update)) *acp.Client {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan struct{})
	go func() { ServeACP(context.Background(), inR, outW, o); outW.Close(); close(done) }()
	t.Cleanup(func() { inW.Close(); <-done })
	return acp.New(outR, inW, acp.Options{ClientName: "test", ClientVersion: "1", OnUpdate: onUpdate, CancelGrace: 10 * time.Second})
}

func (r runRig) serveOptions(h fake, getenv map[string]string) ServeOptions {
	h.prompts = r.prompts
	lim := budget.Limits{USD: 5, Turns: 5, Wall: 20 * time.Second, Depth: 2}
	return ServeOptions{Run: RunOptions{Harness: h.name, Harnesses: []harness.Harness{h}, Budget: &lim, LiveDir: r.live,
		ConfigPath: r.config, WorktreeDir: r.wtDir, PriceURL: r.priceURL, PriceCache: r.priceCache,
		Getenv: func(string) string { return "" }},
		Getenv: func(k string) string { return getenv[k] }}
}

type updates struct {
	mu  sync.Mutex
	raw []string
}

func (u *updates) add(x acp.Update) { u.mu.Lock(); u.raw = append(u.raw, string(x.Raw)); u.mu.Unlock() }
func (u *updates) all() string      { u.mu.Lock(); defer u.mu.Unlock(); return strings.Join(u.raw, "\n") }

func open(t *testing.T, c *acp.Client, cwd string) string {
	t.Helper()
	ctx := context.Background()
	if _, err := c.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	s, err := c.NewSession(ctx, cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetMode(ctx, s.ID, "default"); err != nil {
		t.Fatal(err)
	}
	return s.ID
}

// A prompt is one Refraktal run in the session's folder: the result is
// applied there, its cost arrives as a usage_update, and the prompts it sent
// in _meta.refraktal.
func TestServeACPRunsAPrompt(t *testing.T) {
	r := newRunRig(t)
	var u updates
	parent := filepath.Join(t.TempDir(), "parent-run")
	os.MkdirAll(parent, 0o700)
	c := serve(t, r.serveOptions(writer("claude", "out.txt=good"), map[string]string{ParentRunEnv: parent}), u.add)
	id := open(t, c, r.repo)
	turn, err := c.Prompt(context.Background(), id, "write out.txt")
	if err != nil || turn.StopReason != "end_turn" || turn.Child == nil || turn.Child.Turns != 1 || turn.Child.Outcome != RunApplied {
		t.Fatalf("turn %+v child %+v err %v", turn, turn.Child, err)
	}
	if turn.Cost == nil || !near(turn.Cost.Amount, 0.0123) || turn.Cost.Currency != "USD" {
		t.Fatalf("cost %+v", turn.Cost)
	}
	if b, _ := os.ReadFile(filepath.Join(r.repo, "out.txt")); string(b) != "good\n" {
		t.Fatalf("out.txt = %q", b)
	}
	if !strings.Contains(u.all(), "refraktal: applied") {
		t.Fatalf("no summary chunk: %s", u.all())
	}
	// The run folder is nested in the parent's.
	if m, _ := filepath.Glob(filepath.Join(parent, "runs", "*", "events.jsonl")); len(m) != 1 {
		t.Fatalf("nested run folders %v", m)
	}
	// A second prompt in the session reports the session's cumulative cost.
	os.Remove(filepath.Join(r.repo, "out.txt"))
	turn, _ = c.Prompt(context.Background(), id, "again")
	if turn.Cost == nil || !near(turn.Cost.Amount, 0.0246) {
		t.Fatalf("second cost %+v", turn.Cost)
	}
	// Only the default mode exists.
	if err := c.SetMode(context.Background(), id, "plan"); err == nil {
		t.Fatal("set_mode plan accepted")
	}
}

func TestChildLimits(t *testing.T) {
	now := time.Now()
	flags := budget.Limits{USD: 10, Turns: 20, Wall: 30 * time.Minute, Depth: 2}
	env := `{"usd": 0.5, "turns": 3, "deadline": "` + now.Add(time.Minute).UTC().Format(time.RFC3339) + `", "depth": 0}`
	l, err := childLimits(flags, env, now)
	if err != nil || l.USD != 0.5 || l.Turns != 3 || l.Depth != 0 || l.Wall > time.Minute || l.Wall < 58*time.Second {
		t.Fatalf("%+v %v", l, err)
	}
	// A grant never raises a budget.
	if l, _ := childLimits(budget.Limits{USD: 1, Turns: 2, Wall: time.Second, Depth: 0}, `{"usd": 99, "turns": 99, "depth": 9}`, now); l.USD != 1 || l.Turns != 2 || l.Depth != 0 {
		t.Fatalf("raised: %+v", l)
	}
	var ex *budget.Exhausted
	if _, err := childLimits(flags, `{"deadline": "`+now.Add(-time.Second).UTC().Format(time.RFC3339)+`"}`, now); !errors.As(err, &ex) || ex.Budget != "mins" {
		t.Fatalf("past deadline: %v", err)
	}
	for _, bad := range []string{`{`, `{"usd": -1}`, `{"bogus": 1}`, `{"deadline": "soon"}`} {
		if _, err := childLimits(flags, bad, now); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if l, _ := childLimits(flags, "", now); l != flags {
		t.Fatalf("no grant: %+v", l)
	}
}

// The parent's grant binds the child: no turns left means nothing is sent.
func TestServeACPHonoursTheGrant(t *testing.T) {
	for name, env := range map[string]string{"turns": `{"turns": 0}`, "depth": `{"depth": -1}`, "usd": `{"usd": 0}`} {
		r := newRunRig(t)
		c := serve(t, r.serveOptions(writer("claude", "out.txt=good"), map[string]string{BudgetEnv: env}), nil)
		turn, err := c.Prompt(context.Background(), open(t, c, r.repo), "x")
		if err != nil || turn.StopReason != "max_turn_requests" || turn.Child == nil || turn.Child.Outcome != RunExhausted {
			t.Errorf("%s: %+v %v", name, turn, err)
		}
		if exists(r.prompts) {
			t.Errorf("%s: a prompt reached the harness", name)
		}
	}
}

// session/cancel stops the running run; the turn ends cancelled.
func TestServeACPCancel(t *testing.T) {
	r := newRunRig(t)
	var u updates
	c := serve(t, r.serveOptions(fake{name: "agy", agentMode: "hang"}, nil), u.add)
	id := open(t, c, r.repo)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(1500 * time.Millisecond); cancel() }()
	start := time.Now()
	turn, err := c.Prompt(ctx, id, "x")
	if turn.StopReason != "cancelled" || time.Since(start) > 15*time.Second {
		t.Fatalf("turn %+v err %v after %s", turn, err, time.Since(start))
	}
}

// Nothing from a child's harness crosses verbatim: not its auth
// notifications, not error text carrying account details.
func TestServeACPNeverForwardsAuth(t *testing.T) {
	for _, mode := range []string{"auth", "noauth"} {
		r := newRunRig(t)
		var u updates
		c := serve(t, r.serveOptions(fake{name: "claude", agentMode: mode}, nil), u.add)
		c.Prompt(context.Background(), open(t, c, r.repo), "x")
		if strings.Contains(u.all(), acptest.AuthEmail) {
			t.Errorf("%s: the account email crossed to the client", mode)
		}
	}
}

// A Refraktal drives a child Refraktal (a process, over ACP) that drives a
// harness: the child's result is applied to the user's tree, its prompts
// count against the parent's --turns, its cost is charged as reported, and
// its run folder nests in the parent's, which show renders.
func TestNestedRefraktal(t *testing.T) {
	r := newRunRig(t)
	child := r.refraktal(r.acpChildSpec("claude", fakeSpec{Name: "claude", Env: []string{"ACPTEST_WRITE=out.txt=good"}}))
	res := r.run(t, fake{}, func(o *RunOptions) { o.Harness, o.Harnesses = "refraktal", []harness.Harness{child} })
	if res.Outcome != RunApplied || res.Harness != "refraktal" || res.Budget.Turns != 2 {
		t.Fatalf("%+v", res)
	}
	if b, _ := os.ReadFile(filepath.Join(r.repo, "out.txt")); string(b) != "good\n" {
		t.Fatalf("out.txt = %q", b)
	}
	if len(res.Budget.Charges) != 1 || res.Budget.Charges[0].Source != budget.Reported || !near(res.Budget.Spent, 0.0123) {
		t.Fatalf("charges %+v", res.Budget)
	}
	sr, err := Show(r.repo, "")
	if err != nil || len(sr.Nested) != 1 || sr.Nested[0].Via != "refraktal" || sr.Nested[0].Outcome != RunApplied || sr.Nested[0].Winner != "claude" {
		t.Fatalf("show %+v %v", sr, err)
	}
	var text strings.Builder
	WriteShowText(&text, sr)
	for _, want := range []string{"run " + sr.ID + ": applied", "nested run", "child Refraktal, via refraktal", "candidate refraktal: changes, 1 files, 1 prompts in its own run", "result from: claude"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("show text lacks %q:\n%s", want, text.String())
		}
	}
}

// --depth shrinks down the tree: with --depth 1 the child gets depth 0, so a
// grandchild Refraktal finds its depth spent and runs nothing.
func TestDepthStopsAGrandchild(t *testing.T) {
	r := newRunRig(t)
	marker := filepath.Join(t.TempDir(), "grandchild-prompts")
	grand := r.acpChildSpec("claude", fakeSpec{Name: "claude", Env: []string{"ACPTEST_PROMPTS=" + marker, "ACPTEST_WRITE=out.txt=deep"}})
	child := r.refraktal(r.acpChildSpec("refraktal", fakeSpec{Name: "refraktal", Child: &grand}))
	res := r.run(t, fake{}, func(o *RunOptions) {
		o.Harness, o.Harnesses = "refraktal", []harness.Harness{child}
		o.Budget.Depth = 1
	})
	if exists(marker) || exists(filepath.Join(r.repo, "out.txt")) {
		t.Fatal("the grandchild ran a harness past --depth")
	}
	if res.Outcome == RunApplied {
		t.Fatalf("%+v", res)
	}
	// The child ran, and its refraktal candidate (the grandchild) found its
	// depth spent: refused before it made a run folder or started anything.
	sr, _ := Show(r.repo, "")
	if len(sr.Nested) != 1 || len(sr.Nested[0].Child) != 1 || sr.Nested[0].Child[0].Harness != "refraktal" ||
		sr.Nested[0].Child[0].Outcome != candFailed || len(sr.Nested[0].Nested) != 0 {
		t.Fatalf("tree %+v", sr)
	}
}

// show renders a fan-out run: candidates, the verdict with its judge and
// whether it judged its own candidate, and costs by source; a run is chosen
// by ID or is the latest, and a path is not an ID.
func TestShowAFanOut(t *testing.T) {
	r := newRunRig(t)
	res := r.fan(t, []harness.Harness{writer("claude", "a.txt=1", "ACPTEST_JUDGE=WINNER: B"), writer("codex", "b.txt=2")}, nil)
	sr, err := Show(r.repo, filepath.Base(res.Run))
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	WriteShowText(&text, sr)
	for _, want := range []string{"harness: claude,codex (pinned with --harness)", "candidate claude: changes", "candidate codex: changes",
		"judge claude: no_changes", "verdict: codex by judge", "judge claude, judged its own candidate: true", "reported $0.0", "turns 3 of 5"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("show lacks %q:\n%s", want, text.String())
		}
	}
	var js strings.Builder
	WriteShowJSON(&js, sr)
	var back struct {
		Type    string         `json:"type"`
		Verdict map[string]any `json:"verdict"`
		Costs   ShowCosts      `json:"costs"`
	}
	if json.Unmarshal([]byte(js.String()), &back) != nil || back.Type != "run" || back.Verdict["judged_own"] != true || back.Costs.Turns != 3 {
		t.Fatalf("json %s", js.String())
	}
	if latest, _ := Show(r.repo, ""); latest.ID != sr.ID {
		t.Fatalf("latest %s, want %s", latest.ID, sr.ID)
	}
	for _, bad := range []string{"../x", ".refraktal", "a/b"} {
		if _, err := Show(r.repo, bad); err == nil {
			t.Errorf("%q accepted as a run ID", bad)
		}
	}
	if _, err := Show(t.TempDir(), ""); !errors.Is(err, ErrNoRun) {
		t.Fatalf("no runs: %v", err)
	}
}

// show treats run logs as untrusted: nested runs are followed only under the
// refraktal harness's scratch folder and never through a symlink, and no
// control character (a terminal escape) reaches the text output.
func TestShowTreatsLogsAsUntrusted(t *testing.T) {
	root := t.TempDir()
	run := filepath.Join(root, ".refraktal", "runs", "20260101-000000-aaaaaa")
	write := func(dir, task string) {
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(`{"event":"run_start","task":"`+task+`"}`+"\n"+`{"event":"run_end","outcome":"applied"}`+"\n"), 0o644)
	}
	write(run, `fix it\u001b]0;pwned\u0007\u001b[2J`)
	write(filepath.Join(run, "codex", "runs", "x"), "a harness wrote this") // not the refraktal harness's folder
	elsewhere := t.TempDir()
	write(filepath.Join(elsewhere, "y"), "outside the run folder")
	os.MkdirAll(filepath.Join(run, "refraktal", "runs"), 0o755)
	os.Symlink(filepath.Join(elsewhere, "y"), filepath.Join(run, "refraktal", "runs", "y"))
	sr, err := Show(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(sr.Nested) != 0 {
		t.Fatalf("followed %d nested runs: %+v", len(sr.Nested), sr.Nested)
	}
	var text strings.Builder
	WriteShowText(&text, sr)
	for _, r := range text.String() {
		if r < 0x20 && r != '\n' || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			t.Fatalf("control character %U in show output: %q", r, text.String())
		}
	}
	if !strings.Contains(text.String(), "task: fix it") {
		t.Fatalf("task lost: %q", text.String())
	}
}

// The nesting guard is independent of budgets: a child already nested past
// its depth + 1 levels (REFRAKTAL_NEST, one more per child) refuses to start
// even when its grant's depth would allow it.
func TestNestingGuardStopsAChild(t *testing.T) {
	r := newRunRig(t)
	marker := filepath.Join(t.TempDir(), "child-prompts")
	child := r.refraktal(r.acpChildSpec("claude", fakeSpec{Name: "claude", Env: []string{"ACPTEST_PROMPTS=" + marker}}))
	t.Setenv("REFRAKTAL_NEST", "6") // the parent may start (6 <= 5 + 1); its child is at 7
	res := r.run(t, fake{}, func(o *RunOptions) {
		o.Harness, o.Harnesses = "refraktal", []harness.Harness{child}
		o.Budget.Depth = 5 // the child's grant: depth 4, so only the guard stops it
	})
	if exists(marker) || res.Outcome != RunFailed {
		t.Fatalf("the child ran past the nesting guard: %+v", res)
	}
	events, _ := os.ReadFile(filepath.Join(r.repo, res.Run, "events.jsonl"))
	if !strings.Contains(string(events), `"event":"child_run"`) || !strings.Contains(string(events), `"outcome":"budget_exhausted"`) {
		t.Fatalf("the child's refusal is not recorded:\n%s", events)
	}
}

// A value in a run log cannot forge a line of show's output: newlines and
// bidi or format characters inside values are dropped.
func TestShowValuesCannotForgeLines(t *testing.T) {
	root := t.TempDir()
	run := filepath.Join(root, ".refraktal", "runs", "20260101-000000-bbbbbb")
	os.MkdirAll(run, 0o755)
	os.WriteFile(filepath.Join(run, "events.jsonl"), []byte(
		`{"event":"run_start","task":"fix it\n  verdict: evil by judge (forged)","harness":"claude‮"}`+"\n"+
			`{"event":"run_end","outcome":"failed\n  cost: $0.0000","files":["a\nb"]}`+"\n"), 0o644)
	sr, err := Show(root, "")
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	WriteShowText(&text, sr)
	costs := 0
	for _, l := range strings.Split(text.String(), "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "verdict:") {
			t.Fatalf("a log value forged a verdict line:\n%s", text.String())
		}
		if strings.HasPrefix(l, "cost:") {
			costs++
		}
	}
	if costs != 1 {
		t.Fatalf("a log value forged a cost line:\n%s", text.String())
	}
	if strings.ContainsRune(text.String(), '‮') {
		t.Fatal("a bidi override reached the output")
	}
}
