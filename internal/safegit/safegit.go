// Package safegit runs the git commands Refraktal makes outside
// internal/worktree: with GIT_* variables and API keys stripped from the
// environment, and with fsmonitor and hooks off, so a repository's config
// cannot run a command and no key reaches git.
package safegit

import (
	"context"
	"os"
	"os/exec"
	"strings"

	"refraktal.dev/refraktal/internal/proc"
)

// Env is the environment for a git command: the scrubbed parent environment
// (no session markers, no API keys) without GIT_* variables.
func Env() []string {
	var out []string
	for _, kv := range proc.ScrubEnv(os.Environ()) {
		if !strings.HasPrefix(kv, "GIT_") {
			out = append(out, kv)
		}
	}
	return out
}

// Command is git -C dir with fsmonitor and hooks off, in Env.
func Command(ctx context.Context, dir string, args ...string) *exec.Cmd {
	full := append([]string{"-C", dir, "-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null"}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = Env()
	return cmd
}
