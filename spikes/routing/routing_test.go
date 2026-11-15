package routing

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func read(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func serve(t *testing.T, h http.HandlerFunc) Gemini {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return Gemini{APIKey: "test-key", Endpoint: srv.URL, Client: &http.Client{Timeout: 200 * time.Millisecond}}
}

var bugfix = Input{Task: "Fix the crash in the parser", Repo: RepoContext{Languages: []string{"go"}, Files: 50, HasTests: true}}

// The request/response pair documented at
// https://aistudio.google.com/docs/decisions-api#multiple-questions goes through
// the wire types unchanged: our request structs can express the documented
// request, and the documented response parses into the fields we read.
func TestGeminiDocsFixture(t *testing.T) {
	var want any
	json.Unmarshal(read(t, "docs_multi_request.json"), &want)
	g := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("x-goog-api-key") != "test-key" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("bad method or headers: %s %v", r.Method, r.Header)
		}
		var got any
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &got)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("request differs from the documented one:\n%s", body)
		}
		w.Write(read(t, "docs_multi_response.json"))
	})
	var req geminiRequest
	if err := json.Unmarshal(read(t, "docs_multi_request.json"), &req); err != nil {
		t.Fatal(err)
	}
	resp, _, err := g.ask(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	a := resp.Answers
	if *a["is_defect"].Noul != 0.94 || *a["is_defect"].Confidence != 0.94 {
		t.Errorf("noul answer: %+v", a["is_defect"])
	}
	if a["next_action"].Choice != "human_escalation" || a["next_action"].Probabilities["replacement_shipment"] != 0.18 || *a["next_action"].Confidence != 0.76 {
		t.Errorf("choice answer: %+v", a["next_action"])
	}
	if *a["churn_risk"].Score != 2.99 || a["churn_risk"].Probabilities["3"] != 0.99 {
		t.Errorf("score answer: %+v", a["churn_risk"])
	}
	if resp.Usage.TotalInputTokens != 324 || resp.Usage.TotalOutputTokens != 0 {
		t.Errorf("usage: %+v", resp.Usage)
	}
}

// Happy path for the routing questions. The response body is synthetic (our
// keys, the documented shape); see testdata/README.md.
func TestGeminiDecideOneRequest(t *testing.T) {
	calls := 0
	g := serve(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		var req geminiRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if req.Model != GeminiModel || len(req.Questions) != 5 {
			t.Errorf("model %q, %d questions", req.Model, len(req.Questions))
		}
		in, _ := req.Input.(string)
		if !strings.Contains(in, `"task":"Fix the crash in the parser"`) || !strings.Contains(in, `"size":"small"`) {
			t.Errorf("input: %v", req.Input)
		}
		w.Write(read(t, "routing_response_synthetic.json"))
	})
	d, err := WithFallback{Primary: g, Fallback: Fallback{}}.Decide(context.Background(), bugfix)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("want exactly one HTTP request, got %d", calls)
	}
	if d.Decider != "gemini" || d.FallbackReason != "" || d.Model != GeminiModel {
		t.Errorf("decider: %+v", d)
	}
	if d.TaskKind.Value != "bugfix" || d.TaskKind.Confidence != 0.91 {
		t.Errorf("task kind: %+v", d.TaskKind)
	}
	if d.Harness.Value != "codex" || d.Harness.Probability != 0.78 || d.Harness.Probabilities["claude-code"] != 0.15 {
		t.Errorf("harness: %+v", d.Harness)
	}
	if d.FanOut.Value != "route" || d.FanOut.Probability != 0.12 || d.FanOut.Confidence != 0.88 {
		t.Errorf("fan out: %+v", d.FanOut)
	}
	if d.FanOutWidth.Value != "2" || d.FanOutWidth.Probability != 0.75 || d.FanOutWidth.Probabilities["4"] != 0.05 {
		t.Errorf("width: %+v", d.FanOutWidth)
	}
	if d.NeedsDecomposition.Value != "no" || d.NeedsDecomposition.Probability != 0.05 {
		t.Errorf("decomposition: %+v", d.NeedsDecomposition)
	}
	if d.Usage == nil || d.Usage.InputTokens != 412 || d.Usage.CostUSD < 0.0000411 || d.Usage.CostUSD > 0.0000413 {
		t.Errorf("usage: %+v", d.Usage)
	}
	if o := Policy(d, DefaultThresholds); o.Action != ActionRoute || o.Harnesses[0] != "codex" {
		t.Errorf("policy: %+v", o)
	}
}

