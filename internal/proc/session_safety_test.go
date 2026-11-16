package proc

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
)

func TestSessionPollDoesNotRecordUnprovenReplacement(t *testing.T) {
	leader := Proc{PID: 990001, Start: "Sun Nov 15 15:45:51 2026"}
	old := Proc{PID: 990002, Start: "Sun Nov 15 15:45:52 2026"}
	stranger := old
	stranger.Start = "Sun Nov 15 15:45:54 2026"
	fakePS(t, "exit 0")
	saved := membersOf
	membersOf = func(int) ([]Proc, bool, error) { return []Proc{stranger}, true, nil }
	t.Cleanup(func() { membersOf = saved })
	c := &Child{leader: leader, members: map[int]Proc{old.PID: old}, dropped: map[string]bool{}}
	c.checkSessions()
	if !c.members[old.PID].Same(old) {
		t.Fatalf("unproven replacement became a future kill target: %+v", c.members)
	}
}

func TestSessionSweepUnknownDoesNotBecomeDeadRunKillTarget(t *testing.T) {
	leader := Proc{PID: 990001, Start: "Sun Nov 15 15:45:51 2026"}
	stranger := Proc{PID: 990002, Start: "Sun Nov 15 15:45:54 2026"}
	fakePS(t, "exit 0")
	savedMembers, savedCheck, savedSignal, savedList := membersOf, recheckPID, signal, listForKill
	t.Cleanup(func() { membersOf, recheckPID, signal, listForKill = savedMembers, savedCheck, savedSignal, savedList })
	membersOf = func(int) ([]Proc, bool, error) { return []Proc{stranger}, true, nil }
	recheckPID = func(int) (Proc, error) { return stranger, nil }
	listForKill = func() (map[int]Proc, error) { return map[int]Proc{}, nil }
	signals := 0
	signal = func(int, syscall.Signal) error { signals++; return nil }
	c := &Child{leader: leader, members: map[int]Proc{}, dropped: map[string]bool{}, tracker: NewTracker(leader.PID, func() bool { return false }), record: filepath.Join(t.TempDir(), "child.json")}
	r, err := c.sweepSessions()
	if err != nil || !slices.Contains(pids(r.Unknown), stranger.PID) || len(r.Killed) != 0 || signals != 0 {
		t.Fatalf("unknown session report: %+v, signals %d, err %v", r, signals, err)
	}
	if _, promoted := c.members[stranger.PID]; promoted {
		t.Fatalf("unknown promoted to trusted members: %+v", c.members)
	}
	if err := c.writeRecord(); err != nil {
		t.Fatal(err)
	}
	rec := readRecord(t, c.record)
	raw, err := os.ReadFile(c.record)
	if err != nil {
		t.Fatal(err)
	}
	var recordFields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &recordFields); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(pids(rec.Tracked), stranger.PID) || len(recordFields["unresolved_sessions"]) == 0 {
		t.Fatalf("unsafe unresolved live record: %+v", rec)
	}
}

func TestSessionSweepFollowsVerifiedParentChain(t *testing.T) {
	leader := Proc{PID: 990001, Start: "Sun Nov 15 15:45:51 2026"}
	parent := Proc{PID: 990002, Start: "Sun Nov 15 15:45:52 2026"}
	child := Proc{PID: 990003, PPID: parent.PID, Start: "Sun Nov 15 15:45:53 2026"}
	grandchild := Proc{PID: 990004, PPID: child.PID, Start: "Sun Nov 15 15:45:54 2026"}
	fakePS(t, fmt.Sprintf("echo '%d 1 990001 %d S %s parent'; echo '%d %d 990001 %d S %s child'; echo '%d %d 990001 %d S %s grandchild'", parent.PID, os.Getuid(), parent.Start, child.PID, child.PPID, os.Getuid(), child.Start, grandchild.PID, grandchild.PPID, os.Getuid(), grandchild.Start))
	savedMembers, savedCheck, savedSignal, savedList := membersOf, recheckPID, signal, listForKill
	t.Cleanup(func() { membersOf, recheckPID, signal, listForKill = savedMembers, savedCheck, savedSignal, savedList })
	present := true
	membersOf = func(int) ([]Proc, bool, error) {
		if present {
			return []Proc{grandchild, child}, true, nil
		}
		return nil, false, nil
	}
	recheckPID = func(pid int) (Proc, error) {
		if pid == child.PID {
			return child, nil
		}
		return grandchild, nil
	}
	listForKill = func() (map[int]Proc, error) { return map[int]Proc{}, nil }
	signal = func(int, syscall.Signal) error { present = false; return nil }
	c := &Child{leader: leader, members: map[int]Proc{parent.PID: parent}, dropped: map[string]bool{}}
	r, err := c.sweepSessions()
	if err != nil || len(r.Killed) != 2 || len(r.Unknown) != 0 {
		t.Fatalf("verified ancestry not swept: %+v, err %v", r, err)
	}
}

