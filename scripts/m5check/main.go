// Command m5check sends the routing spike's ten sample tasks to the real
// Gemini Decisions API through Refraktal's router and compares each decision
// and policy outcome with the spike's recorded run. It prints a summary
// (decisions, confidence, latency, match); never the key or a raw response.
// Run it through scripts/m5check.sh.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"refraktal.dev/refraktal/internal/route"
)

type fixture struct {
	ID   string            `json:"id"`
	Task string            `json:"task"`
	Repo route.RepoContext `json:"repo"`
	Want struct {
		TaskKind          string   `json:"task_kind"`
		Harness           string   `json:"harness"`
		HarnessConfidence float64  `json:"harness_confidence"`
		Action            string   `json:"action"`
		Harnesses         []string `json:"harnesses"`
	} `json:"want"`
}

func main() {
	root, _ := os.Getwd()
	key, source := route.LoadKey(root, nil)
	if key == "" {
		fmt.Println("no GEMINI_API_KEY in the environment or .env: nothing to check live")
		os.Exit(1)
	}
	fmt.Printf("key from %s (not shown)\n", source)
	var fx []fixture
	b, err := os.ReadFile(filepath.Join(root, "internal", "route", "testdata", "spike_live.json"))
	if err == nil {
		err = json.Unmarshal(b, &fx)
	}
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	type row struct {
		ID, Kind, Harness, Action, Decider string
		Conf, Latency                      float64
		Match                              bool
		Error                              string `json:",omitempty"`
	}
	var rows []row
	var lat []float64
	matches := 0
	for _, f := range fx {
		d, err := route.Gemini{APIKey: key}.Decide(context.Background(), route.Input{Task: f.Task, Repo: f.Repo})
		r := row{ID: f.ID}
		if err != nil {
			r.Error = err.Error()
			rows = append(rows, r)
			continue
		}
		o := route.Policy(d, route.DefaultThresholds)
		r.Kind, r.Harness, r.Action, r.Decider, r.Conf, r.Latency = d.TaskKind.Value, d.Harness.Value, o.Action, d.Decider, d.Harness.Confidence, d.LatencyMS
		r.Match = r.Kind == f.Want.TaskKind && r.Harness == f.Want.Harness && r.Action == f.Want.Action &&
			strings.Join(o.Harnesses, ",") == strings.Join(f.Want.Harnesses, ",") && math.Abs(r.Conf-f.Want.HarnessConfidence) < 1e-6
		if r.Match {
			matches++
		}
		lat = append(lat, r.Latency)
		rows = append(rows, r)
	}
	fmt.Printf("%-24s %-14s %-12s %6s %-9s %8s  %s\n", "sample", "task kind", "harness", "conf", "policy", "latency", "same as spike")
	for _, r := range rows {
		if r.Error != "" {
			fmt.Printf("%-24s error: %s\n", r.ID, r.Error)
			continue
		}
		fmt.Printf("%-24s %-14s %-12s %6.2f %-9s %6.0fms  %v\n", r.ID, r.Kind, r.Harness, r.Conf, r.Action, r.Latency, r.Match)
	}
	sort.Float64s(lat)
	if len(lat) > 0 {
		fmt.Printf("matches %d/%d; latency median %.0f ms, min %.0f, max %.0f\n", matches, len(fx), lat[len(lat)/2], lat[0], lat[len(lat)-1])
	}
	out, _ := json.MarshalIndent(rows, "", " ")
	os.MkdirAll(filepath.Join(root, ".live", "m5"), 0o755)
	os.WriteFile(filepath.Join(root, ".live", "m5", "samples.json"), out, 0o600)
}