func TestGeminiFailuresFallBack(t *testing.T) {
	cases := map[string]struct {
		handler http.HandlerFunc
		reason  string
	}{
		"http error (real 403 body)": {func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			w.Write(read(t, "unauthenticated_error.json"))
		}, "HTTP 403: 403: Method doesn't allow unregistered callers"},
		"documented safety 400": {func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"code":"safety","message":"Request blocked due to safety violations (harmful content). Please modify your input and retry."}}`)
		}, `HTTP 400: "safety": Request blocked`},
		"server error, no JSON": {func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
		}, "HTTP 503"},
		"timeout": {func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-time.After(500 * time.Millisecond):
			case <-r.Context().Done():
			}
		}, "Client.Timeout"},
		"malformed body": {func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"answers": {"task_kind": `)
		}, "malformed body"},
		"valid JSON, answer missing": {func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"answers":{"task_kind":{"type":"choice","choice":"bugfix","probabilities":{"bugfix":1},"confidence":1}}}`)
		}, `malformed body: bad or missing choice answer "harness"`},
		"choice outside the closed set": {func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"answers":{"task_kind":{"type":"choice","choice":"chore","probabilities":{"chore":1},"confidence":1}}}`)
		}, `bad or missing choice answer "task_kind"`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			d, err := WithFallback{Primary: serve(t, c.handler), Fallback: Fallback{}}.Decide(context.Background(), bugfix)
			if err != nil {
				t.Fatal(err)
			}
			if d.Decider != "fallback" || !strings.Contains(d.FallbackReason, c.reason) {
				t.Errorf("decider %q, reason %q, want reason containing %q", d.Decider, d.FallbackReason, c.reason)
			}
			if d.TaskKind.Value != "bugfix" || d.Usage != nil {
				t.Errorf("fallback decision: %+v", d)
			}
		})
	}
}

func TestMissingKeyUsesFallback(t *testing.T) {
	d, err := WithFallback{Fallback: Fallback{}, Why: "GEMINI_API_KEY not set"}.Decide(context.Background(), bugfix)
	if err != nil || d.Decider != "fallback" || d.FallbackReason != "GEMINI_API_KEY not set" {
		t.Errorf("%+v %v", d, err)
	}
}

func TestQuestionsWithinDocumentedLimits(t *testing.T) {
	qs := Questions()
	for key, q := range qs {
		if len(key) > 64 || strings.Trim(key, "abcdefghijklmnopqrstuvwxyz_") != "" {
			t.Errorf("question key %q breaks [A-Za-z0-9_-]{1,64}", key)
		}
		if q.Instructions == "" {
			t.Errorf("%s: empty instructions", key)
		}
	}
	options := func(key string) []string {
		var out []string
		for k := range qs[key].Criteria.(map[string]string) {
			out = append(out, k)
		}
		return out
	}
	if got := options("task_kind"); len(got) != len(TaskKinds) {
		t.Errorf("task_kind options %v", got)
	}
	for _, h := range options("harness") {
		if !contains(Harnesses, h) {
			t.Errorf("unknown harness option %q", h)
		}
	}
	if n := len(qs["fan_out_width"].Criteria.([]string)); n != MaxFanOut-MinFanOut+1 {
		t.Errorf("width levels: %d", n)
	}
}

func TestFallbackRules(t *testing.T) {
	small := RepoContext{Languages: []string{"go"}, Files: 50, HasTests: true}
	large := RepoContext{Languages: []string{"go"}, Files: 5000, HasTests: true}
	cases := []struct {
		task          string
		repo          RepoContext
		kind, harness string
		action        string
	}{
		{"Fix the off-by-one error in pagination", small, "bugfix", "codex", ActionRoute},
		{"Add unit tests for the backoff calculator", small, "test", "codex", ActionRoute},
		{"Update the README to document the new flag", small, "docs", "codex", ActionRoute},
		{"Rename the Store interface to Repository", small, "refactor", "claude-code", ActionRoute},
		{"Add a dark mode toggle to the settings screen", small, "feature", "antigravity", ActionRoute},
		{"Figure out why the nightly job is flaky", large, "investigation", "claude-code", ActionRoute},
		{"Make it faster.", large, "feature", "claude-code", ActionEscalate},
		{"Add OAuth login, then build an admin page, and also write the migration", large, "feature", "refraktal", ActionEscalate},
	}
	for _, c := range cases {
		d, _ := Fallback{}.Decide(context.Background(), Input{Task: c.task, Repo: c.repo})
		o := Policy(d, DefaultThresholds)
		if d.TaskKind.Value != c.kind || d.Harness.Value != c.harness || o.Action != c.action {
			t.Errorf("%q: got %s/%s/%s, want %s/%s/%s", c.task, d.TaskKind.Value, d.Harness.Value, o.Action, c.kind, c.harness, c.action)
		}
		if d.Decider != "fallback" {
			t.Errorf("decider %q", d.Decider)
		}
		sum := 0.0
		for _, p := range d.Harness.Probabilities {
			sum += p
		}
		if sum < 0.999 || sum > 1.001 {
			t.Errorf("%q: harness probabilities sum to %v", c.task, sum)
		}
	}
	a, _ := Fallback{}.Decide(context.Background(), Input{Task: "Make it faster."})
	b, _ := Fallback{}.Decide(context.Background(), Input{Task: "Make it faster."})
	a.LatencyMS, b.LatencyMS = 0, 0
	if !reflect.DeepEqual(a, b) {
		t.Error("fallback is not deterministic")
	}
}