func TestCleanLeftoversKeepsUnresolvedSessionWithoutKillingItsMembers(t *testing.T) {
	leader := Proc{PID: 990001, Start: "Sun Nov 15 15:45:51 2026"}
	fakePS(t, "exit 0")
	savedMembers, savedCheck := membersOf, recheckPID
	t.Cleanup(func() { membersOf, recheckPID = savedMembers, savedCheck })
	occupied := true
	membersOf = func(int) ([]Proc, bool, error) { return nil, occupied, nil }
	recheckPID = func(int) (Proc, error) { return Proc{}, ErrProcessGone }
	signals := interceptSignals(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "dead.json")
	// Decode through JSON so this red test also compiles before the field exists.
	if err := os.WriteFile(path, []byte(fmt.Sprintf(`{"owner":{"pid":990009},"child":{"pid":990008},"unresolved_sessions":[{"pid":%d,"start":%q}]}`, leader.PID, leader.Start)), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := CleanLeftovers(dir)
	if err != nil || len(r.Kill.Unknown) == 0 || len(*signals) != 0 {
		t.Fatalf("unresolved session: %+v, signals %v, err %v", r, *signals, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("unresolved record removed: %v", err)
	}
	occupied = false
	r, err = CleanLeftovers(dir)
	if err != nil || len(r.Kill.Unknown) != 0 {
		t.Fatalf("empty retry: %+v, err %v", r, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty session record retained: %v", err)
	}
}

func TestSessionSweepRequiresAReservedHarnessLeader(t *testing.T) {
	for _, reaped := range []bool{false, true} {
		t.Run(fmt.Sprintf("reaped_%t", reaped), func(t *testing.T) {
			leader := Proc{PID: 990001, Start: "Sun Nov 15 15:45:51 2026"}
			member := Proc{PID: 990002, Start: "Sun Nov 15 15:45:52 2026"}
			fakePS(t, "exit 0")
			savedMembers, savedCheck, savedSignal, savedList := membersOf, recheckPID, signal, listForKill
			t.Cleanup(func() { membersOf, recheckPID, signal, listForKill = savedMembers, savedCheck, savedSignal, savedList })
			present := true
			membersOf = func(int) ([]Proc, bool, error) {
				if present {
					return []Proc{member}, true, nil
				}
				return nil, false, nil
			}
			recheckPID = func(int) (Proc, error) { return member, nil }
			listForKill = func() (map[int]Proc, error) { return map[int]Proc{}, nil }
			signal = func(int, syscall.Signal) error { present = false; return nil }
			c := &Child{cmd: &exec.Cmd{Process: &os.Process{Pid: leader.PID}}, leader: leader, reaped: make(chan struct{}), members: map[int]Proc{}, dropped: map[string]bool{}}
			if reaped {
				close(c.reaped)
			}
			r, err := c.sweepSessions()
			if err != nil {
				t.Fatal(err)
			}
			if reaped && (len(r.Killed) != 0 || len(r.Unknown) != 1) {
				t.Fatalf("reaped leader proved ownership: %+v", r)
			}
			if !reaped && (len(r.Killed) != 1 || len(r.Unknown) != 0) {
				t.Fatalf("reserved leader failed ownership: %+v", r)
			}
		})
	}
}

func TestSessionSweepLosesItsPinDuringSnapshot(t *testing.T) {
	leader := Proc{PID: 990001, Start: "Sun Nov 15 15:45:51 2026"}
	stranger := Proc{PID: 990002, Start: "Sun Nov 15 15:45:52 2026"}
	fakePS(t, "exit 0")
	savedMembers, savedCheck, savedSignal, savedList := membersOf, recheckPID, signal, listForKill
	t.Cleanup(func() { membersOf, recheckPID, signal, listForKill = savedMembers, savedCheck, savedSignal, savedList })
	c := &Child{cmd: &exec.Cmd{Process: &os.Process{Pid: leader.PID}}, leader: leader, reaped: make(chan struct{}), members: map[int]Proc{}, dropped: map[string]bool{}}
	present := true
	membersOf = func(int) ([]Proc, bool, error) {
		select {
		case <-c.reaped:
		default:
			close(c.reaped)
		}
		if present {
			return []Proc{stranger}, true, nil
		}
		return nil, false, nil
	}
	recheckPID = func(int) (Proc, error) { return stranger, nil }
	listForKill = func() (map[int]Proc, error) { return map[int]Proc{}, nil }
	signal = func(int, syscall.Signal) error { present = false; return nil }
	r, err := c.sweepSessions()
	if err != nil || len(r.Killed) != 0 || len(r.Unknown) != 1 {
		t.Fatalf("lost pin still proved ownership: %+v, err %v", r, err)
	}
}

func TestSessionPollKeepsNewestVerifiedBirth(t *testing.T) {
	old := Proc{PID: 990002, Start: "Sun Nov 15 15:45:52 2026"}
	newer := old
	newer.Start = "Sun Nov 15 15:45:54 2026"
	fakePS(t, "echo '990001 1 990001 501 S Sun Nov 15 15:45:51 2026 harness'")
	saved := membersOf
	membersOf = func(int) ([]Proc, bool, error) { return []Proc{old}, true, nil }
	t.Cleanup(func() { membersOf = saved })
	c := &Child{leader: Proc{PID: 990001, Start: "Sun Nov 15 15:45:51 2026"}, members: map[int]Proc{newer.PID: newer}, dropped: map[string]bool{}}
	c.checkSessions()
	if !c.members[newer.PID].Same(newer) {
		t.Fatalf("stale identity replaced new verified birth: %+v", c.members)
	}
}

// ps is not an atomic snapshot: an old child's PPID can be captured before
// that PID is reused, and the replacement parent's row can be captured later.
func TestSessionSweepRejectsParentBornAfterMember(t *testing.T) {
	leader := Proc{PID: 990001, Start: "Sun Nov 15 15:45:51 2026"}
	parent := Proc{PID: 990002, Start: "Sun Nov 15 15:45:54 2026"}
	stranger := Proc{PID: 990003, PPID: parent.PID, Start: "Sun Nov 15 15:45:52 2026"}
	fakePS(t, fmt.Sprintf("echo '%d 1 990001 %d S %s parent'; echo '%d %d 990001 %d S %s stranger'", parent.PID, os.Getuid(), parent.Start, stranger.PID, stranger.PPID, os.Getuid(), stranger.Start))
	savedMembers, savedCheck, savedList := membersOf, recheckPID, listForKill
	t.Cleanup(func() { membersOf, recheckPID, listForKill = savedMembers, savedCheck, savedList })
	present := true
	membersOf = func(int) ([]Proc, bool, error) {
		if present {
			return []Proc{stranger}, true, nil
		}
		return nil, false, nil
	}
	recheckPID = func(int) (Proc, error) { return stranger, nil }
	listForKill = func() (map[int]Proc, error) { return map[int]Proc{}, nil }
	sent := interceptSignals(t)
	intercept := signal
	signal = func(pid int, sig syscall.Signal) error {
		present = false
		return intercept(pid, sig)
	}
	c := &Child{leader: leader, members: map[int]Proc{parent.PID: parent}, dropped: map[string]bool{}}
	r, err := c.sweepSessions()
	if err != nil || len(*sent) != 0 || len(r.Killed) != 0 || !slices.Contains(pids(r.Unknown), stranger.PID) {
		t.Fatalf("impossible parent chain proved ownership: signals %v, report %+v, err %v", *sent, r, err)
	}
}
