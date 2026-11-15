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
	MCPIsolated = "isolated" // the user's MCP servers and plugins are hidden per run
	MCPInherit  = "inherit"  // the user's setup is used, by choice (config)
)

// Harness is one harness Refraktal can drive.
type Harness interface {
	Name() string
	Mode() string // the permission mode set on every session (docs/DESIGN.md)
	// Prepare finds or installs what is needed to launch the harness.
	Prepare(ctx context.Context, o Options) (Launch, error)
}

// Options say how to prepare a harness for one session.
type Options struct {
	Scratch    string // a folder inside the run folder the harness may use
	Cwd        string // the session folder
	MCP        string // "isolated" or "inherit" (config.MCPIsolated, config.MCPInherit)
	Executable string // optional: the binary to run (see config.Harness.Executable)
	TmpDir     string // optional: a private TMPDIR for the child (used for Codex)
}

func (o Options) inherit() bool { return o.MCP == MCPInherit }

// Launch is how to start a prepared harness, and what was found on the way.
type Launch struct {
	Argv []string
	Env  []string
	Meta json.RawMessage // sent as _meta on session/new
	MCP  string          // the isolation state in effect: isolated or inherit
	// ShellPrefixVar names the variable the harness calls to wrap each shell
	// command (Claude: CLAUDE_CODE_SHELL_PREFIX), or "" if it has none.
	ShellPrefixVar string

	Adapter          string // pinned adapter package@version, or "" for a native ACP harness
	AdapterInstalled bool   // the adapter was installed into the cache just now
	Bundled          string // version of the harness binary the adapter bundles, or ""
	Installed        string // version of the CLI on PATH, or "" if not installed
}

