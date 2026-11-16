// Package price turns token counts into dollars, from LiteLLM's
// model_prices_and_context_window.json, cached on disk, with per-model
// overrides from the config.
//
// Dollars are a budget unit, not a bill: on subscription logins they are
// notional.
package price

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"refraktal.dev/refraktal/internal/acp"
)

// DefaultURL is LiteLLM's public price table.
const DefaultURL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"

// MaxAge is how long a cached table is used before it is fetched again.
const MaxAge = 24 * time.Hour

// Price is a model's price in US dollars per token.
type Price struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
	// Whether a cache rate is given at all: a rate of exactly 0 is a real
	// price; a missing one means "priced as input".
	HasCacheRead  bool `json:"has_cache_read"`
	HasCacheWrite bool `json:"has_cache_write"`
}

// Cost prices one turn's usage. The harnesses report input tokens without
// the cached ones, cached reads and writes separately, and thought tokens as
// part of the output.
func (p Price) Cost(u acp.Usage) float64 {
	read, write := p.CacheRead, p.CacheWrite
	if !p.HasCacheRead {
		read = p.Input
	}
	if !p.HasCacheWrite {
		write = p.Input
	}
	c := float64(u.InputTokens)*p.Input + float64(u.OutputTokens)*p.Output
	if u.CachedReadTokens != nil {
		c += float64(*u.CachedReadTokens) * read
	}
	if u.CachedWriteTokens != nil {
		c += float64(*u.CachedWriteTokens) * write
	}
	return c
}

// Table holds prices by model key.
type Table struct {
	prices    map[string]Price
	overrides map[string]Price // from the config; checked for every key before the table
	Source    string           // "cache", "fetched", "stale cache" or "none"
}

// Lookup returns the price for the first key that has one, and that key. An
// override for any of the keys wins over the table for all of them.
func (t Table) Lookup(keys ...string) (Price, string, bool) {
	for _, k := range keys {
		if p, ok := t.overrides[k]; ok {
			return p, k, true
		}
	}
	for _, k := range keys {
		if p, ok := t.prices[k]; ok {
			return p, k, true
		}
	}
	return Price{}, "", false
}

// Override sets the price for a model; it wins over the table under any key.
func (t *Table) Override(model string, p Price) {
	if t.overrides == nil {
		t.overrides = map[string]Price{}
	}
	t.overrides[model] = p
}

// Parse reads LiteLLM's JSON. Entries without an input and an output price
// are skipped.
func Parse(b []byte) (Table, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return Table{}, err
	}
	t := Table{prices: map[string]Price{}}
	for k, v := range raw {
		var e struct {
			Input      *float64 `json:"input_cost_per_token"`
			Output     *float64 `json:"output_cost_per_token"`
			CacheRead  *float64 `json:"cache_read_input_token_cost"`
			CacheWrite *float64 `json:"cache_creation_input_token_cost"`
		}
		if json.Unmarshal(v, &e) != nil || e.Input == nil || e.Output == nil || !valid(e.Input, e.Output, e.CacheRead, e.CacheWrite) {
			continue
		}
		p := Price{Input: *e.Input, Output: *e.Output}
		if e.CacheRead != nil {
			p.CacheRead, p.HasCacheRead = *e.CacheRead, true
		}
		if e.CacheWrite != nil {
			p.CacheWrite, p.HasCacheWrite = *e.CacheWrite, true
		}
		t.prices[k] = p
	}
	if len(t.prices) == 0 {
		return t, fmt.Errorf("price table has no priced models")
	}
	return t, nil
}

// LoadOptions say where the table comes from.
type LoadOptions struct {
	CachePath string // default os.UserCacheDir()/refraktal/prices.json
	URL       string // default DefaultURL
	Client    *http.Client
	Now       func() time.Time
}

// DefaultCachePath is os.UserCacheDir()/refraktal/prices.json.
func DefaultCachePath() (string, error) {
	d, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "refraktal", "prices.json"), nil
}

// Load returns the cached table if it is fresh; otherwise it fetches it and
// updates the cache. If the fetch fails it falls back to a valid stale cache;
// without one, every model is unpriced. The error, if any, is a warning: Load
// always returns a usable table.
func Load(ctx context.Context, o LoadOptions) (Table, error) {
	if o.URL == "" {
		o.URL = DefaultURL
	}
	if o.Client == nil {
		o.Client = &http.Client{Timeout: 15 * time.Second}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.CachePath == "" {
		p, err := DefaultCachePath()
		if err != nil {
			return Table{Source: "none"}, err
		}
		o.CachePath = p
	}
	cached, cerr := os.ReadFile(o.CachePath)
	var snapshot Table
	if cerr == nil {
		snapshot, cerr = Parse(cached)
		if cerr != nil {
			cerr = fmt.Errorf("invalid cached copy: %w", cerr)
		} else {
			if fi, err := os.Stat(o.CachePath); err == nil && o.Now().Sub(fi.ModTime()) < MaxAge {
				snapshot.Source = "cache"
				return snapshot, nil
			}
		}
	}
	b, ferr := fetch(ctx, o)
	if ferr == nil {
		if t, err := Parse(b); err == nil {
			t.Source = "fetched"
			if err := writeCache(o.CachePath, b); err != nil {
				return t, fmt.Errorf("price table: caching failed: %w", err)
			}
			return t, nil
		} else {
			ferr = err
		}
	}
	if cerr == nil {
		snapshot.Source = "stale cache"
		return snapshot, fmt.Errorf("price table: fetch failed (%v); using the stale cached copy", ferr)
	}
	return Table{Source: "none", prices: map[string]Price{}},
		fmt.Errorf("price table: fetch failed (%v) and no valid cached copy (%v): every model is unpriced", ferr, cerr)
}

func fetch(ctx context.Context, o LoadOptions) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "refraktal")
	resp, err := o.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}

func writeCache(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	// Publish the public price table with the cache's previous 0644 mode.
	if err := f.Chmod(0o644); err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// valid reports whether every price present is finite and not negative.
func valid(prices ...*float64) bool {
	for _, p := range prices {
		if p != nil && (*p < 0 || math.IsNaN(*p) || math.IsInf(*p, 0)) {
			return false
		}
	}
	return true
}
