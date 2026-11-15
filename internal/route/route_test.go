package route

import (
	"context"
	"encoding/json"
	"io"
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
)

type sample struct {
	ID   string      `json:"id"`
	Task string      `json:"task"`
	Repo RepoContext `json:"repo"`
}

func load(t *testing.T, name string, v any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err == nil {
		err = json.Unmarshal(b, v)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

// The spike's ten live Gemini responses, replayed through the product, give
// the decisions and policy outcomes the spike recorded: same request, same
// route.
func TestReplayedGeminiAnswersRouteAsTheSpikeDid(t *testing.T) {
	var fx []struct {
		ID   string          `json:"id"`
		Task string          `json:"task"`
		Repo RepoContext     `json:"repo"`
		Raw  json.RawMessage `json:"raw_response"`
		Want struct {
			TaskKind          string   `json:"task_kind"`
			Harness           string   `json:"harness"`
			Width             string   `json:"width"`
			Action            string   `json:"action"`
			HarnessConfidence float64  `json:"harness_confidence"`
			FanOutP           float64  `json:"fan_out_p"`
			DecomposeP        float64  `json:"decompose_p"`
			Harnesses         []string `json:"harnesses"`
			PolicyWidth       int      `json:"policy_width"`
		} `json:"want"`
	}
	load(t, "spike_live.json", &fx)
	if len(fx) != 10 {
		t.Fatalf("%d fixtures", len(fx))
	}
	for _, f := range fx {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(f.Raw) }))
		d, err := Gemini{APIKey: "k", Endpoint: srv.URL}.Decide(context.Background(), Input{Task: f.Task, Repo: f.Repo})
		srv.Close()
		if err != nil {
			t.Fatalf("%s: %v", f.ID, err)
		}
		o := Policy(d, DefaultThresholds)
		w := f.Want
		if d.TaskKind.Value != w.TaskKind || d.Harness.Value != w.Harness || d.FanOutWidth.Value != w.Width ||
			!near(d.Harness.Confidence, w.HarnessConfidence) || !near(d.FanOut.Probability, w.FanOutP) ||
			!near(d.NeedsDecomposition.Probability, w.DecomposeP) {
			t.Errorf("%s: decision %+v differs from the spike's %+v", f.ID, d, w)
		}
		if o.Action != w.Action || strings.Join(o.Harnesses, ",") != strings.Join(w.Harnesses, ",") || o.Width != w.PolicyWidth {
			t.Errorf("%s: outcome %+v, spike %s %v %d", f.ID, o, w.Action, w.Harnesses, w.PolicyWidth)
		}
	}
}

// The fallback gives exactly the spike fallback's decisions and outcomes on
// the spike's samples.
func TestFallbackRoutesAsTheSpikeDid(t *testing.T) {
	var samples []sample
	load(t, "samples.json", &samples)
	var golden []struct {
		ID       string          `json:"id"`
		Decision json.RawMessage `json:"decision"`
		Policy   Outcome         `json:"policy"`
	}
	load(t, "spike_fallback.json", &golden)
	for i, s := range samples {
		d, _ := Fallback{}.Decide(context.Background(), Input{Task: s.Task, Repo: s.Repo})
		d.LatencyMS = 0
		got, _ := json.Marshal(d)
		var a, b any
		json.Unmarshal(got, &a)
		json.Unmarshal(golden[i].Decision, &b)
		ga, _ := json.Marshal(a)
		gb, _ := json.Marshal(b)
		if string(ga) != string(gb) {
			t.Errorf("%s: fallback decision differs:\n got  %s\n want %s", s.ID, ga, gb)
		}
		if o := Policy(d, DefaultThresholds); o.Action != golden[i].Policy.Action ||
			strings.Join(o.Harnesses, ",") != strings.Join(golden[i].Policy.Harnesses, ",") || o.Width != golden[i].Policy.Width {
			t.Errorf("%s: outcome %+v, want %+v", s.ID, o, golden[i].Policy)
		}
	}
}

// go test never talks to Google: the real endpoint is refused from a test
// binary unless REFRAKTAL_LIVE=1.
func TestNoRealHTTPFromATest(t *testing.T) {
	t.Setenv("REFRAKTAL_LIVE", "")
	start := time.Now()
	_, err := Gemini{APIKey: "k"}.Decide(context.Background(), Input{Task: "x"})
	if err == nil || !strings.Contains(err.Error(), "REFRAKTAL_LIVE") || time.Since(start) > time.Second {
		t.Fatalf("err %v after %s", err, time.Since(start))
	}
}

