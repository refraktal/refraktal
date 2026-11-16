package price

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type priceTestTransport func(*http.Request) (*http.Response, error)

func (f priceTestTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

// Port of the review's concurrent-cache reproduction. Differently sized valid
// tables expose a short write interleaving with a longer write to the same file.
func TestReviewConcurrentPriceCache(t *testing.T) {
	p := filepath.Join(t.TempDir(), "prices.json")
	a := []byte(`{"a":{"input_cost_per_token":1,"output_cost_per_token":2},"padding":"` + strings.Repeat("x", 1<<20) + `"}`)
	b := []byte(`{"b":{"input_cost_per_token":3,"output_cost_per_token":4}}`)
	o := LoadOptions{
		CachePath: p,
		URL:       "https://prices.invalid/table",
		Client: &http.Client{Transport: priceTestTransport(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("offline test transport")
		})},
		Now: func() time.Time { return time.Now().Add(MaxAge + time.Minute) },
	}
	var writerErrors int
	for n := 0; n < 100; n++ {
		start := make(chan struct{})
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for _, data := range [][]byte{a, b} {
			wg.Add(1)
			go func(d []byte) {
				defer wg.Done()
				<-start
				errs <- writeCache(p, d)
			}(data)
		}
		close(start)
		wg.Wait()
		close(errs)
		var failures int
		for err := range errs {
			if err != nil {
				failures++
			}
		}
		writerErrors += failures
		body, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		table, warning := Load(context.Background(), o)
		_, _, pricedA := table.Lookup("a")
		_, _, pricedB := table.Lookup("b")
		if !json.Valid(body) {
			t.Fatalf("iteration=%d cache_bytes=%d invalid_json=true writer_errors=%d source=%s model_a_priced=%t model_b_priced=%t warning=%v", n, len(body), failures, table.Source, pricedA, pricedB, warning)
		}
		if !bytes.Equal(body, a) && !bytes.Equal(body, b) {
			t.Fatalf("iteration=%d cache is not either complete input table", n)
		}
		if warning == nil || table.Source != "stale cache" || pricedA == pricedB {
			t.Fatalf("iteration=%d source=%s model_a_priced=%t model_b_priced=%t warning=%v", n, table.Source, pricedA, pricedB, warning)
		}
	}
	if writerErrors != 0 {
		t.Fatalf("concurrent refreshes returned %d writer errors", writerErrors)
	}
}

func TestLoadCacheRenameFailureCleansTemp(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "prices.json")
	if err := os.Mkdir(p, 0o755); err != nil {
		t.Fatal(err)
	}
	table, warning := Load(context.Background(), LoadOptions{
		CachePath: p,
		URL:       "https://prices.invalid/table",
		Client: &http.Client{Transport: priceTestTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(sample))}, nil
		})},
	})
	if warning == nil || !strings.Contains(warning.Error(), "caching failed") || table.Source != "fetched" {
		t.Fatalf("rename failure: source=%s warning=%v", table.Source, warning)
	}
	if _, _, ok := table.Lookup("gpt-6.1-sol"); !ok {
		t.Fatal("cache rename failure discarded validated fetched prices")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "prices.json" || !entries[0].IsDir() {
		t.Fatalf("cache rename failure left temporary files: %v", entries)
	}
}
