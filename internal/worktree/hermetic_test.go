package worktree

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"refraktal.dev/refraktal/internal/safegit"
)

// TestMain isolates every test from the machine's git configuration. CI
// runners configure Git LFS globally or system-wide; the tests must behave
// the same with or without that. GIT_CONFIG_NOSYSTEM is the one GIT_* variable
// safegit.Env passes through.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "refraktal-git-home-")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", home)
	os.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

// requireFakeLFS fails unless git-lfs resolves to the fake in bin, using the
// PATH from the exact environment Refraktal passes to git.
func requireFakeLFS(t *testing.T, bin string) {
	t.Helper()
	path := ""
	for _, kv := range safegit.Env() {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = v
		}
	}
	for _, dir := range filepath.SplitList(path) {
		candidate := filepath.Join(dir, "git-lfs")
		if fi, err := os.Stat(candidate); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
			if candidate != filepath.Join(bin, "git-lfs") {
				t.Fatalf("git-lfs resolves to %s, not the test's fake in %s", candidate, bin)
			}
			return
		}
	}
	t.Fatalf("git-lfs not found on git's PATH; the fake in %s is missing", bin)
}
