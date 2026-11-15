// Package worktree gives each child a git worktree of its own, collects what
// the child changed, and applies it to the user's working tree.
//
// A linked worktree shares the repository's .git with the child. Refraktal's
// own git commands therefore turn off everything a poisoned config could use
// to run a command: hooks, fsmonitor, signing, filters and diff drivers. Only
// the standard Git LFS filter commands are allowed through.
// (The policy also denies git config to the child.) Its commits use a fixed
// identity, so no user git config is needed.
package worktree

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"refraktal.dev/refraktal/internal/safegit"
)

// Worktree is one child's checkout.
type Worktree struct {
	Repo   string // the user's repository (top level)
	Dir    string // the worktree folder
	Branch string
	Base   string // the commit the worktree started from
	gitDir string // recorded at Create, before the child can rewrite .git
}

// Result is what a child changed, as a commit on the worktree's branch.
type Result struct {
	Files  []string // changed paths, relative to the repository
	Diff   []byte   // binary-safe diff from Base
	Commit string   // "" if nothing changed
}

func git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return gitOperation(dir, "", false).Run(ctx, nil, args...)
}

func gitOperation(dir, gitDir string, skipLFSSmudge bool) *safegit.Operation {
	return &safegit.Operation{
		Dir: dir, GitDir: gitDir, SkipLFSSmudge: skipLFSSmudge,
		Check: func() error { return checkGitfile(dir, gitDir) },
	}
}

func checkGitfile(dir, gitDir string) error {
	if gitDir == "" {
		return nil // Commands in the user's repository use its normal discovery.
	}
	actual, err := readGitfile(dir)
	if err != nil {
		return err
	}
	if actual != gitDir {
		return errors.New("worktree: gitfile no longer points at the expected git directory")
	}
	return nil
}

func readGitfile(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, ".git"))
	if err != nil {
		return "", fmt.Errorf("worktree: read gitfile: %w", err)
	}
	path, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir: ")
	if !ok || path == "" {
		return "", errors.New("worktree: invalid gitfile")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("worktree: resolve gitfile: %w", err)
	}
	return path, nil
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
	g := gitOperation(repo, "", false)
	// Read both in one command: --verify still requires exactly one commit.
	out, err := g.Run(ctx, nil, "rev-parse", "--verify", "--path-format=absolute", "--git-common-dir", "HEAD^{commit}")
	if err != nil {
		return Worktree{}, fmt.Errorf("the repository needs at least one commit: %w", err)
	}
	pathsAndBase := strings.TrimSuffix(string(out), "\n")
	i := strings.LastIndexByte(pathsAndBase, '\n')
	if i < 0 {
		return Worktree{}, errors.New("worktree: missing common git directory or base commit")
	}
	w := Worktree{Repo: repo, Dir: dir, Branch: branch, Base: pathsAndBase[i+1:]}
	commonDir, err := filepath.EvalSymlinks(pathsAndBase[:i])
	if err != nil {
		return Worktree{}, err
	}
	if _, err := g.Run(ctx, nil, "worktree", "add", "--quiet", "--no-checkout", "-b", branch, dir, w.Base); err != nil {
		return Worktree{}, err
	}
	// Git chooses the administrative id (including a suffix on collisions).
	// Read its newly created gitfile once, before handing the tree to a child,
	// and accept only an administrative directory in this repository.
	w.gitDir, err = readGitfile(dir)
	if err != nil {
		return Worktree{}, err
	}
	if filepath.Dir(w.gitDir) != filepath.Join(commonDir, "worktrees") {
		return Worktree{}, errors.New("worktree: gitfile points outside the repository's worktree directories")
	}
	// Discover in the new branch and git directory before any checkout can
	// invoke a filter selected by an onbranch: or gitdir: conditional include.
	g = gitOperation(dir, w.gitDir, false)
	if _, err := g.Run(ctx, nil, "reset", "--hard", "--quiet", "HEAD"); err != nil {
		return Worktree{}, err
	}
	return w, nil
}

