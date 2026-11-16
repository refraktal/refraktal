package proc

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"refraktal.dev/refraktal/internal/acp/acptest"
)

// startSessionWithMember starts a new session whose leader exits at once and
// leaves one member behind. It returns the leader's PID and the member.
func startSessionWithMember(t *testing.T) (int, int) {
	t.Helper()
	sh := exec.Command("sh", "-c", "sleep 30 >/dev/null 2>&1 & echo $!")
	sh.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	out, err := sh.Output()
	if err != nil {
		t.Fatal(err)
	}
	m := readPID(t, bufio.NewReader(strings.NewReader(string(out))))
	t.Cleanup(func() { syscall.Kill(m, syscall.SIGKILL) })
	return sh.Process.Pid, m
}

// startLiveLeader starts a new session whose leader stays alive, running
// `sleep 31` after an exec, with one background member.
func startLiveLeader(t *testing.T) (*exec.Cmd, int) {
	t.Helper()
	lead := exec.Command("sh", "-c", "sleep 30 >/dev/null 2>&1 & echo $!; exec sleep 31")
	lead.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	out, _ := lead.StdoutPipe()
	if err := lead.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lead.Process.Kill(); lead.Wait() })
	m := readPID(t, bufio.NewReader(out))
	t.Cleanup(func() { syscall.Kill(m, syscall.SIGKILL) })
	for deadline := time.Now().Add(3 * time.Second); listed(t, lead.Process.Pid).Cmd != "sleep 31"; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("leader did not exec")
		}
	}
	return lead, m
}

func appendRecord(t *testing.T, path string, p Proc) {
	t.Helper()
	b, _ := json.Marshal(p)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.Write(append(b, '\n'))
	f.Close()
}

