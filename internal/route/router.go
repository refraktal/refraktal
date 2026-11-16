package route

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"refraktal.dev/refraktal/internal/safegit"
)

// productNames maps the router's harness labels (the spike's, kept so its
// requests stay identical) to Refraktal's harness names. "refraktal" has no
// harness until it can run under itself (M7).
var productNames = map[string]string{"claude-code": "claude", "codex": "codex", "antigravity": "agy", "refraktal": ""}

// ProductName is the Refraktal harness for a router label, or "".
func ProductName(label string) string { return productNames[label] }

// KeyEnv is the variable that holds the Gemini API key.
const KeyEnv = "GEMINI_API_KEY"

// LoadKey returns the Gemini API key and where it came from: the environment,
// or the .env file in root, but only if git does not track it. A .env
// committed to a repository someone else wrote could carry their key, and
// task text sent under it would go to their project. The key is never
// logged or printed.
func LoadKey(root string, getenv func(string) string) (key, source string) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if k := strings.TrimSpace(getenv(KeyEnv)); k != "" {
		return k, "environment"
	}
	if _, err := os.Stat(filepath.Join(root, ".env")); err != nil {
		return "", ""
	}
	if !untrackedDotEnv(root) {
		return "", ""
	}
	f, err := os.Open(filepath.Join(root, ".env"))
	if err != nil {
		return "", ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		line = strings.TrimPrefix(line, "export ")
		name, val, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(name) != KeyEnv {
			continue
		}
		val = strings.TrimSpace(val)
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
			val = val[1 : len(val)-1]
		}
		if val != "" {
			return val, ".env"
		}
	}
	return "", ""
}

// untrackedDotEnv reports whether root's .env may supply the key: git must
// confirm root is a work tree, and no tracked path may match .env in any case
// (.ENV opens as .env on a case-insensitive filesystem). Any git error means
// no: the check fails closed.
func untrackedDotEnv(root string) bool {
	return untracked(root, ".env")
}

// untracked reports whether rel (relative to dir) is untracked in a git work
// tree, comparing case-insensitively; any git error means no (fail closed).
func untracked(dir, rel string) bool {
	ctx := context.Background()
	out, err := safegit.Command(ctx, dir, "rev-parse", "--is-inside-work-tree").Output()
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		return false
	}
	tracked, err := safegit.Command(ctx, dir, "ls-files", "-z").Output()
	if err != nil {
		return false
	}
	for _, p := range strings.Split(string(tracked), "\x00") {
		if strings.EqualFold(p, rel) {
			return false
		}
	}
	return true
}

// Options configure a Router.
type Options struct {
	APIKey     string
	Endpoint   string        // default GeminiEndpoint
	Thresholds *Thresholds   // default DefaultThresholds
	CacheDir   string        // default os.UserCacheDir()/refraktal/decisions; "-" disables the cache
	CacheTTL   time.Duration // default 24 h
	LogPath    string        // decision log (JSONL); "" means none
	RunID      string        // joins decision records to the run's results
	Now        func() time.Time
}

// Result is a routing decision and what to do with it.
type Result struct {
	Decision Decision `json:"decision"`
	Outcome  Outcome  `json:"policy"`
	Cached   bool     `json:"cached"`
	Harness  string   `json:"harness"`   // the first Refraktal harness to run
	Run      []string `json:"harnesses"` // every Refraktal harness to run, in order
	Notes    []string `json:"notes,omitempty"`
}

// Router decides which harness gets a task.
type Router struct{ o Options }

