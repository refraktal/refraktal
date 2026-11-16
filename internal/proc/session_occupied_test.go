package proc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestReviewSweepDoesNotAcceptOccupiedSessionAsEmpty(t *testing.T) {
	fakeKillHandles(t)
	// sessionMembers returns this first state when every process in its
	// before-snapshot forks and exits before its after-snapshot. occupied
	// remains true, while the just-forked children were not in before.
	const leaderPID = 424222
	leader := Proc{PID: leaderPID, Start: "Sun Nov 15 15:45:51 2026"}
	member := Proc{PID: leaderPID + 1, Start: leader.Start}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ps"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	origMembers, origSignal := membersOf, signal
	defer func() { membersOf, signal = origMembers, origSignal }()
	calls, signalled := 0, 0
	membersOf = func(int) ([]Proc, bool, error) {
		calls++
		if calls == 1 {
			return nil, true, nil
		}
		return []Proc{member}, true, nil
	}
	signal = func(int, syscall.Signal) error { signalled++; return nil }
	report, err := SweepSession(leader)
	t.Logf("membership checks=%d, signals=%d, killed=%d, alive=%d, unknown=%d, err=%v", calls, signalled, len(report.Killed), len(report.Alive), len(report.Unknown), err)
	if err == nil && calls == 1 && len(report.Alive) == 0 && len(report.Unknown) == 0 {
		t.Fatal("SweepSession returned successful cleanup after an occupied snapshot without checking its surviving children")
	}
}

func TestSweepOccupiedSessionDeadlineIsUnknown(t *testing.T) {
	fakeKillHandles(t)
	fakePS(t, "exit 0")
	savedMembers, savedFor := membersOf, sweepFor
	t.Cleanup(func() { membersOf, sweepFor = savedMembers, savedFor })
	sweepFor = 100 * time.Millisecond
	calls := 0
	membersOf = func(int) ([]Proc, bool, error) { calls++; return nil, true, nil }
	leader := Proc{PID: 990001, Start: "Sun Nov 15 15:45:51 2026"}
	r, err := SweepSession(leader)
	if err != nil || calls < 1 || len(r.Unknown) != 1 || !r.Unknown[0].Same(leader) {
		t.Fatalf("occupied session reported clean: calls %d, report %+v, err %v", calls, r, err)
	}
}

func TestSweepRetriesAnOccupiedSessionUntilEmpty(t *testing.T) {
	fakeKillHandles(t)
	fakePS(t, "exit 0")
	saved := membersOf
	t.Cleanup(func() { membersOf = saved })
	calls := 0
	membersOf = func(int) ([]Proc, bool, error) { calls++; return nil, calls == 1, nil }
	r, err := SweepSession(Proc{PID: 990001, Start: "Sun Nov 15 15:45:51 2026"})
	if err != nil || calls < 3 || len(r.Unknown) != 0 || len(r.Alive) != 0 {
		t.Fatalf("did not confirm emptiness after occupied sample: calls %d, report %+v, err %v", calls, r, err)
	}
}

func TestOccupiedSessionKeepsItsLiveRecordForRetry(t *testing.T) {
	fakeKillHandles(t)
	fakePS(t, "exit 0")
	savedMembers, savedFor, savedCheck := membersOf, sweepFor, recheckPID
	t.Cleanup(func() { membersOf, sweepFor, recheckPID = savedMembers, savedFor, savedCheck })
	sweepFor = 100 * time.Millisecond
	occupied := true
	membersOf = func(int) ([]Proc, bool, error) { return nil, occupied, nil }
	recheckPID = func(int) (Proc, error) { return Proc{}, ErrProcessGone }
	leader := Proc{PID: 990001, Start: "Sun Nov 15 15:45:51 2026"}
	dir := t.TempDir()
	c := &Child{leader: leader, members: map[int]Proc{}, dropped: map[string]bool{}, tracker: NewTracker(leader.PID, func() bool { return false }), record: filepath.Join(dir, "child.json")}
	r, err := c.sweepSessions()
	if err != nil || len(r.Unknown) == 0 {
		t.Fatalf("occupied session reported clean: %+v, err %v", r, err)
	}
	if err := c.writeRecord(); err != nil {
		t.Fatal(err)
	}
	retry, err := CleanLeftovers(dir)
	if err != nil || len(retry.Kill.Unknown) == 0 {
		t.Fatalf("retry lost unresolved occupancy: %+v, err %v", retry, err)
	}
	if _, err := os.Stat(c.record); err != nil {
		t.Fatalf("occupied record removed: %v", err)
	}
	occupied = false
	retry, err = CleanLeftovers(dir)
	if err != nil || len(retry.Kill.Unknown) != 0 {
		t.Fatalf("empty retry: %+v, err %v", retry, err)
	}
	if _, err := os.Stat(c.record); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty record retained: %v", err)
	}
}

func TestReviewSweepForkExitSnapshotRace(t *testing.T) {
	fakeKillHandles(t)
	// Own the session so this fixture does not depend on the runner's ancestry.
	// Fake ps snapshots model a member that forks and exits between snapshots.
	helper := exec.Command("sleep", "60")
	helper.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = helper.Process.Kill()
		_ = helper.Wait()
	})
	helperPID := helper.Process.Pid
	sid, err := getsid(helperPID)
	if err != nil || sid != helperPID {
		t.Fatalf("get helper session: pid=%d sid=%d err=%v", helperPID, sid, err)
	}
	dir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
n=0
if [ -f '%s/count' ]; then read n < '%s/count'; fi
n=$((n+1))
printf '%%s\n' "$n" > '%s/count'
if [ "$n" -lt 3 ]; then
  printf '%%s\n' '%d 1 %d %d S Sun Nov 15 15:45:51 2026 fixture-parent'
else
  printf '%%s\n' '2000000000 1 %d %d S Sun Nov 15 15:45:51 2026 fixture-child'
fi
`, dir, dir, dir, helperPID, sid, os.Getuid(), sid, os.Getuid())
	if err := os.WriteFile(filepath.Join(dir, "ps"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	origSignal := signal
	defer func() { signal = origSignal }()
	signals := 0
	signal = func(int, syscall.Signal) error { signals++; return nil }
	report, err := SweepSession(Proc{PID: sid, Start: "Sun Nov 15 15:45:51 2026"})
	count, _ := os.ReadFile(filepath.Join(dir, "count"))
	t.Logf("ps snapshots=%s signals=%d killed=%d alive=%d unknown=%d err=%v", strings.TrimSpace(string(count)), signals, len(report.Killed), len(report.Alive), len(report.Unknown), err)
	snapshots, countErr := strconv.Atoi(strings.TrimSpace(string(count)))
	if err != nil || countErr != nil || (snapshots <= 3 && len(report.Alive) == 0 && len(report.Unknown) == 0) {
		t.Fatal("occupied session lost its member between snapshots; newly forked child was never scanned, but cleanup reports success")
	}
}
