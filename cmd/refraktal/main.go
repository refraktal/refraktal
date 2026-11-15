// Command refraktal is the Refraktal CLI: a thin front door over the
// refraktal package.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"strings"
	"time"

	"refraktal.dev/refraktal"
	"refraktal.dev/refraktal/internal/budget"
	"refraktal.dev/refraktal/internal/proc"
)

const usage = `usage: refraktal <command> [flags]        task comes from the arg, stdin, or both
  run [task]      plan, dispatch, collect, judge; apply the winner to the tree
  acp             speak ACP on stdio; any client, including a Refraktal, can drive it
  show [run]      print a run: routing decisions, verdicts, cost (default: latest)
  doctor          check harnesses, auth and the Decider; exit nonzero if unusable
  --harness a,b --fan N        run: override the router (pin harnesses, force width)
  --no-apply                   run: leave candidates in worktrees, print branches
  --dry                        run: plan and route only, spawn nothing
  --usd N --turns N --mins N --depth N   run, acp: budgets; children get less
  --json                       run, show, doctor: JSONL on stdout
`

func main() {
	proc.MaybeShellPrefix() // when started as a harness's shell prefix, never returns
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return refraktal.ExitNothingUsable
	}
	switch args[0] {
	case "doctor":
		return doctor(ctx, args[1:], stdout, stderr)
	case "run":
		return runCmd(ctx, args[1:], os.Stdin, stdout, stderr)
	case "acp", "show":
		fmt.Fprintf(stderr, "refraktal %s: not implemented yet in %s\n", args[0], refraktal.Version)
		return refraktal.ExitNothingUsable
	case "-h", "-help", "--help", "help":
		fmt.Fprint(stdout, usage)
		return refraktal.ExitOK
	case "-version", "--version", "version":
		fmt.Fprintf(stdout, "refraktal %s\n", refraktal.Version)
		return refraktal.ExitOK
	default:
		fmt.Fprintf(stderr, "refraktal: unknown command %q\n\n%s", args[0], usage)
		return refraktal.ExitNothingUsable
	}
}

func doctor(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "JSONL on stdout")
	if err := fs.Parse(args); err != nil {
		return refraktal.ExitNothingUsable
	}
	rep, err := refraktal.Doctor(ctx, refraktal.DoctorOptions{ShellPrefix: selfPath()})
	if err != nil {
		fmt.Fprintf(stderr, "refraktal doctor: %v\n", err)
		return refraktal.ExitNothingUsable
	}
	if *asJSON {
		if err := refraktal.WriteDoctorJSON(stdout, rep); err != nil {
			fmt.Fprintf(stderr, "refraktal doctor: %v\n", err)
		}
	} else {
		refraktal.WriteDoctorText(stdout, rep)
	}
	return rep.ExitCode()
}

