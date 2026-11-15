package proc

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
)

var lookupProc = Proc{PID: 42420, PPID: 1, PGID: 42420, UID: 501,
	Start: "Sun Nov 15 15:45:51 2026", Cmd: "helper"}

// fakePS starts only a short shell script, never process-tree helpers.
func fakePS(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ps"), []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func interceptSignals(t *testing.T) *[]int {
	t.Helper()
	orig := signal
	var sent []int
	signal = func(pid int, _ syscall.Signal) error { sent = append(sent, pid); return nil }
	t.Cleanup(func() { signal = orig })
	return &sent
}

func failRecheck(t *testing.T, err error) {
	t.Helper()
	orig := recheckPID
	recheckPID = func(int) (Proc, error) { return Proc{}, err }
	t.Cleanup(func() { recheckPID = orig })
}

func TestKillLookupFailureIsUnknown(t *testing.T) {
	for _, failure := range []error{errors.New("ps unavailable"), context.DeadlineExceeded} {
		t.Run(failure.Error(), func(t *testing.T) {
			failRecheck(t, failure)
			sent := interceptSignals(t)
			r, err := Kill([]Proc{lookupProc})
			if !errors.Is(err, failure) || len(r.Unknown) != 1 || !r.Unknown[0].Same(lookupProc) ||
				len(r.Gone) != 0 || len(r.Killed) != 0 || len(*sent) != 0 {
				t.Fatalf("lookup failure: report %+v, signals %v, err %v", r, *sent, err)
			}
		})
	}
}

func TestListPIDDistinguishesMissingFromFailedPS(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		gone         bool
	}{
		{"empty success", "exit 0", true},
		{"missing on macOS and Linux", "exit 1", true},
		{"diagnostic", "echo 'ps blocked' >&2; exit 1", false},
		{"other exit status", "exit 2", false},
		{"garbage", "echo garbage", false},
		{"garbage with failure", "echo garbage; exit 1", false},
		{"bad start time", "echo '42420 1 42420 501 S not a valid start time helper'", false},
		{"bad state", "echo '42420 1 42420 501 garbage Sun Nov 15 15:45:51 2026 helper'", false},
		{"valid other PID", "echo '42421 1 42421 501 S Sun Nov 15 15:45:51 2026 helper'", true},
		{"zombie", "echo '42420 1 42420 501 Z Sun Nov 15 15:45:51 2026 (helper)'", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakePS(t, tc.script)
			_, err := listPID(lookupProc.PID)
			if err == nil || errors.Is(err, ErrProcessGone) != tc.gone {
				t.Fatalf("lookup err = %v; want gone = %v", err, tc.gone)
			}
			sent := interceptSignals(t)
			r, _ := Kill([]Proc{lookupProc})
			if len(*sent) != 0 || (tc.gone && (len(r.Gone) != 1 || len(r.Unknown) != 0)) ||
				(!tc.gone && (len(r.Unknown) != 1 || len(r.Gone) != 0)) {
				t.Fatalf("report %+v, signals %v", r, *sent)
			}
		})
	}
}

func TestUnavailablePSIsUnknown(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	sent := interceptSignals(t)
	r, err := Kill([]Proc{lookupProc})
	if err == nil || len(r.Unknown) != 1 || len(r.Gone) != 0 || len(*sent) != 0 {
		t.Fatalf("report %+v, signals %v, err %v", r, *sent, err)
	}
}

func TestRealMissingPIDIsGone(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	p := Proc{PID: cmd.Process.Pid, Start: lookupProc.Start}
	if _, err := listPID(p.PID); !errors.Is(err, ErrProcessGone) {
		t.Fatalf("reaped PID: %v", err)
	}
	sent := interceptSignals(t)
	r, err := Kill([]Proc{p})
	if err != nil || len(r.Gone) != 1 || len(r.Unknown) != 0 || len(*sent) != 0 {
		t.Fatalf("reaped PID: report %+v, signals %v, err %v", r, *sent, err)
	}
}

