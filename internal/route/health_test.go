package route

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadKeySkipsGitWhenDotEnvIsMissing(t *testing.T) {
	calls := fakeGit(t)
	dir := t.TempDir()
	getenv := func(string) string { return "" }
	if key, source := LoadKey(dir, getenv); key != "" || source != "" {
		t.Fatal("missing .env supplied a key")
	}
	b, err := os.ReadFile(calls)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if got := strings.Count(string(b), "ARGS "); got != 0 {
		t.Fatalf("missing .env ran %d git commands, want 0", got)
	}
	t.Log("missing .env Git subprocesses: 0")
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("OTHER=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if key, source := LoadKey(dir, getenv); key != "" || source != "" {
		t.Fatal(".env without the key supplied one")
	}
	b, err = os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(b), "ARGS "); got != 2 {
		t.Fatalf("existing .env ran %d git commands, want 2", got)
	}
	t.Log("existing .env Git subprocesses: 2")
}

func BenchmarkKeylessRoute(b *testing.B) {
	for _, mode := range []string{"enabled", "disabled"} {
		b.Run(mode, func(b *testing.B) {
			cache := b.TempDir()
			if mode == "disabled" {
				cache = "-"
			}
			r := New(Options{CacheDir: cache})
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := r.Route(context.Background(), bugfix); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
