// Command refraktal is the Refraktal CLI: a thin front door over the
// refraktal package.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"refraktal.dev/refraktal"
	"refraktal.dev/refraktal/internal/budget"
	"refraktal.dev/refraktal/internal/proc"
)

const exitInterrupted = 130

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
	case "acp":
		return acpCmd(ctx, args[1:], os.Stdin, stdout, stderr)
	case "show":
		return showCmd(args[1:], stdout, stderr)
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

// budgetFlags adds --usd, --turns, --mins and --depth to fs.
func budgetFlags(fs *flag.FlagSet, usdHelp string) func() (budget.Limits, error) {
	d := budget.Defaults
	usd := fs.Float64("usd", d.USD, usdHelp)
	turns := fs.Int("turns", d.Turns, "prompts Refraktal may send to harnesses")
	mins := fs.Float64("mins", d.Wall.Minutes(), "wall-clock minutes; never unlimited")
	depth := fs.Int("depth", d.Depth, "levels of Refraktal that may nest below this run")
	return func() (budget.Limits, error) {
		switch {
		case math.IsNaN(*usd) || math.IsInf(*usd, 0) || math.IsNaN(*mins) || math.IsInf(*mins, 0):
			return budget.Limits{}, fmt.Errorf("--usd and --mins must be finite numbers")
		case *mins > budget.MaxWall.Minutes():
			return budget.Limits{}, fmt.Errorf("--mins must be at most %.0f", budget.MaxWall.Minutes())
		case *usd < 0 || *turns < 0 || *mins <= 0:
			return budget.Limits{}, fmt.Errorf("--usd and --turns must not be negative, and --mins must be more than 0")
		}
		return budget.Limits{USD: *usd, Turns: *turns, Wall: time.Duration(*mins * float64(time.Minute)), Depth: *depth}, nil
	}
}

// acpCmd is `refraktal acp`: Refraktal as an ACP agent on stdio. Its budgets
// are the flags, shrunk to a parent's REFRAKTAL_BUDGET when one is set.
func acpCmd(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("acp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	limits := budgetFlags(fs, "dollars (notional on subscriptions) the run may charge")
	if err := fs.Parse(args); err != nil {
		return refraktal.ExitNothingUsable
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "refraktal acp: takes no task; prompts arrive over ACP")
		return refraktal.ExitNothingUsable
	}
	lim, err := limits()
	if err != nil {
		fmt.Fprintf(stderr, "refraktal acp: %v\n", err)
		return refraktal.ExitNothingUsable
	}
	o := refraktal.ServeOptions{Run: refraktal.RunOptions{Budget: &lim, ShellPrefix: selfPath(), Self: selfPath()}}
	if err := refraktal.ServeACP(ctx, stdin, stdout, o); err != nil {
		fmt.Fprintf(stderr, "refraktal acp: %v\n", err)
		return refraktal.ExitNothingUsable
	}
	return refraktal.ExitOK
}

