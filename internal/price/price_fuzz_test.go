package price

import (
	"bytes"
	"encoding/json"
	"math"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"refraktal.dev/refraktal/internal/acp"
)

func FuzzPriceTable(f *testing.F) {
	for _, s := range []string{
		sample,
		`{"m":{"input_cost_per_token":1e-6,"output_cost_per_token":2e-6,"cache_read_input_token_cost":0}}`,
		`{"ok":{"input_cost_per_token":1e-6,"output_cost_per_token":2e-6},"neg":{"input_cost_per_token":-1e-6,"output_cost_per_token":2e-6},"negcache":{"input_cost_per_token":1e-6,"output_cost_per_token":2e-6,"cache_read_input_token_cost":-1}}`,
		"", "null", "{", `{"m":`,
		`{"m":{"input_cost_per_token":1e9999,"output_cost_per_token":0}}`,
		`{"m":{"input_cost_per_token":"NaN","output_cost_per_token":"Inf"}}`,
		`{"m":{"input_cost_per_token":1,"input_cost_per_token":-1,"output_cost_per_token":0}}`,
		"\xff\xfe\x00", strings.Repeat("[", 10001) + strings.Repeat("]", 10001), strings.Repeat("x", 200<<10),
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		deadline := 2*time.Second + time.Duration(len(b)/(1<<20))*100*time.Millisecond
		watchdog := time.AfterFunc(deadline, func() { panic("price parser exceeded work deadline") })
		defer watchdog.Stop()
		start := time.Now()
		table, err := Parse(b)
		elapsed := time.Since(start)
		watchdog.Stop()
		runtime.ReadMemStats(&after)
		if used, limit := after.TotalAlloc-before.TotalAlloc, uint64(len(b))*64+(1<<20); used > limit {
			t.Fatalf("Parse allocated %d bytes (limit %d)", used, limit)
		}
		if elapsed > deadline {
			t.Fatalf("Parse took %s", elapsed)
		}
		if err != nil {
			if len(table.prices) != 0 || len(table.overrides) != 0 || table.Source != "" {
				t.Fatal("rejected table returned partial prices")
			}
			return
		}
		if len(table.prices) == 0 {
			t.Fatal("accepted empty table")
		}
		var raw map[string]json.RawMessage
		if json.Unmarshal(b, &raw) != nil {
			t.Fatal("accepted invalid JSON")
		}
		for key, p := range table.prices {
			for _, rate := range []float64{p.Input, p.Output, p.CacheRead, p.CacheWrite} {
				if math.IsNaN(rate) || math.IsInf(rate, 0) || rate < 0 {
					t.Fatal("accepted non-finite or negative rate")
				}
			}
			got, selected, ok := table.Lookup(key)
			if !ok || selected != key || got != p {
				t.Fatal("lookup disagrees with parsed table")
			}
			for _, field := range []struct {
				name  string
				has   bool
				rate  float64
				usage acp.Usage
			}{
				{"cache_read_input_token_cost", p.HasCacheRead, p.CacheRead, acp.Usage{CachedReadTokens: i64(1)}},
				{"cache_creation_input_token_cost", p.HasCacheWrite, p.CacheWrite, acp.Usage{CachedWriteTokens: i64(1)}},
			} {
				present := priceFieldPresent(t, raw[key], field.name)
				if field.has != present {
					t.Fatal("missing and zero cache rate confused")
				}
				want := p.Input
				if present {
					want = field.rate
				}
				if p.Cost(field.usage) != want {
					t.Fatal("cache fallback does not match presence flag")
				}
			}
			encoded, marshalErr := json.Marshal(p)
			var roundTrip Price
			if marshalErr != nil || json.Unmarshal(encoded, &roundTrip) != nil || roundTrip != p {
				t.Fatal("price does not round-trip")
			}
		}
		// A typed-decode error after a valid table must not leak its entries.
		rejected, rejectErr := Parse(append(append([]byte{}, b...), '!'))
		if rejectErr == nil || len(rejected.prices) != 0 {
			t.Fatal("invalid suffix exposed a partial table")
		}
		again, againErr := Parse(b)
		if againErr != nil || !reflect.DeepEqual(table, again) {
			t.Fatal("Parse is not deterministic")
		}
	})
}

// Struct decoding folds field names and applies duplicate fields in source
// order. A map lookup would incorrectly treat a mixed-case zero rate as absent.
func priceFieldPresent(t *testing.T, raw []byte, name string) bool {
	t.Helper()
	d := json.NewDecoder(bytes.NewReader(raw))
	if token, err := d.Token(); err != nil || token != json.Delim('{') {
		t.Fatal("priced a non-object")
	}
	present := false
	for d.More() {
		key, err := d.Token()
		if err != nil {
			t.Fatal(err)
		}
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			t.Fatal(err)
		}
		if strings.EqualFold(key.(string), name) {
			present = !bytes.Equal(bytes.TrimSpace(value), []byte("null"))
		}
	}
	return present
}
