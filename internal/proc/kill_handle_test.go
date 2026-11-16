package proc

import (
	"os"
	"slices"
	"syscall"
	"testing"
)

// fakeKillHandles keeps synthetic process snapshots inside the existing
// signal interceptor. No mocked PID is ever passed to an OS signal.
func fakeKillHandles(t *testing.T) {
	t.Helper()
	savedFind, savedSignal := findProcessForKill, signalProcessForKill
	t.Cleanup(func() { findProcessForKill, signalProcessForKill = savedFind, savedSignal })
	findProcessForKill = func(pid int) (*os.Process, error) { return &os.Process{Pid: pid}, nil }
	signalProcessForKill = func(process *os.Process, sig os.Signal) error {
		return signal(process.Pid, sig.(syscall.Signal))
	}
}

func TestKillAcquiresHandleBeforeIdentityRecheck(t *testing.T) {
	savedFind, savedCheck, savedSignal, savedList := findProcessForKill, recheckPID, signalProcessForKill, listForKill
	t.Cleanup(func() {
		findProcessForKill, recheckPID, signalProcessForKill, listForKill = savedFind, savedCheck, savedSignal, savedList
	})
	recorded := lookupProc
	var order []string
	findProcessForKill = func(pid int) (*os.Process, error) {
		order = append(order, "open")
		return &os.Process{Pid: pid}, nil
	}
	recheckPID = func(pid int) (Proc, error) {
		order = append(order, "check")
		return recorded, nil
	}
	signalProcessForKill = func(process *os.Process, sig os.Signal) error {
		order = append(order, "signal")
		if process.Pid != recorded.PID || sig != syscall.SIGKILL {
			t.Fatalf("wrong target or signal: pid=%d signal=%v", process.Pid, sig)
		}
		return nil
	}
	listForKill = func() (map[int]Proc, error) { return map[int]Proc{}, nil }
	r, err := Kill([]Proc{recorded})
	if err != nil || len(r.Killed) != 1 || !slices.Equal(order, []string{"open", "check", "signal"}) {
		t.Fatalf("Kill: order %v, report %+v, err %v", order, r, err)
	}
}
