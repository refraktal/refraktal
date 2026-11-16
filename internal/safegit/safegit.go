// Package safegit runs Refraktal's git commands with a sanitized environment,
// static protections and, when needed, per-command driver neutralisation.
package safegit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"refraktal.dev/refraktal/internal/proc"
)

// Timeout bounds short git queries such as rev-parse and ls-files.
const Timeout = 10 * time.Second

// OperationTimeout bounds worktree operations (collect, apply, remove), which
// scale with repository size and must not fail a finished turn on a large one.
const OperationTimeout = 5 * time.Minute

// Env is the environment for a git command: the scrubbed parent environment
// (no session markers, no API keys) without GIT_* variables. The one exception
// is GIT_CONFIG_NOSYSTEM, which only removes a config source and cannot
// redirect a repository, index or config file.
func Env() []string {
	var out []string
	for _, kv := range proc.ScrubEnv(os.Environ()) {
		if !strings.HasPrefix(kv, "GIT_") || strings.HasPrefix(kv, "GIT_CONFIG_NOSYSTEM=") {
			out = append(out, kv)
		}
	}
	return out
}

// safe are the options every Refraktal git command runs with.
// --no-replace-objects: a child shares the repository's .git and could add
// replacement refs (git replace <base> <stub>) that make Refraktal read a
// different commit, tree or file than the one it pinned.
// protocol.allow=never and GIT_NO_LAZY_FETCH=1 keep missing promised objects
// from invoking a repository-configured transport. These commands need only
// the local object database; a missing object must fail instead of fetching.
var safe = []string{"--no-replace-objects", "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "diff.external=",
	"-c", "core.pager=cat", "-c", "core.sshCommand=false",
	"-c", "commit.gpgSign=false", "-c", "tag.gpgSign=false", "-c", "protocol.allow=never"}

func command(ctx context.Context, dir, gitDir string, args ...string) *exec.Cmd {
	full := []string{"-C", dir}
	if gitDir != "" {
		full = append(full, "--git-dir="+gitDir, "--work-tree="+dir)
	}
	full = append(append(full, safe...), args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = append(Env(), "GIT_TERMINAL_PROMPT=0", "GIT_NO_REPLACE_OBJECTS=1", "GIT_NO_LAZY_FETCH=1",
		"GIT_AUTHOR_NAME=Refraktal", "GIT_AUTHOR_EMAIL=refraktal@localhost",
		"GIT_COMMITTER_NAME=Refraktal", "GIT_COMMITTER_EMAIL=refraktal@localhost")
	return cmd
}

// Cmd is a protected git command. Output performs driver discovery and its
// before/after re-check when the command can run filters or drivers.
type Cmd struct {
	ctx  context.Context
	dir  string
	args []string
}

// Command prepares git -C dir in Env. Output bounds it by Timeout or ctx's
// earlier deadline. rev-parse, ls-files and check-attr cannot run a filter or
// driver, so these queries need only the static protections.
func Command(ctx context.Context, dir string, args ...string) *Cmd {
	return &Cmd{ctx: ctx, dir: dir, args: slices.Clone(args)}
}

// Output runs the command and returns its stdout.
func (c *Cmd) Output() ([]byte, error) {
	ctx, cancel := context.WithTimeout(c.ctx, Timeout)
	defer cancel()
	g := Operation{Dir: c.dir}
	args := c.args
	if len(c.args) > 0 {
		switch c.args[0] {
		case "rev-parse", "ls-files", "check-attr":
			return g.output(ctx, nil, c.args...)
		case "diff", "show", "log":
			// Like worktree's patch commands, disable attribute-selected
			// diff programs and textconv, including ones added mid-command.
			args = append([]string{c.args[0], "--no-ext-diff", "--no-textconv"}, c.args[1:]...)
		}
	}
	return g.Run(ctx, nil, args...)
}

// Operation retains a driver snapshot for one operation in one repository.
// Never reuse it across operations, or across a branch/path change.
// Callers must bound ctx. GitDir pins a linked worktree's administrative path;
// Check, if set, validates its gitfile before discovery and around each command.
type Operation struct {
	Dir, GitDir   string
	SkipLFSSmudge bool // only Collect and Apply, after a child may edit .lfsconfig
	Check         func() error
	drivers       map[string][]driverValue
	discovered    bool
}

func (g *Operation) check() error {
	if g.Check != nil {
		return g.Check()
	}
	return nil
}

// Run executes a command with driver overrides and a before/after re-check.
func (g *Operation) Run(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	if err := g.check(); err != nil {
		return nil, err
	}
	if !g.discovered {
		var err error
		g.drivers, err = g.discoverDrivers(ctx)
		if err != nil {
			return nil, err
		}
	}
	before := g.drivers
	// Invalidate before executing. Only a successful, compared post-command
	// snapshot may serve as the next command's pre-command snapshot.
	g.discovered = false
	out, commandErr := g.output(ctx, input, append(driverOverrides(before), args...)...)
	commandErr = errors.Join(commandErr, g.check())
	after, err := g.discoverDrivers(ctx)
	if err != nil {
		return out, errors.Join(commandErr, fmt.Errorf("worktree: re-check driver configuration: %w", err))
	}
	if !maps.EqualFunc(before, after, slices.Equal[[]driverValue]) {
		return out, errors.Join(commandErr, errors.New("worktree: driver configuration changed during git command"))
	}
	g.drivers, g.discovered = after, commandErr == nil
	return out, commandErr
}

// Preserve the distinction between a bare boolean (true) and an empty one (false).
type driverValue struct {
	text     string
	explicit bool
}

// Snapshot keys and all their values for the before/after comparison. Discovery
// cannot invoke a driver and uses the same environment and static protections.
// No --local restriction: includes and all scopes count, as do duplicate values.
func (g *Operation) discoverDrivers(ctx context.Context) (map[string][]driverValue, error) {
	out, err := g.output(ctx, nil, "config", "--null", "--get-regexp", `^(filter\..*|merge\..*\.driver|lfs\..*)$`)
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 && len(out) == 0 {
			return nil, nil // No configured drivers.
		}
		return nil, err
	}
	drivers := map[string][]driverValue{}
	for _, record := range strings.Split(string(out), "\x00") {
		if record == "" {
			continue
		}
		key, value, explicit := strings.Cut(record, "\n")
		if strings.Contains(key, "=") {
			return nil, errors.New("worktree: cannot safely override a driver whose config key contains '='")
		}
		drivers[key] = append(drivers[key], driverValue{value, explicit})
	}
	return drivers, nil
}

