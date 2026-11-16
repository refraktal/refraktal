package refraktal

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"refraktal.dev/refraktal/internal/config"
	"refraktal.dev/refraktal/internal/proc"
	"refraktal.dev/refraktal/internal/route"
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
	// LeftRunning and Unknown count the check's processes still running, or
	// whose exit could not be confirmed, after its cleanup; its live record
	// is then kept for a retry. CleanupFailed: the cleanup reported an error
	// (its raw text goes only to the notices, never to the run log).
	LeftRunning   int  `json:"left_running"`
	Unknown       int  `json:"unknown"`
	CleanupFailed bool `json:"cleanup_failed,omitempty"`
	cleanupErr    string
}

// testCheckShutdown, when set, edits a check's shutdown report: a seam for
// tests to simulate a survivor without defeating the real kill.
var testCheckShutdown func(*proc.ShutdownReport)

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
	// make reads the first of these that exists; run that one explicitly.
	makefile := ""
	for _, name := range makefiles {
		if mk := worktree.Show(ctx, root, base, name); mk != nil {
			if regexp.MustCompile(`(?m)^test\s*:`).Match(mk) {
				makefile = name
				kinds = append(kinds, "make")
			}
			break
		}
	}
	if worktree.Show(ctx, root, base, "go.mod") != nil {
		kinds = append(kinds, "go")
	}
	if npmTest(worktree.Show(ctx, root, base, "package.json")) != "" {
		kinds = append(kinds, "npm")
	}
	switch {
	case len(kinds) == 0:
		p.Note = "no check: no config entry, and no makefile test target, go.mod or package.json test script"
	case len(kinds) > 1:
		p.Note = "no check: more than one kind detected (" + strings.Join(kinds, ", ") + "); set one in the config's checks"
	default:
		p.Source = kinds[0]
		switch kinds[0] {
		case "make":
			p.Steps = []checkStep{{"test", []string{"make", "-f", makefile, "test"}}}
		case "go":
			p.Steps = []checkStep{{"build", []string{"go", "build", "./..."}}, {"test", []string{"go", "test", "./..."}}}
		case "npm":
			p.Steps = []checkStep{{"test", []string{"npm", "test"}}}
		}
	}
	return p
}

// testDefinitions are files that can define how a repository's tests run,
// flagged whatever the check command is.
var testDefinitions = []string{"package.json", "pyproject.toml", "setup.cfg", "setup.py", "tox.ini", "pytest.ini", "noxfile.py",
	"conftest.py", "Cargo.toml", "Rakefile", "Gemfile", "build.gradle", "build.gradle.kts", "pom.xml", "CMakeLists.txt", "justfile", "Taskfile.yml"}

// makefiles are the names make looks for, in its order.
var makefiles = []string{"GNUmakefile", "makefile", "Makefile"}