// runCmd is `refraktal run [task] [flags]`. Flags may come before or after
// the task. The task comes from the argument, stdin, or both.
func runCmd(ctx context.Context, args []string, stdin *os.File, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	harnessName := fs.String("harness", "", "run on this harness: claude, codex or agy (default: the router chooses)")
	noApply := fs.Bool("no-apply", false, "leave the result on the worktree's branch")
	asJSON := fs.Bool("json", false, "JSONL on stdout")
	fan := fs.Int("fan", 0, "fan-out width: 1 for one harness (wider fan-out arrives in M6)")
	dry := fs.Bool("dry", false, "plan and route only: no worktree, no harness")
	d := budget.Defaults
	usd := fs.Float64("usd", d.USD, "dollar budget, checked before each turn (notional on subscriptions)")
	turns := fs.Int("turns", d.Turns, "prompts Refraktal may send to harnesses")
	mins := fs.Float64("mins", d.Wall.Minutes(), "wall-clock minutes; never unlimited")
	depth := fs.Int("depth", d.Depth, "levels of Refraktal that may nest below this run")
	var words []string
	for {
		if err := fs.Parse(args); err != nil {
			return refraktal.ExitNothingUsable
		}
		if fs.NArg() == 0 {
			break
		}
		words = append(words, fs.Arg(0))
		args = fs.Args()[1:]
	}
	switch {
	case *fan > 1 || *fan < 0:
		fmt.Fprintln(stderr, "refraktal run: fan-out to several harnesses arrives in M6; use --fan 1 or leave it to the router")
		return refraktal.ExitNothingUsable
	case math.IsNaN(*usd) || math.IsInf(*usd, 0) || math.IsNaN(*mins) || math.IsInf(*mins, 0):
		fmt.Fprintln(stderr, "refraktal run: --usd and --mins must be finite numbers")
		return refraktal.ExitNothingUsable
	case *mins > budget.MaxWall.Minutes():
		fmt.Fprintf(stderr, "refraktal run: --mins must be at most %.0f\n", budget.MaxWall.Minutes())
		return refraktal.ExitNothingUsable
	case *usd < 0 || *turns < 0 || *mins <= 0:
		fmt.Fprintln(stderr, "refraktal run: --usd and --turns must not be negative, and --mins must be more than 0")
		return refraktal.ExitNothingUsable
	case strings.Contains(*harnessName, ","):
		fmt.Fprintln(stderr, "refraktal run: one harness at a time until fan-out lands")
		return refraktal.ExitNothingUsable
	}
	task, err := readTask(words, stdin)
	if err != nil {
		fmt.Fprintf(stderr, "refraktal run: reading stdin: %v\n", err)
		return refraktal.ExitNothingUsable
	}
	enc := json.NewEncoder(stdout)
	lim := budget.Limits{USD: *usd, Turns: *turns, Wall: time.Duration(*mins * float64(time.Minute)), Depth: *depth}
	res, err := refraktal.Run(ctx, refraktal.RunOptions{Task: task, Harness: *harnessName, NoApply: *noApply, Budget: &lim,
		Dry: *dry, Fan: *fan,
		ShellPrefix: selfPath(),
		OnNotice: func(n string) {
			if *asJSON {
				enc.Encode(map[string]string{"type": "notice", "text": n})
			} else {
				fmt.Fprintln(stderr, "notice: "+n)
			}
		}})
	if err != nil {
		fmt.Fprintf(stderr, "refraktal run: %v\n", err)
		return refraktal.ExitNothingUsable
	}
	if *asJSON {
		enc.Encode(struct {
			Type string `json:"type"`
			refraktal.RunResult
			Exit int `json:"exit"`
		}{"result", res, res.ExitCode()})
		return res.ExitCode()
	}
	if res.Outcome == refraktal.RunPlanned {
		fmt.Fprintf(stdout, "plan: run %s", res.Harness)
		if rt := res.Route; rt != nil {
			d := rt.Decision
			fmt.Fprintf(stdout, " (router: %s, task kind %s, harness confidence %.2f, policy %s)", d.Decider, d.TaskKind.Value, d.Harness.Confidence, rt.Outcome.Action)
		}
		fmt.Fprintf(stdout, "\nrun folder: %s\n", res.Run)
		return res.ExitCode()
	}
	if res.Reply != "" {
		fmt.Fprintln(stdout, res.Reply)
		fmt.Fprintln(stdout)
	}
	fmt.Fprintf(stdout, "%s: %s", res.Harness, res.Outcome)
	if len(res.Files) > 0 {
		fmt.Fprintf(stdout, ", %d files: %s", len(res.Files), strings.Join(res.Files, " "))
	}
	fmt.Fprintf(stdout, " (permissions: %d allowed, %d denied)\n", res.Allowed, res.Denied)
	b := res.Budget
	fmt.Fprintf(stdout, "budget: $%.4f of $%.2f charged", b.Spent, b.Limits.USD)
	if b.Estimated > 0 {
		fmt.Fprintf(stdout, " (plus $%.4f estimated for a cut-off call)", b.Estimated)
	}
	for _, c := range b.Charges {
		fmt.Fprintf(stdout, "; %s $%.4f", c.Source, c.USD)
	}
	fmt.Fprintf(stdout, "; turns %d of %d\n", b.Turns, b.Limits.Turns)
	if res.Branch != "" {
		fmt.Fprintf(stdout, "branch %s, worktree %s\n", res.Branch, res.Worktree)
	}
	if res.Error != "" {
		fmt.Fprintf(stdout, "error: %s\n", res.Error)
	}
	fmt.Fprintf(stdout, "run folder: %s\n", res.Run)
	return res.ExitCode()
}

// readTask joins the task words and, when stdin is a pipe or a file, what it
// holds. A terminal or a socket is never read: an IDE or agent tool may hand
// over a socket that never closes, and reading it would hang.
func readTask(words []string, stdin *os.File) (string, error) {
	task := strings.Join(words, " ")
	if stdin == nil {
		return task, nil
	}
	fi, err := stdin.Stat()
	if err != nil || (fi.Mode()&os.ModeNamedPipe == 0 && !fi.Mode().IsRegular()) {
		return task, nil
	}
	b, err := io.ReadAll(stdin)
	if err != nil {
		return "", err
	}
	if in := strings.TrimSpace(string(b)); in != "" {
		task = strings.TrimSpace(task + "\n\n" + in)
	}
	return task, nil
}

// selfPath is this binary, which handles the shell-prefix helper mode in
// main; "" if it cannot be found.
func selfPath() string {
	p, err := os.Executable()
	if err != nil {
		return ""
	}
	return p
}
