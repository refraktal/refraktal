package safegit_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"refraktal.dev/refraktal/internal/safegit"
	"refraktal.dev/refraktal/internal/worktree"
)

func promisorGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(safegit.Env(), "GIT_AUTHOR_NAME=Repro", "GIT_AUTHOR_EMAIL=repro@localhost", "GIT_COMMITTER_NAME=Repro", "GIT_COMMITTER_EMAIL=repro@localhost")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fixture git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestWorktreeSetupDoesNotExecutePromisorRemote(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	promisorGit(t, dir, "init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "data.txt"), []byte("fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	promisorGit(t, dir, "add", "data.txt")
	promisorGit(t, dir, "-c", "commit.gpgSign=false", "commit", "-qm", "fixture")
	blob := promisorGit(t, dir, "rev-parse", "HEAD:data.txt")
	marker := filepath.Join(dir, "REMOTE-RAN")
	helper := filepath.Join(dir, "helper.sh")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\ntouch '"+marker+"'\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	promisorGit(t, dir, "config", "remote.origin.url", "ext::/bin/sh "+helper)
	promisorGit(t, dir, "config", "remote.origin.promisor", "true")
	promisorGit(t, dir, "config", "remote.origin.partialclonefilter", "blob:none")
	promisorGit(t, dir, "config", "protocol.ext.allow", "always")
	if err := os.Remove(filepath.Join(dir, ".git", "objects", blob[:2], blob[2:])); err != nil {
		t.Fatal(err)
	}
	_, err := worktree.CreateAt(context.Background(), dir, filepath.Join(t.TempDir(), "child"), "refraktal/repro", "HEAD")
	_, statErr := os.Stat(marker)
	t.Logf("git_failed=%t configured_remote_executed=%t", err != nil, statErr == nil)
	if !os.IsNotExist(statErr) {
		t.Fatalf("worktree setup executed the repository-configured remote helper while reading an object: %v", statErr)
	}
	if err == nil {
		t.Fatal("worktree setup succeeded despite the missing promised blob")
	}
	if !strings.Contains(err.Error(), blob) {
		t.Fatalf("missing-object error does not name the missing blob %s: %v", blob, err)
	}
}
