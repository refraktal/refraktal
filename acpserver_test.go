package refraktal

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
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
	WallMS     int64 `json:"wall_ms,omitempty"` // Limits.Wall is not in its JSON
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
	if s.WallMS > 0 {
		lim.Wall = time.Duration(s.WallMS) * time.Millisecond
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

// Review M7 (1): one ledger per acp server, seeded from the inherited grant
// and shared by every prompt and session: the grant does not reset.
func TestServeACPSharesOneLedger(t *testing.T) {
	r := newRunRig(t)
	c := serve(t, r.serveOptions(writer("claude", "out.txt=good"), map[string]string{BudgetEnv: `{"turns": 1}`}), nil)
	id := open(t, c, r.repo)
	if turn, _ := c.Prompt(context.Background(), id, "first"); turn.StopReason != "end_turn" {
		t.Fatalf("first: %+v", turn)
	}
	os.Remove(filepath.Join(r.repo, "out.txt"))
	other, _ := c.NewSession(context.Background(), r.repo, nil)
	turn, _ := c.Prompt(context.Background(), other.ID, "second, in another session")
	if turn.StopReason != "max_turn_requests" {
		t.Fatalf("the grant's one turn was used, yet a second prompt ran: %+v", turn)
	}
	if b, _ := os.ReadFile(r.prompts); strings.Count(string(b), "prompt") != 1 {
		t.Fatalf("prompts sent: %q", b)
	}
}

// Review M7 (10): the cost reported upstream is what the run actually spent;
// reservations kept by cancelled children go separately, so the parent can
// hold them without reporting them as spent.
func TestServeACPReportsActualCostAndRetainedSeparately(t *testing.T) {
	r := newRunRig(t)
	o := r.serveOptions(fake{name: "agy", agentMode: "hang"}, nil)
	o.Run.Budget.Wall = 1500 * time.Millisecond // the turn times out: the child keeps its $5 reservation
	c := serve(t, o, nil)
	turn, _ := c.Prompt(context.Background(), open(t, c, r.repo), "x")
	want := 200*1e-6 + 100*2e-6 // the call that finished before the cancel
	if turn.Cost == nil || !near(turn.Cost.Amount, want) {
		t.Fatalf("reported cost %+v, want the actual %v", turn.Cost, want)
	}
	if turn.Child == nil || !near(turn.Child.Retained, 5-want) {
		t.Fatalf("retained %+v, want %v", turn.Child, 5-want)
	}
}

// Without an inherited grant (a standalone server, say an editor's), each
// prompt gets the flags afresh: --turns 1 allows one prompt per run.
func TestStandaloneServeACPBudgetsPerPrompt(t *testing.T) {
	r := newRunRig(t)
	o := r.serveOptions(writer("claude", "out.txt=good"), nil)
	o.Run.Budget.Turns = 1
	c := serve(t, o, nil)
	id := open(t, c, r.repo)
	for i := 0; i < 2; i++ {
		os.Remove(filepath.Join(r.repo, "out.txt"))
		if turn, _ := c.Prompt(context.Background(), id, "x"); turn.StopReason != "end_turn" {
			t.Fatalf("prompt %d: %+v", i, turn)
		}
	}
}

// Review M7 (10), the parent's side: a child Refraktal whose harness turn
// was cancelled reports its actual cost as reported and the reservation it
// kept as retained; the parent holds both against --usd but reports only
// the first as spent.
func TestParentHoldsAChildsRetainedReservation(t *testing.T) {
	r := newRunRig(t)
	spec := r.acpChildSpec("agy", fakeSpec{Name: "agy", Mode: "hang"})
	spec.Flags, spec.WallMS = &budget.Limits{USD: 5, Turns: 5, Depth: 2}, 2500
	res := r.run(t, fake{}, func(o *RunOptions) { o.Harness, o.Harnesses = "refraktal", []harness.Harness{r.refraktal(spec)} })
	actual := 200*1e-6 + 100*2e-6
	if !near(res.Budget.Reported, actual) || !near(res.Budget.Retained, 5-actual) || !near(res.Budget.Spent, 5) {
		sr, _ := Show(r.repo, "")
		var b strings.Builder
		WriteShowText(&b, sr)
		ev, _ := filepath.Glob(filepath.Join(r.repo, res.Run, "refraktal", "runs", "*", "events.jsonl"))
		for _, f := range ev {
			x, _ := os.ReadFile(f)
			for _, l := range strings.Split(string(x), "\n") {
				if strings.Contains(l, "run_end") || strings.Contains(l, "turn") || strings.Contains(l, "child_end") {
					b.WriteString(l[:min(len(l), 300)] + "\n")
				}
			}
		}
		t.Fatalf("budget %+v\n%s", res.Budget, b.String())
	}
}

// Review M7 (3): a session whose folder is a subfolder of the repository
// stays there: the harness works in that subfolder of its worktree, and a
// result that changes anything outside it is refused, applying nothing.
func TestSessionSubfolderIsEnforced(t *testing.T) {
	r := newRunRig(t)
	os.MkdirAll(filepath.Join(r.repo, "sub"), 0o755)
	os.WriteFile(filepath.Join(r.repo, "sub", "keep"), []byte("k\n"), 0o644)
	r.git(t, "add", "-A")
	r.git(t, "commit", "-q", "-m", "sub")
	sub := filepath.Join(r.repo, "sub")
	c := serve(t, r.serveOptions(fake{name: "claude", agentMode: "turn", env: []string{"ACPTEST_RAWWRITE=here.txt=x"}}, nil), nil)
	if turn, _ := c.Prompt(context.Background(), open(t, c, sub), "x"); turn.StopReason != "end_turn" {
		t.Fatalf("inside: %+v", turn)
	}
	if !exists(filepath.Join(sub, "here.txt")) || exists(filepath.Join(r.repo, "here.txt")) {
		t.Fatal("the harness did not work in the session's subfolder")
	}
	c = serve(t, r.serveOptions(fake{name: "claude", agentMode: "turn", env: []string{"ACPTEST_RAWWRITE=../escape.txt=x|ok.txt=y"}}, nil), nil)
	turn, _ := c.Prompt(context.Background(), open(t, c, sub), "x")
	if turn.StopReason == "end_turn" || exists(filepath.Join(r.repo, "escape.txt")) || exists(filepath.Join(sub, "ok.txt")) {
		t.Fatalf("a change outside the session folder was applied: %+v", turn)
	}
}

// Review M7 (4): a client that stops reading cannot hold the server: writes
// are bounded, a stuck transport cancels the runs, their harnesses are shut
// down, and ServeACP returns.
func TestServeACPClientThatStopsReading(t *testing.T) {
	r := newRunRig(t)
	o := r.serveOptions(fake{name: "claude", agentMode: "hang", mcp: harness.MCPInherit}, nil) // the MCP notice is written mid-run
	o.WriteTimeout = 500 * time.Millisecond
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- ServeACP(context.Background(), inR, outW, o) }()
	t.Cleanup(func() { inW.Close(); outR.Close() })
	for _, m := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"` + r.repo + `","mcpServers":[]}}`,
	} {
		io.WriteString(inW, m+"\n")
	}
	buf := make([]byte, 1<<16)
	for got := ""; strings.Count(got, `"id":`) < 2; { // read the two responses, then stop reading
		n, err := outR.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		got += string(buf[:n])
	}
	io.WriteString(inW, `{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"refraktal-1","prompt":[{"type":"text","text":"x"}]}}`+"\n")
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("ServeACP returned no error for a client that stopped reading")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("a client that stopped reading held the server")
	}
	if entries, _ := os.ReadDir(r.live); len(entries) != 0 {
		t.Fatalf("%d live records left: the harness was stranded", len(entries))
	}
}

// Review M7 (12): an idle server honours Ctrl-C (its context): the pending
// read does not hold it.
func TestIdleServeACPHonoursCancel(t *testing.T) {
	inR, inW := io.Pipe()
	defer inW.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ServeACP(ctx, inR, io.Discard, ServeOptions{}) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("an idle server ignored its context")
	}
}

// Child replies and raw errors remain local; the client gets only safe
// notices and Refraktal's structured summary.
func TestServeACPSummaryDoesNotForwardChildReply(t *testing.T) {
	const localPath = "/private/test-agent/session/account.json"
	var out bytes.Buffer
	s := &server{w: bufio.NewWriter(&out)}
	s.start()
	s.chunk("s1", "notice: the price table is unavailable\n")
	s.finish("s1", json.RawMessage(`7`), summary(RunResult{
		Outcome: RunApplied, Harness: "claude", Files: []string{"out.txt"},
		Reply: "Read the local session at " + localPath,
		Error: "private child error at " + localPath,
	}), 0.5, acp.ChildReport{Turns: 1}, "end_turn")
	s.stop()
	var text strings.Builder
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var m struct {
			Params struct {
				Update struct {
					Kind    string `json:"sessionUpdate"`
					Content struct{ Text string }
				} `json:"update"`
			} `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatal(err)
		}
		if m.Params.Update.Kind == "agent_message_chunk" {
			text.WriteString(m.Params.Update.Content.Text)
		}
	}
	want := "notice: the price table is unavailable\nrefraktal: applied (claude), 1 files: out.txt; the error is in the child's run log"
	if got := text.String(); got != want {
		t.Fatalf("client received text outside the structured summary and safe notices: %q", got)
	}
	if strings.Contains(out.String(), localPath) {
		t.Fatal("the child's local path crossed to the client")
	}
}