// testFile reports whether a path is a test file by common conventions:
// *_test.go, test_*.py and *_test.py, *.test.* and *.spec.*, or anything
// under a test, tests or __tests__ folder.
func testFile(path string) bool {
	dirs := strings.Split(path, "/")
	base := dirs[len(dirs)-1]
	for _, d := range dirs[:len(dirs)-1] {
		if d == "test" || d == "tests" || d == "__tests__" {
			return true
		}
	}
	return strings.HasSuffix(base, "_test.go") || strings.HasSuffix(base, "_test.py") ||
		(strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py")) ||
		strings.Contains(base, ".test.") || strings.Contains(base, ".spec.")
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
// definition or the tests it runs, or "" if it does not. Such a candidate
// cannot win on checks. With no check there is nothing to win on checks, so
// nothing is flagged.
func (p checkPlan) touchesDefinition(files []string, diff []byte) string {
	if len(p.Steps) == 0 {
		return ""
	}
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
		case slices.Contains(makefiles, path.Base(f)):
			return "changes a makefile (" + f + ")"
		case slices.Contains(testDefinitions, path.Base(f)): // in any folder: make -C, a monorepo
			// Whatever the check command is: make test may run npm test,
			// pytest or cargo test, which read these.
			return "changes " + f + ", which can define how tests run"
		case testFile(f):
			return "changes test files (" + f + ")"
		case path.Base(f) == "go.mod" && goModToolchainChanged(diff):
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
			in = strings.HasSuffix(l, "/go.mod") // any go.mod, at the root (b/go.mod) or nested
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
// Its environment is env (checkEnv: an allow-list, nothing else). Its output
// goes to logName in logRoot, the candidate's pinned folder in the run
// folder. Its cleanup is reported as a candidate's is (left_running, unknown,
// cleanup_failed); the caller makes the notice.
func runCheck(ctx context.Context, dir string, env []string, logRoot *os.Root, logName, liveDir string, labels map[string]string, s checkStep, limit time.Duration) CheckResult {
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
	if ctx.Err() != nil { // the run's deadline has passed: never start
		fmt.Fprintln(f, "not started: --mins has run out")
		r.Status = CheckTimedOut
		return r
	}
	c, err := proc.Start(proc.Spec{Argv: s.Argv, Dir: dir, BaseEnv: env, Stderr: f, LiveDir: liveDir, Labels: labels})
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
	if testCheckShutdown != nil {
		testCheckShutdown(&sr)
	}
	<-copied
	r.Exit, r.MS, r.Swept = c.ExitCode(), time.Since(start).Milliseconds(), len(sr.Swept.Killed)
	r.LeftRunning, r.Unknown = shutdownCounts(sr)
	r.CleanupFailed, r.cleanupErr = sr.Err != "", sr.Err
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

// cleanupNotice is the notice for a check whose cleanup was incomplete or
// failed, raw (with the error) and safe; ok is false when there is none.
func (r CheckResult) cleanupNotice(harness string) (raw, safe string, ok bool) {
	if r.LeftRunning == 0 && r.Unknown == 0 && !r.CleanupFailed {
		return "", "", false
	}
	safe = fmt.Sprintf("%s check %s: cleanup incomplete: %d still running, %d unknown", harness, r.Name, r.LeftRunning, r.Unknown)
	if r.CleanupFailed {
		safe += ", cleanup error"
	}
	safe += "; live record kept for retry"
	raw = safe
	if r.cleanupErr != "" {
		raw += " (" + r.cleanupErr + ")"
	}
	return raw, safe, true
}

// checkVars are the variables a check inherits; LC_* too. Everything else
// (other providers' keys, tokens, GIT_*) stays out.
var checkVars = []string{"PATH", "HOME", "USER", "LANG", "TERM",
	"GOPATH", "GOROOT", "GOCACHE", "GOMODCACHE", "GOFLAGS", // Go toolchain
	"npm_config_cache", "NPM_CONFIG_CACHE", // npm
	"CARGO_HOME", "RUSTUP_HOME"} // cargo

func checkEnvAllowed(name string) bool {
	return slices.Contains(checkVars, name) || strings.HasPrefix(name, "LC_") || name == "TMPDIR"
}

// checkEnv is a check's whole environment: the allow-listed variables from
// Refraktal's own environment, and TMPDIR in the run folder.
func checkEnv(tmp string) []string {
	env := []string{"TMPDIR=" + tmp}
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if name != "TMPDIR" && checkEnvAllowed(name) {
			env = append(env, kv)
		}
	}
	return env
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
	JudgedOwn bool     `json:"judged_own,omitempty"` // the judge was one of the close-call candidates
	// JudgesTried are judges that could not be started, in the order tried.
	JudgesTried []string `json:"judges_tried,omitempty"`
	Checks      string   `json:"checks"` // where the check came from: config, make, go, npm or none
	Margin      float64  `json:"margin"`
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
	b.WriteString("Which candidate carries out the task best and correctly? The last line of your reply must be exactly WINNER: followed by one space and the candidate's capital letter, or WINNER: none if no candidate correctly carries out the task, with nothing else on that line (no quotes, formatting or punctuation) and nothing after it.")
	return b.String()
}

var winnerLine = regexp.MustCompile(`^WINNER: ([A-Z]|none)$`)

// parseWinner reads the judge's answer from its final non-empty line, which
// must be exactly "WINNER: <letter>" or "WINNER: none": a letter, "none", or
// "" when there is no valid answer. A match anywhere else never counts.
func parseWinner(reply string, labels []string) string {
	lines := strings.Split(strings.TrimRight(reply, " \t\r\n"), "\n")
	m := winnerLine.FindStringSubmatch(strings.TrimSpace(lines[len(lines)-1]))
	if m == nil {
		return ""
	}
	if m[1] == "none" {
		return "none"
	}
	for _, l := range labels {
		if l == m[1] {
			return m[1]
		}
	}
	return ""
}
