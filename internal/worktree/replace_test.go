package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A candidate shares the repository's .git, so it can add a replacement ref
// (git replace <base> <stub>). Refraktal's git commands must read the real
// objects: check detection reads the base commit's files.
func TestReplacementRefsAreIgnored(t *testing.T) {
	repo := t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(repo, "Makefile"), []byte("test:\n\tgo test ./...\n"), 0o644)
	run("add", "-A")
	run("commit", "-q", "-m", "base")
	base := run("rev-parse", "HEAD")
	os.WriteFile(filepath.Join(repo, "Makefile"), []byte("test:\n\ttrue\n"), 0o644)
	run("commit", "-q", "-am", "stub")
	stub := run("rev-parse", "HEAD")
	run("replace", base, stub)
	ctx := context.Background()
	if got := string(Show(ctx, repo, base, "Makefile")); !strings.Contains(got, "go test") {
		t.Fatalf("Show read the replacement: %q", got)
	}
	if head, err := Head(ctx, repo); err != nil || head != stub {
		t.Fatalf("head %s %v", head, err)
	}
}