func TestPolicyThresholds(t *testing.T) {
	mk := func(conf, pFan, pDecomp float64, width string) Decision {
		return Decision{
			Harness: Answer{Value: "codex", Probability: conf, Confidence: conf,
				Probabilities: map[string]float64{"codex": conf, "claude-code": (1 - conf) * 0.6, "antigravity": (1 - conf) * 0.3, "refraktal": (1 - conf) * 0.1}},
			FanOut:             yesNo(pFan, "fan_out", "route"),
			FanOutWidth:        Answer{Value: width},
			NeedsDecomposition: yesNo(pDecomp, "yes", "no"),
		}
	}
	cases := []struct {
		name      string
		d         Decision
		action    string
		harnesses []string
	}{
		{"high confidence routes", mk(0.90, 0.1, 0.1, "2"), ActionRoute, []string{"codex"}},
		{"exactly at route threshold routes", mk(0.70, 0.1, 0.1, "2"), ActionRoute, []string{"codex"}},
		{"just below fans out to two", mk(0.69, 0.1, 0.1, "2"), ActionFanOut, []string{"codex", "claude-code"}},
		{"at wide threshold still two", mk(0.55, 0.1, 0.1, "2"), ActionFanOut, []string{"codex", "claude-code"}},
		{"lower confidence fans out wider", mk(0.54, 0.1, 0.1, "2"), ActionFanOut, []string{"codex", "claude-code", "antigravity"}},
		{"at escalate threshold still fans out", mk(0.40, 0.1, 0.1, "2"), ActionFanOut, []string{"codex", "claude-code", "antigravity"}},
		{"very low confidence escalates", mk(0.39, 0.1, 0.1, "2"), ActionEscalate, []string{}},
		{"very low beats a fan-out answer", mk(0.30, 0.9, 0.1, "4"), ActionEscalate, []string{}},
		{"decomposition escalates", mk(0.90, 0.1, 0.70, "2"), ActionEscalate, []string{}},
		{"decomposition below threshold does not", mk(0.90, 0.1, 0.69, "2"), ActionRoute, []string{"codex"}},
		{"fan-out answer overrides high confidence", mk(0.90, 0.70, 0.1, "3"), ActionFanOut, []string{"codex", "claude-code", "antigravity"}},
		{"fan-out width 4 uses every harness", mk(0.90, 0.95, 0.1, "4"), ActionFanOut, []string{"codex", "claude-code", "antigravity", "refraktal"}},
		{"fan-out answer below threshold is ignored", mk(0.90, 0.69, 0.1, "4"), ActionRoute, []string{"codex"}},
		{"bad width falls back to two", mk(0.90, 0.95, 0.1, ""), ActionFanOut, []string{"codex", "claude-code"}},
	}
	for _, c := range cases {
		o := Policy(c.d, DefaultThresholds)
		if o.Action != c.action || !reflect.DeepEqual(o.Harnesses, c.harnesses) || o.Width != len(c.harnesses) && o.Action != ActionEscalate || o.Reason == "" {
			t.Errorf("%s: got %+v", c.name, o)
		}
	}
	// Thresholds are config, not baked in.
	strict := DefaultThresholds
	strict.RouteConfidence = 0.95
	if o := Policy(mk(0.90, 0.1, 0.1, "2"), strict); o.Action != ActionFanOut {
		t.Errorf("custom threshold ignored: %+v", o)
	}
}

func TestRecordIsOneCompleteLine(t *testing.T) {
	g := serve(t, func(w http.ResponseWriter, r *http.Request) { w.Write(read(t, "routing_response_synthetic.json")) })
	path := filepath.Join(t.TempDir(), "decisions.jsonl")
	for _, dec := range []Decider{g, Fallback{}} {
		d, err := dec.Decide(context.Background(), bugfix)
		if err != nil {
			t.Fatal(err)
		}
		rec := NewRecord("", time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC), bugfix, d, DefaultThresholds, Policy(d, DefaultThresholds))
		if err := rec.Append(path); err != nil {
			t.Fatal(err)
		}
	}
	f, _ := os.Open(path)
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	var lines []map[string]any
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("line is not JSON: %v", err)
		}
		lines = append(lines, m)
	}
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d", len(lines))
	}
	for _, key := range []string{"schema", "id", "ts", "task", "repo", "prompt_input", "questions", "decision", "thresholds", "policy"} {
		for i, m := range lines {
			if _, ok := m[key]; !ok {
				t.Errorf("line %d: missing %q", i, key)
			}
		}
	}
	dec := lines[0]["decision"].(map[string]any)
	for _, key := range []string{"task_kind", "harness", "fan_out", "fan_out_width", "needs_decomposition", "decider", "latency_ms", "usage"} {
		if _, ok := dec[key]; !ok {
			t.Errorf("decision missing %q", key)
		}
	}
	if lines[0]["raw_response"] == nil || lines[1]["raw_response"] != nil {
		t.Error("raw_response should be present for gemini only")
	}
	if lines[0]["ts"] != "2026-11-15T09:00:00Z" || len(lines[0]["questions"].(map[string]any)) != 5 {
		t.Errorf("ts %v", lines[0]["ts"])
	}
}
