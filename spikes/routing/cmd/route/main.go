// Command route is the spike CLI: it routes one task (argument or stdin) and
// prints the decision as JSON, or runs a sample file and prints a table.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	routing "refraktal.dev/spikes/routing"
)

type sample struct {
	ID     string              `json:"id"`
	Task   string              `json:"task"`
	Repo   routing.RepoContext `json:"repo"`
	Expect struct {
		TaskKind string `json:"task_kind"`
		Action   string `json:"action"`
	} `json:"expect"` // a human's label, used only to mark the table
}

func main() {
	langs := flag.String("langs", "", "comma-separated languages present in the repo")
	files := flag.Int("files", 0, "number of source files in the repo")
	tests := flag.Bool("tests", false, "the repo has tests")
	logPath := flag.String("log", "decisions.jsonl", "decision log to append to (empty to disable)")
	fallbackOnly := flag.Bool("fallback", false, "use the deterministic fallback even if GEMINI_API_KEY is set")
	samples := flag.String("samples", "", "run every task in this JSON file and print a table")
	showRequest := flag.Bool("request", false, "print the exact Gemini request body for the task and exit (no call is made)")
	timeout := flag.Duration("timeout", routing.GeminiTimeout, "timeout for the decision call")
	flag.Parse()

	decider := routing.WithFallback{Fallback: routing.Fallback{}, Why: "GEMINI_API_KEY not set"}
	if *fallbackOnly {
		decider.Why = "fallback forced by -fallback"
	} else if key := os.Getenv("GEMINI_API_KEY"); key != "" {
		decider.Primary = routing.Gemini{APIKey: key}
	}
	run := func(id string, in routing.Input) (routing.Decision, routing.Outcome) {
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		defer cancel()
		d, err := decider.Decide(ctx, in)
		if err != nil {
			fatal(err)
		}
		o := routing.Policy(d, routing.DefaultThresholds)
		if *logPath != "" {
			if err := routing.NewRecord(id, time.Now(), in, d, routing.DefaultThresholds, o).Append(*logPath); err != nil {
				fatal(err)
			}
		}
		return d, o
	}

	if *samples != "" {
		var all []sample
		b, err := os.ReadFile(*samples)
		if err == nil {
			err = json.Unmarshal(b, &all)
		}
		if err != nil {
			fatal(err)
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tDECIDER\tKIND\tHARNESS\tP(FAN)\tWIDTH\tP(DECOMP)\tPOLICY\tHARNESSES\tMS\tEXPECTED")
		for _, s := range all {
			d, o := run(s.ID, routing.Input{Task: s.Task, Repo: s.Repo})
			fmt.Fprintf(tw, "%s\t%s\t%s %.2f%s\t%s %.2f\t%.2f\t%s\t%.2f\t%s%s\t%s\t%.1f\t%s/%s\n",
				s.ID, d.Decider, d.TaskKind.Value, d.TaskKind.Confidence, mark(d.TaskKind.Value, s.Expect.TaskKind),
				d.Harness.Value, d.Harness.Confidence, d.FanOut.Probability, d.FanOutWidth.Value,
				d.NeedsDecomposition.Probability, o.Action, mark(o.Action, s.Expect.Action),
				strings.Join(o.Harnesses, ","), d.LatencyMS, s.Expect.TaskKind, s.Expect.Action)
		}
		tw.Flush()
		return
	}

	task := strings.Join(flag.Args(), " ")
	if task == "" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			fatal(err)
		}
		task = strings.TrimSpace(string(b))
	}
	if task == "" {
		fatal(fmt.Errorf("usage: route [flags] <task>   (or pipe the task on stdin)"))
	}
	in := routing.Input{Task: task, Repo: routing.RepoContext{Files: *files, HasTests: *tests}}
	if *langs != "" {
		in.Repo.Languages = strings.Split(*langs, ",")
	}
	if *showRequest {
		b, _ := json.MarshalIndent(routing.GeminiRequestBody(in), "", "  ")
		fmt.Println(string(b))
		return
	}
	d, o := run("", in)
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(struct {
		Decision routing.Decision `json:"decision"`
		Policy   routing.Outcome  `json:"policy"`
	}{d, o})
}

// mark flags a mismatch with the human label.
func mark(got, want string) string {
	if want == "" || got == want {
		return ""
	}
	return " (!)"
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "route:", err)
	os.Exit(1)
}