func driverOverrides(drivers map[string][]driverValue) []string {
	filters, merges := map[string]bool{}, map[string]bool{}
	for key := range drivers {
		if strings.HasPrefix(key, "filter.") {
			if i := strings.LastIndexByte(key, '.'); i > len("filter.") {
				filters[key[:i]] = true
			}
		} else if strings.HasPrefix(key, "merge.") && strings.HasSuffix(key, ".driver") {
			merges[key] = true
		}
	}
	var keys []string
	for name := range filters {
		if name == "filter.lfs" && standardLFS(drivers) {
			for kind, command := range lfsCommands {
				keys = append(keys, name+"."+kind+"="+command)
			}
			continue
		}
		// Empty strings disable these programs, rather than invoking cat.
		keys = append(keys, name+".clean=", name+".smudge=", name+".process=", name+".required=false")
	}
	for key := range merges {
		// An empty merge command runs no program and fails closed with a
		// conflict if a three-way merge ever reaches it.
		keys = append(keys, key+"=")
	}
	slices.Sort(keys)
	var overrides []string
	for _, key := range keys {
		overrides = append(overrides, "-c", key)
	}
	return overrides
}

// Pin allowed commands so a config change cannot replace them during execution.
var lfsCommands = map[string]string{
	"clean": "git-lfs clean -- %f", "smudge": "git-lfs smudge -- %f",
	"process": "git-lfs filter-process",
}

// Only the standard Git LFS commands may run, without extensions or custom
// transfers. Extra values disqualify the driver even if a later entry masks
// them. Preserve required only when all three commands are allowed.
func standardLFS(drivers map[string][]driverValue) bool {
	for key := range drivers {
		if strings.HasPrefix(key, "lfs.extension.") || strings.HasPrefix(key, "lfs.customtransfer.") || key == "lfs.standalonetransferagent" {
			return false
		}
	}
	// git lfs install in more than one scope (system, global, local) repeats
	// the same values; git uses the last. Accept repeats only when every value
	// is exactly the standard command.
	for kind, command := range lfsCommands {
		values := drivers["filter.lfs."+kind]
		if len(values) == 0 {
			return false
		}
		for _, v := range values {
			if v != (driverValue{command, true}) {
				return false
			}
		}
	}
	return true
}

func (g *Operation) output(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	cmd := command(ctx, g.Dir, g.GitDir, args...)
	if g.SkipLFSSmudge {
		cmd.Env = append(cmd.Env, "GIT_LFS_SKIP_SMUDGE=1")
	}
	if input != nil {
		cmd.Stdin = bytes.NewReader(input)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}
