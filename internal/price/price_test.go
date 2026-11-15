package price

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"refraktal.dev/refraktal/internal/acp"
)

const sample = `{
  "sample_spec": {"input_cost_per_token": "see docs"},
  "gpt-6.1-sol": {"input_cost_per_token": 2e-06, "output_cost_per_token": 1e-05, "cache_read_input_token_cost": 1e-07, "cache_creation_input_token_cost": 2.5e-06},
  "gemini/gemini-4-carbon": {"input_cost_per_token": 2e-06, "output_cost_per_token": 1e-05, "cache_read_input_token_cost": 1e-07},
  "image-model": {"output_cost_per_image": 0.04}
}`

func near(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

func i64(v int64) *int64 { return &v }

func TestParseAndCost(t *testing.T) {
	tb, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := tb.Lookup("image-model"); ok {
		t.Fatal("an entry without token prices was kept")
	}
	p, key, ok := tb.Lookup("gemini-4-carbon", "gemini/gemini-4-carbon")
	if !ok || key != "gemini/gemini-4-carbon" {
		t.Fatalf("lookup: %v %q", ok, key)
	}
	// The Codex end-to-end run's usage from M3: uncached input, cached reads,
	// output including thought tokens.
	u := acp.Usage{TotalTokens: 182927, InputTokens: 38883, OutputTokens: 684, CachedReadTokens: i64(143360), CachedWriteTokens: i64(0)}
	want := 38883*2e-06 + 684*1e-05 + 143360*1e-07
	if got := p.Cost(u); !near(got, want) {
		t.Fatalf("cost %v, want %v", got, want)
	}
	// No cache prices: cached tokens are priced as input.
	if got := (Price{Input: 1e-6, Output: 2e-6}).Cost(acp.Usage{CachedReadTokens: i64(10), CachedWriteTokens: i64(5)}); !near(got, 15e-6) {
		t.Fatalf("cache fallback cost %v", got)
	}
	tb.Override("opus", Price{Input: 5e-6, Output: 25e-6})
	if p, _, ok := tb.Lookup("opus"); !ok || p.Input != 5e-6 {
		t.Fatal("override not used")
	}
}

func TestLoadCachesAndFallsBack(t *testing.T) {
	var hits atomic.Int32
	var fail atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if fail.Load() {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		w.Write([]byte(sample))
	}))
	defer srv.Close()
	cache := filepath.Join(t.TempDir(), "prices.json")
	now := time.Now()
	o := LoadOptions{CachePath: cache, URL: srv.URL, Now: func() time.Time { return now }}
	ctx := context.Background()

	tb, err := Load(ctx, o)
	if err != nil || tb.Source != "fetched" || hits.Load() != 1 {
		t.Fatalf("first load: %s %v hits=%d", tb.Source, err, hits.Load())
	}
	tb, err = Load(ctx, o)
	if err != nil || tb.Source != "cache" || hits.Load() != 1 {
		t.Fatalf("fresh cache: %s %v hits=%d", tb.Source, err, hits.Load())
	}
	now = now.Add(MaxAge + time.Minute) // stale
	fail.Store(true)
	tb, err = Load(ctx, o)
	if err == nil || tb.Source != "stale cache" || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale fallback: %s %v", tb.Source, err)
	}
	if _, _, ok := tb.Lookup("gpt-6.1-sol"); !ok {
		t.Fatal("stale table lost its prices")
	}
	os.Remove(cache)
	tb, err = Load(ctx, o)
	if err == nil || tb.Source != "none" || !strings.Contains(err.Error(), "unpriced") {
		t.Fatalf("no cache, no fetch: %s %v", tb.Source, err)
	}
	if _, _, ok := tb.Lookup("gpt-6.1-sol"); ok {
		t.Fatal("empty table priced a model")
	}
}

// 3. An override wins for every key, including when the table has the model
// under a prefixed key.
func TestOverrideWinsOverAPrefixedTableKey(t *testing.T) {
	tb, _ := Parse([]byte(sample))
	tb.Override("gemini-4-carbon", Price{Input: 9e-6, Output: 9e-6})
	p, key, ok := tb.Lookup("gemini/gemini-4-carbon", "gemini-4-carbon")
	if !ok || p.Input != 9e-6 || key != "gemini-4-carbon" {
		t.Fatalf("got %+v from %q, want the override", p, key)
	}
}

// 8. Negative or non-finite prices are skipped.
func TestParseSkipsNegativePrices(t *testing.T) {
	tb, err := Parse([]byte(`{"ok": {"input_cost_per_token": 1e-6, "output_cost_per_token": 2e-6},
		"neg": {"input_cost_per_token": -1e-6, "output_cost_per_token": 2e-6},
		"negcache": {"input_cost_per_token": 1e-6, "output_cost_per_token": 2e-6, "cache_read_input_token_cost": -1}}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"neg", "negcache"} {
		if _, _, ok := tb.Lookup(k); ok {
			t.Errorf("%s kept", k)
		}
	}
}
