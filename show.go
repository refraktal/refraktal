package refraktal

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"refraktal.dev/refraktal/internal/budget"
)

// ShowRun is one run as show renders it, from its run log. Child runs (a
// child Refraktal's) nest under Nested.
type ShowRun struct {
	ID      string         `json:"run"`
	Dir     string         `json:"dir"` // relative to the repository, or absolute for a nested run
	Via     string         `json:"via,omitempty"`
	Task    string         `json:"task,omitempty"`
	Pinned  string         `json:"pinned,omitempty"` // --harness
	Dry     bool           `json:"dry,omitempty"`
	Route   *ShowRoute     `json:"route,omitempty"`
	Child   []ShowChild    `json:"children,omitempty"`
	Checks  string         `json:"checks,omitempty"`
	Verdict map[string]any `json:"verdict,omitempty"`
	Outcome string         `json:"outcome"`
	Winner  string         `json:"winner,omitempty"`
	Files   []string       `json:"files,omitempty"`
	Branch  string         `json:"branch,omitempty"`
	Costs   ShowCosts      `json:"costs"`
	Nested  []ShowRun      `json:"nested,omitempty"`
}

// ShowRoute is the routing decision, when the router chose.
type ShowRoute struct {
	Decider    string   `json:"decider"`
	Action     string   `json:"action"`
	Harnesses  []string `json:"harnesses"`
	Confidence float64  `json:"confidence"`
	Cached     bool     `json:"cached,omitempty"`
}

// ShowChild is one child's turn: a candidate, or the judge.
type ShowChild struct {
	Harness string   `json:"harness"`
	Outcome string   `json:"outcome"`
	Judge   bool     `json:"judge,omitempty"`
	Files   []string `json:"files,omitempty"`
	Turns   int      `json:"child_turns,omitempty"` // prompts a child Refraktal sent
}

// ShowCosts are the run's charges by source, and its turns.
type ShowCosts struct {
	BySource map[string]float64 `json:"by_source"`
	Charged  float64            `json:"charged_usd"`
	Limit    float64            `json:"limit_usd"`
	Turns    int                `json:"turns"`
	TurnsMax int                `json:"turns_limit"`
	Unpriced []string           `json:"unpriced,omitempty"`
}

// ErrNoRun means show found no run to render.
var ErrNoRun = errors.New("no run found")

// Show reads a run from the repository at root: the run with ID id, or the
// latest. It renders child runs nested in it.
func Show(root, id string) (ShowRun, error) {
	runs := filepath.Join(root, ".refraktal", "runs")
	if id == "" {
		entries, err := os.ReadDir(runs)
		if err != nil {
			return ShowRun{}, ErrNoRun
		}
		var ids []string
		for _, e := range entries {
			if e.IsDir() {
				ids = append(ids, e.Name())
			}
		}
		if len(ids) == 0 {
			return ShowRun{}, ErrNoRun
		}
		sort.Strings(ids)
		id = ids[len(ids)-1]
	}
	if id != filepath.Base(id) || strings.HasPrefix(id, ".") {
		return ShowRun{}, fmt.Errorf("%q is not a run ID", id)
	}
	dir := filepath.Join(runs, id)
	if !realDir(dir) || !realDir(runs) {
		return ShowRun{}, fmt.Errorf("%w: %s", ErrNoRun, id)
	}
	r, err := readRun(dir, 0)
	if err != nil {
		return r, err
	}
	r.Dir = filepath.Join(".refraktal", "runs", id)
	return r, nil
}

// maxNesting bounds how deep show follows nested runs.
const maxNesting = 8

