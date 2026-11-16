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

func TestChecksAndJudge(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	os.MkdirAll(repo, 0o755)
	p := filepath.Join(dir, "config.json")
	os.WriteFile(p, []byte(`{"checks": {"`+repo+`": {"test": ["make", "test"], "timeout_mins": 2}}, "judge": {"margin": 0.2}}`), 0o600)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if ch, ok := c.CheckFor(repo); !ok || ch.Test[1] != "test" || ch.TimeoutMins != 2 {
		t.Fatalf("check %+v %v", ch, ok)
	}
	if _, ok := c.CheckFor(dir); ok {
		t.Fatal("a check for another folder matched")
	}
	if c.MarginOr() != 0.2 || (Config{}).MarginOr() != DefaultMargin {
		t.Fatalf("margin %v", c.MarginOr())
	}
	for _, bad := range []string{
		`{"checks": {"` + repo + `": {}}}`,
		`{"checks": {"relative/path": {"test": ["x"]}}}`,
		`{"checks": {"` + repo + `": {"test": ["x"], "timeout_mins": -1}}}`,
		`{"judge": {"margin": 1.5}}`,
	} {
		os.WriteFile(p, []byte(bad), 0o600)
		if _, err := Load(p); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestPassEnv(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(p, []byte(`{"harnesses": {"codex": {"pass_env": ["OPENAI_API_KEY"]}}}`), 0o600)
	if c, err := Load(p); err != nil || c.For("codex").PassEnv[0] != "OPENAI_API_KEY" {
		t.Fatalf("%+v %v", c, err)
	}
	os.WriteFile(p, []byte(`{"harnesses": {"codex": {"pass_env": ["AWS_SECRET_ACCESS_KEY"]}}}`), 0o600)
	if _, err := Load(p); err == nil {
		t.Fatal("pass_env accepted a variable that is not a provider key")
	}
}
