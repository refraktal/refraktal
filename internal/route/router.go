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
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
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
	if exec.Command("git", "-C", root, "ls-files", "--error-unmatch", ".env").Run() == nil {
		return "", "" // tracked by git: not a key source
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

// Options configure a Router.
type Options struct {
	APIKey     string
	Endpoint   string        // default GeminiEndpoint
	Thresholds *Thresholds   // default DefaultThresholds
	CacheDir   string        // default os.UserCacheDir()/refraktal/decisions; "-" disables the cache
	CacheTTL   time.Duration // default 24 h
	LogPath    string        // decision log (JSONL); "" means none
	Now        func() time.Time
}

// Result is a routing decision and what to do with it.
type Result struct {
	Decision Decision `json:"decision"`
	Outcome  Outcome  `json:"policy"`
	Cached   bool     `json:"cached"`
	Harness  string   `json:"harness"` // the Refraktal harness to run
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
	var d Decision
	if err != nil || json.Unmarshal(b, &d) != nil || d.Decider != "gemini" {
		return Decision{}, false
	}
	return d, true
}

func (r Router) store(hash string, d Decision) {
	if r.o.CacheDir == "-" || r.o.CacheDir == "" || d.Decider != "gemini" {
		return
	}
	if os.MkdirAll(r.o.CacheDir, 0o700) != nil {
		return
	}
	b, err := json.Marshal(d)
	if err != nil {
		return
	}
	tmp := filepath.Join(r.o.CacheDir, hash+".tmp")
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, filepath.Join(r.o.CacheDir, hash+".json"))
	}
}

// Route answers the routing questions (from the cache, Gemini, or the
// fallback), applies the policy, picks the harness and logs the decision.
func (r Router) Route(ctx context.Context, in Input) (Result, error) {
	hash := requestHash(in)
	var res Result
	if d, ok := r.cached(hash); ok && r.o.APIKey != "" {
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
		r.store(hash, d)
	}
	res.Outcome = Policy(res.Decision, *r.o.Thresholds)
	res.Harness, res.Notes = pick(res.Decision, res.Outcome)
	if r.o.LogPath != "" {
		rec := NewRecord("", r.o.Now(), in, res.Decision, *r.o.Thresholds, res.Outcome)
		if err := rec.Append(r.o.LogPath); err != nil {
			res.Notes = append(res.Notes, "decision log not written: "+err.Error())
		}
	}
	return res, nil
}

// pick chooses the one harness to run until fan-out lands (M6): the routed
// harness, the top of a fan-out, or for an escalation the most probable
// harness; a label without a Refraktal harness is skipped.
func pick(d Decision, o Outcome) (string, []string) {
	var notes []string
	order := o.Harnesses
	switch o.Action {
	case ActionFanOut:
		notes = append(notes, fmt.Sprintf("the router advised fanning out to %d harnesses (%s); running the top one until fan-out lands", o.Width, o.Reason))
	case ActionEscalate:
		notes = append(notes, "the router escalated ("+o.Reason+"); running the most probable harness until planning lands")
	}
	if o.Action == ActionEscalate {
		order = nil
	}
	order = append(append([]string{}, order...), byProbability(d)...) // then every harness by probability
	for _, label := range order {
		if name := ProductName(label); name != "" {
			if label != order[0] {
				notes = append(notes, fmt.Sprintf("the router chose %s, which cannot run yet; using %s", order[0], label))
			}
			return name, notes
		}
	}
	return "", append(notes, "no harness available")
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
