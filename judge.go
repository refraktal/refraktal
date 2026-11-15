package refraktal

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"refraktal.dev/refraktal/internal/config"
	"refraktal.dev/refraktal/internal/proc"
	"refraktal.dev/refraktal/internal/route"
	"refraktal.dev/refraktal/internal/safegit"
	"refraktal.dev/refraktal/internal/worktree"
)

// Check statuses.
const (
	CheckPassed   = "passed"
	CheckFailed   = "failed"
	CheckTimedOut = "timed_out"
	CheckError    = "error" // could not be started
)

// CheckResult is one check command's result in one candidate.
type CheckResult struct {
	Name   string `json:"name"` // build or test
	Status string `json:"status"`
	Exit   int    `json:"exit"`
	MS     int64  `json:"ms"`
	Swept  int    `json:"swept,omitempty"` // processes the session sweep killed afterwards
}

// checkStep is one command to run in each candidate.
type checkStep struct {
	Name string   `json:"name"`
	Argv []string `json:"argv"`
}

// checkPlan is how judging checks candidates: the same commands in every
// candidate, decided from the base commit only.
type checkPlan struct {
	Steps   []checkStep   `json:"steps,omitempty"`
	Source  string        `json:"source"` // config, make, go, npm or none
	Timeout time.Duration `json:"-"`
	Note    string        `json:"note,omitempty"`
	config  string        // the config file's path relative to the repository, when it is inside it
}

// planChecks decides the check commands: from the config file, else by safe
// detection in the base commit (never a candidate's files).
func planChecks(ctx context.Context, root, base string, cfg config.Config, configPath string) checkPlan {
	p := checkPlan{Source: "none", Timeout: config.DefaultCheckMins * time.Minute}
	if rel, err := filepath.Rel(canonPath(root), canonPath(configPath)); err == nil && !strings.HasPrefix(rel, "..") {
		p.config = filepath.ToSlash(rel)
	}
	if c, ok := cfg.CheckFor(root); ok {
		p.Source = "config"
		if len(c.Build) > 0 {
			p.Steps = append(p.Steps, checkStep{"build", c.Build})
		}
		if len(c.Test) > 0 {
			p.Steps = append(p.Steps, checkStep{"test", c.Test})
		}
		if c.TimeoutMins > 0 {
			p.Timeout = time.Duration(c.TimeoutMins * float64(time.Minute))
		}
		return p
	}
	var kinds []string
	if mk := worktree.Show(ctx, root, base, "Makefile"); mk != nil && regexp.MustCompile(`(?m)^test\s*:`).Match(mk) {
		kinds = append(kinds, "make")
	}
	if worktree.Show(ctx, root, base, "go.mod") != nil {
		kinds = append(kinds, "go")
	}
	if npmTest(worktree.Show(ctx, root, base, "package.json")) != "" {
		kinds = append(kinds, "npm")
	}
	switch {
	case len(kinds) == 0:
		p.Note = "no check: no config entry, and no Makefile test target, go.mod or package.json test script"
	case len(kinds) > 1:
		p.Note = "no check: more than one kind detected (" + strings.Join(kinds, ", ") + "); set one in the config's checks"
	default:
		p.Source = kinds[0]
		switch kinds[0] {
		case "make":
			p.Steps = []checkStep{{"test", []string{"make", "test"}}}
		case "go":
			p.Steps = []checkStep{{"build", []string{"go", "build", "./..."}}, {"test", []string{"go", "test", "./..."}}}
		case "npm":
			p.Steps = []checkStep{{"test", []string{"npm", "test"}}}
		}
	}
	return p
}

// npmTest returns package.json's test script, or "" for none or npm's
// placeholder.
func npmTest(b []byte) string {
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if b == nil || json.Unmarshal(b, &pkg) != nil {
		return ""
	}
	t := strings.TrimSpace(pkg.Scripts["test"])
	if strings.Contains(t, "no test specified") {
		return ""
	}
	return t
}

func canonPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// touchesDefinition says why a candidate's change touches the check's own
// definition, or "" if it does not. Such a candidate cannot win on checks.
func (p checkPlan) touchesDefinition(files []string, diff []byte) string {
	tools := map[string]bool{}
	for _, s := range p.Steps {
		tools[filepath.Base(s.Argv[0])] = true
	}
	for _, f := range files {
		switch {
		case strings.HasPrefix(f, ".github/"):
			return "changes CI files (" + f + ")"
		case p.config != "" && f == p.config:
			return "changes the config file that defines the check"
		case f == "Makefile" && (tools["make"] || p.Source == "make"):
			return "changes the Makefile the check runs"
		case f == "package.json" && (tools["npm"] || tools["yarn"] || tools["pnpm"]):
			return "changes package.json, which defines the test script"
		case f == "go.mod" && tools["go"] && goModToolchainChanged(diff):
			return "changes the go or toolchain line of go.mod"
		}
	}
	return ""
}

var goLine = regexp.MustCompile(`^[+-](go|toolchain)\s`)

// goModToolchainChanged reports whether the diff changes go.mod's go or
// toolchain line.
func goModToolchainChanged(diff []byte) bool {
	in := false
	for _, l := range strings.Split(string(diff), "\n") {
		if strings.HasPrefix(l, "diff --git ") {
			in = strings.HasSuffix(l, " b/go.mod")
			continue
		}
		if in && !strings.HasPrefix(l, "+++") && !strings.HasPrefix(l, "---") && goLine.MatchString(l) {
			return true
		}
	}
	return false
}

// runCheck runs one check command in a candidate's worktree as a harness
// child is run: a session of its own under the process tracker and the
// session sweep, no GIT_* variables or API keys, stopped at the deadline.
// Its environment is the one Refraktal's own git commands get
// (safegit.Env). Its output goes to logName in logRoot, the candidate's
// pinned folder in the run folder.
func runCheck(ctx context.Context, dir string, logRoot *os.Root, logName, liveDir string, labels map[string]string, s checkStep, limit time.Duration) CheckResult {
	r := CheckResult{Name: s.Name, Exit: -1}
	start := time.Now()
	if logRoot == nil {
		r.Status = CheckError
		return r
	}
	f, err := logRoot.OpenFile(logName, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		r.Status = CheckError
		return r
	}
	defer f.Close()
	fmt.Fprintf(f, "$ %s\n", strings.Join(s.Argv, " "))
	c, err := proc.Start(proc.Spec{Argv: s.Argv, Dir: dir, BaseEnv: safegit.Env(), Stderr: f, LiveDir: liveDir, Labels: labels})
	if err != nil {
		fmt.Fprintf(f, "not started: %v\n", err)
		r.Status = CheckError
		return r
	}
	c.Stdin.Close()
	copied := make(chan struct{})
	go func() { io.Copy(f, c.Stdout); close(copied) }()
	cctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	timedOut := false
	select {
	case <-c.Exited():
	case <-cctx.Done():
		timedOut = true
	}
	sr := c.Shutdown(2 * time.Second)
	<-copied
	r.Exit, r.MS, r.Swept = c.ExitCode(), time.Since(start).Milliseconds(), len(sr.Swept.Killed)
	switch {
	case timedOut:
		r.Status = CheckTimedOut
	case r.Exit == 0:
		r.Status = CheckPassed
	default:
		r.Status = CheckFailed
	}
	return r
}

// Ranks: a candidate that passed every check ranks above one with no check
// (or a flagged one); one that does not apply or fails a check is out.
const (
	rankOut       = 0
	rankUnchecked = 1
	rankPassed    = 2
)

func (c *Candidate) rank(plan checkPlan) int {
	if c.Applies == nil || !*c.Applies {
		return rankOut
	}
	for _, r := range c.Checks {
		if r.Status != CheckPassed {
			return rankOut
		}
	}
	if len(plan.Steps) == 0 || c.Flagged != "" {
		return rankUnchecked
	}
	return rankPassed
}

// Verdict is how the winner was chosen.
type Verdict struct {
	Winner    string   `json:"winner,omitempty"` // harness; "" for none
	Method    string   `json:"method"`           // checks, pre-score, judge or none
	Reason    string   `json:"reason"`
	CloseCall []string `json:"close_call,omitempty"` // harnesses sent to the judge
	Judge     string   `json:"judge,omitempty"`      // the judging harness
	Checks    string   `json:"checks"`               // where the check came from: config, make, go, npm or none
	Margin    float64  `json:"margin"`
}

