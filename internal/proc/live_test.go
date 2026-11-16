package proc

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

func readRecord(t *testing.T, path string) Record {
	t.Helper()
	var r Record
	b, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(b, &r)
	}
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestLiveRecordFollowsTheChild(t *testing.T) {
	dir := t.TempDir()
	self, _ := os.Executable()
	c, err := Start(Spec{Argv: []string{self}, Env: []string{helperEnv + "=detach", "GORACE=atexit_sleep_ms=0"},
		PollEvery: 20 * time.Millisecond, LiveDir: dir, Labels: map[string]string{"harness": "fake"}})
	if err != nil {
		t.Fatal(err)
	}
	grandchild := readPID(t, bufio.NewReader(c.Stdout))
	defer syscall.Kill(grandchild, syscall.SIGKILL)

	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(files) != 1 {
		t.Fatalf("want one live record, got %v", files)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		r := readRecord(t, files[0])
		if slices.Contains(pids(r.Tracked), grandchild) {
			if r.Owner.PID != os.Getpid() || r.Owner.Start == "" || r.Child.PID != c.PID() || r.Labels["harness"] != "fake" {
				t.Fatalf("record %+v", r)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d never reached the record", grandchild)
		}
	}
	if rep := c.Shutdown(2 * time.Second); rep.Err != "" {
		t.Fatal(rep.Err)
	}
	if _, err := os.Stat(files[0]); !os.IsNotExist(err) {
		t.Fatal("record not removed after a clean shutdown")
	}
}

// deadOwner returns the identity of a process that has exited.
func deadOwner(t *testing.T) Proc {
	t.Helper()
	p := exec.Command("sleep", "30")
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	all, _ := List()
	rec := all[p.Process.Pid]
	p.Process.Kill()
	p.Wait()
	return rec
}

func listed(t *testing.T, pid int) Proc {
	t.Helper()
	all, err := List()
	if err != nil || all[pid].PID != pid {
		t.Fatalf("pid %d not listed: %v", pid, err)
	}
	return all[pid]
}

func TestCleanLeftovers(t *testing.T) {
	dir := t.TempDir()
	all, _ := List()
	me := all[os.Getpid()]

	orphan := startSleep(t, &syscall.SysProcAttr{Setpgid: true})     // left by a dead run
	stillOwned := startSleep(t, &syscall.SysProcAttr{Setpgid: true}) // belongs to a live run
	reused := startSleep(t, &syscall.SysProcAttr{Setpgid: true})     // its PID was recorded for another process
	stale := listed(t, reused)
	stale.Start = "Mon Jan 1 00:00:00 2001"

	write := func(name string, r Record) {
		if err := writeRecord(filepath.Join(dir, name), r); err != nil {
			t.Fatal(err)
		}
	}
	write("dead.json", Record{Owner: deadOwner(t), Child: listed(t, orphan)})
	write("live.json", Record{Owner: me, Child: listed(t, stillOwned)})
	write("reused.json", Record{Owner: deadOwner(t), Child: stale})
	os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{"), 0o600)

	r, err := CleanLeftovers(dir)
	if err != nil {
		t.Fatal(err)
	}
	if r.Records != 4 || r.Live != 1 || r.Dead != 2 || len(r.Errors) != 1 {
		t.Fatalf("report %+v", r)
	}
	if !slices.Contains(pids(r.Kill.Killed), orphan) || running(t, orphan) {
		t.Fatalf("orphan %d not killed: %+v", orphan, r.Kill)
	}
	if !running(t, stillOwned) || !running(t, reused) {
		t.Fatal("killed a process of a live run, or a reused PID")
	}
	if !slices.Contains(pids(r.Kill.Changed), reused) {
		t.Fatalf("reused PID not reported as changed: %+v", r.Kill)
	}
	left, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	for i := range left {
		left[i] = filepath.Base(left[i])
	}
	slices.Sort(left)
	if strings.Join(left, ",") != "broken.json,live.json" {
		t.Fatalf("records left: %v", left)
	}
}

func TestCleanLeftoversWithoutAFolder(t *testing.T) {
	r, err := CleanLeftovers(filepath.Join(t.TempDir(), "missing"))
	if err != nil || r.Records != 0 {
		t.Fatalf("%+v %v", r, err)
	}
}

// Dead-run cleanup kills session members by identity: the session poll put
// a detached member, never tracked by lineage, into the live record.
func TestDeadRunCleanupKillsSessionMembersByIdentity(t *testing.T) {
	live := t.TempDir()
	self, _ := os.Executable()
	c, err := Start(Spec{Argv: []string{self}, Env: []string{helperEnv + "=escape", "GORACE=atexit_sleep_ms=0"},
		PollEvery: time.Hour, LiveDir: live, SessionPollEvery: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	gc := readPID(t, bufio.NewReader(c.Stdout))
	defer syscall.Kill(gc, syscall.SIGKILL)
	files, _ := filepath.Glob(filepath.Join(live, "*.json"))
	if len(files) != 1 {
		t.Fatalf("records %v", files)
	}
	for deadline := time.Now().Add(5 * time.Second); !slices.Contains(pids(readRecord(t, files[0]).Tracked), gc); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the session poll did not record the detached member")
		}
	}
	// The owner dies without shutting down: rewrite the record as a dead run's.
	rec := readRecord(t, files[0])
	rec.Owner = deadOwner(t)
	writeRecord(files[0], rec)
	r, err := CleanLeftovers(live)
	if err != nil || running(t, gc) || !slices.Contains(pids(r.Kill.Killed), gc) {
		t.Fatalf("recorded member %d survived: %+v %v", gc, r, err)
	}
	c.Shutdown(time.Second)
}
