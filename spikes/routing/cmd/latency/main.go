// Command latency tests whether Decisions API latency is flat in the number of
// questions (claim R7). It sends the same input with 1, 5, 25 and 100
// questions, several rounds, in a rotating order so that network drift and
// position in the run average out. Questions are padded by repeating the
// spike's five questions under new keys. One warm-up call (excluded) pays the
// TLS handshake first.
//
//	GEMINI_API_KEY=... go run ./cmd/latency -out results/latency.jsonl
//
// It stops at the first non-200 response (a 429 means the rate limit was hit:
// raise -delay) and never retries.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	routing "refraktal.dev/spikes/routing"
)

type result struct {
	Round       int     `json:"round"` // 0 is the warm-up
	Questions   int     `json:"questions"`
	LatencyMS   float64 `json:"latency_ms"`
	Status      int     `json:"status"`
	InputTokens int     `json:"input_tokens"`
	Answers     int     `json:"answers"`
	Error       string  `json:"error,omitempty"`
}

// questions returns n questions: the spike's set, repeated under new keys.
func questions(n int) map[string]routing.Question {
	base := routing.Questions()
	names := make([]string, 0, len(base))
	for k := range base {
		names = append(names, k)
	}
	sort.Strings(names)
	qs := make(map[string]routing.Question, n)
	for i := 0; i < n; i++ {
		name := names[i%len(names)]
		qs[fmt.Sprintf("q%03d_%s", i, name)] = base[name]
	}
	return qs
}

func call(client *http.Client, endpoint, key, input string, n int) result {
	r := result{Questions: n}
	body, _ := json.Marshal(map[string]any{"model": routing.GeminiModel, "input": input, "questions": questions(n)})
	req, _ := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	req.Header.Set("x-goog-api-key", key)
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	resp.Body.Close()
	r.LatencyMS = float64(time.Since(start).Microseconds()) / 1000
	r.Status = resp.StatusCode
	var out struct {
		Answers map[string]json.RawMessage `json:"answers"`
		Usage   struct {
			TotalInputTokens int `json:"total_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil && r.Status == http.StatusOK {
		r.Error = "malformed body: " + err.Error()
	}
	r.Answers, r.InputTokens = len(out.Answers), out.Usage.TotalInputTokens
	if r.Status != http.StatusOK {
		msg := string(raw)
		if len(msg) > 300 {
			msg = msg[:300]
		}
		r.Error = fmt.Sprintf("HTTP %d: %s", r.Status, msg)
	}
	return r
}

// schedule runs the warm-up and the rounds, writing each result as a JSON line.
func schedule(client *http.Client, endpoint, key string, sizes []int, rounds int, delay time.Duration, w io.Writer) ([]result, error) {
	input := routing.PromptInput(routing.Input{Task: "Fix the off-by-one error in the pagination helper so the last page is not skipped.",
		Repo: routing.RepoContext{Languages: []string{"go"}, Files: 120, HasTests: true}})
	enc := json.NewEncoder(w)
	var all []result
	do := func(round, n int) error {
		r := call(client, endpoint, key, input, n)
		r.Round = round
		enc.Encode(r)
		all = append(all, r)
		if r.Error != "" {
			return fmt.Errorf("round %d, %d questions: %s", round, n, r.Error)
		}
		if r.Answers != n {
			return fmt.Errorf("round %d: sent %d questions, got %d answers", round, n, r.Answers)
		}
		return nil
	}
	if err := do(0, sizes[0]); err != nil {
		return all, err
	}
	for round := 1; round <= rounds; round++ {
		order := append(slices.Clone(sizes[round%len(sizes):]), sizes[:round%len(sizes)]...) // rotate
		for _, n := range order {
			time.Sleep(delay)
			if err := do(round, n); err != nil {
				return all, err
			}
		}
	}
	return all, nil
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

func main() {
	out := flag.String("out", "", "also write each call as a JSON line to this file")
	rounds := flag.Int("rounds", 5, "rounds; each round sends every size once")
	delay := flag.Duration("delay", 8*time.Second, "pause between calls, to stay under the tokens-per-minute limit")
	sizesFlag := flag.String("sizes", "1,5,25,100", "question counts")
	endpoint := flag.String("endpoint", routing.GeminiEndpoint, "API endpoint (for tests)")
	flag.Parse()
	key := os.Getenv("GEMINI_API_KEY")
	if key == "" {
		fmt.Fprintln(os.Stderr, "latency: GEMINI_API_KEY is not set")
		os.Exit(2)
	}
	var sizes []int
	for _, f := range strings.Split(*sizesFlag, ",") {
		var n int
		if _, err := fmt.Sscan(f, &n); err != nil || n < 1 || n > 1000 {
			fmt.Fprintln(os.Stderr, "latency: bad -sizes value:", f)
			os.Exit(2)
		}
		sizes = append(sizes, n)
	}
	var w io.Writer = io.Discard
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			fmt.Fprintln(os.Stderr, "latency:", err)
			os.Exit(1)
		}
		defer f.Close()
		w = f
	}
	all, err := schedule(&http.Client{Timeout: 60 * time.Second}, *endpoint, key, sizes, *rounds, *delay, w)
	fmt.Printf("%-10s %6s %10s %8s %8s %12s\n", "questions", "calls", "median_ms", "min_ms", "max_ms", "input_tokens")
	for _, n := range sizes {
		var lat []float64
		tokens := 0
		for _, r := range all {
			if r.Round > 0 && r.Questions == n && r.Error == "" {
				lat, tokens = append(lat, r.LatencyMS), r.InputTokens
			}
		}
		if len(lat) == 0 {
			continue
		}
		fmt.Printf("%-10d %6d %10.1f %8.1f %8.1f %12d\n", n, len(lat), median(lat), slices.Min(lat), slices.Max(lat), tokens)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "latency: stopped:", err)
		os.Exit(1)
	}
}
