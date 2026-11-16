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
	"sync"

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

// repoLocks serialises the git commands that read or change a repository's
// list of worktrees (worktree add, worktree remove, branch -D). Run in
// parallel, one can read another's half-written administrative folder and
// fail ("failed to read .git/worktrees/<id>/commondir"). Within one
// process only: two Refraktal runs on one repository are not serialised.
var repoLocks sync.Map // canonical repository path -> chan struct{}

func lockRepo(repo string) (unlock func()) {
	unlock, _ = lockRepoContext(context.Background(), repo)
	return unlock
}

func lockRepoContext(ctx context.Context, repo string) (func(), error) {
	key := repo
	if r, err := filepath.EvalSymlinks(repo); err == nil {
		key = r
	}
	m, _ := repoLocks.LoadOrStore(key, make(chan struct{}, 1))
	lock := m.(chan struct{})
	select {
	case lock <- struct{}{}:
		return func() { <-lock }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Root returns the top level of the git repository containing dir.
func Root(ctx context.Context, dir string) (string, error) {
	out, err := git(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("not in a git repository: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Head is the commit the repository's HEAD points at.
func Head(ctx context.Context, repo string) (string, error) {
	out, err := git(ctx, repo, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", fmt.Errorf("the repository needs at least one commit: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Show returns a file's content at a commit, or nil if it is not there. It
// reads the object database only: no checkout, no filters (--no-textconv).
func Show(ctx context.Context, repo, commit, path string) []byte {
	out, err := git(ctx, repo, "show", "--no-textconv", "--no-color", commit+":"+path)
	if err != nil {
		return nil
	}
	return out
}

// Create adds a worktree at dir on a new branch, from the repository's HEAD.
// Uncommitted changes in the user's tree are not part of it.
func Create(ctx context.Context, repo, dir, branch string) (Worktree, error) {
	return CreateAt(ctx, repo, dir, branch, "HEAD")
}

// CreateAt adds a worktree at dir on a new branch, from the commit base, so
// every candidate of a run starts from the same commit.
// If setup fails, it rolls back the checkout and branch. A failed rollback
// returns their handle as well as an error identifying what remains.
func CreateAt(ctx context.Context, repo, dir, branch, base string) (Worktree, error) {
	g := gitOperation(repo, "", false)
	// Read both in one command: --verify still requires exactly one commit.
	out, err := g.Run(ctx, nil, "rev-parse", "--verify", "--path-format=absolute", "--git-common-dir", base+"^{commit}")
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
	unlock := lockRepo(repo)
	_, err = g.Run(ctx, nil, "worktree", "add", "--quiet", "--no-checkout", "-b", branch, dir, w.Base)
	unlock()
	if err != nil {
		return Worktree{}, err
	}
	rollback := func(cause error) (Worktree, error) {
		// Setup may have exhausted or cancelled the run's context. Cleanup
		// gets its own short deadline, so that cancellation cannot leave an
		// otherwise removable worktree and branch behind.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), safegit.Timeout)
		defer cancel()
		if err := w.Remove(cleanupCtx); err != nil {
			return w, fmt.Errorf("%w; rollback of worktree %q on branch %q failed: %v", cause, w.Dir, w.Branch, err)
		}
		return Worktree{}, cause
	}
	// Git chooses the administrative id (including a suffix on collisions).
	// Read its newly created gitfile once, before handing the tree to a child,
	// and accept only an administrative directory in this repository.
	w.gitDir, err = readGitfile(dir)
	if err != nil {
		return rollback(err)
	}
	if filepath.Dir(w.gitDir) != filepath.Join(commonDir, "worktrees") {
		return rollback(errors.New("worktree: gitfile points outside the repository's worktree directories"))
	}
	// Discover in the new branch and git directory before any checkout can
	// invoke a filter selected by an onbranch: or gitdir: conditional include.
	g = gitOperation(dir, w.gitDir, false)
	if _, err := g.Run(ctx, nil, "reset", "--hard", "--quiet", "HEAD"); err != nil {
		return rollback(err)
	}
	return w, nil
}

// Collect commits everything the child changed in the worktree (new files
// included, ignored files not) and returns it as a diff from Base. Rename
// detection is off (--no-renames, whatever diff.renames says), so a file
// moved from elsewhere lists both its old and its new path. Commits
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
	names, err := g.Run(ctx, nil, "diff", "--no-color", "--no-renames", "--src-prefix=a/", "--dst-prefix=b/", "--no-ext-diff", "--no-textconv", "--name-only", "-z", w.Base, "HEAD")
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
	if r.Diff, err = g.Run(ctx, nil, "diff", "--no-color", "--no-renames", "--src-prefix=a/", "--dst-prefix=b/", "--no-ext-diff", "--no-textconv", "--binary", w.Base, "HEAD"); err != nil {
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

// Changed lists modified, deleted, untracked and ignored paths, including
// committed changes from Base. A HEAD other than the recorded base is an
// integrity error even when the commit has the same tree. A judge's worktree
// must have neither changes nor an integrity error.
func (w Worktree) Changed(ctx context.Context) ([]string, error) {
	if w.gitDir == "" {
		return nil, errors.New("worktree: missing recorded git directory")
	}
	if w.Base == "" {
		return nil, errors.New("worktree: missing recorded base commit")
	}
	g := gitOperation(w.Dir, w.gitDir, true)
	head, err := g.Run(ctx, nil, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return nil, err
	}
	commit := strings.TrimSpace(string(head))
	out, err := g.Run(ctx, nil, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignored=matching", "--no-renames")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, rec := range strings.Split(string(out), "\x00") {
		if len(rec) > 3 {
			paths = append(paths, rec[3:])
		}
	}
	if commit != w.Base {
		integrityErr := errors.New("worktree: HEAD differs from the recorded base commit")
		out, err := g.Run(ctx, nil, "diff", "--no-color", "--no-renames", "--no-ext-diff", "--no-textconv", "--name-only", "-z", w.Base, commit)
		if err != nil {
			return paths, errors.Join(integrityErr, err)
		}
		for _, path := range strings.Split(string(out), "\x00") {
			if path != "" {
				paths = append(paths, path)
			}
		}
		slices.Sort(paths)
		return slices.Compact(paths), integrityErr
	}
	return paths, nil
}

// CheckApply reports whether a result's diff applies cleanly to the user's
// working tree (git apply --check), changing nothing.
func (w Worktree) CheckApply(ctx context.Context, r Result) error {
	if len(r.Diff) == 0 {
		return nil
	}
	_, err := gitOperation(w.Repo, "", true).Run(ctx, r.Diff, "apply", "--check", "--binary")
	return err
}

// Remove deletes the worktree and its branch.
func (w Worktree) Remove(ctx context.Context) error {
	unlock, err := lockRepoContext(ctx, w.Repo)
	if err != nil {
		return err
	}
	defer unlock()
	g := gitOperation(w.Repo, "", false)
	if _, err := g.Run(ctx, nil, "worktree", "remove", "--force", w.Dir); err != nil {
		return err
	}
	_, err = g.Run(ctx, nil, "branch", "-D", w.Branch)
	return err
}