// choose ranks the candidates that ended with changes and picks a winner,
// or the close call the judge must settle. Pure: no I/O.
func choose(cands []*Candidate, plan checkPlan, scored bool, margin float64) (winner *Candidate, judge []*Candidate, v Verdict) {
	v = Verdict{Method: "none", Checks: plan.Source, Margin: margin}
	var in []*Candidate
	for _, c := range cands {
		if c.Outcome == candChanges && c.rank(plan) > rankOut {
			in = append(in, c)
		}
	}
	if len(in) == 0 {
		v.Reason = "no candidate applied cleanly and passed the checks"
		return nil, nil, v
	}
	score := func(c *Candidate) float64 {
		if c.Score == nil {
			return -1
		}
		return *c.Score
	}
	sort.SliceStable(in, func(i, j int) bool {
		if ri, rj := in[i].rank(plan), in[j].rank(plan); ri != rj {
			return ri > rj
		}
		return scored && score(in[i]) > score(in[j])
	})
	top := in[0].rank(plan)
	var tier []*Candidate
	for _, c := range in {
		if c.rank(plan) == top {
			tier = append(tier, c)
		}
	}
	lead := tier[0]
	switch {
	case scored && len(tier) > 1 && score(lead)-score(tier[1]) <= margin+1e-12:
		for _, c := range tier {
			if score(lead)-score(c) <= margin+1e-12 {
				judge = append(judge, c)
			}
		}
		v.Reason = fmt.Sprintf("close call: pre-scores within %.2f", margin)
	case !scored && len(tier) > 1:
		judge = tier
		v.Reason = "close call: the checks do not separate them and there is no pre-score"
	case lead.Flagged != "":
		judge = []*Candidate{lead}
		v.Reason = "the leader changed the check's definition (" + lead.Flagged + "): only the judge can pick it"
	}
	if judge != nil {
		for _, c := range judge {
			v.CloseCall = append(v.CloseCall, c.Harness)
		}
		return nil, judge, v
	}
	v.Winner = lead.Harness
	switch {
	case len(tier) == 1 && len(in) > 1 || !scored:
		v.Method, v.Reason = "checks", "the only candidate in the top rank"
	default:
		v.Method, v.Reason = "pre-score", fmt.Sprintf("pre-score %.2f, ahead by more than %.2f", score(lead), margin)
	}
	if len(in) == 1 {
		v.Method, v.Reason = "checks", "the only candidate that applied cleanly and passed the checks"
	}
	return lead, nil, v
}

// judgeCap is the most of each candidate's diff the judge's prompt carries.
const judgeCap = route.ScoreCap

// judgePrompt is the judge's task: the task and the candidates' diffs under
// anonymous letters.
func judgePrompt(task string, cands []*Candidate) string {
	var b strings.Builder
	b.WriteString("You are judging candidate changes for a coding task. Do not change any files and do not run commands: read only.\n\n")
	b.WriteString("Task:\n" + task + "\n\n")
	for _, c := range cands {
		d := c.result.Diff
		note := ""
		if len(d) > judgeCap {
			d, note = d[:judgeCap], " (truncated)"
		}
		fmt.Fprintf(&b, "Candidate %s%s:\n```diff\n%s\n```\n\n", c.Label, note, strings.ToValidUTF8(string(d), ""))
	}
	b.WriteString("Which candidate carries out the task best and correctly? End your reply with one line, exactly `WINNER: <letter>`, or `WINNER: none` if no candidate correctly carries out the task.")
	return b.String()
}

var winnerLine = regexp.MustCompile(`(?mi)^\W*WINNER:\s*([A-Z]|none)\W*$`)

// parseWinner reads the judge's last WINNER line: a letter, "none", or ""
// when there is no valid answer.
func parseWinner(reply string, labels []string) string {
	m := winnerLine.FindAllStringSubmatch(reply, -1)
	if len(m) == 0 {
		return ""
	}
	w := strings.ToUpper(m[len(m)-1][1])
	if w == "NONE" {
		return "none"
	}
	for _, l := range labels {
		if l == w {
			return w
		}
	}
	return ""
}
