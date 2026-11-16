// Package config reads Refraktal's config file: JSON, at
// os.UserConfigDir()/refraktal/config.json. A missing file means defaults.
//
//	{
//	  "harnesses": {
//	    "codex":  {"mcp": "inherit"},
//	    "claude": {"executable": "/usr/local/bin/claude"}
//	  }
//	}
//
// Only the user's own config folder is read. A repository cannot change how
// Refraktal isolates its children.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// MCP settings for a harness.
const (
	MCPIsolated = "isolated" // the default: hide the user's MCP servers and plugins for each run
	MCPInherit  = "inherit"  // use the user's own setup
)

// Config is the whole file.
type Config struct {
	Harnesses map[string]Harness `json:"harnesses,omitempty"`
	// Prices overrides the price table for a model, in US dollars per million
	// tokens: {"gpt-6.1-sol": {"input": 2, "output": 10, "cache_read": 0.1}}.
	Prices map[string]Price `json:"prices,omitempty"`
	// Checks are the commands judging runs in each candidate, per repository
	// path ("~/" allowed): {"~/src/app": {"test": ["make", "test"]}}.
	Checks map[string]Check `json:"checks,omitempty"`
	Judge  Judge            `json:"judge"`
}

// Check is one repository's check commands, as argv lists (no shell).
type Check struct {
	Build       []string `json:"build,omitempty"`
	Test        []string `json:"test,omitempty"`
	TimeoutMins float64  `json:"timeout_mins,omitempty"` // per command; default DefaultCheckMins
}

// ProviderKeys are the API keys no harness receives unless its pass_env
// names them.
var ProviderKeys = []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY"}

// DefaultCheckMins is the per-check time limit when none is set.
const DefaultCheckMins = 10

// Judge configures judging.
type Judge struct {
	// Margin: the top two pre-scores within this are a close call, for the
	// harness judge. nil means DefaultMargin.
	Margin *float64 `json:"margin,omitempty"`
}

// DefaultMargin is the close-call margin when none is set.
const DefaultMargin = 0.10

// MarginOr returns the configured margin, or DefaultMargin.
func (c Config) MarginOr() float64 {
	if c.Judge.Margin != nil {
		return *c.Judge.Margin
	}
	return DefaultMargin
}

// CheckFor returns the check configured for the repository at root, if any.
// Keys are compared after expanding "~/" and resolving symlinks.
func (c Config) CheckFor(root string) (Check, bool) {
	want := canon(root)
	for k, v := range c.Checks {
		if canon(expandHome(k)) == want {
			return v, true
		}
	}
	return Check{}, false
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}

func canon(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// Price is a per-model price override, in US dollars per million tokens.
type Price struct {
	Input      float64  `json:"input"`
	Output     float64  `json:"output"`
	CacheRead  *float64 `json:"cache_read,omitempty"`  // absent: priced as input; 0 is a real price
	CacheWrite *float64 `json:"cache_write,omitempty"` // absent: priced as input; 0 is a real price
}

// Harness holds the settings for one harness.
type Harness struct {
	// MCP is "isolated" (default) or "inherit".
	MCP string `json:"mcp,omitempty"`
	// Executable points the harness at a binary: the CLI an adapter should
	// run instead of its bundled one (CLAUDE_CODE_EXECUTABLE, CODEX_PATH),
	// or the agy binary.
	Executable string `json:"executable,omitempty"`
	// PassEnv names provider API keys this harness may receive from the
	// environment (OPENAI_API_KEY, ANTHROPIC_API_KEY, GEMINI_API_KEY,
	// GOOGLE_API_KEY). By default none is passed, so no harness silently
	// bills an API key instead of the subscription.
	PassEnv []string `json:"pass_env,omitempty"`
}

// For returns the settings for a harness, with defaults filled in.
func (c Config) For(name string) Harness {
	h := c.Harnesses[name]
	if h.MCP == "" {
		h.MCP = MCPIsolated
	}
	return h
}

// DefaultPath is os.UserConfigDir()/refraktal/config.json.
func DefaultPath() (string, error) {
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "refraktal", "config.json"), nil
}

// Load reads the file at path. A missing file gives the defaults; unknown
// fields and bad values are errors, so a typo never silently turns isolation
// off or on.
func Load(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		if err == nil {
			return c, fmt.Errorf("%s: expected EOF after first JSON value", path)
		}
		return c, fmt.Errorf("%s: expected EOF after first JSON value: %w", path, err)
	}
	for name, h := range c.Harnesses {
		switch name {
		case "claude", "codex", "agy", "refraktal":
		default:
			return c, fmt.Errorf("%s: unknown harness %q (want claude, codex, agy or refraktal)", path, name)
		}
		for _, k := range h.PassEnv {
			if !slices.Contains(ProviderKeys, k) {
				return c, fmt.Errorf("%s: harnesses.%s.pass_env: %q is not one of %s", path, name, k, strings.Join(ProviderKeys, ", "))
			}
		}
		switch h.MCP {
		case "", MCPIsolated, MCPInherit:
		default:
			return c, fmt.Errorf("%s: harnesses.%s.mcp is %q (want %q or %q)", path, name, h.MCP, MCPIsolated, MCPInherit)
		}
	}
	for model, p := range c.Prices {
		if p.Input < 0 || p.Output < 0 || (p.CacheRead != nil && *p.CacheRead < 0) || (p.CacheWrite != nil && *p.CacheWrite < 0) || (p.Input == 0 && p.Output == 0) {
			return c, fmt.Errorf("%s: prices.%s needs non-negative input and output prices (US dollars per million tokens)", path, model)
		}
	}
	for repo, ch := range c.Checks {
		if len(ch.Build) == 0 && len(ch.Test) == 0 {
			return c, fmt.Errorf("%s: checks.%s needs a build or test command (an argv list)", path, repo)
		}
		if !filepath.IsAbs(expandHome(repo)) {
			return c, fmt.Errorf("%s: checks.%s: the key must be an absolute path or start with ~/", path, repo)
		}
		if math.IsNaN(ch.TimeoutMins) || math.IsInf(ch.TimeoutMins, 0) || ch.TimeoutMins < 0 || ch.TimeoutMins > 1440 {
			return c, fmt.Errorf("%s: checks.%s.timeout_mins must be between 0 and 1440", path, repo)
		}
	}
	if m := c.Judge.Margin; m != nil && (math.IsNaN(*m) || *m < 0 || *m > 1) {
		return c, fmt.Errorf("%s: judge.margin must be between 0 and 1", path)
	}
	return c, nil
}
