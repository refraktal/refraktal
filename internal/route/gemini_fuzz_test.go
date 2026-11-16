package route

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"os"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// An in-memory RoundTripper exercises ask and Decide, including the body
// limit, without sockets, a server, credentials, or a real provider.
type fuzzTransport struct{ body []byte }

func (r fuzzTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(r.body))}, nil
}

// Long local runs: add -fuzzminimizetime=2s. Minimizing a large new input runs
// the whole check (two ReadMemStats calls) per candidate and can stall the
// worker for minutes, which looks like a hang.
func FuzzDecisionAnswers(f *testing.F) {
	b, err := os.ReadFile("testdata/spike_live.json")
	if err != nil {
		f.Fatal(err)
	}
	var fixtures []struct {
		Raw json.RawMessage `json:"raw_response"`
	}
	if json.Unmarshal(b, &fixtures) != nil {
		f.Fatal("invalid replay fixture")
	}
	for _, fixture := range fixtures {
		f.Add([]byte(fixture.Raw))
	}
	for _, s := range []string{"", "null", "{", `{"answers":`, `{"answers":{"harness":{"type":"choice","choice":"codex","confidence":1,"probabilities":{"codex":1e9999}}}}`, `{"answers":{"harness":{"type":"choice","choice":"codex","confidence":"NaN","probabilities":{"codex":"Inf"}}}}`, "\xff\xfe\x00", strings.Repeat("[", 10001) + strings.Repeat("]", 10001), strings.Repeat("x", 16<<10), `{"answers":{},"answers":null}`} {
		f.Add([]byte(s))
	}
	// Mutate real successful answers to keep validation branches reachable.
	for _, replace := range []func(map[string]any){
		func(a map[string]any) {
			a["harness"].(map[string]any)["probabilities"] = map[string]any{"claude-code": 1}
			a["harness"].(map[string]any)["choice"] = "codex"
		},
		func(a map[string]any) {
			a["fan_out_width"].(map[string]any)["probabilities"].(map[string]any)["unknown"] = 2.0
		},
		func(a map[string]any) { a["harness"].(map[string]any)["confidence"] = 2.0 },
		func(a map[string]any) { a["fan_out"].(map[string]any)["noul"] = 3.0 },
		func(a map[string]any) { a["harness"].(map[string]any)["probabilities"].(map[string]any)["codex"] = 1.5 },
	} {
		var body map[string]any
		if json.Unmarshal(fixtures[0].Raw, &body) != nil {
			f.Fatal("invalid fixture")
		}
		replace(body["answers"].(map[string]any))
		seed, _ := json.Marshal(body)
		f.Add(seed)
	}
	f.Fuzz(checkDecisionAnswers)
}

