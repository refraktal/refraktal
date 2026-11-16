package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"refraktal.dev/refraktal/internal/proc"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain makes the test binary safe if it is ever started as a shell
// prefix: it then acts as the helper instead of running the tests.
func TestMain(m *testing.M) {
	proc.MaybeShellPrefix()
	if os.Getenv("REFRAKTAL_TEST_STDIN_INTERRUPT") == "1" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		fmt.Fprintln(os.Stderr, "ready")
		os.Exit(runCmd(ctx, []string{"task"}, os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

func TestCommands(t *testing.T) {
	// Outside any git repository: a case that wrongly reached refraktal.Run
	// fails at once instead of starting a real harness.
	t.Chdir(t.TempDir())
	for _, tc := range []struct {
		args []string
		code int
		out  string // expected in stdout or stderr
	}{
		{nil, 3, "usage: refraktal"},
		{[]string{"--help"}, 0, "doctor          check harnesses"},
		{[]string{"run", "fix it", "--harness", "claude,codex"}, 3, "not in a git repository"},
		{[]string{"run", "--fan", "2", "x"}, 3, "not in a git repository"},
		{[]string{"run", "--fan", "-1", "x"}, 3, "--fan must not be negative"},
		{[]string{"run", "--dry", "x"}, 3, "not in a git repository"},
		{[]string{"run", "--mins", "0", "--harness", "claude", "x"}, 3, "--mins must be more than 0"},
		{[]string{"run", "--usd", "-1", "--harness", "claude", "x"}, 3, "must not be negative"},
		{[]string{"run", "--usd", "NaN", "--harness", "claude", "x"}, 3, "finite"},
		{[]string{"run", "--usd", "Inf", "--harness", "claude", "x"}, 3, "finite"},
		{[]string{"run", "--mins", "1e12", "--harness", "claude", "x"}, 3, "at most"},
		{[]string{"show"}, 3, "no run found"},
		{[]string{"show", "a", "b"}, 3, "one run at a time"},
		{[]string{"acp", "a task"}, 3, "takes no task"},
		{[]string{"acp", "--mins", "0"}, 3, "--mins must be more than 0"},
		{[]string{"bogus"}, 3, `unknown command "bogus"`},
		{[]string{"doctor", "--bogus"}, 3, "flag provided but not defined"},
	} {
		var out, errb bytes.Buffer
		code := run(context.Background(), tc.args, &out, &errb)
		if code != tc.code || !strings.Contains(out.String()+errb.String(), tc.out) {
			t.Errorf("%v: code %d, output %q", tc.args, code, out.String()+errb.String())
		}
	}
}

func TestReadTask(t *testing.T) {
	// A pipe and a regular file are read and joined to the words.
	r, w, _ := os.Pipe()
	w.WriteString("details from stdin\n")
	w.Close()
	if got, err := readTask(context.Background(), []string{"fix", "it"}, r); err != nil || got != "fix it\n\ndetails from stdin" {
		t.Fatalf("pipe: %q %v", got, err)
	}
	f := filepath.Join(t.TempDir(), "task")
	os.WriteFile(f, []byte("only stdin"), 0o600)
	fh, _ := os.Open(f)
	if got, _ := readTask(context.Background(), nil, fh); got != "only stdin" {
		t.Fatalf("file: %q", got)
	}
	// A terminal-like device and a socket that never closes are not read.
	null, _ := os.Open(os.DevNull)
	if got, _ := readTask(context.Background(), []string{"x"}, null); got != "x" {
		t.Fatalf("/dev/null: %q", got)
	}
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	sock := os.NewFile(uintptr(fds[0]), "sock")
	defer sock.Close()
	defer syscall.Close(fds[1])
	done := make(chan string, 1)
	go func() { got, _ := readTask(context.Background(), []string{"y"}, sock); done <- got }()
	select {
	case got := <-done:
		if got != "y" {
			t.Fatalf("socket: %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("readTask blocked on a socket that never closes")
	}
}

func TestRunDoubleDashEndsOptionParsing(t *testing.T) {
	t.Chdir(t.TempDir()) // Stop at repository discovery, before any harness runs.
	for _, args := range [][]string{
		{"--", "fix", "--usd", "NaN", "--json", "--help"},
		{"fix", "--", "--fan", "-1", "--harness", "bogus"},
		{"--dry", "fix", "--", "--mins", "0", "--unknown"},
	} {
		var stdout, stderr bytes.Buffer
		code := runCmd(context.Background(), args, nil, &stdout, &stderr)
		if code != 3 || !strings.Contains(stderr.String(), "not in a git repository") || stdout.Len() != 0 {
			t.Errorf("%q: code %d, stdout %q, stderr %q", args, code, stdout.String(), stderr.String())
		}
	}
}

func TestTaskArgumentsAfterDoubleDashAreUnchanged(t *testing.T) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "")
	fan := fs.Int("fan", 0, "")
	args := []string{"first", "--fan", "2", "second", "--", "  keep\nspacing  ", "--json", "--fan", "-1", "--", "last"}
	got, err := parseTaskArgs(fs, args)
	want := []string{"first", "second", "  keep\nspacing  ", "--json", "--fan", "-1", "--", "last"}
	if err != nil || !slices.Equal(got, want) || *asJSON || *fan != 2 {
		t.Fatalf("words %q, json %v, fan %d, error %v; want %q, false, 2, nil", got, *asJSON, *fan, err, want)
	}
	if task, err := readTask(context.Background(), got, nil); err != nil || task != "first second   keep\nspacing   --json --fan -1 -- last" {
		t.Fatalf("task %q, error %v", task, err)
	}
}

func TestRunInterruptsOpenStdin(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if _, err := w.WriteString("partial task"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- runCmd(ctx, nil, r, io.Discard, io.Discard) }()
	// The writer stays open until runCmd has returned; cancellation must
	// interrupt collection without waiting for the producer to send EOF.
	time.Sleep(25 * time.Millisecond)
	cancel()
	select {
	case code := <-done:
		if code != 130 {
			t.Fatalf("cancelled stdin collection returned %d, want 130", code)
		}
	case <-time.After(time.Second):
		w.Close()
		<-done
		t.Fatal("runCmd waited for the open stdin producer after cancellation")
	}
	// Collection must leave the caller's descriptor open and stop reading
	// it. Bytes sent after return belong to the caller's next read.
	const next = "next read belongs to the caller"
	if _, err := w.WriteString(next); err != nil {
		t.Fatal(err)
	}
	w.Close()
	if remaining, err := io.ReadAll(r); err != nil || !strings.HasSuffix(string(remaining), next) {
		t.Fatalf("stdin unusable after cancellation: %q, %v", remaining, err)
	}
}

func TestRunSIGINTInterruptsOpenStdin(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "REFRAKTAL_TEST_STDIN_INTERRUPT=1")
	cmd.Stdin = r
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()
	ready := make([]byte, len("ready\n"))
	if _, err := io.ReadFull(stderr, ready); err != nil || string(ready) != "ready\n" {
		t.Fatalf("signal helper not ready: %q, %v", ready, err)
	}
	// More than a pipe can buffer: completing this write proves that the
	// helper entered stdin collection before receiving the signal.
	written := make(chan error, 1)
	go func() {
		_, err := w.WriteString(strings.Repeat("partial task ", 1<<18))
		written <- err
	}()
	select {
	case err := <-written:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("signal helper did not read the task pipe")
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if cmd.ProcessState.ExitCode() != 130 {
			t.Fatalf("SIGINT exit = %d, want 130: %v", cmd.ProcessState.ExitCode(), err)
		}
	case <-time.After(2 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatal("SIGINT did not interrupt stdin collection with its producer still open")
	}
}
