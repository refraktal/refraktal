package refraktal

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"refraktal.dev/refraktal/internal/harness"
)

type runRig struct {
	repo, wtDir, live, config, prompts string
}

func newRunRig(t *testing.T) runRig {
	t.Helper()
	d := t.TempDir()
	r := runRig{repo: filepath.Join(d, "repo"), wtDir: filepath.Join(d, "worktrees"), live: filepath.Join(d, "live"),
		config: filepath.Join(d, "config.json"), prompts: filepath.Join(d, "prompts")}
	os.MkdirAll(r.repo, 0o755)
	r.git(t, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(r.repo, "README"), []byte("x\n"), 0o644)
	r.git(t, "add", "-A")
	r.git(t, "commit", "-q", "-m", "init")
	return r
}

func (r runRig) git(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", r.repo}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return string(out)
}

func (r runRig) run(t *testing.T, h fake, mut func(*RunOptions)) RunResult {
	t.Helper()
	h.prompts = r.prompts
	o := RunOptions{Task: "do the thing", Harness: h.name, Dir: r.repo, Limit: 10 * time.Second,
		LiveDir: r.live, ConfigPath: r.config, WorktreeDir: r.wtDir, Harnesses: []harness.Harness{h}}
	if mut != nil {
		mut(&o)
	}
	res, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

func TestRunAppliesWhatThePolicyAllows(t *testing.T) {
	r := newRunRig(t)
	res := r.run(t, fake{name: "claude", agentMode: "tools"}, nil)
	if res.Outcome != RunApplied || res.ExitCode() != ExitOK || res.Allowed != 1 || res.Denied != 2 {
		t.Fatalf("result %+v", res)
	}
	if b, _ := os.ReadFile(filepath.Join(r.repo, "hello.txt")); string(b) != "hello\n" {
		t.Fatalf("hello.txt in the working tree = %q", b)
	}
	if exists(filepath.Join(r.wtDir, "outside.txt")) || exists(filepath.Join(r.repo, "pushed.txt")) {
		t.Fatal("a denied action happened")
	}
	if strings.Join(res.Files, ",") != "hello.txt" || res.Branch != "" || res.Worktree != "" {
		t.Fatalf("files %v branch %q worktree %q", res.Files, res.Branch, res.Worktree)
	}
	if entries, _ := os.ReadDir(r.wtDir); len(entries) != 0 {
		t.Fatal("worktree not removed after applying")
	}
	if out := r.git(t, "branch", "--list", "refraktal/*"); strings.TrimSpace(out) != "" {
		t.Fatalf("branch left: %s", out)
	}
	if res.Cost == nil || res.Usage == nil || res.Usage.TotalTokens != 900 {
		t.Fatalf("usage %+v cost %+v", res.Usage, res.Cost)
	}
	events, _ := os.ReadFile(filepath.Join(r.repo, res.Run, "events.jsonl"))
	if strings.Count(string(events), `"event":"permission"`) != 3 || !strings.Contains(string(events), "deny-list: git push") {
		t.Fatalf("permission decisions not logged:\n%s", events)
	}
	if !exists(filepath.Join(r.repo, res.Run, "result.diff")) {
		t.Fatal("no result.diff in the run folder")
	}
	if out := r.git(t, "status", "--porcelain"); strings.Contains(out, ".refraktal") {
		t.Fatalf("run folder visible to git: %s", out)
	}
}

func TestRunNoApplyKeepsTheBranch(t *testing.T) {
	r := newRunRig(t)
	res := r.run(t, fake{name: "claude", agentMode: "tools"}, func(o *RunOptions) { o.NoApply = true })
	if res.Outcome != RunKept || res.ExitCode() != ExitOK || res.Branch == "" || res.Worktree == "" {
		t.Fatalf("result %+v", res)
	}
	if exists(filepath.Join(r.repo, "hello.txt")) {
		t.Fatal("--no-apply changed the working tree")
	}
	if !exists(filepath.Join(res.Worktree, "hello.txt")) {
		t.Fatal("the kept worktree lacks the change")
	}
	if out := r.git(t, "log", "--oneline", res.Branch, "-1"); !strings.Contains(out, "do the thing") {
		t.Fatalf("branch head: %s", out)
	}
}

func TestRunWithNoChanges(t *testing.T) {
	r := newRunRig(t)
	res := r.run(t, fake{name: "codex", agentMode: "turn"}, nil)
	if res.Outcome != RunNoChanges || res.ExitCode() != ExitOK || res.StopReason != "end_turn" || !strings.Contains(res.Reply, "Hello") {
		t.Fatalf("result %+v", res)
	}
	if entries, _ := os.ReadDir(r.wtDir); len(entries) != 0 {
		t.Fatal("worktree not removed")
	}
}

func TestRunTimesOut(t *testing.T) {
	r := newRunRig(t)
	res := r.run(t, fake{name: "agy", agentMode: "hang"}, func(o *RunOptions) { o.Limit = 300 * time.Millisecond })
	if res.Outcome != RunTimedOut || res.ExitCode() != ExitBudget || res.StopReason != "cancelled" {
		t.Fatalf("result %+v", res)
	}
}

func TestRunWithALoggedOutHarness(t *testing.T) {
	r := newRunRig(t)
	res := r.run(t, fake{name: "claude", agentMode: "noauth"}, nil)
	if res.Outcome != RunUnusable || res.ExitCode() != ExitNothingUsable || !strings.Contains(res.Error, StatusLoggedOut) {
		t.Fatalf("result %+v", res)
	}
	if _, err := os.Stat(filepath.Join(r.prompts)); err == nil {
		t.Fatal("prompt sent to a harness that failed the handshake")
	}
	if entries, _ := os.ReadDir(r.wtDir); len(entries) != 0 {
		t.Fatal("worktree not removed")
	}
}

func TestRunNotices(t *testing.T) {
	r := newRunRig(t)
	os.MkdirAll(filepath.Join(r.repo, ".claude"), 0o755)
	os.WriteFile(filepath.Join(r.repo, ".claude", "settings.json"), []byte(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"true"}]}]}}`), 0o644)
	r.git(t, "add", "-A")
	r.git(t, "commit", "-q", "-m", "hooks")
	var seen []string
	res := r.run(t, fake{name: "claude", agentMode: "turn"}, func(o *RunOptions) { o.OnNotice = func(s string) { seen = append(seen, s) } })
	if len(res.Notices) != 1 || len(seen) != 1 || !strings.Contains(seen[0], ".claude/settings.json") {
		t.Fatalf("notices %v", res.Notices)
	}

	os.WriteFile(r.config, []byte(`{"harnesses":{"codex":{"mcp":"inherit"}}}`), 0o600)
	res = r.run(t, fake{name: "codex", agentMode: "turn"}, nil)
	if res.MCP != harness.MCPInherit || len(res.Notices) != 1 || !strings.Contains(res.Notices[0], "your own MCP servers") {
		t.Fatalf("mcp %q notices %v", res.MCP, res.Notices)
	}
}

func TestRunNeedsATaskAndAKnownHarness(t *testing.T) {
	r := newRunRig(t)
	if _, err := Run(context.Background(), RunOptions{Harness: "claude", Dir: r.repo}); err == nil {
		t.Fatal("want an error for an empty task")
	}
	_, err := Run(context.Background(), RunOptions{Task: "x", Harness: "nope", Dir: r.repo, LiveDir: r.live,
		ConfigPath: r.config, WorktreeDir: r.wtDir, Harnesses: []harness.Harness{fake{name: "claude"}}})
	if err == nil || !strings.Contains(err.Error(), "unknown harness") {
		t.Fatalf("got %v", err)
	}
}

func TestRunNoApplyKeepsTheWorktreeEvenWithoutChanges(t *testing.T) {
	r := newRunRig(t)
	res := r.run(t, fake{name: "claude", agentMode: "turn"}, func(o *RunOptions) { o.NoApply = true })
	if res.Outcome != RunNoChanges || res.Worktree == "" || !exists(res.Worktree) {
		t.Fatalf("result %+v", res)
	}
}
