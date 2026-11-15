package proc

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const helperEnv = "PROC_TEST_HELPER"

// TestMain lets the test binary act as a child process: with PROC_TEST_HELPER
// set, it runs that helper instead of the tests.
func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case "":
		os.Exit(m.Run())
	case "detach": // start a child in a process group of its own, print its PID, exit when stdin closes
		kid := exec.Command("sleep", "30")
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
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c, err := Start(Spec{Argv: []string{self}, Env: append(env, helperEnv+"="+name, "GORACE=atexit_sleep_ms=0"), PollEvery: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// running reports whether pid is alive and not a zombie.
func running(pid int) bool {
	all, err := List()
	_, ok := all[pid]
	return err == nil && ok
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
	if running(pid) {
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
	oldCmd := now
	oldCmd.Cmd = "some-mcp-server --stdio" // the recorded process ran another command
	r, err := Kill([]Proc{oldStart, oldCmd})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Killed) != 0 || len(r.Changed) != 2 {
		t.Fatalf("want both left alone as changed: %+v", r)
	}
	if !running(pid) {
		t.Fatal("unrelated process was killed")
	}

	if r, _ := Kill([]Proc{{PID: 1, Start: all[1].Start, Cmd: all[1].Cmd}}); len(r.Killed) != 0 {
		t.Fatal("Kill targeted PID 1")
	}
}

// A process in the group of a live tracked process is tracked even when it is
// not a descendant by parent PID, as a tool's background child is after its
// shell exits and it is reparented to PID 1.
func TestTrackerFollowsATrackedProcessGroup(t *testing.T) {
	c := helper(t, "detach")
	line, _ := bufio.NewReader(c.Stdout).ReadString('\n')
	leader, _ := strconv.Atoi(strings.TrimSpace(line))
	defer syscall.Kill(leader, syscall.SIGKILL)
	waitTracked(t, c, leader)

	// Started by the test, not the child, but placed in the leader's group.
	member := exec.Command("sleep", "30")
	member.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: leader}
	if err := member.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { member.Process.Kill(); member.Wait() }()
	waitTracked(t, c, member.Process.Pid)

	// A process outside every tracked group and lineage is not tracked.
	stranger := exec.Command("sleep", "30")
	stranger.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := stranger.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { stranger.Process.Kill(); stranger.Wait() }()
	time.Sleep(100 * time.Millisecond)
	if slices.Contains(pids(c.Tracked()), stranger.Process.Pid) {
		t.Fatal("an unrelated process was tracked")
	}

	r := c.Shutdown(2 * time.Second)
	if !slices.Contains(pids(r.Kill.Killed), member.Process.Pid) || running(stranger.Process.Pid) == false {
		t.Fatalf("member not killed or stranger killed: %+v", r.Kill)
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
	out := "  10     1    10 Ss   Sun Nov 15 15:45:51 2026 /bin/sh -c sleep  20\n" +
		"  11    10    10 Z    Sun Nov 15 15:45:52 2026 (sleep)\n" +
		"  12    10    10 SE   Sun Nov 15 15:45:53 2026 (claude)\n" +
		"garbage\n"
	got := parsePS(out)
	want := Proc{PID: 10, PPID: 1, PGID: 10, Start: "Sun Nov 15 15:45:51 2026", Cmd: "/bin/sh -c sleep 20"}
	if len(got) != 1 || got[10] != want {
		t.Fatalf("got %+v", got)
	}
}
