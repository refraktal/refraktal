package proc

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
)

// requirePIDFDKillSafety replaces only the reference test's removed opt-in
// gate. Its synthetic PID is mapped to an owned helper's real pidfd, so a
// coincidentally occupied synthetic PID can never receive a real signal.
func requirePIDFDKillSafety(t *testing.T) {
	t.Helper()
	cmd := pidfdVictim(t)
	savedFind := findProcessForKill
	t.Cleanup(func() { findProcessForKill = savedFind })
	findProcessForKill = func(int) (*os.Process, error) { return os.FindProcess(cmd.Process.Pid) }
}

// pidfdCount uses only the public /proc fd listing. Unlike a signal(0)
// probe, a newly opened pidfd proves FindProcess acquired a kernel handle.
func pidfdCount(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if target == "anon_inode:[pidfd]" {
			n++
		}
	}
	return n
}

func pidfdVictim(t *testing.T) *exec.Cmd {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self)
	cmd.Env = append(ScrubEnv(os.Environ()), helperEnv+"=deaf", "GORACE=atexit_sleep_ms=0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	})
	before := pidfdCount(t)
	probe, err := os.FindProcess(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	after := pidfdCount(t)
	if err := probe.Release(); err != nil {
		t.Fatal(err)
	}
	if after != before+1 {
		t.Skip("os.FindProcess did not acquire a pidfd; kernel or syscall policy uses the documented PID fallback")
	}
	return cmd
}

func TestKillPinsLinuxProcessBeforeIdentityRecheck(t *testing.T) {
	cmd := pidfdVictim(t)
	recorded := Proc{PID: cmd.Process.Pid, Start: lookupProc.Start}
	before := pidfdCount(t)
	savedFind, savedCheck, savedSignal, savedList := findProcessForKill, recheckPID, signal, listForKill
	t.Cleanup(func() {
		findProcessForKill, recheckPID, signal, listForKill = savedFind, savedCheck, savedSignal, savedList
	})
	opened := false
	findProcessForKill = func(pid int) (*os.Process, error) {
		process, err := os.FindProcess(pid)
		opened = err == nil
		if opened && pidfdCount(t) != before+1 {
			t.Fatal("Kill's FindProcess did not open a pidfd")
		}
		return process, err
	}
	recheckPID = func(pid int) (Proc, error) {
		if !opened {
			t.Fatal("identity was rechecked before the process handle was acquired")
		}
		return recorded, nil
	}
	signal = func(int, syscall.Signal) error {
		t.Fatal("Kill used a PID-based signal instead of the acquired process handle")
		return nil
	}
	listForKill = func() (map[int]Proc, error) { return map[int]Proc{}, nil }
	r, err := Kill([]Proc{recorded})
	if err != nil || !slices.Contains(pids(r.Killed), recorded.PID) {
		t.Fatalf("Kill: report %+v, err %v", r, err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("owned helper exited successfully instead of receiving SIGKILL")
	}
	if state, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); !ok || !state.Signaled() || state.Signal() != syscall.SIGKILL {
		t.Fatalf("owned helper did not receive SIGKILL: %v", cmd.ProcessState)
	}
	if got := pidfdCount(t); got != before-1 { // Wait released the exec.Cmd's original pidfd too.
		t.Fatalf("process handle leaked: before %d pidfds, after Wait %d", before, got)
	}
}

func TestKillReleasesLinuxHandleOnRejectedIdentity(t *testing.T) {
	cmd := pidfdVictim(t)
	recorded := Proc{PID: cmd.Process.Pid, Start: lookupProc.Start}
	for _, tc := range []struct {
		name string
		proc Proc
		err  error
	}{
		{"reused PID", Proc{PID: recorded.PID, Start: "Sun Nov 15 15:45:52 2026"}, nil},
		{"lookup failed", Proc{}, errors.New("inspection unavailable")},
		{"already gone", Proc{}, ErrProcessGone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := pidfdCount(t)
			savedCheck := recheckPID
			t.Cleanup(func() { recheckPID = savedCheck })
			recheckPID = func(int) (Proc, error) { return tc.proc, tc.err }
			Kill([]Proc{recorded})
			if got := pidfdCount(t); got != before {
				t.Fatalf("rejected target leaked a handle: before %d pidfds, after %d", before, got)
			}
			if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
				t.Fatalf("rejected owned helper was signalled: %v", err)
			}
		})
	}
}
