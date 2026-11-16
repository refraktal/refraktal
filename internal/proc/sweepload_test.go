package proc

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// chainMembers lists the chain's members by PID, oldest first.
func chainMembers(dir string) []int {
	paths, _ := filepath.Glob(filepath.Join(dir, "member-*"))
	var out []int
	for _, p := range paths {
		if pid, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(p), "member-")); err == nil {
			out = append(out, pid)
		}
	}
	sort.Ints(out)
	return out
}

// startChain starts a chain leader in a session of its own and hooks the
// session lookup: after each listing and before the kill, the newest member
// forks one new member (up to forks times), so every sweep pass leaves one
// process its listing never saw, as a fork bomb does under load.
func startChain(t *testing.T, forks int) (Proc, string) {
	t.Helper()
	dir := t.TempDir()
	self, _ := os.Executable()
	cmd := exec.Command(self)
	cmd.Env = append(os.Environ(), helperEnv+"=chain", "CHAIN_DIR="+dir, "GORACE=atexit_sleep_ms=0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go cmd.Wait()
	t.Cleanup(func() {
		for _, pid := range chainMembers(dir) {
			syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	waitUntil(t, "the chain leader to start", func() bool { return len(chainMembers(dir)) == 1 })
	all, err := List()
	if err != nil {
		t.Fatal(err)
	}
	leader := all[cmd.Process.Pid]
	original := membersOf
	done := 0
	membersOf = func(sid int) ([]Proc, bool, error) {
		members, occupied, err := original(sid)
		if sid == leader.PID && done < forks && len(members) > 0 {
			before := chainMembers(dir)
			newest := before[len(before)-1]
			os.WriteFile(filepath.Join(dir, "fork-"+strconv.Itoa(newest)), nil, 0o600)
			waitUntil(t, "a fork between the listing and the kill", func() bool { return len(chainMembers(dir)) > len(before) })
			done++
		}
		return members, occupied, err
	}
	t.Cleanup(func() { membersOf = original })
	return leader, dir
}

func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); !ok(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func survivors(t testing.TB, dir string) []int {
	t.Helper()
	all, err := List()
	if err != nil {
		t.Fatalf("inspect process exit: %v", err)
	}
	var out []int
	for _, pid := range chainMembers(dir) {
		if _, ok := all[pid]; ok {
			out = append(out, pid)
		}
	}
	return out
}

// A session whose members keep forking while the sweep runs is swept until
// it is empty: a fixed number of passes left the newest generation running
// (and unreported) when each pass was slow, as on a loaded machine.
func TestSweepSessionOutlastsForksDuringTheSweep(t *testing.T) {
	leader, dir := startChain(t, 5)
	kr, err := SweepSession(leader)
	if err != nil {
		t.Fatal(err)
	}
	if s := survivors(t, dir); len(s) != 0 {
		t.Fatalf("%d chain members survived the sweep (reported alive: %d)", len(s), len(kr.Alive))
	}
}

// A session that never empties within the sweep's time is reported: the
// members of the last listing come back as alive, so the record is kept for
// retry and the run reports them; a survivor is never silent. (A process
// forked after the last listing cannot be named by this sweep, but its
// parent is.)
func TestSweepSessionReportsWhatOutlivesIt(t *testing.T) {
	defer func(d time.Duration) { sweepFor = d }(sweepFor)
	sweepFor = 500 * time.Millisecond
	leader, dir := startChain(t, 1_000)
	kr, _ := SweepSession(leader)
	if s := survivors(t, dir); len(s) > 0 && len(kr.Alive) == 0 {
		t.Fatalf("%d members outlived the sweep, and none was reported alive", len(s))
	}
	if len(kr.Alive) == 0 {
		t.Fatal("the sweep ended inside its time although members kept forking")
	}
}

// A process sent SIGKILL can take more than a second to disappear on a
// loaded machine (it must be scheduled to exit). Kill waits long enough for
// that before calling it alive: reporting a dying process as left running
// kept its live record after the process was gone.
func TestKillWaitsForASlowDeath(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go cmd.Wait()
	defer cmd.Process.Kill()
	all, err := List()
	if err != nil {
		t.Fatal(err)
	}
	victim := all[cmd.Process.Pid]
	original := listForKill
	var since time.Time
	listForKill = func() (map[int]Proc, error) {
		m, err := original()
		if since.IsZero() {
			since = time.Now()
		}
		if err == nil && time.Since(since) < 1500*time.Millisecond {
			m[victim.PID] = victim // still dying, as under load
		}
		return m, err
	}
	defer func() { listForKill = original }()
	kr, err := Kill([]Proc{victim})
	if err != nil || len(kr.Killed) != 1 || len(kr.Alive) != 0 {
		t.Fatalf("a process that took 1.5 s to die was reported alive: %+v %v", kr, err)
	}
}
