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
	testWorktreeSetupDoesNotExecutePromisorRemote(t, "ext")
}

func TestWorktreeSetupDoesNotExecutePromisorRemoteWithoutNoLazyFetch(t *testing.T) {
	withoutNoLazyFetch(t)
	testWorktreeSetupDoesNotExecutePromisorRemote(t, "ext")
}

func TestWorktreeSetupDoesNotExecuteCustomPromisorRemoteWithoutNoLazyFetch(t *testing.T) {
	t.Setenv("GIT_ALLOW_PROTOCOL", "ext:refraktaltest")
	withoutNoLazyFetch(t)
	for _, remote := range []struct {
		name, protocol string
	}{
		{"named", "refraktaltest"},
		{"empty", ""},
		{"slash-alias", "/"},
	} {
		t.Run(remote.name, func(t *testing.T) {
			testWorktreeSetupDoesNotExecutePromisorRemote(t, remote.protocol)
		})
	}
}

// withoutNoLazyFetch simulates Git without lazy-fetch suppression by removing
// the environment variable just before the real Git starts. The seam exists
// only in tests; safegit must always set GIT_NO_LAZY_FETCH in production.
func withoutNoLazyFetch(t *testing.T) {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	body := "#!/bin/sh\nunset GIT_NO_LAZY_FETCH\nexec '" + strings.ReplaceAll(git, "'", "'\\''") + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func testWorktreeSetupDoesNotExecutePromisorRemote(t *testing.T, protocol string) {
	t.Helper()
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
	remoteURL := "ext::/bin/sh " + helper
	if protocol == "/" {
		remoteURL = "fixture"
	} else if protocol != "ext" {
		bin := t.TempDir()
		helper = filepath.Join(bin, "git-remote-"+protocol)
		remoteURL = protocol + "::fixture"
		t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	helperScript := []byte("#!/bin/sh\ntouch '" + marker + "'\nexit 1\n")
	if err := os.WriteFile(helper, helperScript, 0700); err != nil {
		t.Fatal(err)
	}
	promisorGit(t, dir, "config", "remote.origin.url", remoteURL)
	promisorGit(t, dir, "config", "remote.origin.promisor", "true")
	promisorGit(t, dir, "config", "remote.origin.partialclonefilter", "blob:none")
	policy := "protocol.allow"
	if protocol != "" {
		policy = "protocol." + protocol + ".allow"
	}
	promisorGit(t, dir, "config", policy, "always")
	if protocol == "/" {
		// An empty alias override can fall through to this executable. The
		// safe self-alias must reject recursion before dispatching anything.
		bin := t.TempDir()
		if err := os.WriteFile(filepath.Join(bin, "git-"), helperScript, 0700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		promisorGit(t, dir, "config", "remote.origin.vcs", protocol)
		alias := "!'" + strings.ReplaceAll(helper, "'", "'\\''") + "'"
		promisorGit(t, dir, "config", "alias.remote-/.command", alias)
		promisorGit(t, dir, "config", "alias.remote-", alias)
		promisorGit(t, dir, "config", "help.autocorrect", "-1")
	}
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
	message := err.Error()
	if !strings.HasPrefix(message, "git reset --hard --quiet HEAD:") {
		t.Fatalf("worktree setup failed before checkout: %v", err)
	}
	// The lazy-fetch subprocess can reject the transport before reset writes
	// the requested OID to its stdin. That gives reset SIGPIPE instead of a
	// missing-object diagnostic; both failures must leave the helper unrun.
	transportDenied := strings.Contains(message, "fatal: transport '") && strings.Contains(message, "' not allowed")
	if !strings.Contains(message, blob) && !transportDenied {
		t.Fatalf("checkout error neither names missing blob %s nor denies transport: %v", blob, err)
	}
}