func TestCleanLeftoversKeepsUnknownForRetry(t *testing.T) {
	fakePS(t, "echo '42420 1 42420 501 S Sun Nov 15 15:45:51 2026 helper'")
	dir := t.TempDir()
	path := filepath.Join(dir, "dead.json")
	if err := writeRecord(path, Record{Owner: Proc{PID: 42422, Start: lookupProc.Start}, Child: lookupProc}); err != nil {
		t.Fatal(err)
	}
	failRecheck(t, errors.New("ps blocked"))
	sent := interceptSignals(t)
	r, err := CleanLeftovers(dir)
	if err != nil || len(r.Kill.Unknown) != 1 || len(r.Kill.Gone) != 0 || len(*sent) != 0 {
		t.Fatalf("report %+v, signals %v, err %v", r, *sent, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("unknown process lost its live record: %v", err)
	}
	// The next cleanup can confirm absence and remove the retained record.
	recheckPID = func(int) (Proc, error) { return Proc{}, ErrProcessGone }
	r, err = CleanLeftovers(dir)
	if err != nil || len(r.Kill.Gone) != 1 || len(r.Kill.Unknown) != 0 {
		t.Fatalf("retry: report %+v, err %v", r, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("confirmed-gone record retained: %v", err)
	}
}

func TestKillFailedVerificationIsUnknown(t *testing.T) {
	fakePS(t, "echo 'ps blocked' >&2; exit 1")
	orig := recheckPID
	recheckPID = func(int) (Proc, error) { return lookupProc, nil }
	t.Cleanup(func() { recheckPID = orig })
	sent := interceptSignals(t)
	r, err := Kill([]Proc{lookupProc})
	if err == nil || !slices.Equal(*sent, []int{lookupProc.PID}) || len(r.Unknown) != 1 || len(r.Alive) != 0 {
		t.Fatalf("verification: report %+v, signals %v, err %v", r, *sent, err)
	}
}

func TestSessionSweepLookupFailureIsUnknown(t *testing.T) {
	fakePS(t, "echo '42420 1 42420 501 S Sun Nov 15 15:45:51 2026 helper'")
	orig := membersOf
	membersOf = func(int) ([]Proc, bool, error) { return []Proc{lookupProc}, true, nil }
	t.Cleanup(func() { membersOf = orig })
	failRecheck(t, errors.New("ps blocked"))
	sent := interceptSignals(t)
	r, err := SweepSession(lookupProc)
	if err == nil || len(r.Unknown) != 1 || len(r.Gone) != 0 || len(*sent) != 0 {
		t.Fatalf("sweep: report %+v, signals %v, err %v", r, *sent, err)
	}
}

func TestMalformedListingCannotConfirmAbsence(t *testing.T) {
	fakePS(t, "echo '42420 1 42420 501 S Sun Nov 15 15:45:51 2026 helper'; echo garbage")
	if _, err := List(); err == nil {
		t.Fatal("partially malformed ps listing succeeded")
	}
	sent := interceptSignals(t)
	r, err := SweepSession(lookupProc)
	if err == nil || len(r.Unknown) != 1 || len(*sent) != 0 {
		t.Fatalf("sweep: report %+v, signals %v, err %v", r, *sent, err)
	}
	c := &Child{cmd: &exec.Cmd{Process: &os.Process{Pid: lookupProc.PID}}}
	if err := c.killGroup([]Proc{lookupProc}); err == nil || len(*sent) != 0 {
		t.Fatalf("group: signals %v, err %v", *sent, err)
	}
}

func TestGroupLookupFailureNeverSignals(t *testing.T) {
	fakePS(t, "echo '42420 1 42420 501 S Sun Nov 15 15:45:51 2026 helper'")
	failRecheck(t, errors.New("ps blocked"))
	sent := interceptSignals(t)
	c := &Child{cmd: &exec.Cmd{Process: &os.Process{Pid: lookupProc.PID}}}
	if err := c.killGroup([]Proc{lookupProc}); err == nil || len(*sent) != 0 {
		t.Fatalf("group: signals %v, err %v", *sent, err)
	}
}

func TestShutdownKeepsLiveRecordForUnknownProcess(t *testing.T) {
	for _, source := range []string{"tree", "sweep"} {
		t.Run(source, func(t *testing.T) {
			self, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			c, err := Start(Spec{Argv: []string{self}, Env: []string{helperEnv + "=exit", "GORACE=atexit_sleep_ms=0"},
				LiveDir: t.TempDir(), PollEvery: time.Hour, SessionPollEvery: time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			defer c.cmd.Process.Kill()
			pending := lookupProc
			if c.leader.Start != "" {
				pending.Start = c.leader.Start
			}
			if source == "tree" {
				c.tracker.mu.Lock()
				c.tracker.seen[pending.PID] = pending
				c.tracker.mu.Unlock()
			} else {
				orig := membersOf
				membersOf = func(int) ([]Proc, bool, error) { return []Proc{pending}, true, nil }
				t.Cleanup(func() { membersOf = orig })
			}
			orig := recheckPID
			recheckPID = func(pid int) (Proc, error) {
				if pid == pending.PID {
					return Proc{}, errors.New("ps blocked")
				}
				return listPID(pid)
			}
			t.Cleanup(func() { recheckPID = orig })
			r := c.Shutdown(time.Second)
			kr := r.Kill
			if source == "sweep" {
				kr = r.Swept
			}
			if len(kr.Unknown) != 1 || slices.Contains(pids(kr.Gone), pending.PID) {
				t.Fatalf("shutdown report %+v", r)
			}
			if _, err := os.Stat(c.record); err != nil {
				t.Fatalf("shutdown removed live record for unknown process: %v", err)
			}
			if rec := readRecord(t, c.record); !slices.Contains(pids(rec.Tracked), pending.PID) {
				t.Fatal("live record omitted the unknown process")
			}
		})
	}
}