// Collect commits everything the child changed in the worktree (new files
// included, ignored files not) and returns it as a diff from Base. Commits
// the child made itself are included.
func (w Worktree) Collect(ctx context.Context, message string) (Result, error) {
	if w.gitDir == "" {
		return Result{}, errors.New("worktree: missing recorded git directory")
	}
	g := gitOperation(w.Dir, w.gitDir, true)
	if _, err := g.Run(ctx, nil, "add", "-A"); err != nil {
		return Result{}, err
	}
	if out, err := g.Run(ctx, nil, "diff", "--cached", "--name-only", "-z", "--no-color", "--no-ext-diff", "--no-textconv"); err != nil {
		return Result{}, err
	} else if len(out) > 0 {
		if _, err := g.Run(ctx, nil, "commit", "--quiet", "--no-verify", "-m", message); err != nil {
			return Result{}, err
		}
	}
	names, err := g.Run(ctx, nil, "diff", "--no-color", "--src-prefix=a/", "--dst-prefix=b/", "--no-ext-diff", "--no-textconv", "--name-only", "-z", w.Base, "HEAD")
	if err != nil {
		return Result{}, err
	}
	var r Result
	for _, n := range strings.Split(string(names), "\x00") {
		if n != "" {
			r.Files = append(r.Files, n)
		}
	}
	if len(r.Files) == 0 {
		return r, nil
	}
	if r.Diff, err = g.Run(ctx, nil, "diff", "--no-color", "--src-prefix=a/", "--dst-prefix=b/", "--no-ext-diff", "--no-textconv", "--binary", w.Base, "HEAD"); err != nil {
		return r, err
	}
	head, err := g.Run(ctx, nil, "rev-parse", "HEAD")
	r.Commit = strings.TrimSpace(string(head))
	return r, err
}

// Apply applies a result's diff to the user's working tree, without staging
// it. It checks first, so a diff that does not apply cleanly changes nothing.
// It returns a notice naming LFS paths left as pointers, or an empty string.
func (w Worktree) Apply(ctx context.Context, r Result) (string, error) {
	if len(r.Diff) == 0 {
		return "", nil
	}
	g := gitOperation(w.Repo, "", true)
	lfs := map[string]bool{}
	checkLFS := func() error {
		// NUL records preserve spaces, quotes and newlines in filenames.
		out, err := g.Run(ctx, []byte(strings.Join(r.Files, "\x00")+"\x00"), "check-attr", "-z", "--stdin", "filter")
		if err != nil {
			return err
		}
		fields := strings.Split(string(out), "\x00")
		for i := 0; i+2 < len(fields); i += 3 {
			if fields[i+1] == "filter" && fields[i+2] == "lfs" {
				lfs[fields[i]] = true
			}
		}
		return nil
	}
	if err := checkLFS(); err != nil {
		return "", err
	}
	for _, args := range [][]string{{"apply", "--check", "--binary"}, {"apply", "--binary"}} {
		if _, err := g.Run(ctx, r.Diff, args...); err != nil {
			return "", err
		}
	}
	// Include both the user's attributes and attributes added by the patch;
	// removing an attribute must not suppress the notice either.
	if err := checkLFS(); err != nil {
		return "", err
	}
	if len(lfs) > 0 {
		// git lfs checkout does not help here: it works from the index, and the
		// patch is applied to the working tree only. The objects are local.
		return fmt.Sprintf("LFS-tracked files %q are left as LFS pointers. Restore each with: "+
			"git lfs smudge -- FILE < FILE > FILE.tmp && mv FILE.tmp FILE", slices.Sorted(maps.Keys(lfs))), nil
	}
	return "", nil
}

// Remove deletes the worktree and its branch.
func (w Worktree) Remove(ctx context.Context) error {
	g := gitOperation(w.Repo, "", false)
	if _, err := g.Run(ctx, nil, "worktree", "remove", "--force", w.Dir); err != nil {
		return err
	}
	_, err := g.Run(ctx, nil, "branch", "-D", w.Branch)
	return err
}
