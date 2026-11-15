package policy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"refraktal.dev/refraktal/internal/acp"
)

var options = []acp.PermissionOption{
	{ID: "yes", Kind: "allow_once"}, {ID: "always", Kind: "allow_always"}, {ID: "no", Kind: "reject_once"},
}

// request builds a permission request the way the harnesses send them.
func request(t *testing.T, kind string, rawInput map[string]any, locations ...string) acp.PermissionRequest {
	t.Helper()
	locs := []map[string]string{}
	for _, l := range locations {
		locs = append(locs, map[string]string{"path": l})
	}
	raw, _ := json.Marshal(map[string]any{"sessionId": "s", "toolCall": map[string]any{
		"toolCallId": "t", "kind": kind, "title": "t", "rawInput": rawInput, "locations": locs}, "options": options})
	var req acp.PermissionRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	req.Raw = raw
	return req
}

func TestDecide(t *testing.T) {
	wt := t.TempDir()
	os.MkdirAll(filepath.Join(wt, "src"), 0o755)
	outside := t.TempDir()
	os.Symlink(outside, filepath.Join(wt, "escape")) // a link inside the worktree that points out of it
	p := Policy{Worktree: wt}

	cases := []struct {
		name  string
		req   acp.PermissionRequest
		allow bool
	}{
		// Claude
		{"claude edit inside", request(t, "edit", map[string]any{"file_path": filepath.Join(wt, "src", "a.go")}, filepath.Join(wt, "src", "a.go")), true},
		{"claude edit outside", request(t, "edit", map[string]any{"file_path": filepath.Join(outside, "a.go")}), false},
		{"edit via relative escape", request(t, "edit", map[string]any{"file_path": filepath.Join(wt, "..", "x")}), false},
		{"edit through a symlink out", request(t, "edit", map[string]any{"file_path": filepath.Join(wt, "escape", "x")}), false},
		{"edit with no path", request(t, "edit", map[string]any{}), false},
		{"claude touch", request(t, "execute", map[string]any{"command": "touch marker.txt"}), true},
		{"git push", request(t, "execute", map[string]any{"command": "git push origin HEAD"}), false},
		{"git -C push", request(t, "execute", map[string]any{"command": "git -C . push"}), false},
		{"push after &&", request(t, "execute", map[string]any{"command": "git add -A && git commit -m x && git push"}), false},
		{"env-prefixed publish", request(t, "execute", map[string]any{"command": "NPM_TOKEN=x npm publish --access public"}), false},
		{"git status", request(t, "execute", map[string]any{"command": "git status --short"}), true},
		{"git log mentioning push", request(t, "execute", map[string]any{"command": "git log --grep push"}), true},
		{"write to /tmp", request(t, "execute", map[string]any{"command": "echo hi > /tmp/refraktal-policy-test"}), false},
		{"write to home", request(t, "execute", map[string]any{"command": "touch ~/x"}), false},
		{"dot-dot escape", request(t, "execute", map[string]any{"command": "printf hi > ../outside.txt"}), false},
		{"leading-slash pattern counts as a path", request(t, "execute", map[string]any{"command": "grep -rn '/api/v9/' ."}), false},
		{"relative pattern", request(t, "execute", map[string]any{"command": "grep -rn 'api/v9' ."}), true},
		{"bash -c push", request(t, "execute", map[string]any{"command": `bash -c "git push origin HEAD"`}), false},
		{"xargs push", request(t, "execute", map[string]any{"command": "echo origin | xargs git push"}), false},
		{"quoted program name", request(t, "execute", map[string]any{"command": `g"i"t push`}), false},
		{"git -c alias", request(t, "execute", map[string]any{"command": "git -c alias.p=push p"}), false},
		{"git --git-dir push", request(t, "execute", map[string]any{"command": "git --git-dir .git push"}), false},
		{"git config", request(t, "execute", map[string]any{"command": "git config core.fsmonitor ./x"}), false},
		{"dot-dot through a system folder", request(t, "execute", map[string]any{"command": "cat /usr/../etc/x > /usr/../tmp/y"}), false},
		{"new folder outside", request(t, "execute", map[string]any{"command": "mkdir -p /refraktal-no-such/a && touch /refraktal-no-such/a/b"}), false},
		{"homebrew is user-writable", request(t, "execute", map[string]any{"command": "touch /opt/homebrew/x"}), false},
		{"variable in a path", request(t, "execute", map[string]any{"command": `touch "$HOME/x"`}), false},
		{"cd out", request(t, "execute", map[string]any{"command": "cd .. && ls"}), false},
		{"variable without a path", request(t, "execute", map[string]any{"command": "echo $PATH"}), true},
		{"system tool path", request(t, "execute", map[string]any{"command": "/usr/bin/env ls > /dev/null"}), true},
		{"absolute path inside", request(t, "execute", map[string]any{"command": "cat " + filepath.Join(wt, "src")}), true},
		// Codex
		{"codex cwd outside", request(t, "execute", map[string]any{"command": "ls", "cwd": outside}), false},
		{"codex argv list", request(t, "execute", map[string]any{"command": []any{"git", "push"}, "cwd": wt}), false},
		{"codex file change", request(t, "edit", map[string]any{"changes": map[string]any{filepath.Join(wt, "a"): map[string]any{}}}), true},
		{"codex file change out", request(t, "edit", map[string]any{"changes": map[string]any{filepath.Join(outside, "a"): map[string]any{}}}), false},
		// Antigravity
		{"agy command line", request(t, "execute", map[string]any{"CommandLine": "echo hi"}), true},
		{"agy push", request(t, "execute", map[string]any{"CommandLine": "git push"}), false},
		{"agy write", request(t, "edit", map[string]any{"Path": filepath.Join(wt, "hello.txt")}, filepath.Join(wt, "hello.txt")), true},
		// Other kinds
		{"read anywhere", request(t, "read", map[string]any{"file_path": "/etc/hosts"}), true},
		{"fetch", request(t, "fetch", map[string]any{"url": "https://example.com"}), true},
		{"unknown kind without paths", request(t, "other", map[string]any{"server": "x"}), false},
		{"unknown kind inside", request(t, "other", map[string]any{"path": filepath.Join(wt, "a")}), true},
		{"unknown kind outside", request(t, "other", map[string]any{"path": filepath.Join(outside, "a")}), false},
	}
	for _, tc := range cases {
		d := p.Decide(tc.req)
		if d.Allow != tc.allow {
			t.Errorf("%s: allow=%v (%s), want %v", tc.name, d.Allow, d.Reason, tc.allow)
		}
		want := "no"
		if tc.allow {
			want = "yes"
		}
		if d.OptionID != want {
			t.Errorf("%s: option %q, want %q", tc.name, d.OptionID, want)
		}
	}
}

func TestDecideWithoutARejectOption(t *testing.T) {
	req := request(t, "execute", map[string]any{"command": "git push"})
	req.Options = []acp.PermissionOption{{ID: "yes", Kind: "allow_once"}}
	if d := (Policy{Worktree: t.TempDir()}).Decide(req); d.Allow || d.OptionID != "" {
		t.Fatalf("want deny with the cancelled outcome, got %+v", d)
	}
}
