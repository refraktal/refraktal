package worktree

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestVerifyApplyAtomicityFilesystemError reproduces the lifecycle model's
// faithful half-apply counterexample against the real Apply; it is enforced
// now that Apply rolls back (root bypasses the permission: see
// TestFailedApplyIsRolledBackViaSeam for the variant that runs as root).
// No harness or linked worktree is started.
func TestVerifyApplyAtomicityFilesystemError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires filesystem permissions that root can bypass")
	}
	r := repo(t)
	blocked := filepath.Join(r, "z")
	if err := os.Mkdir(blocked, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(blocked, 0o755) })
	patch := []byte("diff --git a/a.txt b/a.txt\nindex 5626abf..f719efd 100644\n--- a/a.txt\n+++ b/a.txt\n@@ -1 +1 @@\n-one\n+two\ndiff --git a/z/b.txt b/z/b.txt\nnew file mode 100644\nindex 0000000..f719efd\n--- /dev/null\n+++ b/z/b.txt\n@@ -0,0 +1 @@\n+two\n")
	w := Worktree{Repo: r}
	result := Result{Diff: patch, Files: []string{"a.txt", "z/b.txt"}}
	if err := w.CheckApply(context.Background(), result); err != nil {
		t.Fatalf("fixture must pass git apply --check: %v", err)
	}
	_, err := w.Apply(context.Background(), result, snapStore(t))
	if err == nil {
		t.Fatal("fixture requires the second file write to fail")
	}
	b, readErr := os.ReadFile(filepath.Join(r, "a.txt"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if _, statErr := os.Stat(filepath.Join(blocked, "b.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("fixture requires the second file to remain absent: %v", statErr)
	}
	t.Logf("apply_error=%v read_error=%v first_file=%q", err, readErr, b)
	if string(b) != "one\n" {
		t.Fatalf("tree partially applied after filesystem error: a.txt=%q", b)
	}
}
