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
		{"cd to a variable", request(t, "execute", map[string]any{"command": "cd $HOME && touch x"}), false},
		{"ln to a variable", request(t, "execute", map[string]any{"command": "ln -s $HOME h"}), false},
		{"git --config-env", request(t, "execute", map[string]any{"command": "git --config-env=alias.p=X p"}), false},
		{"GIT_CONFIG in env", request(t, "execute", map[string]any{"command": "GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=alias.p GIT_CONFIG_VALUE_0=push git p"}), false},
		{"export GIT_CONFIG", request(t, "execute", map[string]any{"command": "export GIT_CONFIG_PARAMETERS=x; git status"}), false},
		{"npm flag with a value", request(t, "execute", map[string]any{"command": "npm --registry https://r.example publish"}), false},
		{"usr local is user-writable", request(t, "execute", map[string]any{"command": "cp x /usr/local/bin/x"}), false},
		{"git commit message mentioning push", request(t, "execute", map[string]any{"command": `git commit -m "fix the push button"`}), true},
		{"GIT_DIR in env", request(t, "execute", map[string]any{"command": "GIT_DIR=/x/.git git status"}), false},
		{"HOME in env", request(t, "execute", map[string]any{"command": "HOME=. git p"}), false},
		{"export XDG_CONFIG_HOME", request(t, "execute", map[string]any{"command": "export XDG_CONFIG_HOME=.; git p"}), false},
		{"bare cd", request(t, "execute", map[string]any{"command": "cd && touch x"}), false},
		{"cd dash", request(t, "execute", map[string]any{"command": "cd - && touch x"}), false},
		{"git unknown global option", request(t, "execute", map[string]any{"command": "git --frobnicate x push"}), false},
		{"git --attr-source push", request(t, "execute", map[string]any{"command": "git --attr-source HEAD push"}), false},
		{"git --no-pager log", request(t, "execute", map[string]any{"command": "git --no-pager log --oneline -3"}), true},
		{"cd into a subfolder", request(t, "execute", map[string]any{"command": "cd src && ls"}), true},
		{"read into GIT_ variable", request(t, "execute", map[string]any{"command": "read GIT_CONFIG_PARAMETERS < f; export GIT_CONFIG_PARAMETERS; git p"}), false},
		{"append assignment", request(t, "execute", map[string]any{"command": "GIT_CONFIG_PARAMETERS+=x git p"}), false},
		{"cd with an option and no folder", request(t, "execute", map[string]any{"command": "cd -P && touch x"}), false},
		{"pushd stack rotation", request(t, "execute", map[string]any{"command": "pushd +1 && touch x"}), false},
		{"cd -P into a subfolder", request(t, "execute", map[string]any{"command": "cd -P src && ls"}), true},
		{"nameref to GIT_DIR", request(t, "execute", map[string]any{"command": "declare -n x=GIT_DIR; x=/y; git p"}), false},
		{"nameref without the name", request(t, "execute", map[string]any{"command": "v=G; declare -n x=$v; git p"}), false},
		{"export an expanded name", request(t, "execute", map[string]any{"command": "n=GI; export ${n}T_DIR=/y"}), false},
		{"printf -v expanded", request(t, "execute", map[string]any{"command": "printf -v $n x"}), false},
		{"plain printf", request(t, "execute", map[string]any{"command": "printf 'hi\\n' > out.txt"}), true},
		{"echo PATH", request(t, "execute", map[string]any{"command": "echo $PATH"}), true},
		{"nameref in an option cluster", request(t, "execute", map[string]any{"command": "declare -gn x=y; x=/z"}), false},
		{"backtick with declare", request(t, "execute", map[string]any{"command": "declare x=`printf GI`"}), false},
		{"plain declare", request(t, "execute", map[string]any{"command": "declare -i n=3; echo $n"}), true},
		{"unknown kind with a non-path target", request(t, "other", map[string]any{"target": "production"}), false},
		{"unknown kind: path inside, to outside", request(t, "other", map[string]any{"path": filepath.Join(wt, "a"), "to": filepath.Join(outside, "a")}), false},
		{"unknown kind with changes", request(t, "other", map[string]any{"path": filepath.Join(wt, "a"), "changes": map[string]any{filepath.Join(outside, "a"): map[string]any{}}}), false},
		{"unknown kind with a camelCase destination", request(t, "other", map[string]any{"path": filepath.Join(wt, "a"), "toPath": filepath.Join(outside, "a")}), false},
		{"unknown kind with a nested path", request(t, "other", map[string]any{"path": filepath.Join(wt, "a"), "args": map[string]any{"out": filepath.Join(outside, "a")}}), false},
		{"unknown kind with a non-string path", request(t, "other", map[string]any{"path": []any{filepath.Join(outside, "a")}}), false},
		{"move with to outside", request(t, "move", map[string]any{"from": filepath.Join(wt, "a"), "to": filepath.Join(outside, "a")}), false},
		{"move with to inside", request(t, "move", map[string]any{"from": filepath.Join(wt, "a"), "to": filepath.Join(wt, "b")}), true},
		{"edit target outside", request(t, "edit", map[string]any{"path": filepath.Join(wt, "a"), "target": filepath.Join(outside, "a")}), false},
		{"system tool path", request(t, "execute", map[string]any{"command": "/usr/bin/env ls > /dev/null"}), true},
		{"absolute path inside", request(t, "execute", map[string]any{"command": "cat " + filepath.Join(wt, "src")}), true},
		// Codex
		{"codex cwd outside", request(t, "execute", map[string]any{"command": "ls", "cwd": outside}), false},
		{"codex argv list", request(t, "execute", map[string]any{"command": []any{"git", "push"}, "cwd": wt}), false},
		{"codex file change", request(t, "edit", map[string]any{"changes": map[string]any{filepath.Join(wt, "a"): map[string]any{}}}), true},
		{"codex file change out", request(t, "edit", map[string]any{"changes": map[string]any{filepath.Join(outside, "a"): map[string]any{}}}), false},
		{"codex move out of the worktree", request(t, "edit", map[string]any{"changes": map[string]any{
			filepath.Join(wt, "a"): map[string]any{"type": "update", "move_path": filepath.Join(outside, "a")}}}), false},
		{"codex move inside", request(t, "edit", map[string]any{"changes": map[string]any{
			filepath.Join(wt, "a"): map[string]any{"type": "update", "move_path": filepath.Join(wt, "b")}}}), true},
		{"move with a destination outside", request(t, "move", map[string]any{"path": filepath.Join(wt, "a"), "destination": filepath.Join(outside, "a")}), false},
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

