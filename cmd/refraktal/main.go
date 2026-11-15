// Command refraktal is the Refraktal CLI: a thin front door over the
// refraktal package.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"

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
	case "run", "acp", "show":
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
