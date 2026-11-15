// Package policy answers the permission requests that reach Refraktal from a
// child harness.
//
// It is a gate, not a sandbox: it sees a command's text, not everything the
// command touches, and a harness decides by its own rules which actions to
// ask about at all.
//
//   - Reads, searches and other non-mutating tools: allowed.
//   - File edits, deletes and moves: allowed only when every path is inside
//     the child's worktree.
//   - Shell commands: allowed, unless they match the deny-list (pushing,
//     publishing, releasing) or name a path outside the worktree.
package policy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"refraktal.dev/refraktal/internal/acp"
)

// Decision is the policy's answer to one request.
type Decision struct {
	Allow    bool     `json:"allow"`
	Reason   string   `json:"reason"`
	Kind     string   `json:"tool_kind"`
	Title    string   `json:"title,omitempty"`
	Command  string   `json:"command,omitempty"`
	Paths    []string `json:"paths,omitempty"`
	OptionID string   `json:"option_id"` // "" means the cancelled outcome: no suitable option was offered
}

// Policy decides for one child.
type Policy struct {
	Worktree string // absolute path of the child's worktree
}

// denied lists commands that reach outside the machine or publish something.
// Each is a program followed by a subcommand, matched after the program's
// leading flags.
var denied = [][]string{
	{"git", "push"},
	{"gh", "release"}, {"gh", "pr", "merge"}, {"gh", "repo", "delete"},
	{"npm", "publish"}, {"pnpm", "publish"}, {"yarn", "publish"}, {"yarn", "npm", "publish"}, {"bun", "publish"},
	{"cargo", "publish"}, {"twine", "upload"}, {"poetry", "publish"}, {"uv", "publish"}, {"flit", "publish"},
	{"gem", "push"}, {"docker", "push"}, {"podman", "push"}, {"goreleaser", "release"}, {"mvn", "deploy"},
}

// outsideOK are paths outside the worktree that a command may name.
var outsideOK = []string{"/dev/null", "/dev/stdin", "/dev/stdout", "/dev/stderr", "/dev/tty",
	"/usr/", "/bin/", "/sbin/", "/opt/homebrew/", "/System/", "/Library/Developer/"}

// Decide answers one request.
func (p Policy) Decide(req acp.PermissionRequest) Decision {
	d := Decision{Kind: req.ToolCall.Kind, Title: req.ToolCall.Title}
	in := rawInput(req.ToolCall.RawInput)
	d.Paths = paths(req, in)
	switch d.Kind {
	case "edit", "delete", "move":
		d.Allow, d.Reason = p.filesInside(d.Paths)
	case "execute":
		d.Command = command(in, req.ToolCall.Title)
		d.Allow, d.Reason = p.commandOK(d.Command, in)
	default:
		d.Allow, d.Reason = true, "does not change files"
	}
	if d.Allow {
		d.OptionID = first(req, "allow_once", "allow_always")
	} else {
		d.OptionID = first(req, "reject_once", "reject_always")
	}
	return d
}

func first(req acp.PermissionRequest, kinds ...string) string {
	for _, k := range kinds {
		if id := req.Option(k); id != "" {
			return id
		}
	}
	return ""
}

func rawInput(raw json.RawMessage) map[string]any {
	var m map[string]any
	json.Unmarshal(raw, &m)
	return m
}

// command finds the shell command: Claude and Codex send rawInput.command
// (a string, or an argv list), Antigravity rawInput.CommandLine.
func command(in map[string]any, title string) string {
	for _, k := range []string{"command", "CommandLine", "cmd"} {
		switch v := in[k].(type) {
		case string:
			return v
		case []any:
			var parts []string
			for _, p := range v {
				if s, ok := p.(string); ok {
					parts = append(parts, s)
				}
			}
			return strings.Join(parts, " ")
		}
	}
	return title
}

// paths collects the file paths a request names: the ACP locations, and the
// path fields harnesses put in rawInput.
func paths(req acp.PermissionRequest, in map[string]any) []string {
	var out []string
	var loc struct {
		ToolCall struct {
			Locations []struct {
				Path string `json:"path"`
			} `json:"locations"`
		} `json:"toolCall"`
	}
	json.Unmarshal(req.Raw, &loc)
	for _, l := range loc.ToolCall.Locations {
		out = append(out, l.Path)
	}
	for _, k := range []string{"file_path", "path", "Path", "notebook_path", "new_path", "old_path"} {
		if s, ok := in[k].(string); ok && s != "" {
			out = append(out, s)
		}
	}
	if ch, ok := in["changes"].(map[string]any); ok { // Codex file changes, keyed by path
		for k := range ch {
			out = append(out, k)
		}
	}
	return out
}

