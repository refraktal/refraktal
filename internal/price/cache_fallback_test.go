package price

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadCorruptCache(t *testing.T) {
	// Syntactically valid JSON with no priced entries is also an invalid table.
	for _, body := range []string{`{"broken":`, `{}`, `null`} {
		t.Run(body, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "prices.json")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			o := LoadOptions{CachePath: path, URL: "https://prices.invalid/table",
				Client: &http.Client{Transport: priceTestTransport(func(*http.Request) (*http.Response, error) {
					return nil, errors.New("offline test transport")
				})}}
			got, warning := Load(context.Background(), o)
			if _, _, priced := got.Lookup("gpt-6.1-sol"); priced || got.Source != "none" {
				t.Fatalf("invalid cache treated as valid: priced=%t source=%s", priced, got.Source)
			}
			if warning == nil || !strings.Contains(warning.Error(), "invalid cached copy") || !strings.Contains(warning.Error(), "every model is unpriced") {
				t.Fatalf("missing corrupt-cache warning: %v", warning)
			}

			o.Client = &http.Client{Transport: priceTestTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(sample))}, nil
			})}
			got, warning = Load(context.Background(), o)
			p, _, priced := got.Lookup("gpt-6.1-sol")
			if warning != nil || got.Source != "fetched" || !priced || p.Input != 2e-6 {
				t.Fatalf("corrupt-cache replacement: source=%s priced=%t price=%+v warning=%v", got.Source, priced, p, warning)
			}
			contents, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(contents, []byte(sample)) {
				t.Fatalf("fetched table did not replace corrupt file: err=%v", err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != 0o644 {
				t.Fatalf("published cache mode: got %04o, want 0644", got)
			}
		})
	}
}

func TestLoadInvalidFetchPreservesGoodCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prices.json")
	if err := os.WriteFile(path, []byte(sample), 0o600); err != nil {
		t.Fatal(err)
	}
	o := LoadOptions{CachePath: path, URL: "https://prices.invalid/table",
		Now: func() time.Time { return time.Now().Add(MaxAge + time.Minute) },
		Client: &http.Client{Transport: priceTestTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"broken":`))}, nil
		})}}
	got, warning := Load(context.Background(), o)
	if _, _, priced := got.Lookup("gpt-6.1-sol"); !priced || got.Source != "stale cache" || warning == nil {
		t.Fatalf("invalid refresh lost good snapshot: source=%s priced=%t warning=%v", got.Source, priced, warning)
	}
	contents, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(contents, []byte(sample)) {
		t.Fatalf("invalid refresh replaced good disk cache: err=%v", err)
	}
}