func liveFixture(t *testing.T) json.RawMessage {
	var fx []struct {
		Raw json.RawMessage `json:"raw_response"`
	}
	load(t, "spike_live.json", &fx)
	return fx[0].Raw
}

// The key goes in the x-goog-api-key header only: never in the URL, never in
// the body.
func TestKeyOnlyInTheHeader(t *testing.T) {
	const key = "test-key-not-real-123"
	var url, header, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		url, header = r.URL.String(), r.Header.Get("x-goog-api-key")
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.Write(liveFixture(t))
	}))
	defer srv.Close()
	if _, err := (Gemini{APIKey: key, Endpoint: srv.URL}).Decide(context.Background(), Input{Task: "x"}); err != nil {
		t.Fatal(err)
	}
	if header != key || strings.Contains(url, key) || strings.Contains(body, key) {
		t.Fatalf("header %q, url %q, key in body %v", header, url, strings.Contains(body, key))
	}
}

func TestLoadKey(t *testing.T) {
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if k, src := LoadKey(dir, func(string) string { return "" }); k != "" || src != "" {
		t.Fatalf("no key: %q %q", k, src)
	}
	os.WriteFile(filepath.Join(dir, ".env"), []byte("# comment\nOTHER=1\nexport GEMINI_API_KEY=\"from-dotenv\"\n"), 0o600)
	if k, src := LoadKey(dir, func(string) string { return "" }); k != "from-dotenv" || src != ".env" {
		t.Fatalf(".env: %q %q", k, src)
	}
	if k, src := LoadKey(dir, func(string) string { return "from-env" }); k != "from-env" || src != "environment" {
		t.Fatalf("environment first: %q %q", k, src)
	}
}

func router(t *testing.T, key, endpoint string) Router {
	t.Helper()
	repo := t.TempDir()
	exec.Command("git", "-C", repo, "init", "-q").Run() // the log is written only in a git work tree, untracked
	return New(Options{APIKey: key, Endpoint: endpoint, CacheDir: t.TempDir(), LogPath: filepath.Join(repo, "decisions.jsonl")})
}

var bugfix = Input{Task: "Fix the off-by-one error in pagination: the last page of results is dropped.", Repo: RepoContext{Languages: []string{"go"}, Files: 140, HasTests: true}}