func (p Policy) inside(path, cwd string) bool {
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	path = resolve(filepath.Clean(path))
	root := resolve(p.Worktree)
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

// resolve follows symlinks in the longest existing prefix of path, so a link
// inside the worktree that points out of it is seen for what it is.
func resolve(path string) string {
	dir, rest := path, ""
	for {
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return path
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = parent
	}
}

func (p Policy) filesInside(paths []string) (bool, string) {
	if len(paths) == 0 {
		return false, "file change names no path to check"
	}
	for _, f := range paths {
		if !p.inside(f, p.Worktree) {
			return false, "path outside the worktree: " + f
		}
	}
	return true, "inside the worktree"
}

func (p Policy) commandOK(cmd string, in map[string]any) (bool, string) {
	if strings.TrimSpace(cmd) == "" {
		return false, "no command to check"
	}
	cwd := p.Worktree
	if c, ok := in["cwd"].(string); ok && c != "" {
		if !p.inside(c, p.Worktree) {
			return false, "working directory outside the worktree: " + c
		}
		cwd = c
	}
	for _, seg := range segments(cmd) {
		for _, rule := range denied {
			if matches(seg, rule) {
				return false, "deny-list: " + strings.Join(rule, " ")
			}
		}
	}
	for _, tok := range strings.Fields(cmd) {
		for _, cand := range pathCandidates(tok) {
			if p.outside(cand, cwd) {
				return false, "path outside the worktree: " + cand
			}
		}
	}
	return true, "allowed command"
}

// segments splits a command line into simple commands at ; & | ( ) and
// newlines, and each into words. Quotes are stripped, not parsed.
func segments(cmd string) [][]string {
	var out [][]string
	for _, s := range strings.FieldsFunc(cmd, func(r rune) bool { return strings.ContainsRune(";&|()\n`", r) }) {
		var words []string
		for _, w := range strings.Fields(s) {
			if w = strings.Trim(w, `"'`); w != "" {
				words = append(words, w)
			}
		}
		if len(words) > 0 {
			out = append(out, words)
		}
	}
	return out
}

// matches reports whether a simple command runs rule's program and
// subcommand. It skips environment assignments, wrappers (sudo, env,
// command) and the program's flags (with the values of git -C and -c).
func matches(words, rule []string) bool {
	i := 0
	for i < len(words) && (strings.Contains(words[i], "=") || words[i] == "sudo" || words[i] == "env" || words[i] == "command" || words[i] == "exec") {
		i++
	}
	if i >= len(words) || filepath.Base(words[i]) != rule[0] {
		return false
	}
	i++
	for _, want := range rule[1:] {
		for i < len(words) && strings.HasPrefix(words[i], "-") {
			if words[i] == "-C" || words[i] == "-c" {
				i++
			}
			i++
		}
		if i >= len(words) || words[i] != want {
			return false
		}
		i++
	}
	return true
}

// pathCandidates returns the parts of a word that may be paths: the word
// without quotes and redirections, and anything after an "=".
func pathCandidates(tok string) []string {
	tok = strings.Trim(tok, `"'`)
	tok = strings.TrimLeft(tok, "0123456789<>&")
	var out []string
	if tok != "" {
		out = append(out, tok)
	}
	if _, v, ok := strings.Cut(tok, "="); ok && v != "" {
		out = append(out, strings.Trim(v, `"'`))
	}
	return out
}

// outside reports whether s looks like a path that leaves the worktree. Only
// absolute paths, ~ paths and paths with a ".." element are checked, and an
// absolute path counts only if it or its parent exists, so a regular
// expression such as /api/ is not taken for a path.
func (p Policy) outside(s, cwd string) bool {
	home, _ := os.UserHomeDir()
	switch {
	case strings.HasPrefix(s, "~/") || s == "~":
		if home == "" {
			return true
		}
		s = filepath.Join(home, strings.TrimPrefix(s, "~"))
	case filepath.IsAbs(s):
		if _, err := os.Lstat(s); err != nil {
			if _, err := os.Lstat(filepath.Dir(filepath.Clean(s))); err != nil {
				return false
			}
		}
	case strings.Contains("/"+s+"/", "/../"):
	default:
		return false
	}
	if p.inside(s, cwd) {
		return false
	}
	for _, ok := range outsideOK {
		if s == strings.TrimSuffix(ok, "/") || strings.HasPrefix(s, ok) {
			return false
		}
	}
	return true
}
