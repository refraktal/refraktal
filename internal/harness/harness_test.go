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
mkdir -p "$p/node_modules/.bin" "$p/node_modules/@anthropic-ai/claude-agent-sdk-test"
printf '#!/bin/sh\n' > "$p/node_modules/.bin/claude-agent-acp"; chmod +x "$p/node_modules/.bin/claude-agent-acp"
printf '#!/bin/sh\necho "2.1.323 (Claude Code)"\n' > "$p/node_modules/@anthropic-ai/claude-agent-sdk-test/claude"
chmod +x "$p/node_modules/@anthropic-ai/claude-agent-sdk-test/claude"
`)
	script(t, bin, "claude", `echo "2.1.325 (Claude Code)"`)
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	return bin, calls
}

func TestAdapterIsInstalledOnceAndVersionsAreRead(t *testing.T) {
	_, calls := fakeNPM(t)
	cache := t.TempDir()
	claude := Defaults(cache)[0]
	if claude.Name() != "claude" || claude.Mode() != "default" || claude.MCP() != MCPIsolated {
		t.Fatalf("claude facts: %s %s %s", claude.Name(), claude.Mode(), claude.MCP())
	}
	l, err := claude.Prepare(context.Background(), t.TempDir())
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

	l, err = claude.Prepare(context.Background(), t.TempDir())
	if err != nil || l.AdapterInstalled {
		t.Fatalf("second prepare reinstalled: %+v %v", l, err)
	}
	if b2, _ := os.ReadFile(calls); strings.Count(string(b2), "install") != 1 {
		t.Fatalf("npm ran %d times", strings.Count(string(b2), "install"))
	}
}

func TestMissingNPMIsReported(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := Defaults(t.TempDir())[1].Prepare(context.Background(), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "npm not found") {
		t.Fatalf("want npm not found, got %v", err)
	}
}

func TestAgyGetsAFreshGeminiDir(t *testing.T) {
	bin := t.TempDir()
	script(t, bin, "agy", `echo 1.5.9`)
	t.Setenv("PATH", bin)
	scratch := t.TempDir()
	l, err := Defaults("")[2].Prepare(context.Background(), scratch)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(bin, "agy"), "acp", "--gemini-dir", filepath.Join(scratch, "gemini-dir")}
	if strings.Join(l.Argv, " ") != strings.Join(want, " ") || l.Installed != "1.5.9" || l.Adapter != "" {
		t.Fatalf("launch %+v", l)
	}
	if fi, err := os.Stat(want[3]); err != nil || !fi.IsDir() {
		t.Fatal("gemini dir not created")
	}

	t.Setenv("PATH", t.TempDir())
	if _, err := Defaults("")[2].Prepare(context.Background(), scratch); err == nil {
		t.Fatal("want an error when agy is not installed")
	}
}