// New returns a Router.
func New(o Options) Router {
	if o.Thresholds == nil {
		t := DefaultThresholds
		o.Thresholds = &t
	}
	if o.CacheTTL == 0 {
		o.CacheTTL = 24 * time.Hour
	}
	if o.CacheDir == "" {
		if d, err := os.UserCacheDir(); err == nil {
			o.CacheDir = filepath.Join(d, "refraktal", "decisions")
		}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return Router{o: o}
}

// requestHash identifies the exact request: the Decisions API answers
// identical requests identically, so its answer can be cached.
func requestHash(in Input) string {
	b, _ := json.Marshal(GeminiRequestBody(in))
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// cacheEntry is a cached decision with the provider's raw response, so a
// cache hit's record keeps its provenance.
type cacheEntry struct {
	Decision Decision        `json:"decision"`
	Raw      json.RawMessage `json:"raw,omitempty"`
}

func (r Router) cached(hash string) (Decision, bool) {
	if r.o.CacheDir == "-" || r.o.CacheDir == "" {
		return Decision{}, false
	}
	p := filepath.Join(r.o.CacheDir, hash+".json")
	fi, err := os.Stat(p)
	if err != nil || r.o.Now().Sub(fi.ModTime()) > r.o.CacheTTL {
		return Decision{}, false
	}
	b, err := os.ReadFile(p)
	var e cacheEntry
	if err != nil || json.Unmarshal(b, &e) != nil || e.Decision.Decider != "gemini" {
		return Decision{}, false
	}
	e.Decision.Raw = e.Raw
	return e.Decision, true
}

func (r Router) store(hash string, d Decision) {
	if r.o.CacheDir == "-" || r.o.CacheDir == "" || d.Decider != "gemini" {
		return
	}
	if os.MkdirAll(r.o.CacheDir, 0o700) != nil {
		return
	}
	raw := d.Raw
	if !json.Valid(raw) {
		raw = nil
	}
	b, err := json.Marshal(cacheEntry{Decision: d, Raw: raw})
	if err != nil {
		return
	}
	f, err := os.CreateTemp(r.o.CacheDir, hash+"-*.tmp") // unique: concurrent writers never share a temp file
	if err != nil {
		return
	}
	_, werr := f.Write(b)
	if cerr := f.Close(); werr == nil && cerr == nil {
		if os.Rename(f.Name(), filepath.Join(r.o.CacheDir, hash+".json")) == nil {
			return
		}
	}
	os.Remove(f.Name())
}

// Route answers the routing questions (from the cache, Gemini, or the
// fallback), applies the policy, picks the harness and logs the decision.
func (r Router) Route(ctx context.Context, in Input) (Result, error) {
	var hash string
	var d Decision
	var cached bool
	if r.o.APIKey != "" && r.o.CacheDir != "-" && r.o.CacheDir != "" {
		hash = requestHash(in)
		d, cached = r.cached(hash)
	}
	var res Result
	if cached {
		d.LatencyMS = 0
		res.Decision, res.Cached = d, true
	} else {
		var primary Decider
		why := KeyEnv + " not set"
		if r.o.APIKey != "" {
			primary = Gemini{APIKey: r.o.APIKey, Endpoint: r.o.Endpoint}
		}
		d, err := WithFallback{Primary: primary, Fallback: Fallback{}, Why: why}.Decide(ctx, in)
		if err != nil {
			return res, err
		}
		res.Decision = d
		if hash != "" {
			r.store(hash, d)
		}
	}
	res.Outcome = Policy(res.Decision, *r.o.Thresholds)
	res.Run, res.Notes = pick(res.Decision, res.Outcome)
	if len(res.Run) > 0 {
		res.Harness = res.Run[0]
	}
	if r.o.LogPath != "" {
		rec := NewRecord("", r.o.Now(), in, res.Decision, *r.o.Thresholds, res.Outcome)
		rec.RunID = r.o.RunID
		if !untracked(filepath.Dir(r.o.LogPath), filepath.Base(r.o.LogPath)) {
			res.Notes = append(res.Notes, "decision log not written: it is tracked by git, or not in a git work tree")
		} else if err := rec.Append(r.o.LogPath); err != nil {
			res.Notes = append(res.Notes, "decision log not written: "+err.Error())
		}
	}
	return res, nil
}

// pick chooses the harnesses to run, in order: the routed harness, the
// fan-out's harnesses, or for an escalation the most probable harness
// (planning is M7). A label without a Refraktal harness (refraktal, until
// M7) is skipped and the next most probable harness takes its place.
func pick(d Decision, o Outcome) ([]string, []string) {
	var notes []string
	order, width := o.Harnesses, 1
	switch o.Action {
	case ActionFanOut:
		width = max(1, o.Width)
	case ActionEscalate:
		notes = append(notes, "the router escalated ("+o.Reason+"); running the most probable harness until planning lands")
		order = nil
	}
	var out []string
	add := func(label string) {
		if name := ProductName(label); name != "" && !contains(out, name) && len(out) < width {
			out = append(out, name)
		}
	}
	for _, label := range order {
		if ProductName(label) == "" {
			notes = append(notes, fmt.Sprintf("the router chose %s, which cannot run yet; using the next most probable harness", label))
		}
		add(label)
	}
	for _, label := range byProbability(d) { // fill up, every harness by probability
		add(label)
	}
	if len(out) == 0 {
		return nil, append(notes, "no harness available")
	}
	return out, notes
}

func byProbability(d Decision) []string {
	hs := append([]string(nil), Harnesses...)
	sort.SliceStable(hs, func(i, j int) bool {
		pi, pj := d.Harness.Probabilities[hs[i]], d.Harness.Probabilities[hs[j]]
		if hs[i] == d.Harness.Value {
			pi = 2
		}
		if hs[j] == d.Harness.Value {
			pj = 2
		}
		if pi != pj {
			return pi > pj
		}
		return hs[i] < hs[j]
	})
	return hs
}

// ErrNoHarness means the router found no harness that can run.
var ErrNoHarness = errors.New("route: no harness available")
