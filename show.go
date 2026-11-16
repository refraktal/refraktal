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
	"time"

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
	// Truncated: one of show's limits (bytes, events, directory entries,
	// nesting) was reached, so this run, or the tree below it, is cut short.
	Truncated bool `json:"truncated,omitempty"`
	// LatestIncomplete means the limits prevented examining every run, so
	// the default selection may have missed a newer run.
	LatestIncomplete bool `json:"latest_incomplete,omitempty"`
}

// show's limits across one whole walk (variables for tests).
var (
	showMaxBytes   int64 = 64 << 20 // run-log bytes read
	showMaxEvents        = 200_000  // events parsed
	showMaxEntries       = 10_000   // directory entries listed
)

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

// ShowCosts are the run's actual spend, retained reservations and turns.
type ShowCosts struct {
	BySource map[string]float64 `json:"by_source"`
	Charged  float64            `json:"charged_usd"`
	Retained float64            `json:"retained_usd"`
	Limit    float64            `json:"limit_usd"`
	Turns    int                `json:"turns"`
	TurnsMax int                `json:"turns_limit"`
	Unpriced []string           `json:"unpriced,omitempty"`
}

// ErrNoRun means show found no run to render.
var ErrNoRun = errors.New("no run found")

// Show reads a run from the repository at root: the run with ID id, or the
// latest. It renders child runs nested in it.
//
// Run logs are untrusted (a child process wrote the nested ones). The walk
// opens every folder through its parent's pinned handle, refuses a symlink
// at every component (.refraktal itself included) and checks that what it
// opened is what it inspected, and it stops at global limits on bytes,
// events, directory entries and depth.
func Show(root, id string) (ShowRun, error) {
	if id != "" && (id != filepath.Base(id) || strings.HasPrefix(id, ".")) {
		return ShowRun{}, fmt.Errorf("%q is not a run ID", clean(id))
	}
	repo, err := os.OpenRoot(root)
	if err != nil {
		return ShowRun{}, ErrNoRun
	}
	defer repo.Close()
	base, err := openDir(repo, ".refraktal")
	if err != nil {
		return ShowRun{}, ErrNoRun
	}
	defer base.Close()
	runs, err := openDir(base, "runs")
	if err != nil {
		return ShowRun{}, ErrNoRun
	}
	defer runs.Close()
	w := &walk{bytes: showMaxBytes, events: showMaxEvents, entries: showMaxEntries}
	if id == "" {
		// The latest run: doctor writes folders here too, which are skipped.
		ids := w.dirs(runs)
		w.orderRuns(runs, ids)
		incomplete := w.truncated
		for i := len(ids) - 1; i >= 0; i-- {
			r, err := showOne(runs, ids[i], w)
			if errors.Is(err, errDoctorCheck) {
				continue
			}
			r.Truncated = r.Truncated || incomplete
			r.LatestIncomplete = incomplete
			return r, err
		}
		return ShowRun{}, ErrNoRun
	}
	return showOne(runs, id, w)
}

// errDoctorCheck: the folder is a doctor check's, not a run's.
var errDoctorCheck = errors.New("is a doctor check, not a run")

func showOne(runs *os.Root, id string, w *walk) (ShowRun, error) {
	d, err := openDir(runs, id)
	if err != nil {
		return ShowRun{}, fmt.Errorf("%w: %s", ErrNoRun, clean(id))
	}
	defer d.Close()
	r, err := readRun(d, id, filepath.Join(".refraktal", "runs", id), 0, w)
	if errors.Is(err, errDoctorCheck) {
		return r, fmt.Errorf("%s %w (refraktal doctor wrote it)", clean(id), errDoctorCheck)
	}
	return r, err
}

// maxNesting bounds how deep show follows nested runs.
const maxNesting = 8

// walk is what is left of show's limits for one whole walk.
type walk struct {
	bytes     int64
	events    int
	entries   int
	truncated bool // a limit was reached while reading the current run
}

