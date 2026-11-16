package proc

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"refraktal.dev/refraktal/internal/acp/acptest"
)

func TestChildRecordPersistsSameCountDescendantIdentity(t *testing.T) {
	checkSentinel := acptest.StartSentinel(t)
	// Only our owner is listed by this fake. Injecting tracker snapshots
	// tests registry persistence independently of PID-allocation timing.
	fakePS(t, fmt.Sprintf("echo '%d 1 %d %d S Sun Nov 15 15:45:50 2026 owner'", os.Getpid(), os.Getpid(), os.Getuid()))
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c, err := Start(Spec{Argv: []string{self}, Env: []string{helperEnv + "=exit", "GORACE=atexit_sleep_ms=0"},
		LiveDir: t.TempDir(), PollEvery: time.Millisecond, SessionPollEvery: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		close(c.stop)
		<-c.polling
		<-c.spoll
		c.Stdin.Close()
		c.reap()
		c.Stdout.Close()
	})
	acptest.WaitFor(t, 5*time.Second, "self-expiring fake exit", func() bool {
		select {
		case <-c.Exited():
			return true
		default:
			return false
		}
	})
	old := Proc{PID: 102, PPID: c.PID(), Start: "Sun Nov 15 15:45:51 2026"}
	setObserved := func(p Proc) {
		c.tracker.mu.Lock()
		c.tracker.seen[p.PID] = p
		c.tracker.mu.Unlock()
	}
	stored := func(want Proc) bool {
		r, err := loadRecord(c.record)
		return err == nil && len(r.Tracked) == 1 && r.Tracked[0].Same(want)
	}
	setObserved(old)
	acptest.WaitFor(t, 5*time.Second, "first descendant identity in live record", func() bool { return stored(old) })
	newer := old
	newer.Start = "Sun Nov 15 15:45:52 2026"
	setObserved(newer)
	acptest.WaitFor(t, time.Second, "same-count descendant identity in live record", func() bool { return stored(newer) })
	checkSentinel()
}

func TestChildRecordKeepsNewestVerifiedIdentity(t *testing.T) {
	old := Proc{PID: 102, Start: "Sun Nov 15 15:45:51 2026"}
	newer := old
	newer.Start = "Sun Nov 15 15:45:52 2026"
	for _, sessionNewer := range []bool{false, true} {
		t.Run(fmt.Sprintf("session_newer_%t", sessionNewer), func(t *testing.T) {
			tree, session := newer, old
			if sessionNewer {
				tree, session = old, newer
			}
			tracker := NewTracker(101, func() bool { return true })
			tracker.seen[tree.PID] = tree
			c := &Child{tracker: tracker, record: filepath.Join(t.TempDir(), "child.json"), members: map[int]Proc{session.PID: session}}
			c.writeRecord()
			r := readRecord(t, c.record)
			if len(r.Tracked) != 1 || !r.Tracked[0].Same(newer) {
				t.Fatalf("fresh verified identity masked by stale observation: %+v", r.Tracked)
			}
		})
	}
}

func TestSessionRecordRefreshesReusedPID(t *testing.T) {
	old := Proc{PID: 102, PGID: 101, Start: "Sun Nov 15 15:45:51 2026"}
	newer := old
	newer.Start = "Sun Nov 15 15:45:52 2026"
	c := &Child{
		tracker: NewTracker(101, func() bool { return true }),
		record:  filepath.Join(t.TempDir(), "child.json"),
		leader:  Proc{PID: 101, Start: "Sun Nov 15 15:45:50 2026"},
		members: map[int]Proc{old.PID: old},
		dropped: map[string]bool{},
	}
	c.writeRecord()
	orig := membersOf
	membersOf = func(int) ([]Proc, bool, error) { return []Proc{newer}, true, nil }
	t.Cleanup(func() { membersOf = orig })
	c.checkSessions()
	r := readRecord(t, c.record)
	if len(r.Tracked) != 1 || !r.Tracked[0].Same(newer) {
		t.Fatalf("same-count session identity was not saved: %+v", r.Tracked)
	}
}

func TestChildRecordWritesConcurrent(t *testing.T) {
	c := &Child{
		tracker: NewTracker(100, func() bool { return true }),
		record:  filepath.Join(t.TempDir(), "child.json"),
		members: map[int]Proc{},
		recordBase: Record{
			Owner: Proc{PID: 100, Start: "owner"},
			Child: Proc{PID: 101, Start: "child"},
		},
	}
	concurrentRecordWrites(c)
	if c.recordErr != nil {
		t.Fatalf("concurrent live record writes failed: %v", c.recordErr)
	}
	r := readRecord(t, c.record)
	if !r.Owner.Same(c.recordBase.Owner) || !r.Child.Same(c.recordBase.Child) {
		t.Fatalf("live record changed identities: %+v", r)
	}
}

func TestLiveRecordWritesConcurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "child.json")
	const writers = 16
	start := make(chan struct{})
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	wg.Add(writers)
	for range writers {
		go func() {
			defer wg.Done()
			<-start
			for range 16 {
				if err := writeRecord(path, Record{Child: Proc{PID: 101, Start: "child"}}); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent atomic record write: %v", err)
	}
	if r := readRecord(t, path); r.Child.PID != 101 {
		t.Fatalf("invalid final record: %+v", r)
	}
}

func TestChildRecordErrorsConcurrent(t *testing.T) {
	c := &Child{
		tracker: NewTracker(100, func() bool { return true }),
		record:  filepath.Join(t.TempDir(), "missing", "child.json"),
		members: map[int]Proc{},
	}
	concurrentRecordWrites(c)
	if c.recordErr == nil {
		t.Fatal("live record write error was lost")
	}
}

func concurrentRecordWrites(c *Child) {
	const writers = 16
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(writers)
	for range writers {
		go func() {
			defer wg.Done()
			<-start
			for range 16 {
				c.writeRecord()
			}
		}()
	}
	close(start)
	wg.Wait()
}