func TestSystemPathFollowsLinksBetweenSystemFolders(t *testing.T) {
	if !systemPath("/usr/bin/env") || !systemPath("/usr/lib/cargo/bin/coreutils") || systemPath("/usr/local/bin/x") || systemPath("/etc/passwd") {
		t.Fatal("systemPath misclassifies")
	}
}

// M6: the judge's policy allows reads only.
func TestReadOnlyPolicy(t *testing.T) {
	wt := t.TempDir()
	p := Policy{Worktree: wt, ReadOnly: true}
	opts := []acp.PermissionOption{{ID: "a", Kind: "allow_once"}, {ID: "r", Kind: "reject_once"}}
	for kind, want := range map[string]bool{"switch_mode": false, "read": true, "search": true, "edit": false, "execute": false, "delete": false, "other": false} {
		req := acp.PermissionRequest{Options: opts}
		req.ToolCall.Kind, req.ToolCall.Title = kind, "x"
		req.ToolCall.RawInput = []byte(`{"file_path":"` + wt + `/a.txt","command":"ls"}`)
		if d := p.Decide(req); d.Allow != want {
			t.Errorf("%s: allow=%v (%s)", kind, d.Allow, d.Reason)
		}
	}
}

func TestFileEditResolvesSymlinksBeforeParentComponents(t *testing.T) {
	base := t.TempDir()
	wt := filepath.Join(base, "worktree")
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{wt, filepath.Join(wt, "sub"), filepath.Join(outside, "sub")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	links := map[string]string{
		"dangling":       filepath.Join(outside, "missing.txt"),
		"link":           filepath.Join(outside, "sub"),
		"chain":          "dangling",
		"absolute":       outside,
		"relative":       "../outside/missing.txt",
		"inside":         "sub",
		"inside-chain":   "inside",
		"inside-missing": "sub/missing.txt",
		"loop-a":         "loop-b",
		"loop-b":         "loop-a",
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(wt, name)); err != nil {
			t.Fatal(err)
		}
	}
	p := Policy{Worktree: wt}
	for _, tc := range []struct {
		name  string
		path  string
		allow bool
	}{
		{"dangling_final_symlink", "dangling", false},
		{"parent_after_symlink", "link/../x", false},
		{"absolute_parent_after_symlink", wt + "/link/../x", false},
		{"symlink_chain", "chain", false},
		{"absolute_symlink", "absolute/x", false},
		{"relative_escape", "relative", false},
		{"inside_symlink", "inside/x", true},
		{"inside_symlink_chain", "inside-chain/x", true},
		{"inside_dangling_symlink", "inside-missing", true},
		{"inside_parent_after_symlink", "inside/../x", true},
		{"new_nested_path", "new/nested/x", true},
		{"symlink_loop", "loop-a", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := p.Decide(request(t, "edit", map[string]any{"file_path": tc.path}))
			if d.Allow != tc.allow {
				t.Fatalf("allow=%v (%s), want %v", d.Allow, d.Reason, tc.allow)
			}
		})
	}
}
