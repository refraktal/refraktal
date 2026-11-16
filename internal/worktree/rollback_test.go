package worktree

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// snapStore is an empty, pinned folder for an apply's snapshot.
func snapStore(t *testing.T) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return root
}

// empty reports whether a folder holds nothing.
func empty(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) == 0
}

// treeState is every path under dir except .git: kind, permission bits and
// content (or link target), for a byte-for-byte comparison.
func treeState(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == ".git" {
			return filepath.SkipDir
		}
		info, _ := os.Lstat(p)
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			l, _ := os.Readlink(p)
			out[rel] = "link " + l
		case info.IsDir():
			out[rel] = fmt.Sprintf("dir %o", info.Mode().Perm())
		default:
			b, _ := os.ReadFile(p)
			out[rel] = fmt.Sprintf("file %o %q", info.Mode().Perm(), b)
		}
		return nil
	})
	return out
}

func diffStates(before, after map[string]string) []string {
	var d []string
	for k, v := range before {
		if after[k] != v {
			d = append(d, fmt.Sprintf("%s: %s -> %s", k, v, cmpOr(after[k], "(absent)")))
		}
	}
	for k, v := range after {
		if _, ok := before[k]; !ok {
			d = append(d, fmt.Sprintf("%s: (absent) -> %s", k, v))
		}
	}
	slices.Sort(d)
	return d
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// richResult is a candidate result that changes, adds (in new folders),
// deletes, re-modes and type-changes paths, in a repository that also holds
// z/file.txt.
func richResult(t *testing.T) (repoDir string, w Worktree, res Result) {
	t.Helper()
	ctx := context.Background()
	r := repo(t)
	for name, body := range map[string]string{"b.txt": "bee\n", "run.sh": "#!/bin/sh\n", "z/file.txt": "zed\n", "kind": "a file\n",
		"dir/x": "in a folder\n", "f": "a file that becomes a folder\n"} {
		os.MkdirAll(filepath.Dir(filepath.Join(r, name)), 0o755)
		os.WriteFile(filepath.Join(r, name), []byte(body), 0o644)
	}
	gitc := exec.Command("git", "-C", r, "add", "-A")
	if out, err := gitc.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	gitc = exec.Command("git", "-C", r, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--no-verify", "-m", "more")
	if out, err := gitc.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	w, err := Create(ctx, r, filepath.Join(t.TempDir(), "wt"), "refraktal/rollback")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(w.Dir, "a.txt"), []byte("changed\n"), 0o644)
	os.Remove(filepath.Join(w.Dir, "b.txt"))
	os.Chmod(filepath.Join(w.Dir, "run.sh"), 0o755)
	os.MkdirAll(filepath.Join(w.Dir, "new", "deep"), 0o755)
	os.WriteFile(filepath.Join(w.Dir, "new", "deep", "file.txt"), []byte("new\n"), 0o644)
	os.Symlink("a.txt", filepath.Join(w.Dir, "link"))
	os.Remove(filepath.Join(w.Dir, "kind"))
	os.Symlink("a.txt", filepath.Join(w.Dir, "kind")) // a file becomes a link
	os.WriteFile(filepath.Join(w.Dir, "z", "file.txt"), []byte("zed changed\n"), 0o644)
	os.RemoveAll(filepath.Join(w.Dir, "dir"))
	os.Symlink("a.txt", filepath.Join(w.Dir, "dir")) // a folder becomes a link
	os.Remove(filepath.Join(w.Dir, "f"))
	os.MkdirAll(filepath.Join(w.Dir, "f"), 0o755)
	os.WriteFile(filepath.Join(w.Dir, "f", "y"), []byte("y\n"), 0o644) // a file becomes a folder
	if res, err = w.Collect(ctx, "refraktal"); err != nil {
		t.Fatal(err)
	}
	return r, w, res
}

// V4: a git apply that fails after writing some files (here: z/ is not
// writable, and git writes a.txt first) leaves the user's tree exactly as it
// was. Skipped as root, where the permission does not stop git: the seam
// variant below covers root and CI.
func TestFailedApplyIsRolledBack(t *testing.T) {
	if os.Geteuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("a read-only folder does not stop root")
	}
	r, w, res := richResult(t)
	before := treeState(t, r)
	os.Chmod(filepath.Join(r, "z"), 0o555)
	defer os.Chmod(filepath.Join(r, "z"), 0o755)
	before["z"] = "dir 555"
	store := snapStore(t)
	snap := store.Name()
	_, err := w.Apply(context.Background(), res, store)
	if err == nil {
		t.Fatal("the apply into a read-only folder succeeded")
	}
	if d := diffStates(before, treeState(t, r)); len(d) > 0 {
		t.Fatalf("the failed apply left the tree changed:\n%s\n(error: %v)", strings.Join(d, "\n"), err)
	}
	var ae *ApplyError
	if !errors.As(err, &ae) || !ae.Restored {
		t.Fatalf("want a restored ApplyError, got %v", err)
	}
	if !empty(snap) {
		t.Error("the snapshot was left after a complete restore")
	}
}

