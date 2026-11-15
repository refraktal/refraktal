package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
)

// hookFiles are the project files where each harness reads hooks: commands
// the harness runs on its own, with the child's permissions, outside
// Refraktal's permission policy. Antigravity's project hook location is not
// documented; the paths listed for it are the likely ones.
var hookFiles = map[string][]string{
	"claude": {".claude/settings.json", ".claude/settings.local.json"},
	"codex":  {".codex/hooks.json", ".codex/config.toml"},
	"agy":    {".gemini/settings.json", ".gemini/config/hooks.json", ".agents/hooks.json"},
}

var tomlHooks = regexp.MustCompile(`(?m)^\s*\[\s*hooks[\].]|^\s*hooks\s*=`)

// ProjectHooks returns the files in dir (relative paths) that configure hooks
// for the harness.
func ProjectHooks(dir, harness string) []string {
	var found []string
	for _, rel := range hookFiles[harness] {
		b, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			continue
		}
		switch filepath.Ext(rel) {
		case ".toml":
			if tomlHooks.Match(b) {
				found = append(found, rel)
			}
		case ".json":
			if filepath.Base(rel) == "hooks.json" {
				found = append(found, rel)
				continue
			}
			var m map[string]json.RawMessage
			if json.Unmarshal(b, &m) == nil {
				if h, ok := m["hooks"]; ok && string(h) != "null" && string(h) != "{}" && string(h) != "[]" {
					found = append(found, rel)
				}
			}
		}
	}
	return found
}
