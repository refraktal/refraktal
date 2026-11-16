package acptest

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// NewTortureSpec gives a test a private launch budget and a shared absolute
// expiry. Heavy mode cannot raise the bomb above 256 total launches.
func NewTortureSpec(t *testing.T, heavy bool) TortureSpec {
	t.Helper()
	cap := 64
	if heavy {
		cap = 256
	}
	return TortureSpec{Dir: t.TempDir(), Deadline: time.Now().Add(40 * time.Second), Cap: cap}
}

// WaitFor polls an observed condition. The ticker is a polling interval,
// never a guess about how long a process takes to become ready.
func WaitFor(t *testing.T, timeout time.Duration, what string, ready func() bool) {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if ready() {
			return
		}
		select {
		case <-timer.C:
			t.Fatalf("timed out waiting for %s", what)
		case <-tick.C:
		}
	}
}

// TortureMark opens a gate that a fake observes; it does not sleep to arrange
// a race. Directory and gate names are supplied by the test, never the agent.
func TortureMark(t *testing.T, s TortureSpec, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(s.Dir, name), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TortureHas(s TortureSpec, name string) bool {
	_, err := os.Stat(filepath.Join(s.Dir, name))
	return err == nil
}

// TortureInventory returns only fully written PID records. Callers wait for
// the expected count before using it as a readiness condition.
func TortureInventory(s TortureSpec) []TortureProcess {
	paths, _ := filepath.Glob(filepath.Join(s.Dir, "pid-*.json"))
	var out []TortureProcess
	for _, path := range paths {
		b, err := os.ReadFile(path)
		var p TortureProcess
		if err == nil && json.Unmarshal(b, &p) == nil && p.PID > 1 {
			out = append(out, p)
		}
	}
	return out
}

func TortureSlots(t *testing.T, s TortureSpec) int {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(s.Dir, "slot-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) > s.Cap {
		t.Fatalf("fake exceeded its permanent launch cap: %d > %d", len(paths), s.Cap)
	}
	return len(paths)
}

// TortureCheckScript is committed by the caller, then run in each candidate.
// Its shell and child share the Go fakes' permanent O_EXCL slot namespace
// (shell noclobber), absolute expiry and PID inventory. Polling uses small
// shell utilities, never a fresh race-instrumented test binary per command.
func TortureCheckScript(s TortureSpec) string {
	return fmt.Sprintf(`#!/bin/sh
fixture=$1
deadline=%d
cap=%d
fail() { : > "$fixture/error"; exit 125; }
[ "$(date +%%s)" -lt "$deadline" ] || fail
slot=0
while [ "$slot" -lt "$cap" ]; do
  slotfile=$(printf '%%s/slot-%%03d' "$fixture" "$slot")
  if (set -C; : > "$slotfile") 2>/dev/null; then break; fi
  slot=$((slot + 1))
done
[ "$slot" -lt "$cap" ] || fail
role=${2:-check}
pidfile=$(printf '%%s/pid-%%03d.json' "$fixture" "$slot")
printf '{"PID":%%s,"Parent":%%s,"Role":"%%s"}\n' "$$" "$PPID" "$role" > "$pidfile" || fail
if [ "$role" = check ]; then
  /bin/sh "$0" "$fixture" check-child &
  child=$!
  printf '%%s\n' "$PWD" > "$fixture/check-entered-$slot" || fail
fi
while :; do
  if [ "$(date +%%s)" -ge "$deadline" ]; then
    : > "$fixture/error"
    if [ "$role" = check ]; then wait "$child"; fi
    exit 124
  fi
  # The child ignores the parent's release: it must be killed even if its
  # parent exits normally. Both expire independently if cleanup is broken.
  if [ "$role" = check ] && [ -e "$fixture/release-check" ]; then exit 0; fi
  sleep 0.05
done
`, s.Deadline.Unix(), s.Cap)
}

// StartSentinel must be called by the test, outside the fake's process tree.
// It is a plain, self-expiring sleep of the same user, in a separate session.
// Cleanup checks it before touching it; only its original unreaped process
// handle is used for cleanup. No discovered PID is ever signalled here.
func StartSentinel(t *testing.T) func() {
	t.Helper()
	cmd := exec.Command("sleep", "180") // outlives the longest heavy test; expires if cleanup is skipped
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := false
	check := func() {
		if exited {
			return
		}
		t.Helper()
		if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
			t.Errorf("outside sentinel did not survive: %v", err)
			return
		}
		// Signal 0 also succeeds for an unreaped zombie. Wait4(WNOHANG)
		// distinguishes a killed sentinel from a running one on both hosts.
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(cmd.Process.Pid, &status, syscall.WNOHANG, nil)
		if err != nil || pid != 0 {
			exited = true // Wait4 may have reaped it: never signal this PID again.
			t.Errorf("outside sentinel exited: pid %d, status %d, error %v", pid, status, err)
		}
	}
	t.Cleanup(func() {
		check()
		if !exited {
			cmd.Process.Kill()
		}
		cmd.Wait()
		cmd.Process.Release() // also release a handle after Wait4 reaped an exited sentinel
	})
	check()
	return check
}

// TortureMatches is for read-only liveness assertions, never kill selection.
// The unique absolute directory is an argv element of every fake, including
// processes killed before writing a PID file. Command strings are not logged.
func TortureMatches(s TortureSpec, command string) bool {
	return strings.Contains(command, s.Dir)
}

func TortureError(s TortureSpec) error {
	if !time.Now().Before(s.Deadline) {
		return fmt.Errorf("fake safety expiry reached; shutdown was not proven")
	}
	if TortureHas(s, "error") {
		return fmt.Errorf("torture fake reported a launch or gate failure")
	}
	return nil
}
