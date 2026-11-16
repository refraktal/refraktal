package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// With a real git-lfs (installed on the CI runners): Create smudges, Collect
// commits a pointer, Apply leaves a pointer and names it in the notice, and
// the notice's own instruction restores the content from the local object.
func TestRealGitLFSEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("git-lfs"); err != nil {
		t.Skip("git-lfs is not installed")
	}
	home := t.TempDir() // the standard driver, as git lfs install writes it
	cfg := "[filter \"lfs\"]\n\tclean = git-lfs clean -- %f\n\tsmudge = git-lfs smudge -- %f\n\tprocess = git-lfs filter-process\n\trequired = true\n"
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	ctx, r := context.Background(), repo(t)
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "core.hooksPath=/dev/null"}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return string(out)
	}
	isPointer := func(b string) bool { return strings.HasPrefix(b, "version https://git-lfs.github.com/spec/v1\n") }

	run(r, "lfs", "track", "*.bin")
	if err := os.WriteFile(filepath.Join(r, "big.bin"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(r, "add", ".gitattributes", "big.bin")
	run(r, "commit", "-qm", "lfs")
	if !isPointer(run(r, "cat-file", "-p", "HEAD:big.bin")) {
		t.Fatal("fixture: big.bin not stored as an LFS pointer")
	}

	w, err := Create(ctx, r, filepath.Join(t.TempDir(), "wt"), "refraktal/real-lfs")
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(w.Dir, "big.bin")); string(b) != "one\n" {
		t.Fatalf("Create did not smudge: %q", b)
	}
	os.WriteFile(filepath.Join(w.Dir, "big.bin"), []byte("two\n"), 0o644)
	os.WriteFile(filepath.Join(w.Dir, "plain.txt"), []byte("plain\n"), 0o644)
	res, err := w.Collect(ctx, "lfs change")
	if err != nil {
		t.Fatal(err)
	}
	if !isPointer(run(w.Dir, "cat-file", "-p", "HEAD:big.bin")) {
		t.Fatal("Collect committed raw content instead of a pointer")
	}
	notice, err := w.Apply(ctx, res, snapStore(t))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(notice, "big.bin") || strings.Contains(notice, "plain.txt") {
		t.Fatalf("notice = %q", notice)
	}
	if b, _ := os.ReadFile(filepath.Join(r, "big.bin")); !isPointer(string(b)) {
		t.Fatalf("Apply smudged big.bin: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(r, "plain.txt")); string(b) != "plain\n" {
		t.Fatalf("plain.txt = %q", b)
	}
	restore := exec.Command("sh", "-c", "git lfs smudge -- big.bin < big.bin > big.bin.tmp && mv big.bin.tmp big.bin")
	restore.Dir = r
	if out, err := restore.CombinedOutput(); err != nil {
		t.Fatalf("notice's restore instruction failed: %v %s", err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(r, "big.bin")); string(b) != "two\n" {
		t.Fatalf("notice's restore instruction left %q", b)
	}
	if err := w.Remove(ctx); err != nil {
		t.Fatal(err)
	}
}
