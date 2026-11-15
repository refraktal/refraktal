package safegit

import (
	"os"
	"path/filepath"
	"testing"
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