func readRun(dir string, depth int) (ShowRun, error) {
	r := ShowRun{ID: filepath.Base(dir), Dir: dir, Costs: ShowCosts{BySource: map[string]float64{}}}
	if fi, err := os.Lstat(filepath.Join(dir, "events.jsonl")); err != nil || !fi.Mode().IsRegular() {
		return r, fmt.Errorf("%w: %s has no run log", ErrNoRun, r.ID)
	}
	f, err := os.Open(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		return r, fmt.Errorf("%w: %s has no run log", ErrNoRun, r.ID)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		var e map[string]json.RawMessage
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		str := func(k string) string {
			var s string
			json.Unmarshal(e[k], &s)
			return s
		}
		switch str("event") {
		case "run_start":
			r.Task, r.Pinned = str("task"), str("harness")
			json.Unmarshal(e["dry"], &r.Dry)
		case "route":
			rt := &ShowRoute{Decider: str("decider"), Action: str("action")}
			json.Unmarshal(e["harnesses"], &rt.Harnesses)
			json.Unmarshal(e["confidence"], &rt.Confidence)
			json.Unmarshal(e["cached"], &rt.Cached)
			r.Route = rt
		case "child_end":
			c := ShowChild{Harness: str("harness"), Outcome: str("outcome")}
			json.Unmarshal(e["judge"], &c.Judge)
			if c.Judge {
				c.Harness = strings.TrimPrefix(c.Harness, "judge-") // the attempt's folder name
			}
			json.Unmarshal(e["files"], &c.Files)
			r.Child = append(r.Child, c)
		case "child_run":
			var n int
			json.Unmarshal(e["turns"], &n)
			for i := range r.Child {
				if r.Child[i].Harness == str("harness") {
					r.Child[i].Turns = n
				}
			}
			if len(r.Child) == 0 || r.Child[len(r.Child)-1].Harness != str("harness") {
				r.Child = append(r.Child, ShowChild{Harness: str("harness"), Turns: n})
			}
		case "checks":
			r.Checks = str("source")
		case "verdict":
			json.Unmarshal(e["verdict"], &r.Verdict)
		case "run_end":
			r.Outcome, r.Branch = str("outcome"), str("branch")
			r.Winner = str("winner")
			if r.Winner == "" && (r.Outcome == RunApplied || r.Outcome == RunKept) {
				r.Winner = str("harness") // one candidate: it is the result
			}
			json.Unmarshal(e["files"], &r.Files)
			var b budget.Summary
			if json.Unmarshal(e["budget"], &b) == nil {
				r.Costs.Charged, r.Costs.Limit, r.Costs.Turns, r.Costs.TurnsMax = b.Spent, b.Limits.USD, b.Turns, b.Limits.Turns
				r.Costs.Unpriced = b.Unpriced
				for _, c := range b.Charges {
					r.Costs.BySource[string(c.Source)] += c.USD
				}
			}
			if r.Verdict == nil {
				json.Unmarshal(e["verdict"], &r.Verdict)
			}
		}
	}
	// A child that ran but logged no child_end (it failed to start) still
	// shows through its turn; fold child_run-only entries into their child.
	r.Child = mergeChildren(r.Child)
	if depth < maxNesting {
		// Nested runs: <dir>/refraktal/runs/<id>, made by a child Refraktal in
		// the refraktal harness's scratch folder. Nothing else is followed (a
		// harness writes its own scratch folder), and never a symlink.
		base := filepath.Join(dir, "refraktal", "runs")
		if realDir(filepath.Join(dir, "refraktal")) && realDir(base) {
			entries, _ := os.ReadDir(base)
			for _, e := range entries {
				sub := filepath.Join(base, e.Name())
				if !realDir(sub) {
					continue
				}
				n, err := readRun(sub, depth+1)
				if err != nil {
					continue
				}
				n.Via, n.Dir = "refraktal", sub
				r.Nested = append(r.Nested, n)
			}
		}
	}
	return r, sc.Err()
}

// realDir reports whether path is a directory and not a symlink.
func realDir(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.IsDir()
}

// clean drops control characters (terminal escapes among them) from text
// read from a run log before it is printed.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return -1
		}
		return r
	}, s)
}

func mergeChildren(cs []ShowChild) []ShowChild {
	var out []ShowChild
	idx := map[string]int{}
	for _, c := range cs {
		key := c.Harness
		if c.Judge {
			key = "judge:" + key
		}
		if i, ok := idx[key]; ok {
			if c.Outcome != "" {
				out[i].Outcome, out[i].Files = c.Outcome, c.Files
			}
			if c.Turns > 0 {
				out[i].Turns = c.Turns
			}
			continue
		}
		idx[key] = len(out)
		out = append(out, c)
	}
	return out
}

// WriteShowJSON writes the run as one JSON object (nested runs inside).
func WriteShowJSON(w io.Writer, r ShowRun) error {
	return json.NewEncoder(w).Encode(struct {
		Type string `json:"type"`
		ShowRun
	}{"run", r})
}

