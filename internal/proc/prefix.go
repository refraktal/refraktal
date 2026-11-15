package proc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// Claude Code's Bash tool starts each command's shell as the leader of a new
// session, so a command's background children are outside the harness's
// session and the session sweep misses them. Claude Code calls
// CLAUDE_CODE_SHELL_PREFIX, if set, with the whole command string as one
// argument, from inside that new session. Refraktal sets it, per run and in
// the environment only, to its own binary in the helper mode below: the
// helper records the session's leader and execs the command. At shutdown
// every recorded session is swept too.

// Environment variables of the shell-prefix helper.
const (
	HelperEnv    = "REFRAKTAL_HELPER"      // "shell-prefix" selects the helper
	SessionsEnv  = "REFRAKTAL_SESSIONS"    // the file the helper appends session leaders to
	NextEnv      = "REFRAKTAL_PREFIX_NEXT" // a prefix the user had set, to chain to
	PrefixVarEnv = "REFRAKTAL_PREFIX_VAR"  // the harness's prefix variable, restored for the command
	helperPrefix = "shell-prefix"
)

// MaybeShellPrefix runs the shell-prefix helper and never returns, if this
// process was started as one; otherwise it returns at once. Call it first in
// main (and in TestMain of tests that start the helper).
func MaybeShellPrefix() {
	if os.Getenv(HelperEnv) != helperPrefix {
		return
	}
	if err := shellPrefix(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "refraktal shell prefix:", err)
		os.Exit(126)
	}
}

func shellPrefix(args []string) error {
	if len(args) == 0 {
		return errors.New("no command")
	}
	// Record, then check the shutdown marker, then run. The parent creates
	// the marker before its final read, so a command whose session was
	// recorded too late to be read does not run at all. If the session cannot
	// be recorded, the command does not run either: it would escape the sweep.
	sessions := os.Getenv(SessionsEnv)
	if err := recordSession(sessions); err != nil {
		return fmt.Errorf("session not recorded, so the command was not run: %w", err)
	}
	if _, err := os.Stat(ShutdownMarker(sessions)); err == nil {
		return errors.New("the run is shutting down, so the command was not run")
	}
	next, prefixVar := os.Getenv(NextEnv), os.Getenv(PrefixVarEnv)
	if self, err := os.Executable(); err == nil && next != "" && SameFile(next, self) {
		next = "" // chaining to the helper itself would loop
	}
	// The command and its children must not become helpers, and must see the
	// harness's prefix variable as the user had it (or not at all): a nested
	// claude or refraktal would otherwise route every command through us.
	env := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		name, _, _ := cutEq(kv)
		if name == HelperEnv || name == SessionsEnv || name == NextEnv || name == PrefixVarEnv || (prefixVar != "" && name == prefixVar) {
			continue
		}
		env = append(env, kv)
	}
	if prefixVar != "" && next != "" {
		env = append(env, prefixVar+"="+next)
	}
	var argv []string
	if next != "" {
		argv = append([]string{next}, args...)
	} else {
		argv = append([]string{commandShell(), "-c"}, args...)
	}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return err
	}
	return syscall.Exec(path, argv, env)
}

// commandShell picks the shell Claude Code runs commands with: the user's
// shell if it is bash or zsh, otherwise bash from PATH.
func commandShell() string {
	for _, v := range []string{"CLAUDE_CODE_SHELL", "SHELL"} {
		sh := os.Getenv(v)
		if b := filepath.Base(sh); (b == "bash" || b == "zsh") && executable(sh) {
			return sh
		}
	}
	if p, err := exec.LookPath("bash"); err == nil {
		return p
	}
	return "/bin/sh"
}

func executable(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0
}

// SameFile reports whether two paths name the same file, after symlinks.
func SameFile(a, b string) bool {
	fa, err1 := os.Stat(a)
	fb, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(fa, fb)
}

func cutEq(kv string) (string, string, bool) {
	for i := 0; i < len(kv); i++ {
		if kv[i] == '=' {
			return kv[:i], kv[i+1:], true
		}
	}
	return kv, "", false
}

// recordSession appends this process's session leader, as ps lists it, to
// path: one JSON Proc per line.
func recordSession(path string) error {
	if path == "" {
		return errors.New(SessionsEnv + " not set")
	}
	sid, err := getsid(0)
	if err != nil {
		return err
	}
	leader, err := listPID(sid)
	if err != nil {
		return err
	}
	// Birth identity only: the leader's command line changes when it execs.
	b, err := json.Marshal(struct {
		PID   int    `json:"pid"`
		Start string `json:"start"`
	}{leader.PID, leader.Start})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// ErrProcessGone means ps completed and listed no running process for the PID.
// Lookup failures never wrap this sentinel.
var ErrProcessGone = errors.New("proc: process not listed")

// listPID lists one process, in the same form as List. Only a confirmed
// absence (including a zombie or exiting process) returns ErrProcessGone.
func listPID(pid int) (Proc, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-ww", "-o", "pid=", "-o", "ppid=", "-o", "pgid=", "-o", "uid=", "-o", "stat=",
		"-o", "lstart=", "-o", "command=", "-p", strconv.Itoa(pid))
	cmd.Env = append(ScrubEnv(os.Environ()), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		// Both macOS BSD ps and Linux procps exit 1 when no PID matches.
		// With headings disabled they write nothing to stdout or stderr.
		// A start failure, signal, timeout, other status or diagnostic is not absence.
		var exitErr *exec.ExitError
		if ctx.Err() == nil && errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && len(out) == 0 && len(exitErr.Stderr) == 0 {
			return Proc{}, fmt.Errorf("process %d: %w", pid, ErrProcessGone)
		}
		return Proc{}, fmt.Errorf("ps -p %d: %w", pid, err)
	}
	all, err := parsePS(string(out))
	if err != nil {
		return Proc{}, err
	}
	p, ok := all[pid]
	if !ok {
		return Proc{}, fmt.Errorf("process %d: %w", pid, ErrProcessGone)
	}
	return p, nil
}

// RecordedSessions reads the session leaders a helper recorded, without
// duplicates.
func RecordedSessions(path string) ([]Proc, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	seen := map[string]bool{}
	var out []Proc
	br := bufio.NewReader(f) // no line limit: a bad or very long line is skipped, not fatal
	for {
		line, err := br.ReadBytes('\n')
		var p Proc
		if len(line) < 4096 && json.Unmarshal(line, &p) == nil && p.PID > 1 && p.Start != "" {
			if k := strconv.Itoa(p.PID) + "|" + p.Start; !seen[k] {
				seen[k] = true
				out = append(out, Proc{PID: p.PID, Start: p.Start})
			}
		}
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return out, err
		}
	}
}

// betweenRecordedReads runs between the two reads of the sessions file at
// shutdown; a variable so tests can append a record in that window.
var betweenRecordedReads = func() {}

// ShutdownMarker is the file whose existence tells a helper that the run is
// shutting down: it then refuses to run its command.
func ShutdownMarker(sessions string) string { return sessions + ".closed" }
