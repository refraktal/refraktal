package route

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ScoreCap is the most of a candidate's diff a pre-score sends.
const ScoreCap = 32 << 10

// ScoreQuestion asks whether a candidate's diff does the task.
func ScoreQuestion() Question {
	return Question{Type: "noul",
		Instructions: "`task` was given to a coding agent, and `diff` is the change it made to the repository. Does `diff` correctly and completely carry out `task`? If `diff_truncated` is true, only the start of the change is shown.",
		Criteria: map[string]string{
			"true_description":  "The change does what the task asks, completely, with no evident bug or unrelated change.",
			"false_description": "The change is wrong, incomplete, off-task or evidently broken.",
		}}
}

// ScoreInput is the pre-score's input: the task and the diff, cut to
// ScoreCap bytes (on a UTF-8 boundary) with a flag when it was cut.
func ScoreInput(task string, diff []byte) (string, bool) {
	d := diff
	truncated := len(d) > ScoreCap
	if truncated {
		d = d[:ScoreCap]
	}
	text := strings.ToValidUTF8(string(d), "")
	b, _ := json.Marshal(map[string]any{"task": task, "diff": text, "diff_truncated": truncated})
	return string(b), truncated
}

// Score asks the System 1 model for P(the diff does the task): one request.
func (g Gemini) Score(ctx context.Context, task string, diff []byte) (p float64, truncated bool, err error) {
	in, truncated := ScoreInput(task, diff)
	resp, _, err := g.ask(ctx, geminiRequest{Model: GeminiModel, Input: in, Questions: map[string]Question{"correct": ScoreQuestion()}})
	if err != nil {
		return 0, truncated, err
	}
	a, ok := resp.Answers["correct"]
	if !ok || a.Type != "noul" || a.Noul == nil || !unit(*a.Noul) {
		return 0, truncated, fmt.Errorf("gemini: malformed body: bad or missing yes/no answer %q", "correct")
	}
	return *a.Noul, truncated, nil
}
