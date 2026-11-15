// Package worktree gives each child a git worktree of its own, collects what
// the child changed, and applies it to the user's working tree.
//
// Refraktal's own git commands never run the repository's git hooks
// (core.hooksPath is pointed at an empty value), and its commits use a fixed
// identity, so no user git config is needed.
package worktree

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Worktree is one child's checkout.
type Worktree struct {
	Repo   string // the user's repository (top level)
	Dir    string // the worktree folder
	Branch string
	Base   string // the commit the worktree started from
}

// Result is what a child changed, as a commit on the worktree's branch.
type Result struct {
	Files  []string // changed paths, relative to the repository
	Diff   []byte   // binary-safe diff from Base
	Commit string   // "" if nothing changed
}

func git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	full := append([]string{"-C", dir, "-c", "core.hooksPath=/dev/null"}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Refraktal", "GIT_AUTHOR_EMAIL=refraktal@localhost",
		"GIT_COMMITTER_NAME=Refraktal", "GIT_COMMITTER_EMAIL=refraktal@localhost")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// Root returns the top level of the git repository containing dir.
func Root(ctx context.Context, dir string) (string, error) {
	out, err := git(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("not in a git repository: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Create adds a worktree at dir on a new branch, from the repository's HEAD.
// Uncommitted changes in the user's tree are not part of it.
func Create(ctx context.Context, repo, dir, branch string) (Worktree, error) {
	base, err := git(ctx, repo, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return Worktree{}, fmt.Errorf("the repository needs at least one commit: %w", err)
	}
	w := Worktree{Repo: repo, Dir: dir, Branch: branch, Base: strings.TrimSpace(string(base))}
	if _, err := git(ctx, repo, "worktree", "add", "--quiet", "-b", branch, dir, w.Base); err != nil {
		return Worktree{}, err
	}
	return w, nil
}

// Collect commits everything the child changed in the worktree (new files
// included, ignored files not) and returns it as a diff from Base. Commits
// the child made itself are included.
func (w Worktree) Collect(ctx context.Context, message string) (Result, error) {
	if _, err := git(ctx, w.Dir, "add", "-A"); err != nil {
		return Result{}, err
	}
	if out, err := git(ctx, w.Dir, "status", "--porcelain"); err != nil {
		return Result{}, err
	} else if len(bytes.TrimSpace(out)) > 0 {
		if _, err := git(ctx, w.Dir, "commit", "--quiet", "--no-verify", "-m", message); err != nil {
			return Result{}, err
		}
	}
	names, err := git(ctx, w.Dir, "diff", "--name-only", w.Base, "HEAD")
	if err != nil {
		return Result{}, err
	}
	var r Result
	for _, n := range strings.Split(strings.TrimSpace(string(names)), "\n") {
		if n != "" {
			r.Files = append(r.Files, n)
		}
	}
	if len(r.Files) == 0 {
		return r, nil
	}
	if r.Diff, err = git(ctx, w.Dir, "diff", "--binary", w.Base, "HEAD"); err != nil {
		return r, err
	}
	head, err := git(ctx, w.Dir, "rev-parse", "HEAD")
	r.Commit = strings.TrimSpace(string(head))
	return r, err
}

// Apply applies a result's diff to the user's working tree, without staging
// it. It checks first, so a diff that does not apply cleanly changes nothing.
func (w Worktree) Apply(ctx context.Context, r Result) error {
	if len(r.Diff) == 0 {
		return nil
	}
	for _, args := range [][]string{{"apply", "--check", "--binary"}, {"apply", "--binary"}} {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", w.Repo}, args...)...)
		cmd.Stdin = bytes.NewReader(r.Diff)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// Remove deletes the worktree and its branch.
func (w Worktree) Remove(ctx context.Context) error {
	if _, err := git(ctx, w.Repo, "worktree", "remove", "--force", w.Dir); err != nil {
		return err
	}
	_, err := git(ctx, w.Repo, "branch", "-D", w.Branch)
	return err
}
