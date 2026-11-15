// Command refraktal is the Refraktal CLI: a thin front door over the
// refraktal package.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"refraktal.dev/refraktal"
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
	rep, err := refraktal.Doctor(ctx, refraktal.DoctorOptions{})
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
	harnessName := fs.String("harness", "", "run on this harness: claude, codex or agy")
	noApply := fs.Bool("no-apply", false, "leave the result on the worktree's branch")
	asJSON := fs.Bool("json", false, "JSONL on stdout")
	fan := fs.Int("fan", 0, "fan-out width (not yet: M6)")
	dry := fs.Bool("dry", false, "plan and route only (not yet: M5)")
	var usd, turns, mins, depth string
	fs.StringVar(&usd, "usd", "", "budget (not yet: M4)")
	fs.StringVar(&turns, "turns", "", "budget (not yet: M4)")
	fs.StringVar(&mins, "mins", "", "budget (not yet: M4)")
	fs.StringVar(&depth, "depth", "", "budget (not yet: M4)")
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
	case *fan != 0, *dry, usd != "", turns != "", mins != "", depth != "":
		fmt.Fprintln(stderr, "refraktal run: --fan, --dry and the budget flags are not implemented yet")
		return refraktal.ExitNothingUsable
	case *harnessName == "":
		fmt.Fprintln(stderr, "refraktal run: --harness is required until routing lands")
		return refraktal.ExitNothingUsable
	case strings.Contains(*harnessName, ","):
		fmt.Fprintln(stderr, "refraktal run: one harness at a time until fan-out lands")
		return refraktal.ExitNothingUsable
	}
	task := strings.Join(words, " ")
	if stdin != nil {
		if fi, err := stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice == 0 {
			b, err := io.ReadAll(stdin)
			if err != nil {
				fmt.Fprintf(stderr, "refraktal run: reading stdin: %v\n", err)
				return refraktal.ExitNothingUsable
			}
			if in := strings.TrimSpace(string(b)); in != "" {
				task = strings.TrimSpace(task + "\n\n" + in)
			}
		}
	}
	enc := json.NewEncoder(stdout)
	res, err := refraktal.Run(ctx, refraktal.RunOptions{Task: task, Harness: *harnessName, NoApply: *noApply,
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
	if res.Reply != "" {
		fmt.Fprintln(stdout, res.Reply)
		fmt.Fprintln(stdout)
	}
	fmt.Fprintf(stdout, "%s: %s", res.Harness, res.Outcome)
	if len(res.Files) > 0 {
		fmt.Fprintf(stdout, ", %d files: %s", len(res.Files), strings.Join(res.Files, " "))
	}
	fmt.Fprintf(stdout, " (permissions: %d allowed, %d denied)\n", res.Allowed, res.Denied)
	if res.Branch != "" {
		fmt.Fprintf(stdout, "branch %s, worktree %s\n", res.Branch, res.Worktree)
	}
	if res.Error != "" {
		fmt.Fprintf(stdout, "error: %s\n", res.Error)
	}
	fmt.Fprintf(stdout, "run folder: %s\n", res.Run)
	return res.ExitCode()
}