// dirs lists the real directories in d, sorted, within the entry limit.
func (w *walk) dirs(d *os.Root) []string {
	if w.entries <= 0 {
		w.truncated = true
		return nil
	}
	f, err := d.Open(".")
	if err != nil {
		return nil
	}
	defer f.Close()
	entries, _ := f.ReadDir(w.entries + 1)
	if len(entries) > w.entries {
		entries, w.truncated = entries[:w.entries], true
	}
	w.entries -= len(entries)
	var out []string
	for _, e := range entries {
		if e.IsDir() { // a symlink is not a directory entry here
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// orderRuns orders runs by their recorded start, not the random suffix in
// their IDs. Old or incomplete logs without a timestamp fall back to ID order.
func (w *walk) orderRuns(d *os.Root, ids []string) {
	starts := make(map[string]time.Time, len(ids))
	for _, id := range ids {
		run, err := openDir(d, id)
		if err != nil {
			continue
		}
		f, err := openFile(run, "events.jsonl")
		run.Close()
		if err != nil {
			continue
		}
		if w.events <= 0 {
			w.truncated = true
			f.Close()
			break
		}
		sc := bufio.NewScanner(&budgeted{r: f, w: w})
		sc.Buffer(make([]byte, 4096), 16<<20)
		if sc.Scan() {
			w.events--
			var first struct {
				Time time.Time `json:"t"`
			}
			if json.Unmarshal(sc.Bytes(), &first) == nil {
				starts[id] = first.Time
			}
		}
		if sc.Err() != nil {
			w.truncated = true
		}
		f.Close()
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := starts[ids[i]], starts[ids[j]]
		if !a.Equal(b) {
			return a.Before(b)
		}
		return ids[i] < ids[j]
	})
}

// openDir opens name in parent only if it is a real directory (not a
// symlink) and the folder opened is the one inspected.
func openDir(parent *os.Root, name string) (*os.Root, error) {
	fi, err := parent.Lstat(name)
	if err != nil || !fi.IsDir() {
		return nil, ErrNoRun
	}
	d, err := parent.OpenRoot(name)
	if err != nil {
		return nil, ErrNoRun
	}
	if st, err := d.Stat("."); err != nil || !os.SameFile(fi, st) {
		d.Close()
		return nil, ErrNoRun
	}
	return d, nil
}

// openFile opens name in parent only if it is a regular file (not a
// symlink) and the file opened is the one inspected.
func openFile(parent *os.Root, name string) (*os.File, error) {
	fi, err := parent.Lstat(name)
	if err != nil || !fi.Mode().IsRegular() {
		return nil, ErrNoRun
	}
	f, err := parent.Open(name)
	if err != nil {
		return nil, ErrNoRun
	}
	if st, err := f.Stat(); err != nil || !os.SameFile(fi, st) {
		f.Close()
		return nil, ErrNoRun
	}
	return f, nil
}

// budgeted reads at most the walk's remaining bytes.
type budgeted struct {
	r io.Reader
	w *walk
}

func (b *budgeted) Read(p []byte) (int, error) {
	if b.w.bytes <= 0 {
		b.w.truncated = true
		return 0, io.EOF
	}
	if int64(len(p)) > b.w.bytes {
		p = p[:b.w.bytes]
	}
	n, err := b.r.Read(p)
	b.w.bytes -= int64(n)
	return n, err
}

func readRun(d *os.Root, name, dir string, depth int, w *walk) (ShowRun, error) {
	r := ShowRun{ID: clean(name), Dir: dir, Costs: ShowCosts{BySource: map[string]float64{}}}
	f, err := openFile(d, "events.jsonl")
	if err != nil {
		return r, fmt.Errorf("%w: %s has no run log", ErrNoRun, r.ID)
	}
	defer f.Close()
	r.Truncated = w.truncated
	w.truncated = false
	sawRun, sawDoctor := false, false
	sc := bufio.NewScanner(&budgeted{r: f, w: w})
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		if w.events <= 0 {
			w.truncated = true
			break
		}
		w.events--
		var e map[string]json.RawMessage
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		str := func(k string) string { // every string from the log is cleaned
			var s string
			json.Unmarshal(e[k], &s)
			return clean(s)
		}
		switch str("event") {
		case "doctor":
			sawDoctor = true
		case "run_start":
			sawRun = true
			r.Task, r.Pinned = str("task"), str("harness")
			json.Unmarshal(e["dry"], &r.Dry)
		case "route":
			rt := &ShowRoute{Decider: str("decider"), Action: str("action")}
			json.Unmarshal(e["harnesses"], &rt.Harnesses)
			cleanAll(rt.Harnesses)
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
			cleanAll(c.Files)
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
			cleanMap(r.Verdict)
		case "run_end":
			r.Outcome, r.Branch = str("outcome"), str("branch")
			r.Winner = str("winner")
			if r.Winner == "" && (r.Outcome == RunApplied || r.Outcome == RunKept) {
				r.Winner = str("harness") // one candidate: it is the result
			}
			json.Unmarshal(e["files"], &r.Files)
			cleanAll(r.Files)
			var b budget.Summary
			if json.Unmarshal(e["budget"], &b) == nil {
				r.Costs.Charged, r.Costs.Limit, r.Costs.Turns, r.Costs.TurnsMax = b.Reported, b.Limits.USD, b.Turns, b.Limits.Turns
				// Cancelled grants retain their unused allowance without a
				// charge entry; nested reservations also count toward Spent.
				r.Costs.Retained = max(0, b.Spent-b.Reported)
				r.Costs.Unpriced = cleanAll(b.Unpriced)
				for _, c := range b.Charges {
					r.Costs.BySource[clean(string(c.Source))] += c.USD
				}
			}
			if r.Verdict == nil {
				json.Unmarshal(e["verdict"], &r.Verdict)
				cleanMap(r.Verdict)
			}
		}
	}
	if sawDoctor && !sawRun {
		return r, errDoctorCheck
	}
	r.Truncated = r.Truncated || w.truncated // this run's own log was cut short
	// A child that ran but logged no child_end (it failed to start) still
	// shows through its turn; fold child_run-only entries into their child.
	r.Child = mergeChildren(r.Child)
	// Nested runs: <dir>/refraktal/runs/<id>, made by a child Refraktal in
	// the refraktal harness's scratch folder. Nothing else is followed (a
	// harness writes its own scratch folder), never through a symlink.
	if rr, err := openDir(d, "refraktal"); err == nil {
		defer rr.Close()
		if runs, err := openDir(rr, "runs"); err == nil {
			defer runs.Close()
			if depth >= maxNesting {
				r.Truncated = true
			} else {
				w.truncated = false
				names := w.dirs(runs)
				w.orderRuns(runs, names)
				r.Truncated = r.Truncated || w.truncated
				for _, n := range names {
					sub, err := openDir(runs, n)
					if err != nil {
						continue
					}
					nr, err := readRun(sub, n, filepath.Join(dir, "refraktal", "runs", n), depth+1, w)
					sub.Close()
					if err != nil {
						continue
					}
					nr.Via = "refraktal"
					r.Truncated = r.Truncated || nr.Truncated
					r.Nested = append(r.Nested, nr)
				}
			}
		}
	}
	return r, sc.Err()
}

// clean drops, from a value read from a run log, every control character
// (terminal escapes and newlines among them, so a value cannot forge a line)
// and every bidi or format character (which can reorder what is shown).
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
			return -1
		case r == 0x200e || r == 0x200f || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) || r == 0xfeff:
			return -1
		}
		return r
	}, s)
}

// cleanMap cleans a verdict's string values, and string lists in it.
func cleanMap(m map[string]any) {
	for k, v := range m {
		switch x := v.(type) {
		case string:
			m[k] = clean(x)
		case []any:
			for i, y := range x {
				if s, ok := y.(string); ok {
					x[i] = clean(s)
				}
			}
		}
	}
}

func cleanAll(xs []string) []string {
	for i := range xs {
		xs[i] = clean(xs[i])
	}
	return xs
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
	fmt.Fprintf(w, "; retained reservations: $%.4f", c.Retained)
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
	if r.Truncated {
		fmt.Fprintf(w, "%s(truncated: show's limits on log size, events, folder entries or nesting were reached)\n", in2)
	}
	if r.LatestIncomplete {
		fmt.Fprintf(w, "%s(latest run selection may be incomplete: show could not examine every run)\n", in2)
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
