package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	c, err := Load(filepath.Join(dir, "missing.json"))
	if err != nil || c.For("codex").MCP != MCPIsolated {
		t.Fatalf("missing file: %+v %v", c, err)
	}

	write := func(body string) string {
		p := filepath.Join(dir, "config.json")
		os.WriteFile(p, []byte(body), 0o600)
		return p
	}
	c, err = Load(write(`{"harnesses":{"codex":{"mcp":"inherit"},"claude":{"executable":"/opt/claude"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.For("codex").MCP != MCPInherit || c.For("claude").MCP != MCPIsolated || c.For("claude").Executable != "/opt/claude" {
		t.Fatalf("%+v", c)
	}

	for body, want := range map[string]string{
		`{"harnesses":{"codex":{"mcp":"off"}}}`:     "harnesses.codex.mcp",
		`{"harnesses":{"gemini":{}}}`:               "unknown harness",
		`{"harnesses":{"codex":{"mpc":"inherit"}}}`: "unknown field",
		`{"harness":{}}`:                            "unknown field",
		`{`:                                         "unexpected EOF",
	} {
		if _, err := Load(write(body)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want error containing %q, got %v", body, want, err)
		}
	}
}
