package route

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestScoreSendsTheTruncatedDiffInTheHeaderKeyedRequest(t *testing.T) {
	var got struct {
		Input     string              `json:"input"`
		Questions map[string]Question `json:"questions"`
	}
	answer := `{"answers": {"correct": {"type": "noul", "noul": 0.83}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") != "k" || r.URL.RawQuery != "" {
			http.Error(w, "key", 401)
			return
		}
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got)
		w.Write([]byte(answer))
	}))
	defer srv.Close()
	g := Gemini{APIKey: "k", Endpoint: srv.URL}
	big := []byte(strings.Repeat("+x\n", ScoreCap)) // three times the cap
	p, trunc, err := g.Score(context.Background(), "do it", big)
	if err != nil || p != 0.83 || !trunc {
		t.Fatalf("p=%v truncated=%v err=%v", p, trunc, err)
	}
	var in struct {
		Task      string `json:"task"`
		Diff      string `json:"diff"`
		Truncated bool   `json:"diff_truncated"`
	}
	json.Unmarshal([]byte(got.Input), &in)
	if in.Task != "do it" || len(in.Diff) != ScoreCap || !in.Truncated || got.Questions["correct"].Type != "noul" {
		t.Fatalf("request: task %q, diff %d bytes, truncated %v", in.Task, len(in.Diff), in.Truncated)
	}
	if _, trunc, _ := g.Score(context.Background(), "do it", []byte("+y\n")); trunc {
		t.Fatal("a small diff was marked truncated")
	}
	answer = `{"answers": {"correct": {"type": "noul", "noul": 1.7}}}`
	if _, _, err := g.Score(context.Background(), "do it", []byte("+y\n")); err == nil {
		t.Fatal("an out-of-range answer was accepted")
	}
}
