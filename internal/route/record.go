package route

import (
	"encoding/json"
	"os"
	"syscall"
	"time"
)

// RecordSchema is bumped whenever a field of Record changes meaning or is
// removed. Adding a field does not need a bump.
const RecordSchema = 1

// Record is one line of the decision log: everything needed to replay or
// re-score the decision later. Outcome fields (checks, cost, time of the run it
// led to) are joined on later by ID; they are not known at decision time.
type Record struct {
	Schema      int                 `json:"schema"`
	ID          string              `json:"id"`               // caller-supplied, or the timestamp
	RunID       string              `json:"run_id,omitempty"` // the run this decision chose a harness for
	Time        time.Time           `json:"ts"`               // UTC, RFC 3339 with nanoseconds
	Task        string              `json:"task"`
	Repo        RepoContext         `json:"repo"`
	PromptInput string              `json:"prompt_input"` // the shared input exactly as sent
	Questions   map[string]Question `json:"questions"`
	Decision    Decision            `json:"decision"`
	Thresholds  Thresholds          `json:"thresholds"`
	Outcome     Outcome             `json:"policy"`
	RawResponse json.RawMessage     `json:"raw_response,omitempty"` // provider body, verbatim
}

// NewRecord assembles the record for one decision.
func NewRecord(id string, now time.Time, in Input, d Decision, t Thresholds, o Outcome) Record {
	now = now.UTC()
	if id == "" {
		id = now.Format("20060102T150405.000000000Z")
	}
	raw := d.Raw
	if !json.Valid(raw) {
		raw = nil
	}
	return Record{Schema: RecordSchema, ID: id, Time: now, Task: in.Task, Repo: in.Repo,
		PromptInput: PromptInput(in), Questions: Questions(), Decision: d, Thresholds: t, Outcome: o, RawResponse: raw}
}

// Append writes the record as one JSON line. O_APPEND keeps concurrent writers
// (parent and child Refraktals) from interleaving within a line.
func (r Record) Append(path string) error {
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	// O_NOFOLLOW: a repository could commit the log path as a symlink to a
	// user file.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
