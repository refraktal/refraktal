package routing

import (
	"fmt"
	"sort"
)

// Thresholds for the escalation policy. Starting values, to be tuned against
// the decision log once outcomes are attached. Reasoning:
//
//   - RouteConfidence 0.70: the Decisions API docs use 0.7 in their own routing
//     example. Over four harnesses it means the top choice holds more than
//     twice the mass of all the others together.
//   - EscalateConfidence 0.40: uniform over four harnesses is 0.25. Below 0.40
//     the top choice is barely better than a guess, and a choice question
//     always spreads probability over its options even when none fits, so this
//     is the signal that the question did not suit the task. A planner should
//     look at it. Between the two, fan out.
//   - WideFanOutConfidence 0.55: the midpoint. Above it the runner-up is a
//     second opinion (width 2); below it three candidates are in play.
//   - FanOutProbability 0.70 and DecomposeProbability 0.70: act on a yes/no
//     answer only when it is as sure as a routing answer has to be. Both
//     questions are judgement calls where a false yes costs real money (extra
//     harness runs, or a System 2 planning pass).
type Thresholds struct {
	RouteConfidence      float64 `json:"route_confidence"`
	WideFanOutConfidence float64 `json:"wide_fan_out_confidence"`
	EscalateConfidence   float64 `json:"escalate_confidence"`
	FanOutProbability    float64 `json:"fan_out_probability"`
	DecomposeProbability float64 `json:"decompose_probability"`
}

var DefaultThresholds = Thresholds{
	RouteConfidence:      0.70,
	WideFanOutConfidence: 0.55,
	EscalateConfidence:   0.40,
	FanOutProbability:    0.70,
	DecomposeProbability: 0.70,
}

// Actions the policy can return.
const (
	ActionRoute    = "route"
	ActionFanOut   = "fan_out"
	ActionEscalate = "escalate"
)

// Outcome is what the orchestrator should do with a Decision.
type Outcome struct {
	Action    string   `json:"action"`
	Harnesses []string `json:"harnesses"` // one for route, Width for fan_out, none for escalate
	Width     int      `json:"width"`
	Reason    string   `json:"reason"`
}

// Policy maps a Decision to an Outcome. It is a pure function: no I/O, no clock.
func Policy(d Decision, t Thresholds) Outcome {
	conf := d.Harness.Confidence
	switch {
	case conf < t.EscalateConfidence:
		return Outcome{Action: ActionEscalate, Harnesses: []string{}, Reason: fmt.Sprintf("harness confidence %.2f below %.2f", conf, t.EscalateConfidence)}
	case d.NeedsDecomposition.Probability >= t.DecomposeProbability:
		return Outcome{Action: ActionEscalate, Harnesses: []string{}, Reason: fmt.Sprintf("needs decomposition, p=%.2f", d.NeedsDecomposition.Probability)}
	case d.FanOut.Probability >= t.FanOutProbability:
		w := clampWidth(atoiOr(d.FanOutWidth.Value, MinFanOut))
		return Outcome{Action: ActionFanOut, Harnesses: topHarnesses(d, w), Width: w, Reason: fmt.Sprintf("fan-out advised, p=%.2f", d.FanOut.Probability)}
	case conf >= t.RouteConfidence:
		return Outcome{Action: ActionRoute, Harnesses: []string{d.Harness.Value}, Width: 1, Reason: fmt.Sprintf("harness confidence %.2f", conf)}
	}
	w := MinFanOut
	if conf < t.WideFanOutConfidence {
		w = MinFanOut + 1
	}
	return Outcome{Action: ActionFanOut, Harnesses: topHarnesses(d, w), Width: w, Reason: fmt.Sprintf("harness confidence %.2f below %.2f", conf, t.RouteConfidence)}
}

// topHarnesses returns the n most probable harnesses, chosen one first, ties
// broken by name so the result is deterministic.
func topHarnesses(d Decision, n int) []string {
	hs := append([]string(nil), Harnesses...)
	p := func(h string) float64 {
		if h == d.Harness.Value {
			return 2 // always first
		}
		return d.Harness.Probabilities[h]
	}
	sort.SliceStable(hs, func(i, j int) bool {
		if p(hs[i]) != p(hs[j]) {
			return p(hs[i]) > p(hs[j])
		}
		return hs[i] < hs[j]
	})
	return hs[:n]
}

func clampWidth(w int) int { return max(MinFanOut, min(w, MaxFanOut, len(Harnesses))) }

func atoiOr(s string, def int) int {
	n := 0
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return def
	}
	return n
}
