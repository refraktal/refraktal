package harness

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseVersion(t *testing.T) {
	for in, want := range map[string]string{
		"2.1.325 (Claude Code)\n": "2.1.325",
		"codex-cli 0.169.0":       "0.169.0",
		"1.5.9":                   "1.5.9",
		"v22.1.0-rc.1":            "22.1.0-rc.1",
		"":                        "unknown",
		"no version here":         "unknown",
	} {
		if got := ParseVersion(in); got != want {
			t.Errorf("ParseVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func script(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// fakeNPM puts an npm on PATH that "installs" an adapter whose bundled binary
// prints a version, and a claude CLI that prints another.
func fakeNPM(t *testing.T) (bin string, calls string) {
	t.Helper()
	bin = t.TempDir()
	calls = filepath.Join(bin, "calls")
	script(t, bin, "npm", `echo "$@" >> `+calls+`
while [ "$1" != "--prefix" ]; do shift; done; p=$2
mkdir -p "$p/node_modules/.bin" "$p/node_modules/@anthropic-ai/claude-agent-sdk-test" "$p/node_modules/@openai/codex-test/vendor/x/bin"
printf '#!/bin/sh\n' > "$p/node_modules/.bin/claude-agent-acp"; chmod +x "$p/node_modules/.bin/claude-agent-acp"
printf '#!/bin/sh\n' > "$p/node_modules/.bin/codex-acp"; chmod +x "$p/node_modules/.bin/codex-acp"
cp '`+bin+`/fakecodex' "$p/node_modules/@openai/codex-test/vendor/x/bin/codex"
printf '#!/bin/sh\necho "2.1.323 (Claude Code)"\n' > "$p/node_modules/@anthropic-ai/claude-agent-sdk-test/claude"
chmod +x "$p/node_modules/@anthropic-ai/claude-agent-sdk-test/claude"
`)
	script(t, bin, "claude", `echo "2.1.325 (Claude Code)"`)
	// A bundled codex: --version, and mcp list (which must run with plugins off).
	script(t, bin, "fakecodex", `case "$*" in
  --version) echo "codex-cli 0.166.0" ;;
  "-c features.plugins=false mcp list --json") pwd > "`+bin+`/codex-cwd"
    echo '[{"name":"tools-a","enabled":true},{"name":"off-b","enabled":false},{"name":"tools-c","enabled":true}]' ;;
  *) echo "unexpected: $*" >&2; exit 1 ;;
esac`)
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	return bin, calls
}

func TestAdapterIsInstalledOnceAndVersionsAreRead(t *testing.T) {
	_, calls := fakeNPM(t)
	cache := t.TempDir()
	claude := Defaults(cache)[0]
	if claude.Name() != "claude" || claude.Mode() != "default" {
		t.Fatalf("claude facts: %s %s", claude.Name(), claude.Mode())
	}
	l, err := claude.Prepare(context.Background(), Options{Scratch: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if !l.AdapterInstalled || l.Adapter != "@agentclientprotocol/claude-agent-acp@0.107.0" ||
		l.Bundled != "2.1.323" || l.Installed != "2.1.325" || !strings.Contains(string(l.Meta), "strictMcpConfig") {
		t.Fatalf("launch %+v", l)
	}
	if _, err := os.Stat(l.Argv[0]); err != nil {
		t.Fatalf("adapter binary %s: %v", l.Argv[0], err)
	}
	if partial, _ := filepath.Glob(filepath.Join(cache, "*partial*")); len(partial) != 0 {
		t.Fatalf("partial install left behind: %v", partial)
	}
	b, _ := os.ReadFile(calls)
	if !strings.Contains(string(b), "--ignore-scripts") {
		t.Fatalf("npm called without --ignore-scripts: %s", b)
	}

	l, err = claude.Prepare(context.Background(), Options{Scratch: t.TempDir()})
	if err != nil || l.AdapterInstalled {
		t.Fatalf("second prepare reinstalled: %+v %v", l, err)
	}
	if b2, _ := os.ReadFile(calls); strings.Count(string(b2), "install") != 1 {
		t.Fatalf("npm ran %d times", strings.Count(string(b2), "install"))
	}
}

func TestMissingNPMIsReported(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := Defaults(t.TempDir())[1].Prepare(context.Background(), Options{Scratch: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "npm not found") {
		t.Fatalf("want npm not found, got %v", err)
	}
}

func TestAgyGetsAFreshGeminiDir(t *testing.T) {
	bin := t.TempDir()
	script(t, bin, "agy", `echo 1.5.9`)
	t.Setenv("PATH", bin)
	scratch := t.TempDir()
	l, err := Defaults("")[2].Prepare(context.Background(), Options{Scratch: scratch})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(bin, "agy"), "acp", "--gemini-dir", filepath.Join(scratch, "gemini-dir")}
	if strings.Join(l.Argv, " ") != strings.Join(want, " ") || l.Installed != "1.5.9" || l.Adapter != "" || l.MCP != MCPIsolated {
		t.Fatalf("launch %+v", l)
	}
	if fi, err := os.Stat(want[3]); err != nil || !fi.IsDir() {
		t.Fatal("gemini dir not created")
	}

	l, err = Defaults("")[2].Prepare(context.Background(), Options{Scratch: scratch, MCP: MCPInherit})
	if err != nil || len(l.Argv) != 2 || l.MCP != MCPInherit {
		t.Fatalf("inherit: %+v %v", l, err)
	}

	t.Setenv("PATH", t.TempDir())
	if _, err := Defaults("")[2].Prepare(context.Background(), Options{Scratch: scratch}); err == nil {
		t.Fatal("want an error when agy is not installed")
	}
}

func env(l Launch, name string) (string, bool) {
	for _, kv := range l.Env {
		if k, v, _ := strings.Cut(kv, "="); k == name {
			return v, true
		}
	}
	return "", false
}

func TestCodexIsolationIsBuiltFromItsOwnServerList(t *testing.T) {
	bin, _ := fakeNPM(t)
	cache, cwd, tmp := t.TempDir(), t.TempDir(), t.TempDir()
	codex := Defaults(cache)[1]
	l, err := codex.Prepare(context.Background(), Options{Scratch: t.TempDir(), Cwd: cwd, TmpDir: tmp})
	if err != nil {
		t.Fatal(err)
	}
	cfg, ok := env(l, "CODEX_CONFIG")
	want := `{"features":{"plugins":false},"mcp_servers":{"tools-a":{"enabled":false},"tools-c":{"enabled":false}}}`
	if !ok || cfg != want {
		t.Fatalf("CODEX_CONFIG = %s\nwant          %s", cfg, want)
	}
	if v, _ := env(l, "TMPDIR"); v != tmp || l.MCP != MCPIsolated || l.Bundled != "0.166.0" {
		t.Fatalf("launch %+v", l)
	}
	if got, _ := os.ReadFile(filepath.Join(bin, "codex-cwd")); resolved(t, strings.TrimSpace(string(got))) != resolved(t, cwd) {
		t.Fatalf("mcp list ran in %q, want the session folder %q", got, cwd)
	}

	l, err = codex.Prepare(context.Background(), Options{Scratch: t.TempDir(), Cwd: cwd, MCP: MCPInherit, Executable: "/opt/codex"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg, set := env(l, "CODEX_CONFIG"); set || l.MCP != MCPInherit {
		t.Fatalf("inherit: CODEX_CONFIG %q set, %+v", cfg, l)
	}
	if v, _ := env(l, "CODEX_PATH"); v != "/opt/codex" {
		t.Fatal("executable not passed as CODEX_PATH")
	}
}

func TestCodexIsolationFailureIsAnError(t *testing.T) {
	bin, _ := fakeNPM(t)
	script(t, bin, "fakecodex", `case "$*" in --version) echo 0.166.0 ;; *) exit 3 ;; esac`)
	_, err := Defaults(t.TempDir())[1].Prepare(context.Background(), Options{Scratch: t.TempDir(), Cwd: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "cannot isolate Codex") || !strings.Contains(err.Error(), "inherit") {
		t.Fatalf("want an isolation error naming the inherit option, got %v", err)
	}
}

func TestClaudeInheritDropsStrictMcpConfig(t *testing.T) {
	fakeNPM(t)
	l, err := Defaults(t.TempDir())[0].Prepare(context.Background(), Options{Scratch: t.TempDir(), MCP: MCPInherit})
	if err != nil || len(l.Meta) != 0 || l.MCP != MCPInherit {
		t.Fatalf("%+v %v", l, err)
	}
}

func resolved(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestProjectHooks(t *testing.T) {
	dir := t.TempDir()
	put := func(rel, body string) {
		os.MkdirAll(filepath.Join(dir, filepath.Dir(rel)), 0o755)
		os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644)
	}
	put(".claude/settings.json", `{"permissions":{},"hooks":{"PostToolUse":[{"hooks":[{"type":"command","command":"x"}]}]}}`)
	put(".claude/settings.local.json", `{"hooks":{}}`)
	put(".codex/config.toml", "model = \"x\"\n[hooks.post_tool_use]\ncommand = \"y\"\n")
	put(".gemini/settings.json", `{"theme":"dark"}`)
	if got := ProjectHooks(dir, "claude"); strings.Join(got, ",") != ".claude/settings.json" {
		t.Errorf("claude: %v", got)
	}
	if got := ProjectHooks(dir, "codex"); strings.Join(got, ",") != ".codex/config.toml" {
		t.Errorf("codex: %v", got)
	}
	if got := ProjectHooks(dir, "agy"); len(got) != 0 {
		t.Errorf("agy: %v", got)
	}
	put(".agents/hooks.json", `{}`)
	if got := ProjectHooks(dir, "agy"); strings.Join(got, ",") != ".agents/hooks.json" {
		t.Errorf("agy hooks.json: %v", got)
	}
}
