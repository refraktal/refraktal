package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCreateRollsBackFailedPopulation(t *testing.T) {
	for _, at := range []bool{false, true} {
		t.Run(fmt.Sprintf("CreateAt=%t", at), func(t *testing.T) {
			r := repo(t)
			dir := filepath.Join(t.TempDir(), "failed-checkout")
			const branch = "refraktal/failed-checkout"
			failWorktreeCommands(t, false)
			var w Worktree
			var err error
			if at {
				w, err = CreateAt(context.Background(), r, dir, branch, "HEAD")
			} else {
				w, err = Create(context.Background(), r, dir, branch)
			}
			if err == nil || !strings.Contains(err.Error(), "population failed") {
				t.Fatalf("expected injected population failure, got %v", err)
			}
			if w != (Worktree{}) {
				t.Errorf("successfully rolled back creation returned a handle: %+v", w)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Errorf("failed population left its checkout behind: %v", err)
			}
			if out := fixtureGit(t, r, "branch", "--list", branch); out != "" {
				t.Errorf("failed population left its branch behind: %s", out)
			}
			if out := fixtureGit(t, r, "worktree", "list", "--porcelain"); strings.Count(out, "worktree ") != 1 {
				t.Errorf("failed population left its administrative entry behind: %s", out)
			}
		})
	}
}

func TestCreateReportsFailedRollback(t *testing.T) {
	r := repo(t)
	dir := filepath.Join(t.TempDir(), "failed-checkout")
	const branch = "refraktal/failed-cleanup"
	originalPath := os.Getenv("PATH")
	failWorktreeCommands(t, true)
	w, err := CreateAt(context.Background(), r, dir, branch, "HEAD")
	if err == nil {
		t.Fatal("creation succeeded despite failed population and cleanup")
	}
	for _, detail := range []string{"population failed", "cleanup failed", dir, branch} {
		if !strings.Contains(err.Error(), detail) {
			t.Errorf("error does not report %q: %v", detail, err)
		}
	}
	if w.Repo != r || w.Dir != dir || w.Branch != branch || w.Base == "" || w.gitDir == "" {
		t.Fatalf("failed rollback lost the worktree's identity: %+v", w)
	}
	// Once Git can remove worktrees again, the returned handle can clean up.
	t.Setenv("PATH", originalPath)
	if err := w.Remove(context.Background()); err != nil {
		t.Fatalf("returned handle cannot clean up failed creation: %v", err)
	}
}

func TestCreateRollsBackAfterCancellation(t *testing.T) {
	r := repo(t)
	dir := filepath.Join(t.TempDir(), "cancelled-checkout")
	const branch = "refraktal/cancelled-checkout"
	ready := filepath.Join(t.TempDir(), "reset-started")
	wrapWorktreeGit(t, fmt.Sprintf("*' reset --hard '*) : > '%s'; exec sleep 30;;\n", ready))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, err := CreateAt(ctx, r, dir, branch, "HEAD")
		finished <- err
	}()
	deadline := time.After(5 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	waiting := true
	for waiting {
		select {
		case err := <-finished:
			t.Fatalf("creation ended before reset started: %v", err)
		case <-deadline:
			t.Fatal("reset did not start")
		case <-ticker.C:
			_, err := os.Stat(ready)
			waiting = err != nil
		}
	}
	cancel()
	select {
	case err := <-finished:
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancelled creation, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled creation did not return promptly")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("cancelled population left its checkout behind: %v", err)
	}
	if out := fixtureGit(t, r, "branch", "--list", branch); out != "" {
		t.Errorf("cancelled population left its branch behind: %s", out)
	}
	if out := fixtureGit(t, r, "worktree", "list", "--porcelain"); strings.Count(out, "worktree ") != 1 {
		t.Errorf("cancelled population left its administrative entry behind: %s", out)
	}
}

func TestRemoveHonorsContextWhileRepositoryLocked(t *testing.T) {
	r := repo(t)
	w, err := Create(context.Background(), r, filepath.Join(t.TempDir(), "wt"), "refraktal/locked-cleanup")
	if err != nil {
		t.Fatal(err)
	}
	unlock := lockRepo(r)
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- w.Remove(ctx) }()
	select {
	case err := <-finished:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected deadline while waiting for repository lock, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Remove ignored its deadline while waiting for repository lock")
	}
}

// Wrap real Git so worktree registration succeeds before population fails.
func failWorktreeCommands(t *testing.T, failRemove bool) {
	t.Helper()
	remove := ""
	if failRemove {
		remove = "*' worktree remove '*) echo 'cleanup failed' >&2; exit 24;;\n"
	}
	wrapWorktreeGit(t, "*' reset --hard '*) echo 'population failed' >&2; exit 23;;\n"+remove)
}

func wrapWorktreeGit(t *testing.T, cases string) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	body := fmt.Sprintf("#!/bin/sh\ncase \" $* \" in\n%sesac\nexec '%s' \"$@\"\n", cases, realGit)
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}