// 1a. A recorded session seen empty during the run is dropped for good: if
// its ID is later reused, the new session's processes are strangers.
func TestASessionSeenEmptyIsNeverSwept(t *testing.T) {
	acptest.StartSentinel(t)
	const reused = 999999 // the recorded leader's PID; no such process exists
	stranger := startSleep(t, &syscall.SysProcAttr{Setsid: true})
	var reusedNow atomic.Bool // written by the test, read by the session poller
	orig := membersOf
	membersOf = func(sid int) ([]Proc, bool, error) {
		if sid != reused {
			return orig(sid)
		}
		if !reusedNow.Load() {
			return nil, false, nil // the recorded session has emptied
		}
		all, _ := List()
		return []Proc{all[stranger]}, true, nil // PID reused: a stranger's session now has this ID
	}
	defer func() { membersOf = orig }()

	sessions := filepath.Join(t.TempDir(), "sessions")
	appendRecord(t, sessions, Proc{PID: reused, Start: "Mon Jan 1 00:00:00 2001", Cmd: "zsh"})
	self, _ := os.Executable()
	c, err := Start(Spec{Argv: []string{self}, Env: []string{helperEnv + "=detach", "GORACE=atexit_sleep_ms=0"},
		PollEvery: time.Hour, SessionsFile: sessions, SessionPollEvery: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown(time.Second) // keep the lookup hook installed through cleanup
	readPID(t, bufio.NewReader(c.Stdout))
	acptest.WaitFor(t, 5*time.Second, "session permanently dropped after two empty observations", func() bool {
		c.smu.Lock()
		defer c.smu.Unlock()
		return c.dropped[sessionKey(Proc{PID: reused, Start: "Mon Jan 1 00:00:00 2001"})]
	})
	reusedNow.Store(true)
	c.Shutdown(time.Second)
	if !running(t, stranger) {
		t.Fatal("a stranger in a reused session ID was killed")
	}
}

// 1b. Dead-run cleanup never sweeps by session ID: it kills only processes
// whose identity was recorded.
func TestCleanLeftoversNeverSweepsBySessionID(t *testing.T) {
	lead, member := startLiveLeader(t)
	live := t.TempDir()
	leader := listed(t, lead.Process.Pid)
	if err := writeRecord(filepath.Join(live, "dead.json"), Record{Owner: deadOwner(t), Child: leader}); err != nil {
		t.Fatal(err)
	}
	r, err := CleanLeftovers(live)
	if err != nil || !slices.Contains(pids(r.Kill.Killed), leader.PID) {
		t.Fatalf("recorded child not killed: %+v %v", r, err)
	}
	if !running(t, member) {
		t.Fatal("an unrecorded member was killed by session ID")
	}
}

// 1c. A member that started before the recorded leader cannot belong to the
// leader's session: it is skipped.
func TestSweepSkipsMembersOlderThanTheLeader(t *testing.T) {
	leaderPID, member := startSessionWithMember(t)
	kr, err := SweepSession(Proc{PID: leaderPID, Start: "Mon Jan 1 00:00:00 2099", Cmd: "sh"})
	if err != nil {
		t.Fatal(err)
	}
	if !running(t, member) || slices.Contains(pids(kr.Killed), member) {
		t.Fatal("killed a member older than the recorded leader")
	}
}

// 1d. The harness is not reaped before Shutdown has swept its session: a
// zombie leader keeps the session ID reserved.
func TestExitedChildIsNotReapedBeforeShutdown(t *testing.T) {
	c := helperEvery(t, "escape", time.Hour)
	gc := readPID(t, bufio.NewReader(c.Stdout))
	defer syscall.Kill(gc, syscall.SIGKILL)
	select {
	case <-c.Exited():
	case <-time.After(5 * time.Second):
		t.Fatal("helper did not exit")
	}
	out, _ := exec.Command("ps", "-o", "stat=", "-p", itoa(c.PID())).Output()
	if !strings.HasPrefix(strings.TrimSpace(string(out)), "Z") {
		t.Fatalf("exited child is %q, want a zombie until Shutdown", strings.TrimSpace(string(out)))
	}
	r := c.Shutdown(time.Second)
	if !slices.Contains(pids(r.Swept.Killed), gc) {
		t.Fatalf("detached child not swept: %+v", r.Swept)
	}
	if out, _ := exec.Command("ps", "-o", "stat=", "-p", itoa(c.PID())).Output(); len(strings.TrimSpace(string(out))) != 0 {
		t.Fatal("child not reaped after Shutdown")
	}
}

// 2. The command must not inherit CLAUDE_CODE_SHELL_PREFIX pointing at the
// helper: it gets the user's original value, or none.
func TestShellPrefixRestoresTheUsersPrefix(t *testing.T) {
	dir := t.TempDir()
	self, _ := os.Executable()
	run := func(extra ...string) string {
		sh := exec.Command("sh", "-c", `"$PREFIX" 'echo "prefix=${CLAUDE_CODE_SHELL_PREFIX-unset}"'`)
		sh.Env = append(append(prefixEnv(t, filepath.Join(dir, "sessions")), "CLAUDE_CODE_SHELL_PREFIX="+self,
			"REFRAKTAL_PREFIX_VAR=CLAUDE_CODE_SHELL_PREFIX"), extra...)
		sh.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		out, err := sh.Output()
		if err != nil {
			t.Fatalf("%v %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if got := run(); got != "prefix=unset" {
		t.Errorf("without a user prefix the command saw %q", got)
	}
	user := filepath.Join(dir, "user-prefix.sh")
	os.WriteFile(user, []byte("#!/bin/sh\nexec /bin/sh -c \"$1\"\n"), 0o755)
	if got := run(NextEnv + "=" + user); got != "prefix="+user {
		t.Errorf("with a user prefix the command saw %q", got)
	}
	if got := run(NextEnv + "=" + self); got != "prefix=unset" { // chaining to ourselves would loop
		t.Errorf("chaining to the helper itself: %q", got)
	}
}

// 4. Claude Code runs commands with bash or zsh: a $SHELL that is neither is
// not used.
func TestShellPrefixUsesBashOrZsh(t *testing.T) {
	sh := exec.Command("sh", "-c", `"$PREFIX" 'echo "bash=${BASH_VERSION:+yes} zsh=${ZSH_VERSION:+yes}"'`)
	sh.Env = append(prefixEnv(t, filepath.Join(t.TempDir(), "sessions")), "SHELL=/nonexistent/fish")
	sh.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	out, err := sh.Output()
	if err != nil || !strings.Contains(string(out), "=yes") {
		t.Fatalf("command did not run under bash or zsh: %q %v", out, err)
	}
}

// 6. The helper records its own command line, then execs: a live leader's
// command line differs from the record. Leaders match on PID and start time.
func TestRecordedLeaderWithAChangedCommandIsStillSwept(t *testing.T) {
	lead, member := startLiveLeader(t)
	rec := listed(t, lead.Process.Pid)
	rec.Cmd = "refraktal" // as recorded before the exec
	kr, err := SweepSession(rec)
	if err != nil || !slices.Contains(pids(kr.Killed), member) || running(t, member) {
		t.Fatalf("member of a live recorded session survived: %+v %v", kr, err)
	}
}

// 7. A record appended after the first read of the sessions file is swept by
// the second read.
func TestARecordAppendedLateIsSwept(t *testing.T) {
	sessions := filepath.Join(t.TempDir(), "sessions")
	leaderPID, member := startSessionWithMember(t)
	orig := betweenRecordedReads
	betweenRecordedReads = func() {
		appendRecord(t, sessions, Proc{PID: leaderPID, Start: listed(t, member).Start, Cmd: "zsh"})
	}
	defer func() { betweenRecordedReads = orig }()
	c := helperEvery(t, "deaf", time.Hour)
	c.sessions = sessions
	c.Shutdown(100 * time.Millisecond)
	if running(t, member) {
		t.Fatal("a session recorded after the first read survived")
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// A. go test must never start a real harness: inside a test binary, Start
// refuses any executable but the test binary itself (fakes and helpers
// re-run it), unless REFRAKTAL_LIVE=1.
func TestStartRefusesARealExecutableInATest(t *testing.T) {
	t.Setenv("REFRAKTAL_LIVE", "")
	if c, err := Start(Spec{Argv: []string{"sleep", "5"}}); err == nil || !strings.Contains(err.Error(), "REFRAKTAL_LIVE") {
		if c != nil {
			c.Shutdown(time.Second)
		}
		t.Fatalf("a real executable started inside a test: %v", err)
	}
	// Check the positive branch at the launch guard, without enabling live
	// execution or starting any executable outside this test binary.
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := checkTestLaunch(self); err != nil {
		t.Fatalf("test binary was refused: %v", err)
	}
}

// C. Every child's REFRAKTAL_NEST is one more than its parent's.
func TestChildNestIsIncremented(t *testing.T) {
	t.Setenv("REFRAKTAL_NEST", "2")
	c := helper(t, "env")
	out, _ := io.ReadAll(c.Stdout)
	c.Shutdown(time.Second)
	if !slices.Contains(strings.Split(string(out), "\n"), "REFRAKTAL_NEST=3") {
		t.Fatal("child did not get REFRAKTAL_NEST=3")
	}
}

func runPrefix(t *testing.T, sessions, command string, extra ...string) (string, int) {
	t.Helper()
	sh := exec.Command("sh", "-c", `"$PREFIX" "$CMD"`)
	sh.Env = append(append(prefixEnv(t, sessions), "CMD="+command), extra...)
	if sessions == "" {
		sh.Env = append(sh.Env, SessionsEnv+"=")
	}
	sh.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	out, err := sh.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	return string(out), code
}

// 10. A helper that records after the parent's final read must not run its
// command: it checks the shutdown marker after recording.
func TestHelperRefusesAfterTheShutdownMarker(t *testing.T) {
	sessions := filepath.Join(t.TempDir(), "sessions")
	os.WriteFile(ShutdownMarker(sessions), nil, 0o600)
	out, code := runPrefix(t, sessions, "echo RAN")
	if code != 126 || strings.Contains(out, "RAN") {
		t.Fatalf("exit %d, output %q: the command ran after shutdown began", code, out)
	}
	if l, _ := RecordedSessions(sessions); len(l) != 1 {
		t.Fatal("the helper must record before it checks the marker")
	}
}

// 10. Shutdown creates the marker before its final read of the sessions file.
func TestShutdownCreatesTheMarkerBeforeItsFinalRead(t *testing.T) {
	sessions := filepath.Join(t.TempDir(), "sessions")
	var seen bool
	orig := betweenRecordedReads
	betweenRecordedReads = func() { _, err := os.Stat(ShutdownMarker(sessions)); seen = err == nil }
	defer func() { betweenRecordedReads = orig }()
	c := helperEvery(t, "deaf", time.Hour)
	c.sessions = sessions
	c.Shutdown(100 * time.Millisecond)
	if !seen {
		t.Fatal("no shutdown marker before the final read")
	}
}

// 11. If the helper cannot record its session, it says why and does not run
// the command.
func TestHelperFailsClosedWithoutARecord(t *testing.T) {
	out, code := runPrefix(t, filepath.Join(t.TempDir(), "missing-dir", "sessions"), "echo RAN")
	if code != 126 || strings.Contains(out, "RAN") || !strings.Contains(out, "not recorded") {
		t.Fatalf("exit %d, output %q", code, out)
	}
}

// 12. Records hold PID and start time only; a bad or very long line is
// skipped, not fatal to the records after it.
func TestRecordedSessionsAreBirthIdentityAndSkipBadLines(t *testing.T) {
	sessions := filepath.Join(t.TempDir(), "sessions")
	runPrefix(t, sessions, "true")
	b, _ := os.ReadFile(sessions)
	var m map[string]any
	if json.Unmarshal(b, &m) != nil || len(m) != 2 || m["pid"] == nil || m["start"] == nil {
		t.Fatalf("record %s: want only pid and start", b)
	}
	f, _ := os.OpenFile(sessions, os.O_WRONLY|os.O_APPEND, 0o600)
	f.WriteString("not json\n" + strings.Repeat("x", 200<<10) + "\n" + `{"pid":4242,"start":"Mon Jan 1 00:00:00 2001"}` + "\n")
	f.Close()
	l, err := RecordedSessions(sessions)
	if err != nil || len(l) != 2 || l[1].PID != 4242 {
		t.Fatalf("records %+v %v", l, err)
	}
}

// 13. The live record stays when the session sweep fails, for a later cleanup.
func TestLiveRecordKeptWhenTheSweepFails(t *testing.T) {
	live := t.TempDir()
	orig := membersOf
	membersOf = func(int) ([]Proc, bool, error) { return nil, true, errors.New("ps failed") }
	defer func() { membersOf = orig }()
	self, _ := os.Executable()
	c, err := Start(Spec{Argv: []string{self}, Env: []string{helperEnv + "=deaf", "GORACE=atexit_sleep_ms=0"}, LiveDir: live, PollEvery: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	r := c.Shutdown(100 * time.Millisecond)
	if files, _ := filepath.Glob(filepath.Join(live, "*.json")); len(files) != 1 || r.Err == "" {
		t.Fatalf("record removed after a failed sweep: %v, err %q", files, r.Err)
	}
}

// 17. Identity is birth identity (PID and start time): a tracked process
// that execs is still ours and is killed.
func TestTrackedProcessThatExecsIsStillKilled(t *testing.T) {
	c := helperEvery(t, "execs", 20*time.Millisecond)
	pid := readPID(t, bufio.NewReader(c.Stdout))
	defer syscall.Kill(pid, syscall.SIGKILL)
	for deadline := time.Now().Add(3 * time.Second); !slices.Contains(pids(c.Tracked()), pid); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("not tracked before its exec")
		}
	}
	for deadline := time.Now().Add(3 * time.Second); listed(t, pid).Cmd != "sleep 30"; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("did not exec")
		}
	}
	r := c.Shutdown(time.Second)
	if running(t, pid) || !slices.Contains(pids(r.Kill.Killed), pid) {
		t.Fatalf("the identity-checked kill did not take the tracked process that exec'd: %+v", r.Kill)
	}
}

// Hardening 5. Identity is checked again immediately before each signal: a
// PID whose process changed after the bulk listing is not signalled.
func TestKillRechecksIdentityBeforeEachSignal(t *testing.T) {
	pid := startSleep(t, &syscall.SysProcAttr{Setpgid: true})
	rec := listed(t, pid)
	origRecheck, origSignal := recheckPID, signal
	var signalled []int
	recheckPID = func(p int) (Proc, error) { // the PID now belongs to another process
		q := rec
		q.Start = "Mon Jan 1 00:00:00 2099"
		return q, nil
	}
	signal = func(p int, s syscall.Signal) error { signalled = append(signalled, p); return nil }
	defer func() { recheckPID, signal = origRecheck, origSignal }()
	r, err := Kill([]Proc{rec})
	if err != nil || len(signalled) != 0 || !slices.Contains(pids(r.Changed), pid) {
		t.Fatalf("signalled %v, report %+v, err %v", signalled, r, err)
	}
}

// 1b. A harness child never gets an API key: a harness or tool must not
// silently bill a key instead of the subscription.
func TestChildGetsNoAPIKeys(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "g-key")
	t.Setenv("GOOGLE_API_KEY", "go-key")
	c := helper(t, "env")
	out, _ := io.ReadAll(c.Stdout)
	c.Shutdown(time.Second)
	if strings.Contains(string(out), "GEMINI_API_KEY=") || strings.Contains(string(out), "GOOGLE_API_KEY=") {
		t.Fatal("an API key reached the child")
	}
}
