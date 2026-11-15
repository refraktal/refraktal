// Package routing is a spike of Refraktal's System 1 routing brain: one batched
// decision call that picks a harness and decides route versus fan-out.
package routing

import (
	"context"
	"encoding/json"
	"time"
)

// Closed sets. Changing these changes the dataset; bump RecordSchema if you do.
var (
	TaskKinds = []string{"bugfix", "feature", "refactor", "test", "docs", "investigation"}
	Harnesses = []string{"claude-code", "codex", "antigravity", "refraktal"}
)

// Fan-out widths the width question can return. Level index i maps to MinFanOut+i.
const (
	MinFanOut = 2
	MaxFanOut = 4
)

// RepoContext is the little the router is told about the repository.
type RepoContext struct {
	Languages []string `json:"languages"`
	Files     int      `json:"files"`
	HasTests  bool     `json:"has_tests"`
}

// Input is one routing request.
type Input struct {
	Task string      `json:"task"`
	Repo RepoContext `json:"repo"`
}

// SizeBucket names the repo size. The Decisions API docs say to compare numbers
// in code and pass a named bucket, so the model never sees the raw file count.
func (r RepoContext) SizeBucket() string {
	switch {
	case r.Files <= 20:
		return "tiny"
	case r.Files <= 200:
		return "small"
	case r.Files <= 2000:
		return "medium"
	}
	return "large"
}

// Answer is one answered question.
//
// Probability is the probability of the positive or chosen outcome: P(yes) for
// yes/no questions (so FanOut.Probability is P(fan out) even when Value is
// "route"), and P(Value) for choice and width answers. Confidence is the
// probability that Value is right: max(p, 1-p) for yes/no, the top probability
// otherwise. For choice answers the two are therefore the same number.
type Answer struct {
	Value         string             `json:"value"`
	Probability   float64            `json:"probability"`
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Score         *float64           `json:"score,omitempty"` // width only: expected rubric level
}

// Usage is what the provider reported, plus a cost computed from list price.
type Usage struct {
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	TotalTokens  int     `json:"total_tokens"`
	CostUSD      float64 `json:"cost_usd"`    // computed here, not reported by the API
	CostSource   string  `json:"cost_source"` // always "computed"
}

// Decision is the full answer set for one task.
type Decision struct {
	TaskKind           Answer `json:"task_kind"`
	Harness            Answer `json:"harness"`
	FanOut             Answer `json:"fan_out"`       // value: "route" or "fan_out"
	FanOutWidth        Answer `json:"fan_out_width"` // value: "2".."4"; ignore when routing
	NeedsDecomposition Answer `json:"needs_decomposition"`

	Decider        string  `json:"decider"`                   // "gemini" or "fallback"
	Model          string  `json:"model,omitempty"`           // provider model id
	FallbackReason string  `json:"fallback_reason,omitempty"` // why the primary was not used
	LatencyMS      float64 `json:"latency_ms"`
	Usage          *Usage  `json:"usage,omitempty"`

	Raw json.RawMessage `json:"-"` // provider response body, kept for the decision record
}

// Decider answers the routing questions for one task.
type Decider interface {
	Decide(ctx context.Context, in Input) (Decision, error)
}

// WithFallback tries Primary and, on any error (or when Primary is nil), uses
// Fallback. Why says why Primary is nil, for example "GEMINI_API_KEY not set".
type WithFallback struct {
	Primary  Decider
	Fallback Decider
	Why      string
}

func (w WithFallback) Decide(ctx context.Context, in Input) (Decision, error) {
	start := time.Now()
	reason := w.Why
	if w.Primary != nil {
		d, err := w.Primary.Decide(ctx, in)
		if err == nil {
			return d, nil
		}
		reason = err.Error()
	}
	d, err := w.Fallback.Decide(ctx, in)
	d.FallbackReason = reason
	// Latency covers the failed primary attempt too: that is what the caller waited.
	d.LatencyMS = float64(time.Since(start).Microseconds()) / 1000
	return d, err
}

// yesNo builds a yes/no Answer from P(yes).
func yesNo(pYes float64, yes, no string) Answer {
	a := Answer{Value: no, Probability: pYes, Confidence: 1 - pYes}
	if pYes >= 0.5 {
		a.Value, a.Confidence = yes, pYes
	}
	return a
}
