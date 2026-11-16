package worktree

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

// Fan-out creates and removes several worktrees of one repository at once.
// Concurrent git worktree commands can read another's half-written
// administrative folder; Create and Remove must not fail because of that.
func TestConcurrentWorktrees(t *testing.T) {
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"commit", "-q", "--allow-empty", "-m", "init"}} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	dir := t.TempDir()
	ctx := context.Background()
	for round := 0; round < 10; round++ {
		var wg sync.WaitGroup
		wts := make([]Worktree, 16)
		errs := make([]error, 16)
		for i := range wts {
			wg.Add(1)
			go func() {
				defer wg.Done()
				name := fmt.Sprintf("r%d-%d", round, i)
				wts[i], errs[i] = Create(ctx, repo, filepath.Join(dir, name), "b/"+name)
			}()
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d, worktree %d: %v", round, i, err)
			}
		}
		for i := range wts {
			wg.Add(1)
			go func() { defer wg.Done(); errs[i] = wts[i].Remove(ctx) }()
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d, remove %d: %v", round, i, err)
			}
		}
	}
}
