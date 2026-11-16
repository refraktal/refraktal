package proc

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"refraktal.dev/refraktal/internal/acp/acptest"
)

const helperEnv = "PROC_TEST_HELPER"

// TestMain lets the test binary act as a child process: with PROC_TEST_HELPER
// set, it runs that helper instead of the tests.
func TestMain(m *testing.M) {
	MaybeShellPrefix()
	acptest.RunIfTorture()
	if os.Getenv(helperEnv) != "" {
		// Old helpers are bounded too, even if a failed assertion skips shutdown.
		time.AfterFunc(40*time.Second, func() { os.Exit(124) })
	}
	switch os.Getenv(helperEnv) {
	case "":
		os.Exit(m.Run())
	case "chain": // in CHAIN_DIR: record member-<pid>; fork the next generation (same session) each time fork-<pid> appears
		dir := os.Getenv("CHAIN_DIR")
		self, _ := os.Executable()
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("member-%d", os.Getpid())), nil, 0o600)
		trigger := filepath.Join(dir, fmt.Sprintf("fork-%d", os.Getpid()))
		for {
			if os.Remove(trigger) == nil {
				kid := exec.Command(self)
				kid.Env = os.Environ() // same session: no Setsid
				if kid.Start() == nil {
					go kid.Wait()
				}
			}
			time.Sleep(5 * time.Millisecond)
		}
	case "exit": // exit immediately without starting any descendants
	case "detach": // start a child in a process group of its own, print its PID, exit when stdin closes
		kid := exec.Command("sleep", "30")
		kid.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := kid.Start(); err != nil {
			os.Exit(2)
		}
		fmt.Println(kid.Process.Pid)
		io.Copy(io.Discard, os.Stdin)
	case "join": // start a child inside the existing group JOIN_PGID, print its PID, exit when stdin closes
		pgid, _ := strconv.Atoi(os.Getenv("JOIN_PGID"))
		kid := exec.Command("sleep", "30")
		kid.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: pgid}
		if err := kid.Start(); err != nil {
			os.Exit(2)
		}
		fmt.Println(kid.Process.Pid)
		io.Copy(io.Discard, os.Stdin)
	case "bg": // a tool shell leading its own group: prints its PID, waits for a line, then backgrounds a sleep, prints its PID and exits
		sh := exec.Command("sh", "-c", `echo $$; read go; sleep 30 & echo $!`)
		sh.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		sh.Stdin, sh.Stdout = os.Stdin, os.Stdout
		if sh.Run() != nil {
			os.Exit(2)
		}
		io.Copy(io.Discard, os.Stdin)
	case "escape": // after the tracker's first poll, start a child in a group of its own, print its PID and exit at once
		time.Sleep(300 * time.Millisecond)
		kid := exec.Command("sleep", "30")
		kid.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := kid.Start(); err != nil {
			os.Exit(2)
		}
		fmt.Println(kid.Process.Pid)
	case "prefixed": // like Claude, but keep the tool leader alive until a release line
		time.Sleep(300 * time.Millisecond) // after the tracker's first poll, so only the recorded-session sweep can find it
		sh := exec.Command("sh", "-c", `"$PREFIX" 'sleep 30 >/dev/null 2>&1 & echo $!'; read release`)
		sh.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		sh.Env = append(os.Environ(), HelperEnv+"=shell-prefix") // as Claude passes its environment to the prefix
		sh.Stdin, sh.Stdout = os.Stdin, os.Stdout
		if sh.Run() != nil {
			os.Exit(2)
		}
		io.Copy(io.Discard, os.Stdin)
	case "execs": // a child in a group of its own that execs half a second after it starts
		kid := exec.Command("sh", "-c", "sleep 0.5; exec sleep 30")
		kid.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := kid.Start(); err != nil {
			os.Exit(2)
		}
		fmt.Println(kid.Process.Pid)
		io.Copy(io.Discard, os.Stdin)
	case "deaf": // ignore stdin closing
		time.Sleep(time.Hour)
	case "env":
		for _, kv := range os.Environ() {
			fmt.Println(kv)
		}
	}
	os.Exit(0)
}

