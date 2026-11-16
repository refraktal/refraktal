package route

// Everything specific to Google's Gemini Decisions API lives in this file.
//
// Source: https://aistudio.google.com/docs/decisions-api (fetched 2026-11-15).
// Every field name below is taken from that page; nothing here is assumed
// except where a comment says ASSUMPTION.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"testing"
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
	// No redirects: a redirect is a Decider error, and the router falls back.
	noRedirect := *client
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client = &noRedirect
	if err := checkTestEndpoint(endpoint); err != nil {
		return out, nil, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return out, nil, err
	}
	hreq.Header.Set("x-goog-api-key", g.APIKey) // the header only: never a URL query string
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
	if resp.StatusCode != http.StatusOK {
		// The status and a digest only: a provider's error body never reaches
		// notices, output, records or doctor.
		h := sha256.Sum256(raw)
		return out, nil, fmt.Errorf("gemini: HTTP %d (response %d bytes, sha256 %s)", resp.StatusCode, len(raw), hex.EncodeToString(h[:6]))
	}
	jsonErr := json.Unmarshal(raw, &out)
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

// unit reports whether p is a finite probability.
func unit(p float64) bool { return !math.IsNaN(p) && p >= 0 && p <= 1 }

func choiceAnswer(r geminiResponse, key string, allowed []string) (Answer, error) {
	a, ok := r.Answers[key]
	if !ok || a.Type != "choice" || a.Confidence == nil || !unit(*a.Confidence) || !slices.Contains(allowed, a.Choice) || len(a.Probabilities) == 0 {
		return Answer{}, fmt.Errorf("gemini: malformed body: bad or missing choice answer %q", key)
	}
	for k, p := range a.Probabilities {
		if !slices.Contains(allowed, k) || !unit(p) {
			return Answer{}, fmt.Errorf("gemini: malformed body: answer %q has an unknown option or a probability outside [0,1]", key)
		}
	}
	if _, ok := a.Probabilities[a.Choice]; !ok {
		return Answer{}, fmt.Errorf("gemini: malformed body: answer %q has no probability for its choice", key)
	}
	return Answer{Value: a.Choice, Probability: a.Probabilities[a.Choice], Confidence: *a.Confidence, Probabilities: a.Probabilities}, nil
}

func noulAnswer(r geminiResponse, key, yes, no string) (Answer, error) {
	a, ok := r.Answers[key]
	if !ok || a.Type != "noul" || a.Noul == nil || !unit(*a.Noul) {
		return Answer{}, fmt.Errorf("gemini: malformed body: bad or missing yes/no answer %q", key)
	}
	return yesNo(*a.Noul, yes, no), nil
}

// widthAnswer turns the score answer into a width: the most probable rubric
// level (as the docs advise for a discrete level), offset by MinFanOut.
func widthAnswer(r geminiResponse) (Answer, error) {
	a, ok := r.Answers["fan_out_width"]
	if !ok || a.Type != "score" || a.Score == nil || a.Confidence == nil || !unit(*a.Confidence) || math.IsNaN(*a.Score) || math.IsInf(*a.Score, 0) {
		return Answer{}, fmt.Errorf("gemini: malformed body: bad or missing score answer %q", "fan_out_width")
	}
	if len(a.Probabilities) != MaxFanOut-MinFanOut+1 {
		return Answer{}, fmt.Errorf("gemini: malformed body: width probabilities must contain only the known levels")
	}
	out := Answer{Confidence: *a.Confidence, Score: a.Score, Probabilities: map[string]float64{}}
	best := -1.0
	for i := 0; i <= MaxFanOut-MinFanOut; i++ {
		p, ok := a.Probabilities[strconv.Itoa(i)]
		if !ok || !unit(p) {
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

// checkTestEndpoint refuses, inside a test binary, any endpoint that is not a
// loopback address (a test server), unless REFRAKTAL_LIVE=1: go test never
// talks to Google.
func checkTestEndpoint(endpoint string) error {
	if !testing.Testing() || os.Getenv("REFRAKTAL_LIVE") == "1" {
		return nil
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(u.Hostname()); u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback()) {
		return nil
	}
	return fmt.Errorf("gemini: refusing %s from a test binary: only a local test server (set REFRAKTAL_LIVE=1 for live checks)", u.Hostname())
}
