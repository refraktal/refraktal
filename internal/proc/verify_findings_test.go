package proc

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// These opt-in reproductions deliberately fail while the documented OS races
// remain. They use the production lookup/signal seams and never signal an OS
// process. Run with REFRAKTAL_VERIFY_FINDINGS=1 go test ./internal/proc -run TestVerifyFinding.
func requireVerifyFinding(t *testing.T) {
	t.Helper()
	if os.Getenv("REFRAKTAL_VERIFY_FINDINGS") != "1" {
		t.Skip("known kill-safety findings; set REFRAKTAL_VERIFY_FINDINGS=1 to reproduce")
	}
}

func TestVerifyFindingPIDReuseAfterRecheck(t *testing.T) {
	requireVerifyFinding(t)
	recorded := Proc{PID: 999991, Start: "Sun Nov 15 15:45:51 2026"}
	stranger := recorded
	stranger.Start = "Sun Nov 15 15:45:52 2026"
	current := recorded
	savedCheck, savedSignal, savedList := recheckPID, signal, listForKill
	t.Cleanup(func() { recheckPID, signal, listForKill = savedCheck, savedSignal, savedList })
	recheckPID = func(int) (Proc, error) {
		observed := current
		// The scheduler can run exit/reap/new-process after ps captured its row,
		// before Kill gets to syscall.Kill. Distinct seconds avoid ps aliasing.
		current = stranger
		return observed, nil
	}
	unrelatedSignals := 0
	signal = func(int, syscall.Signal) error {
		if !current.Same(recorded) {
			unrelatedSignals++
		}
		return nil
	}
	listForKill = func() (map[int]Proc, error) { return map[int]Proc{current.PID: current}, nil }
	if _, err := Kill([]Proc{recorded}); err != nil {
		t.Fatal(err)
	}
	if unrelatedSignals != 0 {
		t.Fatalf("Kill signalled %d unrelated process after successful identity recheck", unrelatedSignals)
	}
}

func TestVerifyFindingRecordedSessionReusedBetweenPolls(t *testing.T) {
	requireVerifyFinding(t)
	// Both old and replacement leaders have exited and been reaped. An
	// unrelated replacement member still occupies the reused session ID.
	recorded := Proc{PID: 999992, Start: "Sun Nov 15 15:45:51 2026"}
	original := Proc{PID: 999993, Start: "Sun Nov 15 15:45:52 2026"}
	stranger := Proc{PID: original.PID, Start: "Sun Nov 15 15:45:54 2026"}
	fakePS(t, "exit 0") // neither leader exists in the listing
	savedMembers, savedCheck, savedSignal, savedList := membersOf, recheckPID, signal, listForKill
	t.Cleanup(func() { membersOf, recheckPID, signal, listForKill = savedMembers, savedCheck, savedSignal, savedList })
	current := original
	present := true
	membersOf = func(sid int) ([]Proc, bool, error) {
		if sid == recorded.PID && present {
			return []Proc{current}, true, nil
		}
		return nil, false, nil
	}
	recheckPID = func(int) (Proc, error) { return current, nil }
	unrelatedSignals := 0
	signal = func(int, syscall.Signal) error {
		if current.Same(stranger) {
			unrelatedSignals++
		}
		present = false
		return nil
	}
	listForKill = func() (map[int]Proc, error) { return map[int]Proc{}, nil }
	sessions := filepath.Join(t.TempDir(), "sessions")
	appendRecord(t, sessions, recorded)
	c := &Child{sessions: sessions, dropped: map[string]bool{}, members: map[int]Proc{}}
	c.checkSessions()  // session occupied by our descendant at last poll
	current = stranger // entire empty/reuse/fork/reap interval occurs before next poll
	if _, err := c.sweepSessions(); err != nil {
		t.Fatal(err)
	}
	if unrelatedSignals != 0 {
		t.Fatalf("session sweep signalled %d unrelated process in session reused between polls", unrelatedSignals)
	}
}