func helper(t *testing.T, name string, env ...string) *Child {
	t.Helper()
	return helperEvery(t, name, 20*time.Millisecond, env...)
}

// helperEvery starts a helper whose tracker polls at the given interval; with
// an interval of an hour, only the first poll and the test's own Poll calls run.
func helperEvery(t *testing.T, name string, every time.Duration, env ...string) *Child {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c, err := Start(Spec{Argv: []string{self}, Env: append(env, helperEnv+"="+name, "GORACE=atexit_sleep_ms=0"), PollEvery: every})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// running reports whether pid is alive and not a zombie.
func running(t testing.TB, pid int) bool {
	t.Helper()
	all, err := List()
	if err != nil {
		t.Fatalf("inspect process exit: %v", err)
	}
	_, ok := all[pid]
	return ok
}

func pids(ps []Proc) []int {
	out := []int{}
	for _, p := range ps {
		out = append(out, p.PID)
	}
	return out
}

func waitTracked(t *testing.T, c *Child, pid int) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if slices.Contains(pids(c.Tracked()), pid) {
			return
		}
	}
	t.Fatalf("pid %d never tracked; tracked %v", pid, pids(c.Tracked()))
}

func TestTreeKillCatchesAChildThatLeftTheGroup(t *testing.T) {
	c := helper(t, "detach")
	line, err := bufio.NewReader(c.Stdout).ReadString('\n')
	pid, _ := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || pid == 0 {
		t.Fatalf("no pid from helper: %q %v", line, err)
	}
	defer syscall.Kill(pid, syscall.SIGKILL)
	waitTracked(t, c, pid)
	if g, _ := syscall.Getpgid(pid); g == c.PID() {
		t.Fatalf("the grandchild is in the child's group %d; the test needs it outside", g)
	}

	r := c.Shutdown(2 * time.Second)
	if !r.ExitedOnItsOwn {
		t.Error("helper did not exit when stdin closed")
	}
	if !slices.Contains(pids(r.Kill.Killed), pid) || len(r.Kill.Alive) != 0 || r.Err != "" {
		t.Fatalf("grandchild %d not killed: %+v", pid, r)
	}
	if running(t, pid) {
		t.Fatalf("grandchild %d still running", pid)
	}
}

func TestShutdownKillsAChildThatIgnoresStdin(t *testing.T) {
	c := helper(t, "deaf")
	pid := c.PID()
	r := c.Shutdown(100 * time.Millisecond)
	if r.ExitedOnItsOwn {
		t.Fatal("deaf helper exited on its own")
	}
	// The process-group kill runs first, so the identity-checked kill usually
	// finds the child already gone.
	if !slices.Contains(append(pids(r.Kill.Killed), pids(r.Kill.Gone)...), pid) {
		t.Fatalf("child %d neither killed nor gone: %+v", pid, r.Kill)
	}
	select {
	case <-c.Exited():
	case <-time.After(2 * time.Second):
		t.Fatal("child not reaped after kill")
	}
}

// A PID reused by an unrelated process must not be killed: the record's start
// time or command line no longer matches.
func TestKillLeavesAReusedPIDAlone(t *testing.T) {
	other := exec.Command("sleep", "30")
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	pid := other.Process.Pid
	defer func() { other.Process.Kill(); other.Wait() }()
	all, err := List()
	if err != nil {
		t.Fatal(err)
	}
	now, ok := all[pid]
	if !ok {
		t.Fatalf("pid %d not listed", pid)
	}

	oldStart := now
	oldStart.Start = "Mon Jan 1 00:00:00 2001" // the recorded process started long ago
	r, err := Kill([]Proc{oldStart})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Killed) != 0 || len(r.Changed) != 1 {
		t.Fatalf("want it left alone as changed: %+v", r)
	}
	if !running(t, pid) {
		t.Fatal("unrelated process was killed")
	}

	if r, _ := Kill([]Proc{{PID: 1, Start: all[1].Start, Cmd: all[1].Cmd}}); len(r.Killed) != 0 {
		t.Fatal("Kill targeted PID 1")
	}
}

