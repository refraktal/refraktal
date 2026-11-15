package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// repo makes a repository with one commit and git hooks that would leave a
// marker if any of them ran.
func repo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	os.MkdirAll(dir, 0o755)
	run("init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644)
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("build/\n"), 0o644)
	run("add", "-A")
	run("commit", "-q", "-m", "init")
	// post-checkout runs on worktree add and post-commit on commit; neither
	// is skipped by --no-verify.
	for _, name := range []string{"pre-commit", "post-checkout", "post-commit"} {
		hook := filepath.Join(dir, ".git", "hooks", name)
		os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+filepath.Join(dir, "HOOK-RAN")+"\n"), 0o755)
	}
	return dir
}

func TestCreateCollectApplyRemove(t *testing.T) {
	ctx := context.Background()
	r := repo(t)
	if root, err := Root(ctx, filepath.Join(r, ".git", "..")); err != nil || filepath.Base(root) != "repo" {
		t.Fatalf("root %q %v", root, err)
	}
	w, err := Create(ctx, r, filepath.Join(t.TempDir(), "wt"), "refraktal/test")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(w.Dir, "a.txt"), []byte("one\ntwo\n"), 0o644)
	os.WriteFile(filepath.Join(w.Dir, "new.bin"), []byte{0, 1, 2, 255}, 0o644)
	os.MkdirAll(filepath.Join(w.Dir, "build"), 0o755)
	os.WriteFile(filepath.Join(w.Dir, "build", "out"), []byte("x"), 0o644) // ignored

	res, err := w.Collect(ctx, "refraktal test")
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(res.Files)
	if strings.Join(res.Files, ",") != "a.txt,new.bin" || res.Commit == "" || len(res.Diff) == 0 {
		t.Fatalf("result %+v", res)
	}
	if _, err := os.Stat(filepath.Join(r, "HOOK-RAN")); err == nil {
		t.Fatal("the repository's git hook ran")
	}

	if err := w.Apply(ctx, res); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(r, "a.txt")); string(b) != "one\ntwo\n" {
		t.Fatalf("a.txt = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(r, "new.bin")); string(b) != "\x00\x01\x02\xff" {
		t.Fatalf("new.bin = %q", b)
	}
	if err := w.Remove(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(w.Dir); !os.IsNotExist(err) {
		t.Fatal("worktree folder still there")
	}
	if out, _ := exec.Command("git", "-C", r, "branch", "--list", "refraktal/test").Output(); len(out) != 0 {
		t.Fatal("branch not deleted")
	}
}

func TestCollectIncludesTheChildsOwnCommits(t *testing.T) {
	ctx := context.Background()
	w, err := Create(ctx, repo(t), filepath.Join(t.TempDir(), "wt"), "refraktal/own")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(w.Dir, "b.txt"), []byte("b\n"), 0o644)
	git(ctx, w.Dir, "add", "b.txt")
	git(ctx, w.Dir, "commit", "-q", "-m", "child commit")
	res, err := w.Collect(ctx, "refraktal")
	if err != nil || strings.Join(res.Files, ",") != "b.txt" {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestNoChanges(t *testing.T) {
	ctx := context.Background()
	w, err := Create(ctx, repo(t), filepath.Join(t.TempDir(), "wt"), "refraktal/none")
	if err != nil {
		t.Fatal(err)
	}
	res, err := w.Collect(ctx, "refraktal")
	if err != nil || len(res.Files) != 0 || res.Commit != "" || w.Apply(ctx, res) != nil {
		t.Fatalf("%+v %v", res, err)
	}
}

// A diff that conflicts with the user's own edits must leave their tree as
// it was.
func TestApplyConflictChangesNothing(t *testing.T) {
	ctx := context.Background()
	r := repo(t)
	w, err := Create(ctx, r, filepath.Join(t.TempDir(), "wt"), "refraktal/conflict")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(w.Dir, "a.txt"), []byte("child\n"), 0o644)
	os.WriteFile(filepath.Join(w.Dir, "c.txt"), []byte("c\n"), 0o644)
	res, err := w.Collect(ctx, "refraktal")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(r, "a.txt"), []byte("mine\n"), 0o644)
	if err := w.Apply(ctx, res); err == nil {
		t.Fatal("want a conflict")
	}
	if b, _ := os.ReadFile(filepath.Join(r, "a.txt")); string(b) != "mine\n" {
		t.Fatalf("user's edit lost: %q", b)
	}
	if _, err := os.Stat(filepath.Join(r, "c.txt")); err == nil {
		t.Fatal("part of a failed diff was applied")
	}
}

func TestCreateNeedsACommit(t *testing.T) {
	dir := t.TempDir()
	exec.Command("git", "-C", dir, "init", "-q").Run()
	if _, err := Create(context.Background(), dir, filepath.Join(t.TempDir(), "wt"), "b"); err == nil || !strings.Contains(err.Error(), "at least one commit") {
		t.Fatalf("got %v", err)
	}
}

// The child shares .git: a config it poisoned must not run anything when
// Refraktal collects and applies the result.
func TestPoisonedConfigRunsNothing(t *testing.T) {
	ctx := context.Background()
	r := repo(t)
	w, err := Create(ctx, r, filepath.Join(t.TempDir(), "wt"), "refraktal/poison")
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "RAN")
	evil := filepath.Join(t.TempDir(), "evil.sh")
	os.WriteFile(evil, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755)
	for _, kv := range [][]string{{"core.fsmonitor", evil}, {"diff.external", evil}, {"core.pager", evil}} {
		if out, err := exec.Command("git", "-C", w.Dir, "config", kv[0], kv[1]).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	os.WriteFile(filepath.Join(w.Dir, "a.txt"), []byte("changed\n"), 0o644)
	res, err := w.Collect(ctx, "refraktal")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Apply(ctx, res); err != nil {
		t.Fatal(err)
	}
	w.Remove(ctx)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a command from the poisoned git config ran")
	}
}
