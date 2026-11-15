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
	"os"
	"path/filepath"
)

// MCP settings for a harness.
const (
	MCPIsolated = "isolated" // the default: hide the user's MCP servers and plugins for each run
	MCPInherit  = "inherit"  // use the user's own setup
)

// Config is the whole file.
type Config struct {
	Harnesses map[string]Harness `json:"harnesses,omitempty"`
}

// Harness holds the settings for one harness.
type Harness struct {
	// MCP is "isolated" (default) or "inherit".
	MCP string `json:"mcp,omitempty"`
	// Executable points the harness at a binary: the CLI an adapter should
	// run instead of its bundled one (CLAUDE_CODE_EXECUTABLE, CODEX_PATH),
	// or the agy binary.
	Executable string `json:"executable,omitempty"`
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
	for name, h := range c.Harnesses {
		switch name {
		case "claude", "codex", "agy":
		default:
			return c, fmt.Errorf("%s: unknown harness %q (want claude, codex or agy)", path, name)
		}
		switch h.MCP {
		case "", MCPIsolated, MCPInherit:
		default:
			return c, fmt.Errorf("%s: harnesses.%s.mcp is %q (want %q or %q)", path, name, h.MCP, MCPIsolated, MCPInherit)
		}
	}
	return c, nil
}
