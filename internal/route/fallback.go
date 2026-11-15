package route

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// Fallback is the deterministic Decider: keyword and repo-shape rules, no key,
// no network. Its probabilities are fixed ordinal levels, NOT calibrated: they
// exist so the same policy can run over its answers.
type Fallback struct{}

const (
	fbStrong = 0.80 // an unambiguous keyword match
	fbLean   = 0.70 // a rule fired on a clear task kind
	fbSplit  = 0.50 // two kinds tied
	fbGuess  = 0.30 // nothing matched
)

// Keywords match at the start of a word (" fix" matches fixes and fixing); a
// trailing space makes the match a whole word. Order follows TaskKinds.
var kindWords = map[string][]string{
	"bugfix":        {" fix", " bug", " crash", " panic", " failing", " broken", " regression", " off by one", " wrong "},
	"feature":       {" add ", " implement", " support ", " create ", " introduce"},
	"refactor":      {" refactor", " rename", " migrate", " extract", " clean up", " restructure", " replace ", " move "},
	"test":          {" test", " coverage", " unit "},
	"docs":          {" readme", " docs", " document", " comment", " changelog"},
	"investigation": {" why ", " investigate", " figure out", " find out", " explain", " profile", " root cause", " diagnose"},
}

var (
	broadWords = []string{" across", " every ", " entire", " whole ", " throughout", " all ", " migrate", " everywhere"}
	uiWords    = []string{" ui ", " css", " frontend", " front end", " button", " layout", " screenshot", " react", " dark mode"}
	vagueWords = []string{" faster", " better", " improve", " optimi", " nicer", " redesign", " modernize", " clean up"}
)

func hits(text string, words []string) int {
	n := 0
	for _, w := range words {
		if strings.Contains(text, w) {
			n++
		}
	}
	return n
}

// normalize lower-cases, turns punctuation into spaces and pads with spaces.
func normalize(s string) string {
	s = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return ' '
	}, strings.ToLower(s))
	return " " + strings.Join(strings.Fields(s), " ") + " "
}

// spread puts p on the chosen option and shares the rest equally.
func spread(options []string, chosen string, p float64) Answer {
	m := make(map[string]float64, len(options))
	for _, o := range options {
		m[o] = (1 - p) / float64(len(options)-1)
	}
	m[chosen] = p
	return Answer{Value: chosen, Probability: p, Confidence: p, Probabilities: m}
}

func (Fallback) Decide(_ context.Context, in Input) (Decision, error) {
	start := time.Now()
	text := normalize(in.Task)
	big := in.Repo.SizeBucket() == "medium" || in.Repo.SizeBucket() == "large"

	// Task kind: most keyword hits wins; a tie or no hit lowers confidence.
	kind, best, tied := "feature", 0, false
	for _, k := range TaskKinds {
		if n := hits(text, kindWords[k]); n > best {
			kind, best, tied = k, n, false
		} else if n == best && n > 0 {
			tied = true
		}
	}
	kindConf := fbStrong
	if best == 0 {
		kindConf = fbGuess
	} else if tied {
		kindConf = fbSplit
	}

	broad := hits(text, broadWords) > 0
	vague := hits(text, vagueWords) > 0 || kindConf < fbSplit
	// Several deliverables: joined clauses, or simply a very long task.
	multi := strings.Count(text, " and ")+strings.Count(text, " then ")+strings.Count(text, " also ") >= 2 || len(text) > 400

	pDecompose := 0.10
	if multi {
		pDecompose = fbStrong
	} else if broad && big {
		pDecompose = 0.60
	}

	// Harness: first rule that fires. These priors are placeholders (see Questions).
	var harness Answer
	switch {
	case multi:
		harness = spread(Harnesses, "refraktal", 0.60)
	case hits(text, uiWords) > 0:
		harness = spread(Harnesses, "antigravity", fbLean)
	case broad || kind == "refactor" || kind == "investigation":
		harness = spread(Harnesses, "claude-code", fbLean)
	case kindConf >= fbStrong && (kind == "bugfix" || kind == "test") && in.Repo.HasTests:
		harness = spread(Harnesses, "codex", 0.75) // a test suite gives a clear check
	case kindConf >= fbStrong:
		harness = spread(Harnesses, "codex", fbLean)
	default:
		harness = spread(Harnesses, "claude-code", 0.35) // unsure: below the escalate threshold
	}

	pFan, width := 0.15, MinFanOut
	if vague {
		pFan = 0.75
		if big {
			width = MinFanOut + 1
		}
	} else if broad && big {
		pFan = 0.60
	}
	w := make([]string, 0, MaxFanOut-MinFanOut+1)
	for i := MinFanOut; i <= MaxFanOut; i++ {
		w = append(w, strconv.Itoa(i))
	}

	return Decision{
		TaskKind:           spread(TaskKinds, kind, kindConf),
		Harness:            harness,
		FanOut:             yesNo(pFan, "fan_out", "route"),
		FanOutWidth:        spread(w, strconv.Itoa(width), 0.60),
		NeedsDecomposition: yesNo(pDecompose, "yes", "no"),
		Decider:            "fallback",
		LatencyMS:          float64(time.Since(start).Microseconds()) / 1000,
	}, nil
}
