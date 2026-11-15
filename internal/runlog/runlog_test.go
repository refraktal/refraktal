package runlog

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"refraktal.dev/refraktal/internal/proc"
)

func readLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<22)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("not JSONL: %v", err)
		}
		out = append(out, m)
	}
	return out
}

func TestRedaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	l, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	const email, home = "someone@example.com", "/Users/someone"
	big := `{"sessionUpdate":"available_commands_update","availableCommands":[` +
		strings.Repeat(`{"name":"x","description":"y"},`, 2000) + `{"name":"z"}]}`

	l.Update("claude", "s1", "agent_message_chunk", json.RawMessage(`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"OK"}}`))
	l.Update("claude", "s1", "available_commands_update", json.RawMessage(big))
	l.Update("claude", "s1", "account_update", json.RawMessage(`{"sessionUpdate":"account_update","email":"`+email+`"}`))
	l.Update("claude", "s1", "tool_call", json.RawMessage(`{"sessionUpdate":"tool_call","huge":"`+strings.Repeat("a", MaxVerbatim)+`"}`))
	l.Notification("claude", "auth/status_update", json.RawMessage(`{"email":"`+email+`","organization":"Acme"}`))
	l.Procs("tracked", []proc.Proc{{PID: 7, PPID: 1, PGID: 7, Start: "Sun Nov 15 15:45:51 2026", Cmd: home + "/.local/bin/my-mcp-server --stdio"}})
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	raw, _ := os.ReadFile(path)
	for _, secret := range []string{email, "Acme", home, "my-mcp-server"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("log contains %q", secret)
		}
	}
	lines := readLines(t, path)
	if len(lines) != 6 {
		t.Fatalf("want 6 lines, got %d", len(lines))
	}
	if _, ok := lines[0]["update"]; !ok {
		t.Error("allowlisted chunk not logged verbatim")
	}
	for i, why := range map[int]string{1: "large repeated update", 2: "unknown kind", 3: "oversized allowlisted update", 4: "notification"} {
		d, ok := lines[i]["digest"].(map[string]any)
		if !ok || d["sha256"] == "" || d["bytes"].(float64) == 0 {
			t.Errorf("%s: want a digest, got %v", why, lines[i])
		}
		if _, ok := lines[i]["update"]; ok {
			t.Errorf("%s: logged verbatim", why)
		}
	}
	if lines[4]["method"] != "auth/status_update" {
		t.Error("notification method missing")
	}
	for _, l := range lines {
		if l["t"] == nil || l["event"] == nil {
			t.Errorf("line without t or event: %v", l)
		}
	}
}

func TestCreateRefusesAnExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	os.WriteFile(path, []byte("keep\n"), 0o600)
	if _, err := Create(path); err == nil {
		t.Fatal("Create overwrote an existing log")
	}
}

func TestNewRunWritesTheIgnoreFile(t *testing.T) {
	root := t.TempDir()
	r, err := NewRun(root)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, ".refraktal", ".gitignore")); string(b) != "*\n" {
		t.Fatalf(".gitignore = %q", b)
	}
	if fi, err := os.Stat(r.Dir); err != nil || !fi.IsDir() || r.Rel != filepath.Join(".refraktal", "runs", r.ID) {
		t.Fatalf("run %+v: %v", r, err)
	}
	os.WriteFile(filepath.Join(root, ".refraktal", ".gitignore"), []byte("other\n"), 0o644)
	if _, err := NewRun(root); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, ".refraktal", ".gitignore")); string(b) != "*\n" {
		t.Fatalf(".gitignore not restored: %q", b)
	}
}

// A repository may commit .refraktal, runs or .gitignore as symlinks to files
// outside it. NewRun must refuse rather than write through them.
func TestNewRunRefusesSymlinks(t *testing.T) {
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim")
	os.WriteFile(victim, []byte("keep\n"), 0o644)

	cases := map[string]func(root string){
		".gitignore": func(root string) {
			os.Mkdir(filepath.Join(root, ".refraktal"), 0o755)
			os.Symlink(victim, filepath.Join(root, ".refraktal", ".gitignore"))
		},
		".refraktal": func(root string) { os.Symlink(outside, filepath.Join(root, ".refraktal")) },
		"runs": func(root string) {
			os.Mkdir(filepath.Join(root, ".refraktal"), 0o755)
			os.Symlink(outside, filepath.Join(root, ".refraktal", "runs"))
		},
	}
	for name, plant := range cases {
		root := t.TempDir()
		plant(root)
		if _, err := NewRun(root); err == nil {
			t.Errorf("%s symlink: NewRun did not refuse", name)
		}
		if b, _ := os.ReadFile(victim); string(b) != "keep\n" {
			t.Fatalf("%s symlink: file outside the repo overwritten: %q", name, b)
		}
		if entries, _ := os.ReadDir(outside); len(entries) != 1 {
			t.Fatalf("%s symlink: wrote into the folder outside the repo", name)
		}
	}
}
