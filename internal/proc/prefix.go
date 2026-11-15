package proc

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
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
	// Record first; a failure to record must not stop the command, but is
	// reported on stderr.
	if err := recordSession(os.Getenv(SessionsEnv)); err != nil {
		fmt.Fprintln(os.Stderr, "refraktal shell prefix: session not recorded:", err)
	}
	next := os.Getenv(NextEnv)
	env := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() { // the command and its children must not become helpers
		if name, _, _ := cutEq(kv); name != HelperEnv && name != SessionsEnv && name != NextEnv {
			env = append(env, kv)
		}
	}
	var argv []string
	if next != "" {
		argv = append([]string{next}, args...)
	} else {
		sh := os.Getenv("CLAUDE_CODE_SHELL")
		if sh == "" {
			sh = os.Getenv("SHELL")
		}
		if sh == "" {
			sh = "/bin/sh"
		}
		argv = append([]string{sh, "-c"}, args...)
	}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return err
	}
	return syscall.Exec(path, argv, env)
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
	b, err := json.Marshal(leader)
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

// listPID lists one process, in the same form as List.
func listPID(pid int) (Proc, error) {
	cmd := exec.Command("ps", "-ww", "-o", "pid=", "-o", "ppid=", "-o", "pgid=", "-o", "uid=", "-o", "stat=",
		"-o", "lstart=", "-o", "command=", "-p", strconv.Itoa(pid))
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return Proc{}, fmt.Errorf("ps -p %d: %w", pid, err)
	}
	p, ok := parsePS(string(out))[pid]
	if !ok {
		return Proc{}, fmt.Errorf("process %d not listed", pid)
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
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var p Proc
		if json.Unmarshal(sc.Bytes(), &p) != nil || p.PID <= 1 {
			continue
		}
		k := strconv.Itoa(p.PID) + "|" + p.Start + "|" + p.Cmd
		if !seen[k] {
			seen[k] = true
			out = append(out, p)
		}
	}
	return out, sc.Err()
}

// sweepRecorded sweeps every session recorded in path. A session whose
// leader's PID now belongs to another process is skipped.
func sweepRecorded(path string) (KillReport, error) {
	r := KillReport{Killed: []Proc{}, Gone: []Proc{}, Changed: []Proc{}, Alive: []Proc{}}
	leaders, err := RecordedSessions(path)
	if err != nil {
		return r, err
	}
	for _, l := range leaders {
		kr, err := SweepSession(l)
		if err != nil && !errors.Is(err, ErrSessionReused) {
			return r, err
		}
		r.Killed = append(r.Killed, kr.Killed...)
		r.Gone = append(r.Gone, kr.Gone...)
		r.Changed = append(r.Changed, kr.Changed...)
		r.Alive = append(r.Alive, kr.Alive...)
	}
	return r, nil
}