// rootHelper starts a helper with plain exec, in the test's own session, and
// a tracker rooted at it. The group-rule tests need this: a process can join
// a process group only within its own session, and a Child leads a new one.
func rootHelper(t *testing.T, name string, env ...string) (*Tracker, *bufio.Reader) {
	t.Helper()
	self, _ := os.Executable()
	cmd := exec.Command(self)
	cmd.Env = append(append(os.Environ(), env...), helperEnv+"="+name, "GORACE=atexit_sleep_ms=0")
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() { stdin.Close(); cmd.Process.Kill(); <-done })
	go func() { cmd.Wait(); close(done) }()
	tr := NewTracker(cmd.Process.Pid, func() bool {
		select {
		case <-done:
			return false
		default:
			return true
		}
	})
	return tr, bufio.NewReader(stdout)
}

// A process in the group of a live tracked process is tracked even when it is
// not a descendant by parent PID, as a tool's background child is after its
// shell exits and it is reparented to PID 1.
func TestTrackerFollowsATrackedProcessGroup(t *testing.T) {
	tr, out := rootHelper(t, "detach")
	leader := readPID(t, out)
	defer syscall.Kill(leader, syscall.SIGKILL)
	if err := tr.Poll(); err != nil || !slices.Contains(pids(tr.Tracked()), leader) {
		t.Fatalf("leader %d not tracked: %v %v", leader, pids(tr.Tracked()), err)
	}
	// Started by the test, not the root, but placed in the leader's group.
	member := startSleep(t, &syscall.SysProcAttr{Setpgid: true, Pgid: leader})
	// Outside every tracked group and lineage.
	stranger := startSleep(t, &syscall.SysProcAttr{Setpgid: true})
	if err := tr.Poll(); err != nil {
		t.Fatal(err)
	}
	got := pids(tr.Tracked())
	if !slices.Contains(got, member) || slices.Contains(got, stranger) {
		t.Fatalf("tracked %v: want member %d, not stranger %d", got, member, stranger)
	}
}

func TestChildEnvironmentIsScrubbed(t *testing.T) {
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "cli")
	t.Setenv("CLAUDE_CODE_SSE_PORT", "1234")
	t.Setenv("REFRAKTAL_KEEP", "yes")
	c := helper(t, "env", "EXTRA=1")
	out, _ := io.ReadAll(c.Stdout)
	c.Shutdown(time.Second)
	env := strings.Split(string(out), "\n")
	for _, name := range sessionMarkers {
		for _, kv := range env {
			if strings.HasPrefix(kv, name+"=") {
				t.Errorf("%s reached the child", name)
			}
		}
	}
	if !slices.Contains(env, "REFRAKTAL_KEEP=yes") || !slices.Contains(env, "EXTRA=1") {
		t.Error("ordinary or extra variables were dropped")
	}
}

func TestParsePS(t *testing.T) {
	out := "  10     1    10   501 Ss   Sun Nov 15 15:45:51 2026 /bin/sh -c sleep  20\n" +
		"  11    10    10   501 Z    Sun Nov 15 15:45:52 2026 (sleep)\n" +
		"  12    10    10   501 SE   Sun Nov 15 15:45:53 2026 (claude)\n"
	got, err := parsePS(out)
	want := Proc{PID: 10, PPID: 1, PGID: 10, UID: 501, Start: "Sun Nov 15 15:45:51 2026", Cmd: "/bin/sh -c sleep 20"}
	if err != nil || len(got) != 1 || got[10] != want {
		t.Fatalf("got %+v, err %v", got, err)
	}
}

func readPID(t *testing.T, r *bufio.Reader) int {
	t.Helper()
	line, err := r.ReadString('\n')
	pid, _ := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || pid == 0 {
		t.Fatalf("no pid: %q %v", line, err)
	}
	return pid
}

func startSleep(t *testing.T, attr *syscall.SysProcAttr) int {
	t.Helper()
	p := exec.Command("sleep", "30")
	p.SysProcAttr = attr
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Process.Kill(); p.Wait() })
	return p.Process.Pid
}

