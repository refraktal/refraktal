// Package harness holds what Refraktal knows about each coding-agent harness:
// how to launch it over ACP, the pinned adapter, the permission mode, MCP
// isolation, and how to read the bundled and installed versions.
package harness

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"refraktal.dev/refraktal/internal/proc"
)

// MCP isolation states, as doctor and the run log report them.
const (
	MCPIsolated    = "isolated"    // the user's MCP servers and plugins are hidden per run
	MCPInherit     = "inherit"     // the user's setup is used, by choice
	MCPUnavailable = "unavailable" // no per-run mechanism: the user's setup is used
)

// Harness is one harness Refraktal can drive.
type Harness interface {
	Name() string
	Mode() string // the permission mode set on every session (docs/DESIGN.md)
	MCP() string  // the MCP isolation state with default settings
	// Prepare finds or installs what is needed to launch the harness. scratch
	// is a folder inside the run folder that the harness may use.
	Prepare(ctx context.Context, scratch string) (Launch, error)
}

// Launch is how to start a prepared harness, and what was found on the way.
type Launch struct {
	Argv []string
	Env  []string
	Meta json.RawMessage // sent as _meta on session/new

	Adapter          string // pinned adapter package@version, or "" for a native ACP harness
	AdapterInstalled bool   // the adapter was installed into the cache just now
	Bundled          string // version of the harness binary the adapter bundles, or ""
	Installed        string // version of the CLI on PATH, or "" if not installed
}

// Defaults returns the v1 harnesses: Claude Code, Codex and Antigravity.
// Adapters are installed under cacheDir.
func Defaults(cacheDir string) []Harness {
	return []Harness{
		&adapted{name: "claude", mode: "default", mcp: MCPIsolated, cli: "claude",
			pkg: "@agentclientprotocol/claude-agent-acp", version: "0.107.0", bin: "claude-agent-acp",
			bundled: "@anthropic-ai/claude-agent-sdk-*/claude", cache: cacheDir,
			meta: json.RawMessage(`{"claudeCode":{"options":{"strictMcpConfig":true}}}`)},
		&adapted{name: "codex", mode: "workspace-write", mcp: MCPUnavailable, cli: "codex",
			pkg: "@agentclientprotocol/codex-acp", version: "2.6.0", bin: "codex-acp",
			bundled: "@openai/codex-*/vendor/*/bin/codex", cache: cacheDir},
		agy{},
	}
}

// DefaultCacheDir is os.UserCacheDir()/refraktal/adapters.
func DefaultCacheDir() (string, error) {
	d, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "refraktal", "adapters"), nil
}

// adapted is a harness driven through a pinned npm ACP adapter that bundles
// its own harness binary.
type adapted struct {
	name, mode, mcp string
	cli             string // the harness's own CLI, for the installed version
	pkg, version    string
	bin             string // the adapter's executable in node_modules/.bin
	bundled         string // glob, under node_modules, for the bundled harness binary
	cache           string
	meta            json.RawMessage
}

func (a *adapted) Name() string { return a.name }
func (a *adapted) Mode() string { return a.mode }
func (a *adapted) MCP() string  { return a.mcp }

func (a *adapted) Prepare(ctx context.Context, _ string) (Launch, error) {
	l := Launch{Adapter: a.pkg + "@" + a.version, Meta: a.meta, Installed: installedVersion(ctx, a.cli)}
	dir := filepath.Join(a.cache, strings.NewReplacer("@", "", "/", "-").Replace(a.pkg)+"-"+a.version)
	bin := filepath.Join(dir, "node_modules", ".bin", a.bin)
	if _, err := os.Stat(bin); err != nil {
		if err := install(ctx, dir, l.Adapter); err != nil {
			return l, err
		}
		l.AdapterInstalled = true
	}
	l.Argv = []string{bin}
	if m, _ := filepath.Glob(filepath.Join(dir, "node_modules", a.bundled)); len(m) > 0 {
		l.Bundled = Version(ctx, m[0])
	}
	return l, nil
}

// install installs pkg into dir with npm, without running install scripts.
// It installs into a fresh folder and renames it into place, so a failed
// install never leaves a half-filled dir behind.
func install(ctx context.Context, dir, pkg string) error {
	npm, err := exec.LookPath("npm")
	if err != nil {
		return errors.New("npm not found: it is needed to install the pinned ACP adapter")
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	var r [4]byte
	rand.Read(r[:])
	tmp := dir + ".partial-" + hex.EncodeToString(r[:])
	defer os.RemoveAll(tmp)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, npm, "install", "--prefix", tmp, "--ignore-scripts", "--no-audit", "--no-fund", pkg)
	cmd.Env = proc.ScrubEnv(os.Environ())
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("npm install %s: %v: %s", pkg, err, lastLine(out))
	}
	if err := os.Rename(tmp, dir); err != nil && !errors.Is(err, os.ErrExist) {
		if _, statErr := os.Stat(dir); statErr != nil { // not just a concurrent install that won
			return err
		}
	}
	return nil
}

func lastLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return lines[len(lines)-1]
}

// agy is Antigravity, which speaks ACP natively.
type agy struct{}

func (agy) Name() string { return "agy" }
func (agy) Mode() string { return "default" }
func (agy) MCP() string  { return MCPIsolated }

// Prepare isolates Antigravity with a fresh --gemini-dir inside scratch. That
// hides all of ~/.gemini (MCP servers, settings, history) for this run only.
func (agy) Prepare(ctx context.Context, scratch string) (Launch, error) {
	l := Launch{Installed: installedVersion(ctx, "agy")}
	bin, err := exec.LookPath("agy")
	if err != nil {
		return l, errors.New("agy not installed")
	}
	dir := filepath.Join(scratch, "gemini-dir")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return l, err
	}
	l.Argv = []string{bin, "acp", "--gemini-dir", dir}
	return l, nil
}

func installedVersion(ctx context.Context, cli string) string {
	bin, err := exec.LookPath(cli)
	if err != nil {
		return ""
	}
	return Version(ctx, bin)
}

var semver = regexp.MustCompile(`\d+\.\d+\.\d+[0-9A-Za-z.+-]*`)

// Version runs bin --version and returns the first version number it prints,
// or "unknown".
func Version(ctx context.Context, bin string) string {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--version")
	cmd.Env = proc.ScrubEnv(os.Environ())
	out, _ := cmd.Output()
	return ParseVersion(string(out))
}

// ParseVersion returns the first version number in s, or "unknown".
func ParseVersion(s string) string {
	if v := semver.FindString(s); v != "" {
		return v
	}
	return "unknown"
}
