package proc

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// prefixEnv is the environment that makes the test binary the shell-prefix
// helper, recording into sessions.
func prefixEnv(t *testing.T, sessions string) []string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return append(os.Environ(), HelperEnv+"=shell-prefix", SessionsEnv+"="+sessions, "PREFIX="+self,
		"SHELL=/bin/sh", "GORACE=atexit_sleep_ms=0")
}

// Claude calls the prefix with the whole command as one argument, from a new
// session's leader shell. The helper must record that session, run the
// command with the shell, and not pass its own switch on.
func TestShellPrefixRecordsTheSessionAndRunsTheCommand(t *testing.T) {
	sessions := filepath.Join(t.TempDir(), "sessions")
	sh := exec.Command("sh", "-c", `"$PREFIX" 'echo "helper=${REFRAKTAL_HELPER:-unset}"; sleep 30 >/dev/null 2>&1 & echo $!'; sleep 30`)
	sh.Env = prefixEnv(t, sessions)
	sh.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	out, _ := sh.StdoutPipe()
	if err := sh.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { sh.Process.Kill(); sh.Wait() }()
	r := bufio.NewReader(out)
	line, _ := r.ReadString('\n')
	if strings.TrimSpace(line) != "helper=unset" {
		t.Fatalf("the command saw %q: the helper's switch leaked into it", line)
	}
	bg := readPID(t, r)
	defer syscall.Kill(bg, syscall.SIGKILL)

	leaders, err := RecordedSessions(sessions)
	if err != nil || len(leaders) != 1 || leaders[0].PID != sh.Process.Pid || leaders[0].Start == "" {
		t.Fatalf("recorded %+v (%v), want the leader shell %d", leaders, err, sh.Process.Pid)
	}
	if sid, _ := getsid(bg); sid != sh.Process.Pid {
		t.Fatalf("background child in session %d, want %d", sid, sh.Process.Pid)
	}
	stranger := startSleep(t, &syscall.SysProcAttr{Setsid: true})
	kr, err := SweepSession(leaders[0])
	if err != nil || !slices.Contains(pids(kr.Killed), bg) || running(bg) {
		t.Fatalf("background child %d not swept: %+v %v", bg, kr, err)
	}
	if !running(stranger) {
		t.Fatal("the sweep killed a process in another session")
	}
}

func TestShellPrefixChainsAUserPrefix(t *testing.T) {
	dir := t.TempDir()
	sessions, marker := filepath.Join(dir, "sessions"), filepath.Join(dir, "user-prefix-ran")
	user := filepath.Join(dir, "user-prefix.sh")
	os.WriteFile(user, []byte("#!/bin/sh\ntouch "+marker+"\nexec /bin/sh -c \"$1\"\n"), 0o755)
	sh := exec.Command("sh", "-c", `"$PREFIX" 'echo chained'`)
	sh.Env = append(prefixEnv(t, sessions), NextEnv+"="+user)
	sh.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	out, err := sh.Output()
	if err != nil || strings.TrimSpace(string(out)) != "chained" {
		t.Fatalf("output %q %v", out, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("the user's prefix did not run")
	}
	if l, _ := RecordedSessions(sessions); len(l) != 1 {
		t.Fatalf("session not recorded: %+v", l)
	}
}

// A child whose tool commands run in sessions of their own (as Claude's do):
// the harness-session sweep cannot reach their background children, the
// recorded-session sweep must.
func TestShutdownSweepsRecordedSessions(t *testing.T) {
	sessions := filepath.Join(t.TempDir(), "sessions")
	self, _ := os.Executable()
	c, err := Start(Spec{Argv: []string{self}, PollEvery: time.Hour, SessionsFile: sessions,
		Env: []string{SessionsEnv + "=" + sessions, "PREFIX=" + self, "SHELL=/bin/sh", "GORACE=atexit_sleep_ms=0", helperEnv + "=prefixed"}})
	if err != nil {
		t.Fatal(err)
	}
	bg := readPID(t, bufio.NewReader(c.Stdout))
	defer syscall.Kill(bg, syscall.SIGKILL)
	if sid, _ := getsid(bg); sid == c.PID() {
		t.Fatal("the tool command ran in the harness's session; the test needs a session of its own")
	}
	// The tracker finds descendants through their parents: until the tool
	// shell that started bg has exited (and bg is reparented), bg is still a
	// descendant and Shutdown's last poll would track it. Wait for that
	// observed condition, so only the recorded-session sweep can reach bg.
	waitUntil(t, "the background child to leave the harness's tree", func() bool {
		all, err := List()
		if err != nil {
			return false
		}
		for p, seen := all[bg], 0; p.PID > 1 && seen < 64; p, seen = all[p.PPID], seen+1 {
			if p.PPID == c.PID() {
				return false
			}
		}
		return true
	})
	r := c.Shutdown(2 * time.Second)
	if slices.Contains(pids(r.Tracked), bg) {
		t.Fatalf("the tracker saw the background child; the test needs it untracked: %+v", r.Tracked)
	}
	if !slices.Contains(pids(r.Swept.Killed), bg) || running(bg) || r.Err != "" {
		t.Fatalf("background child %d of a recorded session survived: %+v", bg, r)
	}
}