// A tracked process that sits in a group led by an untracked process must not
// pull that group's other members into tracking.
func TestTrackerIgnoresAGroupWhoseLeaderIsNotTracked(t *testing.T) {
	leader := startSleep(t, &syscall.SysProcAttr{Setpgid: true}) // the test's, not the root's
	tr, out := rootHelper(t, "join", "JOIN_PGID="+strconv.Itoa(leader))
	joined := readPID(t, out)
	defer syscall.Kill(joined, syscall.SIGKILL)
	stranger := startSleep(t, &syscall.SysProcAttr{Setpgid: true, Pgid: leader})
	if err := tr.Poll(); err != nil {
		t.Fatal(err)
	}
	got := pids(tr.Tracked())
	if !slices.Contains(got, joined) {
		t.Fatalf("the root's own descendant %d not tracked: %v", joined, got)
	}
	if slices.Contains(got, stranger) || slices.Contains(got, leader) {
		t.Fatalf("tracked through a group whose leader is untracked: %v (leader %d, stranger %d)", got, leader, stranger)
	}
}

// A tool shell backgrounds a child and exits; the child is reparented. It
// must not survive shutdown. Before the session sweep this was tested by
// polling the tracker until the group rule found the child, which failed
// intermittently on the macOS CI runner (2 of 13 runs, never locally; cause
// not found). The session sweep kills it whether or not tracking saw it, so
// the test asserts that, with no retries.
func TestReparentedBackgroundChildDoesNotSurviveShutdown(t *testing.T) {
	c := helperEvery(t, "bg", time.Hour)
	out := bufio.NewReader(c.Stdout)
	shell := readPID(t, out)
	fmt.Fprintln(c.Stdin, "go")
	bg := readPID(t, out)
	defer syscall.Kill(bg, syscall.SIGKILL)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		all, _ := List()
		if _, up := all[shell]; !up && all[bg].PID == bg && all[bg].PPID != shell {
			break // reparented, and its shell has exited
		}
		if time.Now().After(deadline) {
			t.Fatal("background child was not reparented")
		}
	}
	if sid, err := getsid(bg); err != nil || sid != c.PID() {
		t.Fatalf("background child in session %d (%v), want the child's %d", sid, err, c.PID())
	}
	r := c.Shutdown(time.Second)
	if running(t, bg) || !slices.Contains(append(pids(r.Kill.Killed), pids(r.Swept.Killed)...), bg) {
		t.Fatalf("reparented child %d survived: kill %+v swept %+v", bg, r.Kill, r.Swept)
	}
}

// A child that detaches before the tracker ever sees it, in a group of its
// own and reparented, escapes the parent-PID tree and the group kill. The
// session sweep must find it.
func TestSessionSweepCatchesAnUntrackedDetachedChild(t *testing.T) {
	stranger := startSleep(t, &syscall.SysProcAttr{Setsid: true}) // another session: must survive
	c := helperEvery(t, "escape", time.Hour)
	pid := readPID(t, bufio.NewReader(c.Stdout))
	defer syscall.Kill(pid, syscall.SIGKILL)
	select {
	case <-c.Exited():
	case <-time.After(5 * time.Second):
		t.Fatal("helper did not exit")
	}
	r := c.Shutdown(time.Second)
	if slices.Contains(pids(r.Tracked), pid) || slices.Contains(pids(r.Kill.Killed), pid) {
		t.Fatalf("the child was tracked; the test needs it untracked: %+v", r)
	}
	if !slices.Contains(pids(r.Swept.Killed), pid) || running(t, pid) || r.Err != "" {
		t.Fatalf("detached child %d not swept: %+v", pid, r)
	}
	if !running(t, stranger) {
		t.Fatal("the sweep killed a process in another session")
	}
}

// A session ID is a PID. If the leader's PID now belongs to another process,
// the ID may name an unrelated session: nothing may be killed.
func TestSessionSweepRefusesAReusedID(t *testing.T) {
	leader := startSleep(t, &syscall.SysProcAttr{Setsid: true})
	rec := listed(t, leader)
	rec.Start = "Mon Jan 1 00:00:00 2001" // the recorded leader was another process
	r, err := SweepSession(rec)
	if !errors.Is(err, ErrSessionReused) || len(r.Killed) != 0 || !running(t, leader) {
		t.Fatalf("err %v report %+v", err, r)
	}
}