// V4, through the seam: the whole patch is applied (changes, additions in
// new folders, a deletion, a mode change, a file turned into a link) and
// then the apply fails. Runs as root and on CI.
func TestFailedApplyIsRolledBackViaSeam(t *testing.T) {
	r, w, res := richResult(t)
	before := treeState(t, r)
	testAfterGitApply = func(string) error { return errors.New("injected: disk full") }
	defer func() { testAfterGitApply = nil }()
	store := snapStore(t)
	snap := store.Name()
	_, err := w.Apply(context.Background(), res, store)
	if err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("err = %v", err)
	}
	if d := diffStates(before, treeState(t, r)); len(d) > 0 {
		t.Fatalf("the failed apply left the tree changed:\n%s", strings.Join(d, "\n"))
	}
	var ae *ApplyError
	if !errors.As(err, &ae) || !ae.Restored || len(ae.Unrestored) > 0 {
		t.Fatalf("want a restored ApplyError, got %#v", err)
	}
	// A later, successful apply leaves no snapshot behind.
	testAfterGitApply = nil
	if _, err := w.Apply(context.Background(), res, store); err != nil {
		t.Fatal(err)
	}
	if !empty(snap) {
		t.Error("the snapshot was left after a successful apply")
	}
}

// V4: when the restore itself fails, the error says so, names the paths not
// restored and where the snapshot is; the snapshot is kept and holds the
// original bytes.
func TestFailedRestoreIsReported(t *testing.T) {
	r, w, res := richResult(t)
	testAfterGitApply = func(repo string) error {
		// Something put a non-empty folder where a.txt must be restored.
		os.Remove(filepath.Join(repo, "a.txt"))
		os.MkdirAll(filepath.Join(repo, "a.txt", "x"), 0o755)
		return errors.New("injected")
	}
	defer func() { testAfterGitApply = nil }()
	store := snapStore(t)
	snap := store.Name()
	_, err := w.Apply(context.Background(), res, store)
	var ae *ApplyError
	if !errors.As(err, &ae) || ae.Restored || !slices.Contains(ae.Unrestored, "a.txt") || ae.Snapshot != snap {
		t.Fatalf("want an unrestored ApplyError naming a.txt and %s, got %#v", snap, err)
	}
	if !strings.Contains(err.Error(), "a.txt") || !strings.Contains(err.Error(), snap) {
		t.Errorf("the message does not name the path and the snapshot: %v", err)
	}
	found := false
	filepath.WalkDir(snap, func(p string, d fs.DirEntry, _ error) error {
		if b, _ := os.ReadFile(p); d != nil && !d.IsDir() && string(b) == "one\n" {
			found = true
		}
		return nil
	})
	if !found {
		t.Error("the snapshot does not hold a.txt's original bytes")
	}
	if b, _ := os.ReadFile(filepath.Join(r, "b.txt")); string(b) != "bee\n" {
		t.Errorf("the other paths were not restored: b.txt = %q", b)
	}
}

// V4: a failed apply that left a link to a folder outside the repository
// where a folder was cannot make the restore write or delete through it.
func TestRestoreNeverFollowsALinkOut(t *testing.T) {
	r, w, res := richResult(t)
	before := treeState(t, r)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "x"), []byte("not yours\n"), 0o644)
	testAfterGitApply = func(repo string) error {
		os.RemoveAll(filepath.Join(repo, "dir"))
		os.Symlink(outside, filepath.Join(repo, "dir")) // dir/x would be outside/x
		return errors.New("injected")
	}
	defer func() { testAfterGitApply = nil }()
	_, err := w.Apply(context.Background(), res, snapStore(t))
	if b, _ := os.ReadFile(filepath.Join(outside, "x")); string(b) != "not yours\n" {
		t.Fatalf("the restore wrote through the link: outside/x = %q", b)
	}
	if d := diffStates(before, treeState(t, r)); len(d) > 0 {
		t.Fatalf("not restored:\n%s\n(%v)", strings.Join(d, "\n"), err)
	}
}

// Run-5 #5: a step after git apply (the LFS attribute check, here failing
// because the context is cancelled) keeps the snapshot until it passes, and
// its failure rolls back like an apply failure.
func TestPostApplyFailureIsRolledBack(t *testing.T) {
	r, w, res := richResult(t)
	before := treeState(t, r)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	testAfterGitApply = func(string) error { cancel(); return nil }
	defer func() { testAfterGitApply = nil }()
	store := snapStore(t)
	_, err := w.Apply(ctx, res, store)
	if err == nil {
		t.Fatal("the post-apply check succeeded on a cancelled context")
	}
	if d := diffStates(before, treeState(t, r)); len(d) > 0 {
		t.Fatalf("the failed post-apply step left the tree changed:\n%s\n(error: %v)", strings.Join(d, "\n"), err)
	}
	var ae *ApplyError
	if !errors.As(err, &ae) || !ae.Restored {
		t.Fatalf("want a restored ApplyError, got %v", err)
	}
	if !empty(store.Name()) {
		t.Error("the snapshot was left after a complete restore")
	}
}