// showCmd is `refraktal show [run] [--json]`.
func showCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "JSON on stdout")
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
	if len(words) > 1 {
		fmt.Fprintln(stderr, "refraktal show: one run at a time")
		return refraktal.ExitNothingUsable
	}
	root, err := refraktal.RepoRoot()
	if err != nil {
		fmt.Fprintf(stderr, "refraktal show: %v\n", err)
		return refraktal.ExitNothingUsable
	}
	id := ""
	if len(words) == 1 {
		id = words[0]
	}
	r, err := refraktal.Show(root, id)
	if err != nil {
		fmt.Fprintf(stderr, "refraktal show: %v\n", err)
		return refraktal.ExitNothingUsable
	}
	if *asJSON {
		refraktal.WriteShowJSON(stdout, r)
	} else {
		refraktal.WriteShowText(stdout, r)
	}
	return refraktal.ExitOK
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
	harnessName := fs.String("harness", "", "run on these harnesses: claude, codex or agy, comma-separated to fan out (default: the router chooses)")
	noApply := fs.Bool("no-apply", false, "leave the result on the worktree's branch")
	asJSON := fs.Bool("json", false, "JSONL on stdout")
	fan := fs.Int("fan", 0, "cap the router's fan-out width (1: one harness; 0: no cap)")
	dry := fs.Bool("dry", false, "plan and route only: no worktree, no harness")
	limits := budgetFlags(fs, "dollar budget, checked before each turn (notional on subscriptions)")
	words, err := parseTaskArgs(fs, args)
	if err != nil {
		return refraktal.ExitNothingUsable
	}
	if *fan < 0 {
		fmt.Fprintln(stderr, "refraktal run: --fan must not be negative")
		return refraktal.ExitNothingUsable
	}
	lim, err := limits()
	if err != nil {
		fmt.Fprintf(stderr, "refraktal run: %v\n", err)
		return refraktal.ExitNothingUsable
	}
	task, err := readTask(ctx, words, stdin)
	if err != nil {
		fmt.Fprintf(stderr, "refraktal run: reading stdin: %v\n", err)
		if errors.Is(err, context.Canceled) {
			return exitInterrupted
		}
		return refraktal.ExitNothingUsable
	}
	enc := json.NewEncoder(stdout)
	res, err := refraktal.Run(ctx, refraktal.RunOptions{Task: task, Harness: *harnessName, NoApply: *noApply, Budget: &lim,
		Dry: *dry, Fan: *fan,
		ShellPrefix: selfPath(), Self: selfPath(),
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
		fmt.Fprintf(stdout, "plan: run %s", strings.Join(res.Plan, ", "))
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
	for _, c := range res.Candidates {
		fmt.Fprintf(stdout, "candidate %s (%s): %s", c.Label, c.Harness, c.Outcome)
		if c.Applies != nil && !*c.Applies {
			fmt.Fprint(stdout, ", does not apply")
		}
		for _, k := range c.Checks {
			fmt.Fprintf(stdout, ", %s %s", k.Name, k.Status)
		}
		if c.Score != nil {
			fmt.Fprintf(stdout, ", pre-score %.2f", *c.Score)
		}
		if c.Flagged != "" {
			fmt.Fprintf(stdout, ", flagged: %s", c.Flagged)
		}
		if c.Branch != "" {
			fmt.Fprintf(stdout, ", branch %s", c.Branch)
		}
		fmt.Fprintln(stdout)
	}
	if v := res.Verdict; v != nil {
		fmt.Fprintf(stdout, "verdict: %s by %s (%s)", cmpOr(v.Winner, "no winner"), v.Method, v.Reason)
		if v.Judge != "" {
			fmt.Fprintf(stdout, "; judge %s", v.Judge)
			if v.JudgedOwn {
				fmt.Fprint(stdout, " (judged its own candidate)")
			}
		}
		if len(v.JudgesTried) > 0 {
			fmt.Fprintf(stdout, "; could not start %s", strings.Join(v.JudgesTried, ", "))
		}
		fmt.Fprintln(stdout)
	}
	fmt.Fprintf(stdout, "%s: %s", cmpOr(res.Harness, "run"), res.Outcome)
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

// parseTaskArgs accepts flags between task words, up to the first --. The
// remaining arguments are task words, even when they look like known flags.
func parseTaskArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var literal []string
	if i := slices.Index(args, "--"); i >= 0 {
		args, literal = args[:i], args[i+1:]
	}
	var words []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return append(words, literal...), nil
		}
		words = append(words, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// readTask joins the task words and, when stdin is a pipe or a file, what it
// holds. A terminal or a socket is never read: an IDE or agent tool may hand
// over a socket that never closes, and reading it would hang.
func readTask(ctx context.Context, words []string, stdin *os.File) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	task := strings.Join(words, " ")
	if stdin == nil {
		return task, nil
	}
	fi, err := stdin.Stat()
	if err != nil || (fi.Mode()&os.ModeNamedPipe == 0 && !fi.Mode().IsRegular()) {
		return task, nil
	}
	var b []byte
	if fi.Mode()&os.ModeNamedPipe != 0 {
		b, err = readTaskPipe(ctx, stdin)
	} else {
		b, err = io.ReadAll(stdin)
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		return "", err
	}
	if in := strings.TrimSpace(string(b)); in != "" {
		task = strings.TrimSpace(task + "\n\n" + in)
	}
	return task, nil
}

// readTaskPipe uses a private, pollable descriptor so cancellation can close
// the read without closing the caller's stdin or leaving a blocked goroutine.
// Inherited stdin can be a blocking descriptor, unlike an os.Pipe reader.
func readTaskPipe(ctx context.Context, stdin *os.File) ([]byte, error) {
	raw, err := stdin.SyscallConn()
	if err != nil {
		return nil, err
	}
	fd := -1
	var flags uintptr
	var dupErr error
	syscall.ForkLock.RLock()
	err = raw.Control(func(original uintptr) {
		var errno syscall.Errno
		flags, _, errno = syscall.Syscall(syscall.SYS_FCNTL, original, syscall.F_GETFL, 0)
		if errno != 0 {
			dupErr = errno
			return
		}
		fd, dupErr = syscall.Dup(int(original))
		if dupErr == nil {
			syscall.CloseOnExec(fd)
			dupErr = syscall.SetNonblock(fd, true)
		}
	})
	syscall.ForkLock.RUnlock()
	if err != nil || dupErr != nil {
		if fd >= 0 {
			_ = syscall.Close(fd)
		}
		return nil, errors.Join(err, dupErr)
	}
	// Dup shares file status flags. Restore the caller's original mode after
	// our reader and cancellation callback have both finished.
	defer raw.Control(func(original uintptr) {
		_ = syscall.SetNonblock(int(original), flags&syscall.O_NONBLOCK != 0)
	})
	reader := os.NewFile(uintptr(fd), stdin.Name())
	defer reader.Close()
	closed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = reader.Close()
		close(closed)
	})
	b, err := io.ReadAll(reader)
	if !stop() {
		<-closed
	}
	return b, err
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

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