// Defaults returns the v1 harnesses: Claude Code, Codex and Antigravity.
// Adapters are installed under cacheDir.
func Defaults(cacheDir string) []Harness {
	return []Harness{
		&adapted{name: "claude", mode: "default", cli: "claude", exeEnv: "CLAUDE_CODE_EXECUTABLE",
			pkg: "@agentclientprotocol/claude-agent-acp", version: "0.107.0", bin: "claude-agent-acp",
			bundled: "@anthropic-ai/claude-agent-sdk-*/claude", cache: cacheDir, isolate: isolateClaude},
		&adapted{name: "codex", mode: "workspace-write", cli: "codex", exeEnv: "CODEX_PATH",
			pkg: "@agentclientprotocol/codex-acp", version: "2.6.0", bin: "codex-acp",
			bundled: "@openai/codex-*/vendor/*/bin/codex", cache: cacheDir, isolate: configureCodex},
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
	name, mode   string
	cli          string // the harness's own CLI, for the installed version
	exeEnv       string // the adapter's variable for running another harness binary
	pkg, version string
	bin          string // the adapter's executable in node_modules/.bin
	bundled      string // glob, under node_modules, for the bundled harness binary
	cache        string
	// isolate adds the per-run settings to l: MCP isolation (unless o says
	// inherit) and anything else the harness needs. harnessBin is the harness
	// binary the adapter will run.
	isolate func(ctx context.Context, o Options, harnessBin string, l *Launch) error
}

func (a *adapted) Name() string { return a.name }
func (a *adapted) Mode() string { return a.mode }

func (a *adapted) Prepare(ctx context.Context, o Options) (Launch, error) {
	l := Launch{Adapter: a.pkg + "@" + a.version, Installed: installedVersion(ctx, a.cli), MCP: MCPIsolated}
	if o.inherit() {
		l.MCP = MCPInherit
	}
	dir := filepath.Join(a.cache, strings.NewReplacer("@", "", "/", "-").Replace(a.pkg)+"-"+a.version)
	bin := filepath.Join(dir, "node_modules", ".bin", a.bin)
	if _, err := os.Stat(bin); err != nil {
		if err := install(ctx, dir, l.Adapter); err != nil {
			return l, err
		}
		l.AdapterInstalled = true
	}
	l.Argv = []string{bin}
	harnessBin := ""
	if m, _ := filepath.Glob(filepath.Join(dir, "node_modules", a.bundled)); len(m) > 0 {
		harnessBin = m[0]
		l.Bundled = Version(ctx, harnessBin)
	}
	if o.Executable != "" {
		harnessBin = o.Executable
		l.Env = append(l.Env, a.exeEnv+"="+o.Executable)
	}
	if err := a.isolate(ctx, o, harnessBin, &l); err != nil {
		return l, err
	}
	return l, nil
}

// isolateClaude hides the user's MCP servers and plugins with the Agent SDK's
// strictMcpConfig option. Project settings (CLAUDE.md, .claude/settings.json)
// still load: the adapter asks for user, project and local setting sources.
func isolateClaude(_ context.Context, o Options, _ string, l *Launch) error {
	l.ShellPrefixVar = "CLAUDE_CODE_SHELL_PREFIX"
	if !o.inherit() {
		l.Meta = json.RawMessage(`{"claudeCode":{"options":{"strictMcpConfig":true}}}`)
	}
	return nil
}

// configureCodex sets the child's private TMPDIR, which Codex uses as its
// writable temp root instead of the user's, and builds CODEX_CONFIG, which
// codex-acp merges into each session's config.
//
// /tmp stays writable: codex-acp 2.6.0 sends its own workspace-write sandbox
// policy with excludeSlashTmp false on every turn, which overrides
// sandbox_workspace_write.exclude_slash_tmp in the config (M3 open test).
//
// For MCP isolation it turns plugins off and disables, by name, every enabled
// MCP server Codex lists from its own config for this folder. The list comes
// from Codex itself (`codex mcp list --json`), so Refraktal never reads or
// copies the user's config files. A partial entry ({"enabled": false})
// merges with the server's own definition; an empty mcp_servers table does
// not remove anything.
func configureCodex(ctx context.Context, o Options, codex string, l *Launch) error {
	if o.TmpDir != "" {
		l.Env = append(l.Env, "TMPDIR="+o.TmpDir)
	}
	if o.inherit() {
		return nil
	}
	cfg := map[string]any{}
	{
		names, err := codexMCPServers(ctx, codex, o.Cwd)
		if err != nil {
			return fmt.Errorf("cannot isolate Codex from your MCP servers (set harnesses.codex.mcp to %q to run without isolation): %w", MCPInherit, err)
		}
		servers := map[string]any{}
		for _, n := range names {
			servers[n] = map[string]any{"enabled": false}
		}
		cfg["features"] = map[string]any{"plugins": false}
		if len(servers) > 0 {
			cfg["mcp_servers"] = servers
		}
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	l.Env = append(l.Env, "CODEX_CONFIG="+string(b))
	return nil
}

// codexMCPServers lists the enabled MCP servers Codex would start in cwd, as
// configured (plugins off, so plugin-provided servers are not listed: those
// go with the plugins).
func codexMCPServers(ctx context.Context, codex, cwd string) ([]string, error) {
	if codex == "" {
		return nil, errors.New("no Codex binary found in the adapter")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, codex, "-c", "features.plugins=false", "mcp", "list", "--json")
	cmd.Dir = cwd
	cmd.Env = proc.ScrubEnv(os.Environ())
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("codex mcp list: %w", err)
	}
	var list []struct {
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("codex mcp list: %w", err)
	}
	var names []string
	for _, s := range list {
		if s.Enabled && s.Name != "" {
			names = append(names, s.Name)
		}
	}
	return names, nil
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

// Prepare isolates Antigravity with a fresh --gemini-dir inside the scratch
// folder. That hides all of ~/.gemini (MCP servers, settings, history) for
// this run only.
func (agy) Prepare(ctx context.Context, o Options) (Launch, error) {
	bin := o.Executable
	if bin == "" {
		var err error
		if bin, err = exec.LookPath("agy"); err != nil {
			return Launch{}, errors.New("agy not installed")
		}
	}
	l := Launch{Installed: Version(ctx, bin), Argv: []string{bin, "acp"}, MCP: MCPInherit}
	if !o.inherit() {
		dir := filepath.Join(o.Scratch, "gemini-dir")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return l, err
		}
		l.Argv = append(l.Argv, "--gemini-dir", dir)
		l.MCP = MCPIsolated
	}
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

// PriceKeys are the price-table keys to try, in order, for a harness's model.
// LiteLLM lists OpenAI models without a prefix and Gemini models under
// "gemini/". Claude reports its own cost, so its key matters only for
// estimates.
func PriceKeys(harness, model string) []string {
	if model == "" {
		return nil
	}
	switch harness {
	case "agy":
		return []string{"gemini/" + model, model}
	case "codex":
		return []string{model, "openai/" + model}
	case "claude":
		return []string{model, "anthropic/" + model}
	}
	return []string{model}
}