// Without a key, on an error, or past the 2 s timeout, routing falls back to
// the deterministic decider and says why.
func TestFallback(t *testing.T) {
	res, err := router(t, "", "").Route(context.Background(), bugfix)
	if err != nil || res.Decision.Decider != "fallback" || res.Decision.FallbackReason != "GEMINI_API_KEY not set" || res.Harness == "" {
		t.Fatalf("no key: %+v %v", res, err)
	}
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"code":403,"message":"denied"}}`, http.StatusForbidden)
	}))
	defer failing.Close()
	res, _ = router(t, "k", failing.URL).Route(context.Background(), bugfix)
	if res.Decision.Decider != "fallback" || !strings.Contains(res.Decision.FallbackReason, "HTTP 403") {
		t.Fatalf("HTTP error: %+v", res.Decision)
	}
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(5 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	start := time.Now()
	res, _ = router(t, "k", slow.URL).Route(context.Background(), bugfix)
	if res.Decision.Decider != "fallback" || time.Since(start) > 3*time.Second {
		t.Fatalf("timeout: %+v after %s", res.Decision, time.Since(start))
	}
}

// Identical requests are answered from the cache until it expires.
func TestCache(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write(liveFixture(t))
	}))
	defer srv.Close()
	now := time.Now()
	r := New(Options{APIKey: "k", Endpoint: srv.URL, CacheDir: t.TempDir(), Now: func() time.Time { return now }})
	a, _ := r.Route(context.Background(), bugfix)
	b, _ := r.Route(context.Background(), bugfix)
	if a.Cached || !b.Cached || hits.Load() != 1 || a.Decision.Harness.Value != b.Decision.Harness.Value {
		t.Fatalf("cached %v %v, hits %d", a.Cached, b.Cached, hits.Load())
	}
	other := bugfix
	other.Task += " Now."
	if c, _ := r.Route(context.Background(), other); c.Cached || hits.Load() != 2 {
		t.Fatal("a different request was served from the cache")
	}
	now = now.Add(25 * time.Hour)
	if d, _ := r.Route(context.Background(), bugfix); d.Cached || hits.Load() != 3 {
		t.Fatal("an expired entry was used")
	}
}

// The policy's harness is mapped to a Refraktal harness; a fan-out runs its
// top harness for now; refraktal, which cannot run yet, is skipped.
func TestPick(t *testing.T) {
	probs := map[string]float64{"refraktal": 0.5, "claude-code": 0.3, "codex": 0.15, "antigravity": 0.05}
	d := Decision{Harness: Answer{Value: "refraktal", Confidence: 0.5, Probabilities: probs}}
	h, notes := pick(d, Outcome{Action: ActionRoute, Harnesses: []string{"refraktal"}})
	if h != "claude" || len(notes) != 1 || !strings.Contains(notes[0], "cannot run yet") {
		t.Fatalf("refraktal: %q %v", h, notes)
	}
	d.Harness.Value = "codex"
	h, notes = pick(d, Outcome{Action: ActionFanOut, Width: 2, Harnesses: []string{"codex", "refraktal"}})
	if h != "codex" || !strings.Contains(strings.Join(notes, " "), "fanning out to 2") {
		t.Fatalf("fan-out: %q %v", h, notes)
	}
	d.Harness.Value = "antigravity"
	h, notes = pick(d, Outcome{Action: ActionEscalate, Harnesses: []string{}})
	if h != "agy" || !strings.Contains(strings.Join(notes, " "), "escalated") {
		t.Fatalf("escalate: %q %v", h, notes)
	}
}

// Every decision is logged with its probabilities, thresholds and outcome.
func TestDecisionLog(t *testing.T) {
	r := router(t, "", "")
	r.Route(context.Background(), bugfix)
	b, err := os.ReadFile(r.o.LogPath)
	var rec Record
	if err == nil {
		err = json.Unmarshal(b, &rec)
	}
	if err != nil || rec.Schema != RecordSchema || rec.Thresholds != DefaultThresholds || rec.Outcome.Action == "" ||
		len(rec.Decision.Harness.Probabilities) != len(Harnesses) || rec.PromptInput == "" {
		t.Fatalf("record %+v %v", rec, err)
	}
}

// A repository could commit .refraktal/decisions.jsonl as a symlink to a
// user file: the log must not be written through it.
func TestDecisionLogRefusesASymlink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	os.WriteFile(victim, []byte("keep\n"), 0o600)
	link := filepath.Join(dir, "decisions.jsonl")
	os.Symlink(victim, link)
	res, _ := New(Options{CacheDir: "-", LogPath: link}).Route(context.Background(), bugfix)
	if b, _ := os.ReadFile(victim); string(b) != "keep\n" {
		t.Fatalf("decision log written through a symlink: %q", b)
	}
	if !strings.Contains(strings.Join(res.Notes, " "), "decision log not written") {
		t.Fatalf("notes %v", res.Notes)
	}
}

// A .env committed to the repository (a cloned, untrusted one) is not a key
// source: only an untracked .env is.
func TestTrackedDotEnvIsIgnored(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	git("init", "-q")
	os.WriteFile(filepath.Join(dir, ".env"), []byte("GEMINI_API_KEY=attackers-key\n"), 0o600)
	git("add", "-f", ".env") // as a repository that commits its .env would
	git("commit", "-q", "-m", "x")
	if k, src := LoadKey(dir, func(string) string { return "" }); k != "" {
		t.Fatalf("a tracked .env supplied the key (%s)", src)
	}
}

// The .env check fails closed: outside a git work tree, or when a tracked
// path matches .env in any case (as .ENV opens as .env on macOS), the .env
// is not a key source.
func TestDotEnvCheckFailsClosed(t *testing.T) {
	plain := t.TempDir() // not a git repository: git errors, so no key
	os.WriteFile(filepath.Join(plain, ".env"), []byte("GEMINI_API_KEY=k\n"), 0o600)
	if k, _ := LoadKey(plain, func(string) string { return "" }); k != "" {
		t.Fatal("read a .env outside a git work tree")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	run("init", "-q")
	run("config", "core.ignorecase", "false")
	os.WriteFile(filepath.Join(dir, ".ENV"), []byte("GEMINI_API_KEY=attackers-key\n"), 0o600)
	run("add", "-f", ".ENV")
	run("commit", "-q", "-m", "x")
	if k, _ := LoadKey(dir, func(string) string { return "" }); k != "" {
		t.Fatal("a tracked .ENV supplied the key")
	}
}
