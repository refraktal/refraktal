package refraktal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeShowLog(t *testing.T, root, id, log string) string {
	t.Helper()
	dir := filepath.Join(root, ".refraktal", "runs", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestShowOrdersRunsByFirstEventTime(t *testing.T) {
	root := t.TempDir()
	const older = "20261116-120000-ffffff"
	const newer = "20261116-120000-000000"
	writeShowLog(t, root, older, `{"event":"run_start","t":"2026-11-16T12:00:00.1Z","task":"older"}`+"\n"+
		`{"event":"run_end","t":"2026-11-16T13:00:00Z","outcome":"applied"}`+"\n")
	writeShowLog(t, root, newer, `{"event":"notice","t":"2026-11-16T12:00:00.9Z"}`+"\n"+
		`{"event":"run_start","t":"2026-11-16T12:00:01Z","task":"newer"}`+"\n")
	got, err := Show(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != newer {
		t.Fatalf("latest run = %q, want %q from first event timestamps", got.ID, newer)
	}

	// Nested runs use the same chronological order, with IDs breaking ties.
	parent := writeShowLog(t, root, "parent", `{"event":"run_start"}`+"\n")
	for _, tc := range []struct{ id, stamp string }{
		{older, "2026-11-16T12:00:00.1Z"},
		{newer, "2026-11-16T12:00:00.9Z"},
		{"20261116-120000-000001", "2026-11-16T12:00:00.9Z"},
	} {
		// Nested logs live directly under the harness scratch folder.
		dir := filepath.Join(parent, "refraktal", "runs", tc.id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(`{"event":"run_start","t":"`+tc.stamp+`"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err = Show(root, "parent")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, run := range got.Nested {
		ids = append(ids, run.ID)
	}
	if strings.Join(ids, ",") != older+","+newer+",20261116-120000-000001" {
		t.Fatalf("nested run order = %v", ids)
	}
}

func TestShowLatestPreservesEnumerationTruncation(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 10_001; i++ {
		writeShowLog(t, root, fmt.Sprintf("run-%d", i), `{"event":"run_start","t":"2026-11-16T12:00:00Z"}`+"\n")
	}
	got, err := Show(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Truncated {
		t.Fatal("latest run lost truncation from incomplete directory enumeration")
	}
	var text, encoded strings.Builder
	WriteShowText(&text, got)
	if !strings.Contains(text.String(), "latest run selection may be incomplete") {
		t.Fatalf("latest run uncertainty is not disclosed:\n%s", text.String())
	}
	if err := WriteShowJSON(&encoded, got); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(encoded.String()), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["truncated"] != true || doc["latest_incomplete"] != true {
		t.Fatalf("JSON lost latest selection truncation: %s", encoded.String())
	}
}

func TestShowLatestReportsTimestampScanFailure(t *testing.T) {
	root := t.TempDir()
	writeShowLog(t, root, "readable", `{"event":"run_start","t":"2026-11-16T12:00:00Z"}`+"\n")
	// An unreadable first event prevents chronological selection, even if
	// another run can still be rendered within the shared byte budget.
	writeShowLog(t, root, "oversized", strings.Repeat("x", (16<<20)+1)+"\n")
	got, err := Show(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Truncated || !got.LatestIncomplete {
		t.Fatalf("failed timestamp scan did not disclose incomplete selection: truncated=%v latest_incomplete=%v", got.Truncated, got.LatestIncomplete)
	}
}

func TestShowSeparatesSpendAndRetainedReservations(t *testing.T) {
	for _, tc := range []struct {
		name, charges string
	}{
		{"cancelled_child", `[{"source":"reported","usd":1},{"source":"computed","usd":0.25},{"source":"estimated","usd":0.5}]`},
		{"nested_reservation", `[{"source":"reported","usd":1},{"source":"computed","usd":0.25},{"source":"retained","usd":3.75}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeShowLog(t, root, "run", `{"event":"run_start"}`+"\n"+
				`{"event":"run_end","outcome":"failed","budget":{"spent_usd":5,"reported_usd":1.25,"limits":{"usd":10},"charges":`+tc.charges+`}}`+"\n")
			got, err := Show(root, "run")
			if err != nil {
				t.Fatal(err)
			}
			if got.Costs.Charged != 1.25 {
				t.Fatalf("charged = %v, want actual spend 1.25; reservations are not spend", got.Costs.Charged)
			}
			var text, encoded strings.Builder
			WriteShowText(&text, got)
			for _, want := range []string{"$1.2500", "retained reservations: $3.7500"} {
				if !strings.Contains(text.String(), want) {
					t.Errorf("text lacks %q:\n%s", want, text.String())
				}
			}
			if err := WriteShowJSON(&encoded, got); err != nil {
				t.Fatal(err)
			}
			// by_source is an object, so inspect the two numeric fields independently.
			var raw struct {
				Costs map[string]json.RawMessage `json:"costs"`
			}
			if err := json.Unmarshal([]byte(encoded.String()), &raw); err != nil {
				t.Fatal(err)
			}
			for key, want := range map[string]float64{"charged_usd": 1.25, "retained_usd": 3.75} {
				var value float64
				if err := json.Unmarshal(raw.Costs[key], &value); err != nil {
					t.Fatalf("JSON lacks numeric %s: %s", key, encoded.String())
				}
				if value != want {
					t.Errorf("%s = %v, want %v", key, value, want)
				}
			}
		})
	}
}
