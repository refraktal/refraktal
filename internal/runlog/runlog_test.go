package runlog

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"refraktal.dev/refraktal/internal/proc"
)

func TestErrorFieldLogsOnlyCodeAndCoarseLength(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	l, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for _, raw := range []string{"auth for alice@example.com", "login for bob@example.org"} {
		if err := l.Event("error", map[string]any{"error": ErrorField("auth", raw)}); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]any{"code": "auth", "bytes_bucket": float64(32)}
	lines := readLines(t, path)
	if len(lines) != 2 {
		t.Fatalf("logged %d events; want 2", len(lines))
	}
	for _, event := range lines {
		if !reflect.DeepEqual(event["error"], want) {
			t.Fatalf("error fields = %v; want only %v", event["error"], want)
		}
	}
}

func TestErrorFieldLengthBuckets(t *testing.T) {
	if got := ErrorField("auth", ""); got != nil {
		t.Fatalf("empty error = %v; want nil", got)
	}
	for _, tc := range []struct {
		raw  string
		want int
	}{
		{"x", 1}, {"xx", 2}, {"xxx", 4}, {"12345678", 8}, {"123456789", 16},
		{"ééé", 8}, {strings.Repeat("x", 65536), 65536},
		{strings.Repeat("x", 65537), 65536}, {strings.Repeat("x", 1<<20), 65536},
	} {
		want := map[string]any{"code": "auth", "bytes_bucket": tc.want}
		if got := ErrorField("auth", tc.raw); !reflect.DeepEqual(got, want) {
			t.Errorf("%d bytes: fields = %v; want %v", len(tc.raw), got, want)
		}
	}
}

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

func TestRunWritesStayInPinnedFolder(t *testing.T) {
	r, err := NewRun(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	held, outside := r.Dir+"-held", t.TempDir()
	if err := os.Rename(r.Dir, held); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, r.Dir); err != nil {
		t.Fatal(err)
	}
	l, err := r.Create("events.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Event("pinned", nil); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteFile("result.diff", []byte("diff\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"claude", "tmp"} {
		if err := r.Mkdir(name, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Errorf("run writes escaped into outside folder: %v, %v", entries, err)
	}
	for _, name := range []string{"events.jsonl", "result.diff", "claude", "tmp"} {
		if _, err := os.Stat(filepath.Join(held, name)); err != nil {
			t.Errorf("%s missing from pinned folder: %v", name, err)
		}
	}
}

func TestRunCloseStopsLaterWrites(t *testing.T) {
	r, err := NewRun(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if l, err := r.Create("events.jsonl"); err == nil {
		l.Close()
		t.Error("Create used a closed run")
	}
	if err := r.WriteFile("result.diff", nil, 0o600); err == nil {
		t.Error("WriteFile used a closed run")
	}
	if err := r.Mkdir("tmp", 0o700); err == nil {
		t.Error("Mkdir used a closed run")
	}
}

func TestNewRunWritesTheIgnoreFile(t *testing.T) {
	root := t.TempDir()
	r, err := NewRun(root)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if b, _ := os.ReadFile(filepath.Join(root, ".refraktal", ".gitignore")); string(b) != "*\n" {
		t.Fatalf(".gitignore = %q", b)
	}
	if fi, err := os.Stat(r.Dir); err != nil || !fi.IsDir() || r.Rel != filepath.Join(".refraktal", "runs", r.ID) {
		t.Fatalf("run %+v: %v", r, err)
	}
	os.WriteFile(filepath.Join(root, ".refraktal", ".gitignore"), []byte("other\n"), 0o644)
	r2, err := NewRun(root)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
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
		if r, err := NewRun(root); err == nil {
			r.Close()
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

// Hardening 3. A folder that does not exist yet under a temp folder is
// refused too: the nearest existing parent is resolved, then the rest added.
func TestCheckNotTempRefusesAFolderNotYetCreated(t *testing.T) {
	for _, p := range []string{filepath.Join(os.TempDir(), "refraktal-not-yet", "worktrees"), "/tmp/refraktal-not-yet/x"} {
		if err := CheckNotTemp(p); err == nil {
			t.Errorf("%s was accepted", p)
		}
	}
}

func TestNewRunDirectorySwapCannotWriteOutsideRoot(t *testing.T) {
	for _, target := range []string{".refraktal", filepath.Join(".refraktal", "runs")} {
		t.Run(target, func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			r, err := NewRun(root)
			if err != nil {
				t.Fatal(err)
			}
			r.Close()
			path := filepath.Join(root, target)
			held := path + "-held"
			stop, done := make(chan struct{}), make(chan error, 1)
			go func() {
				for {
					select {
					case <-stop:
						done <- nil
						return
					default:
					}
					if err := os.Rename(path, held); err != nil {
						done <- err
						return
					}
					if err := os.Symlink(outside, path); err != nil {
						if os.IsExist(err) {
							// NewRun may create the missing directory in this gap.
							if err := os.RemoveAll(held); err != nil {
								done <- err
								return
							}
							continue
						}
						done <- err
						return
					}
					runtime.Gosched()
					if err := os.Remove(path); err != nil {
						done <- err
						return
					}
					if err := os.Rename(held, path); err != nil {
						if fi, statErr := os.Lstat(path); statErr != nil || !fi.IsDir() {
							done <- err
							return
						}
						if err := os.RemoveAll(held); err != nil {
							done <- err
							return
						}
					}
				}
			}()
			defer func() {
				close(stop)
				if err := <-done; err != nil {
					t.Errorf("directory swap: %v", err)
				}
			}()
			for range 300 {
				// Races may fail safely; they must never redirect a write.
				if r, err := NewRun(root); err == nil {
					r.Close()
				}
				entries, err := os.ReadDir(outside)
				if err != nil {
					t.Fatal(err)
				}
				if len(entries) != 0 {
					t.Fatalf("directory swap redirected a write outside root: %s", entries[0].Name())
				}
			}
		})
	}
}

func TestNewRunIgnoreCheckHasBoundedMemory(t *testing.T) {
	root := t.TempDir()
	r, err := NewRun(root)
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	path := filepath.Join(root, ".refraktal", ".gitignore")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("*\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(8 << 20); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	r, err = NewRun(root)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 1<<20 {
		t.Fatalf("checking an 8 MiB .gitignore allocated %d bytes; want under 1 MiB", allocated)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "*\n" {
		t.Fatalf("oversized .gitignore not replaced: %q, %v", b, err)
	}
}