// A partially decoded parameter object must not start or cancel work,
// create a session, or initialize the shared budget.
func TestServeACPRejectsMalformedParametersBeforeStateChanges(t *testing.T) {
	dir := t.TempDir()
	cwd, err := json.Marshal(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, method, params string
		notification         bool
	}{
		{"new", "session/new", `{"cwd":` + string(cwd) + `,"cwd":7}`, false},
		{"set mode", "session/set_mode", `{"sessionId":"s1","modeId":"default","modeId":7}`, false},
		{"prompt", "session/prompt", `{"sessionId":"s1","prompt":[{"type":"text","text":"valid task"},{"type":"text","text":7}]}`, false},
		{"cancel", "session/cancel", `{"sessionId":"s1","sessionId":7}`, false},
		{"cancel notification", "session/cancel", `{"sessionId":"s1","sessionId":7}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			running, cancel := context.WithCancel(context.Background())
			defer cancel()
			workStarted := false
			release := make(chan struct{})
			s := &server{
				ctx: context.Background(), out: make(chan []byte, 8), dead: make(chan struct{}),
				sess: map[string]*serverSession{"s1": {cwd: dir}},
				o: ServeOptions{Now: time.Now, Getenv: func(string) string {
					workStarted = true
					<-release
					// A dispatched malformed prompt must stop here, before Run
					// can access the filesystem or launch any child process.
					return `{"depth":-1}`
				}},
			}
			if tc.method == "session/cancel" {
				s.sess["s1"].cancel = cancel
			}
			id := json.RawMessage(`19`)
			if tc.notification {
				id = nil
			}
			s.handle(rpc{ID: id, Method: tc.method, Params: json.RawMessage(tc.params)})
			s.mu.Lock()
			promptStarted := tc.method == "session/prompt" && s.sess["s1"].cancel != nil
			s.mu.Unlock()
			close(release)
			s.wg.Wait()
			if workStarted || promptStarted {
				t.Error("malformed parameters dispatched work")
			}
			if s.ledger != nil || s.lerr != nil {
				t.Error("malformed parameters initialized the shared budget")
			}
			if s.next != 0 || len(s.sess) != 1 {
				t.Error("malformed parameters created a session")
			}
			if running.Err() != nil {
				t.Error("malformed parameters cancelled a running prompt")
			}
			if tc.notification {
				if len(s.out) != 0 {
					t.Fatal("a malformed notification received a response")
				}
				return
			}
			if len(s.out) != 1 {
				t.Fatalf("got %d frames, want one invalid-params response", len(s.out))
			}
			var reply rpc
			if err := json.Unmarshal(<-s.out, &reply); err != nil {
				t.Fatal(err)
			}
			if string(reply.ID) != string(id) || reply.Error == nil || reply.Error.Code != -32602 {
				t.Fatalf("malformed parameters did not return invalid params: %+v", reply)
			}
		})
	}
}

// Review M7 (7): a large result never makes a frame past MaxFrame: the
// accounting (cost) goes first in its own small frame, then the summary in
// pieces, then the response.
func TestServeACPSplitsLargeSummaries(t *testing.T) {
	old := maxChunkBytes
	maxChunkBytes = 1000
	defer func() { maxChunkBytes = old }()
	var out bytes.Buffer
	s := &server{w: bufio.NewWriter(&out)}
	s.start()
	big := strings.Repeat("é<\x01", 5000) // multibyte and escaped characters
	s.finish("s1", json.RawMessage(`7`), big, 0.5, acp.ChildReport{Turns: 1}, "end_turn")
	s.stop()
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	var text strings.Builder
	for i, l := range lines {
		if len(l) > 8*maxChunkBytes {
			t.Fatalf("frame %d is %d bytes", i, len(l))
		}
		var m struct {
			ID     json.RawMessage `json:"id"`
			Params struct {
				Update struct {
					Kind    string `json:"sessionUpdate"`
					Content struct{ Text string }
				} `json:"update"`
			} `json:"params"`
		}
		json.Unmarshal([]byte(l), &m)
		switch {
		case i == 0 && m.Params.Update.Kind != "usage_update":
			t.Fatalf("the first frame is %q, want the usage_update", l[:min(len(l), 80)])
		case i == len(lines)-1 && string(m.ID) != "7":
			t.Fatal("the response is not last")
		case m.Params.Update.Kind == "agent_message_chunk":
			text.WriteString(m.Params.Update.Content.Text)
		}
	}
	if text.String() != big || len(lines) < 10 {
		t.Fatalf("summary reassembled to %d bytes in %d frames", len(text.String()), len(lines))
	}
}

// Review M7 (3), rename variant (automated review): deleting a file outside
// the session folder and recreating it inside looks like a rename, whose
// name-only listing shows only the new path. The confinement check must see
// both sides.
func TestSessionSubfolderSeesRenames(t *testing.T) {
	r := newRunRig(t)
	content := strings.Repeat("a line that git will recognise as the same file\n", 20)
	os.MkdirAll(filepath.Join(r.repo, "sub"), 0o755)
	os.WriteFile(filepath.Join(r.repo, "sub", "keep"), []byte("k\n"), 0o644)
	os.WriteFile(filepath.Join(r.repo, "move.txt"), []byte(content), 0o644)
	r.git(t, "add", "-A")
	r.git(t, "commit", "-q", "-m", "files")
	c := serve(t, r.serveOptions(fake{name: "claude", agentMode: "turn",
		env: []string{"ACPTEST_RAWDELETE=../move.txt", "ACPTEST_RAWWRITE=moved.txt=" + strings.TrimSuffix(content, "\n")}}, nil), nil)
	turn, _ := c.Prompt(context.Background(), open(t, c, filepath.Join(r.repo, "sub")), "x")
	if !exists(filepath.Join(r.repo, "move.txt")) || turn.StopReason == "end_turn" {
		t.Fatalf("a rename from outside the session folder was applied: %+v", turn)
	}
}

// Review M7 (9): notices reach the ACP client as structured, safe text:
// no raw OS error, URL or path (the CLI still shows the full text).
func TestServeACPNoticesCarryNoRawErrors(t *testing.T) {
	r := newRunRig(t)
	var u updates
	o := r.serveOptions(writer("claude", "out.txt=good"), nil)
	o.Run.PriceURL = "http://127.0.0.1:1/prices.json" // nothing listens: the fetch fails with an OS error
	o.Run.PriceCache = filepath.Join(t.TempDir(), "secret-cache-dir", "prices.json")
	c := serve(t, o, u.add)
	c.Prompt(context.Background(), open(t, c, r.repo), "x")
	got := u.all()
	if !strings.Contains(got, "price table") {
		t.Fatalf("no price notice at all: %s", got)
	}
	for _, raw := range []string{"127.0.0.1", "secret-cache-dir", "connection refused", "dial tcp"} {
		if strings.Contains(got, raw) {
			t.Errorf("the client got %q in a notice", raw)
		}
	}
}

// Review M7 (6): show walks the run tree through pinned handles and refuses
// a symlink at every component, .refraktal itself included.
func TestShowRefusesASymlinkedRefraktalFolder(t *testing.T) {
	root, other := t.TempDir(), t.TempDir()
	run := filepath.Join(other, "runs", "20260101-000000-cccccc")
	os.MkdirAll(run, 0o755)
	os.WriteFile(filepath.Join(run, "events.jsonl"), []byte(`{"event":"run_end","outcome":"applied"}`+"\n"), 0o644)
	os.Symlink(other, filepath.Join(root, ".refraktal"))
	if sr, err := Show(root, ""); err == nil {
		t.Fatalf("followed a symlinked .refraktal: %+v", sr)
	}
}

// Review M7 (8): show's walk has global caps (bytes, events, directory
// entries, depth); past one it stops and says the output is truncated.
func TestShowHasGlobalCaps(t *testing.T) {
	root := t.TempDir()
	run := filepath.Join(root, ".refraktal", "runs", "20260101-000000-dddddd")
	os.MkdirAll(run, 0o755)
	var log strings.Builder
	for i := 0; i < 50; i++ {
		log.WriteString(`{"event":"child_end","harness":"h` + strconv.Itoa(i) + `","outcome":"failed"}` + "\n")
	}
	os.WriteFile(filepath.Join(run, "events.jsonl"), []byte(log.String()), 0o644)
	for i := 0; i < 6; i++ {
		sub := filepath.Join(run, "refraktal", "runs", "n"+strconv.Itoa(i))
		os.MkdirAll(sub, 0o755)
		os.WriteFile(filepath.Join(sub, "events.jsonl"), []byte(`{"event":"run_end","outcome":"applied"}`+"\n"), 0o644)
	}
	defer func(e, n int) { showMaxEvents, showMaxEntries = e, n }(showMaxEvents, showMaxEntries)
	showMaxEvents, showMaxEntries = 10, 3
	sr, err := Show(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if !sr.Truncated || len(sr.Child) > 10 || len(sr.Nested) > 3 {
		t.Fatalf("caps not applied: truncated=%v children=%d nested=%d", sr.Truncated, len(sr.Child), len(sr.Nested))
	}
	var text strings.Builder
	WriteShowText(&text, sr)
	if !strings.Contains(text.String(), "truncated") {
		t.Fatalf("no truncation note:\n%s", text.String())
	}
}

// Review M7 (11): an error names a run only after cleaning it.
func TestShowErrorsAreClean(t *testing.T) {
	_, err := Show(t.TempDir(), "x\x1b[31mred")
	if err == nil || strings.ContainsRune(err.Error(), 0x1b) {
		t.Fatalf("error %q", err)
	}
}

// E2E finding: doctor writes a folder under .refraktal/runs too. show's
// default is the latest run, never a doctor check, and naming a doctor
// check says what it is.
func TestShowSkipsDoctorChecks(t *testing.T) {
	r := newRunRig(t)
	res := r.run(t, fake{name: "codex", agentMode: "turn"}, nil)
	time.Sleep(1100 * time.Millisecond) // run IDs sort by the second
	rep, err := Doctor(context.Background(), DoctorOptions{Root: r.repo, LiveDir: r.live, Harnesses: []harness.Harness{},
		Getenv: func(string) string { return "" }, Timeout: 5 * time.Second, Neutral: filepath.Join(t.TempDir(), "neutral"),
		ConfigPath: r.config})
	if err != nil {
		t.Fatal(err)
	}
	sr, err := Show(r.repo, "")
	if err != nil || sr.ID != filepath.Base(res.Run) || sr.Outcome != RunNoChanges {
		t.Fatalf("latest is %q (%s, %v), want the run %q", sr.ID, sr.Outcome, err, filepath.Base(res.Run))
	}
	if _, err := Show(r.repo, filepath.Base(rep.Run)); err == nil || !strings.Contains(err.Error(), "doctor") {
		t.Fatalf("a doctor check shown as a run: %v", err)
	}
}