// WriteShowText renders the run for a person, nested runs indented.
func WriteShowText(w io.Writer, r ShowRun) {
	writeShow(w, r, "")
}

// cleanWriter drops control characters from everything written except the
// newline ending each line.
type cleanWriter struct{ w io.Writer }

func (c cleanWriter) Write(p []byte) (int, error) {
	lines := strings.SplitAfter(string(p), "\n")
	for _, l := range lines {
		nl := strings.HasSuffix(l, "\n")
		out := clean(strings.TrimSuffix(l, "\n"))
		if nl {
			out += "\n"
		}
		if _, err := io.WriteString(c.w, out); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

func writeShow(w io.Writer, r ShowRun, in string) {
	if _, ok := w.(cleanWriter); !ok {
		w = cleanWriter{w}
	}
	head := "run " + r.ID
	if r.Via != "" {
		head = "nested run " + r.ID + " (child Refraktal, via " + r.Via + ")"
	}
	fmt.Fprintf(w, "%s%s: %s\n", in, head, cmpOrS(r.Outcome, "unfinished"))
	in2 := in + "  "
	if r.Task != "" {
		fmt.Fprintf(w, "%stask: %s\n", in2, firstLine(r.Task))
	}
	switch {
	case r.Route != nil:
		fmt.Fprintf(w, "%sroute: %s chose %s (%s, harness confidence %.2f", in2, r.Route.Decider, strings.Join(r.Route.Harnesses, ", "),
			r.Route.Action, r.Route.Confidence)
		if r.Route.Cached {
			fmt.Fprint(w, ", cached")
		}
		fmt.Fprintln(w, ")")
	case r.Pinned != "":
		fmt.Fprintf(w, "%sharness: %s (pinned with --harness)\n", in2, r.Pinned)
	}
	for _, c := range r.Child {
		role := "candidate"
		if c.Judge {
			role = "judge"
		}
		fmt.Fprintf(w, "%s%s %s: %s", in2, role, c.Harness, cmpOrS(c.Outcome, "no result"))
		if len(c.Files) > 0 {
			fmt.Fprintf(w, ", %d files", len(c.Files))
		}
		if c.Turns > 0 {
			fmt.Fprintf(w, ", %d prompts in its own run", c.Turns)
		}
		fmt.Fprintln(w)
	}
	if r.Checks != "" {
		fmt.Fprintf(w, "%schecks: %s\n", in2, r.Checks)
	}
	if v := r.Verdict; v != nil {
		s := func(k string) string { x, _ := v[k].(string); return x }
		fmt.Fprintf(w, "%sverdict: %s by %s (%s)", in2, cmpOrS(s("winner"), "no winner"), s("method"), s("reason"))
		if j := s("judge"); j != "" {
			own, _ := v["judged_own"].(bool)
			fmt.Fprintf(w, "; judge %s, judged its own candidate: %v", j, own)
		}
		fmt.Fprintln(w)
	}
	c := r.Costs
	var parts []string
	for _, src := range []string{"reported", "computed", "unpriced", "estimated"} {
		if v, ok := c.BySource[src]; ok {
			parts = append(parts, fmt.Sprintf("%s $%.4f", src, v))
		}
	}
	fmt.Fprintf(w, "%scost: $%.4f of $%.2f charged", in2, c.Charged, c.Limit)
	if len(parts) > 0 {
		fmt.Fprintf(w, " (%s)", strings.Join(parts, ", "))
	}
	if len(c.Unpriced) > 0 {
		fmt.Fprintf(w, "; unpriced: %s", strings.Join(c.Unpriced, ", "))
	}
	fmt.Fprintf(w, "; turns %d of %d\n", c.Turns, c.TurnsMax)
	if r.Winner != "" {
		fmt.Fprintf(w, "%sresult from: %s\n", in2, r.Winner)
	}
	if len(r.Files) > 0 {
		fmt.Fprintf(w, "%sfiles: %s\n", in2, strings.Join(r.Files, " "))
	}
	if r.Branch != "" {
		fmt.Fprintf(w, "%sbranch: %s\n", in2, r.Branch)
	}
	for _, n := range r.Nested {
		writeShow(w, n, in2)
	}
}

func cmpOrS(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// RepoRoot is the git top level of the current folder, or the folder itself
// outside a repository: where run folders live.
func RepoRoot() (string, error) { return defaultRoot() }
