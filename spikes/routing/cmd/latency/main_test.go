package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fake answers every question it is sent and charges 100 tokens per question.
func fake(t *testing.T, status int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") != "k" {
			t.Error("missing api key header")
		}
		var req struct {
			Questions map[string]json.RawMessage `json:"questions"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if status != 200 {
			w.WriteHeader(status)
			fmt.Fprint(w, `{"error":{"code":429,"message":"Resource exhausted"}}`)
			return
		}
		answers := map[string]any{}
		for k := range req.Questions {
			answers[k] = map[string]any{"type": "noul", "noul": 0.9, "confidence": 0.9}
		}
		json.NewEncoder(w).Encode(map[string]any{"answers": answers,
			"usage": map[string]any{"total_input_tokens": 100 * len(req.Questions)}})
	}))
}

func TestQuestionsAreUniqueAndPadded(t *testing.T) {
	for _, n := range []int{1, 5, 25, 100} {
		if got := len(questions(n)); got != n {
			t.Errorf("questions(%d) has %d", n, got)
		}
	}
}

func TestScheduleInterleavesAndCountsAnswers(t *testing.T) {
	srv := fake(t, 200)
	defer srv.Close()
	var buf bytes.Buffer
	all, err := schedule(srv.Client(), srv.URL, "k", []int{1, 5, 25}, 3, 0, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1+3*3 || strings.Count(buf.String(), "\n") != len(all) {
		t.Fatalf("got %d results, %d lines", len(all), strings.Count(buf.String(), "\n"))
	}
	if all[1].Questions == all[4].Questions { // rounds 1 and 2 start with different sizes
		t.Error("order is not rotated between rounds")
	}
	for _, r := range all {
		if r.Answers != r.Questions || r.InputTokens != 100*r.Questions {
			t.Errorf("%+v", r)
		}
	}
}

func TestScheduleStopsOnRateLimit(t *testing.T) {
	srv := fake(t, 429)
	defer srv.Close()
	all, err := schedule(srv.Client(), srv.URL, "k", []int{1, 5}, 3, 0, &bytes.Buffer{})
	if err == nil || len(all) != 1 || !strings.Contains(err.Error(), "429") {
		t.Fatalf("want a stop after the first 429: err=%v results=%d", err, len(all))
	}
}