func checkDecisionAnswers(t *testing.T, b []byte) {
	g := Gemini{Endpoint: "http://127.0.0.1/decisions", Client: &http.Client{Transport: fuzzTransport{b}}}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	deadline := 2*time.Second + time.Duration(len(b)/(1<<20))*100*time.Millisecond
	watchdog := time.AfterFunc(deadline, func() { panic("decision parser exceeded work deadline") })
	defer watchdog.Stop()
	start := time.Now()
	d, err := g.Decide(context.Background(), Input{Task: "fixture"})
	elapsed := time.Since(start)
	watchdog.Stop()
	runtime.ReadMemStats(&after)
	if used, limit := after.TotalAlloc-before.TotalAlloc, uint64(len(b))*64+(1<<20); used > limit {
		t.Fatalf("decision allocated %d bytes (limit %d)", used, limit)
	}
	if elapsed > deadline {
		t.Fatalf("decision took %s", elapsed)
	}
	if err != nil && !reflect.DeepEqual(d, Decision{}) {
		t.Fatal("rejected decision returned partial result")
	}
	var wire geminiResponse
	if json.Unmarshal(b, &wire) != nil {
		if err == nil {
			t.Fatal("accepted malformed JSON")
		}
		return
	}
	// Check each answer independently as well, so an invalid first answer
	// cannot hide a bug in a later validation branch.
	for _, tc := range []struct {
		key     string
		allowed []string
	}{{"task_kind", TaskKinds}, {"harness", Harnesses}} {
		a, answerErr := choiceAnswer(wire, tc.key, tc.allowed)
		if answerErr != nil {
			if !reflect.DeepEqual(a, Answer{}) {
				t.Fatal("partial choice on error")
			}
			continue
		}
		w := wire.Answers[tc.key]
		if _, ok := w.Probabilities[w.Choice]; !ok {
			t.Fatal("accepted choice missing from probability map")
		}
		checkFuzzAnswer(t, a, tc.allowed)
		if a.Probability != w.Probabilities[w.Choice] {
			t.Fatal("choice probability mismatch")
		}
	}
	width, widthErr := widthAnswer(wire)
	if widthErr == nil {
		for key, p := range wire.Answers["fan_out_width"].Probabilities {
			i, e := strconv.Atoi(key)
			if e != nil || strconv.Itoa(i) != key || i < 0 || i > MaxFanOut-MinFanOut || !fuzzProbability(p) {
				t.Fatal("accepted unknown width option or invalid probability")
			}
		}
		checkFuzzAnswer(t, width, []string{"2", "3", "4"})
		best := -1.0
		want := ""
		for i := MinFanOut; i <= MaxFanOut; i++ {
			k := strconv.Itoa(i)
			if p := width.Probabilities[k]; p > best {
				best, want = p, k
			}
		}
		if width.Value != want || width.Probability != best {
			t.Fatal("width is not the most probable level")
		}
	} else if !reflect.DeepEqual(width, Answer{}) {
		t.Fatal("partial width on error")
	}
	for _, tc := range []struct{ key, yes, no string }{{"fan_out", "fan_out", "route"}, {"needs_decomposition", "yes", "no"}} {
		a, e := noulAnswer(wire, tc.key, tc.yes, tc.no)
		if e != nil {
			if !reflect.DeepEqual(a, Answer{}) {
				t.Fatal("partial yes/no on error")
			}
			continue
		}
		checkFuzzAnswer(t, a, []string{tc.yes, tc.no})
		p := *wire.Answers[tc.key].Noul
		want := tc.no
		if p >= 0.5 {
			want = tc.yes
		}
		if a.Value != want || a.Probability != p || a.Confidence != math.Max(p, 1-p) {
			t.Fatal("yes/no consistency failed")
		}
	}
	if err != nil {
		return
	}
	checkFuzzAnswer(t, d.TaskKind, TaskKinds)
	checkFuzzAnswer(t, d.Harness, Harnesses)
	checkFuzzAnswer(t, d.FanOut, []string{"fan_out", "route"})
	checkFuzzAnswer(t, d.NeedsDecomposition, []string{"yes", "no"})
	checkFuzzAnswer(t, d.FanOutWidth, []string{"2", "3", "4"})
	if !bytes.Equal(d.Raw, b) || d.Decider != "gemini" {
		t.Fatal("accepted decision lost provenance")
	}
	encoded, marshalErr := json.Marshal(d)
	var roundTrip Decision
	if marshalErr != nil || json.Unmarshal(encoded, &roundTrip) != nil {
		t.Fatal("decision cannot round-trip")
	}
	d.Raw = nil
	d.LatencyMS = 0
	roundTrip.LatencyMS = 0
	if !reflect.DeepEqual(d, roundTrip) {
		t.Fatal("decision changed on round-trip")
	}
}

func checkFuzzAnswer(t *testing.T, a Answer, allowed []string) {
	t.Helper()
	if !slices.Contains(allowed, a.Value) || !fuzzProbability(a.Probability) || !fuzzProbability(a.Confidence) {
		t.Fatal("accepted invalid choice, probability or confidence")
	}
	for key, p := range a.Probabilities {
		if !slices.Contains(allowed, key) || !fuzzProbability(p) {
			t.Fatal("accepted invalid probability map")
		}
	}
	if a.Score != nil && (math.IsNaN(*a.Score) || math.IsInf(*a.Score, 0)) {
		t.Fatal("accepted non-finite score")
	}
}

func fuzzProbability(p float64) bool {
	return !math.IsNaN(p) && !math.IsInf(p, 0) && p >= 0 && p <= 1
}

func TestDecisionRejectsChoiceWithoutProbability(t *testing.T) {
	checkDecisionAnswers(t, []byte(`{"answers":{"harness":{"type":"choice","choice":"codex","confidence":1,"probabilities":{"claude-code":1}}}}`))
}

func TestDecisionRejectsUnknownWidthOption(t *testing.T) {
	checkDecisionAnswers(t, []byte(`{"answers":{"fan_out_width":{"type":"score","score":0,"confidence":1,"probabilities":{"0":1,"1":0,"2":0,"unknown":2}}}}`))
}
