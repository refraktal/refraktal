package harness

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionsAreProbedOnlyWhenRequested(t *testing.T) {
	for _, probe := range []bool{false, true} {
		for _, name := range []string{"adapted", "agy"} {
			t.Run(name+"/"+map[bool]string{false: "normal", true: "doctor"}[probe], func(t *testing.T) {
				bin := t.TempDir()
				calls := filepath.Join(bin, "calls")
				script(t, bin, "test-cli", `echo version >> '`+calls+`'; echo 1.2.3`)
				t.Setenv("PATH", bin)
				var h Harness
				o := Options{MCP: MCPInherit, ProbeVersions: probe}
				wantCount := 1
				if name == "adapted" {
					cache := t.TempDir()
					dir := filepath.Join(cache, "test-1", "node_modules")
					if err := os.MkdirAll(filepath.Join(dir, ".bin"), 0o700); err != nil {
						t.Fatal(err)
					}
					script(t, filepath.Join(dir, ".bin"), "adapter", "")
					script(t, dir, "bundled", `echo version >> '`+calls+`'; echo 4.5.6`)
					h = &adapted{pkg: "test", version: "1", cli: "test-cli", bin: "adapter", bundled: "bundled", cache: cache,
						isolate: func(_ context.Context, _ Options, executable string, _ *Launch) error {
							if executable != filepath.Join(dir, "bundled") {
								t.Errorf("executable discovery changed: %q", executable)
							}
							return nil
						}}
					wantCount = 2
				} else {
					h = agy{}
					o.Executable = filepath.Join(bin, "test-cli")
				}
				l, err := h.Prepare(context.Background(), o)
				if err != nil {
					t.Fatal(err)
				}
				if !probe {
					wantCount = 0
					if l.Installed != "" || l.Bundled != "" {
						t.Fatalf("unused versions were populated: %+v", l)
					}
				} else if l.Installed != "1.2.3" || (name == "adapted" && l.Bundled != "4.5.6") {
					t.Fatalf("doctor versions changed: %+v", l)
				}
				b, err := os.ReadFile(calls)
				if err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
				if got := strings.Count(string(b), "version\n"); got != wantCount {
					t.Fatalf("version subprocesses = %d, want %d", got, wantCount)
				}
				t.Logf("version subprocesses: %d", wantCount)
			})
		}
	}
}
