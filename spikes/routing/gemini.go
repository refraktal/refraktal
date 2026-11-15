package routing

// Everything specific to Google's Gemini Decisions API lives in this file.
//
// Source: https://aistudio.google.com/docs/decisions-api (fetched 2026-11-15).
// Every field name below is taken from that page; nothing here is assumed
// except where a comment says ASSUMPTION.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

const (
	GeminiEndpoint = "https://generativelanguage.googleapis.com/v1beta/decisions"
	GeminiModel    = "gemini-decision-1"
	// Documented end-to-end latency is 150 to 250 ms. Two seconds leaves room for
	// a cold TLS handshake and a slow tail while keeping the router out of the
	// way: past that, the instant fallback is the better answer. To be tuned
	// from the latencies run-live.sh records.
	GeminiTimeout = 2 * time.Second
	// List price, paid tier: $0.10 per 1M input tokens, output always 0 tokens.
	// The API reports tokens, not dollars, so cost is computed.
	GeminiUSDPerInputToken = 0.10 / 1e6
)

// Question is one entry of the request's "questions" map. Criteria is a
// map[string]string for choice, a []string for score, and optional
// {true_description,false_description} for noul.
type Question struct {
	Type         string `json:"type"` // "noul", "choice" or "score"
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

type geminiRequest struct {
	Model     string              `json:"model"`
	Input     any                 `json:"input"` // string or content blocks
	Questions map[string]Question `json:"questions"`
}

type geminiAnswer struct {
	Type          string             `json:"type"`
	Noul          *float64           `json:"noul"`
	Choice        string             `json:"choice"`
	Score         *float64           `json:"score"`
	Probabilities map[string]float64 `json:"probabilities"`
	Confidence    *float64           `json:"confidence"`
}

type geminiResponse struct {
	Answers map[string]geminiAnswer `json:"answers"`
	Usage   *struct {
		TotalInputTokens  int `json:"total_input_tokens"`
		TotalOutputTokens int `json:"total_output_tokens"`
		TotalTokens       int `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		// A string ("safety") on documented 400s but a number (403) on the auth
		// error observed live, so it is kept raw.
		Code    json.RawMessage `json:"code"`
		Message string          `json:"message"`
	} `json:"error"`
}

// Gemini is the Decider backed by the Gemini Decisions API.
type Gemini struct {
	APIKey   string
	Endpoint string       // defaults to GeminiEndpoint
	Client   *http.Client // defaults to a client with GeminiTimeout
}

// ask sends one request and returns the parsed response plus the raw body.
func (g Gemini) ask(ctx context.Context, req geminiRequest) (geminiResponse, []byte, error) {
	var out geminiResponse
	body, err := json.Marshal(req)
	if err != nil {
		return out, nil, err
	}
	endpoint, client := g.Endpoint, g.Client
	if endpoint == "" {
		endpoint = GeminiEndpoint
	}
	if client == nil {
		client = &http.Client{Timeout: GeminiTimeout}
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return out, nil, err
	}
	hreq.Header.Set("x-goog-api-key", g.APIKey)
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(hreq)
	if err != nil {
		return out, nil, fmt.Errorf("gemini: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return out, nil, fmt.Errorf("gemini: read body: %w", err)
	}
	jsonErr := json.Unmarshal(raw, &out)
	if resp.StatusCode != http.StatusOK {
		if jsonErr == nil && out.Error != nil {
			return out, raw, fmt.Errorf("gemini: HTTP %d: %s: %s", resp.StatusCode, out.Error.Code, out.Error.Message)
		}
		return out, raw, fmt.Errorf("gemini: HTTP %d", resp.StatusCode)
	}
	if jsonErr != nil {
		return out, raw, fmt.Errorf("gemini: malformed body: %w", jsonErr)
	}
	return out, raw, nil
}

// GeminiRequestBody is the exact body Decide sends for in.
func GeminiRequestBody(in Input) any {
	return geminiRequest{Model: GeminiModel, Input: PromptInput(in), Questions: Questions()}
}

// Decide asks all five routing questions in one HTTP request.
func (g Gemini) Decide(ctx context.Context, in Input) (Decision, error) {
	start := time.Now()
	resp, raw, err := g.ask(ctx, GeminiRequestBody(in).(geminiRequest))
	if err != nil {
		return Decision{}, err
	}
	d := Decision{Decider: "gemini", Model: GeminiModel, Raw: raw}
	if d.TaskKind, err = choiceAnswer(resp, "task_kind", TaskKinds); err != nil {
		return Decision{}, err
	}
	if d.Harness, err = choiceAnswer(resp, "harness", Harnesses); err != nil {
		return Decision{}, err
	}
	if d.FanOut, err = noulAnswer(resp, "fan_out", "fan_out", "route"); err != nil {
		return Decision{}, err
	}
	if d.NeedsDecomposition, err = noulAnswer(resp, "needs_decomposition", "yes", "no"); err != nil {
		return Decision{}, err
	}
	if d.FanOutWidth, err = widthAnswer(resp); err != nil {
		return Decision{}, err
	}
	if u := resp.Usage; u != nil {
		d.Usage = &Usage{InputTokens: u.TotalInputTokens, OutputTokens: u.TotalOutputTokens, TotalTokens: u.TotalTokens,
			CostUSD: float64(u.TotalInputTokens) * GeminiUSDPerInputToken, CostSource: "computed"}
	}
	d.LatencyMS = float64(time.Since(start).Microseconds()) / 1000
	return d, nil
}

func choiceAnswer(r geminiResponse, key string, allowed []string) (Answer, error) {
	a, ok := r.Answers[key]
	if !ok || a.Type != "choice" || a.Confidence == nil || !contains(allowed, a.Choice) {
		return Answer{}, fmt.Errorf("gemini: malformed body: bad or missing choice answer %q", key)
	}
	return Answer{Value: a.Choice, Probability: a.Probabilities[a.Choice], Confidence: *a.Confidence, Probabilities: a.Probabilities}, nil
}

func noulAnswer(r geminiResponse, key, yes, no string) (Answer, error) {
	a, ok := r.Answers[key]
	if !ok || a.Type != "noul" || a.Noul == nil {
		return Answer{}, fmt.Errorf("gemini: malformed body: bad or missing yes/no answer %q", key)
	}
	return yesNo(*a.Noul, yes, no), nil
}

// widthAnswer turns the score answer into a width: the most probable rubric
// level (as the docs advise for a discrete level), offset by MinFanOut.
func widthAnswer(r geminiResponse) (Answer, error) {
	a, ok := r.Answers["fan_out_width"]
	if !ok || a.Type != "score" || a.Score == nil || a.Confidence == nil {
		return Answer{}, fmt.Errorf("gemini: malformed body: bad or missing score answer %q", "fan_out_width")
	}
	out := Answer{Confidence: *a.Confidence, Score: a.Score, Probabilities: map[string]float64{}}
	best := -1.0
	for i := 0; i <= MaxFanOut-MinFanOut; i++ {
		p, ok := a.Probabilities[strconv.Itoa(i)]
		if !ok {
			return Answer{}, fmt.Errorf("gemini: malformed body: width level %d missing", i)
		}
		w := strconv.Itoa(MinFanOut + i)
		out.Probabilities[w] = p
		if p > best {
			best, out.Value, out.Probability = p, w, p
		}
	}
	return out, nil
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
