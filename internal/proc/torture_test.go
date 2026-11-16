package proc

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"syscall"
	"testing"
	"time"

	"refraktal.dev/refraktal/internal/acp"
	"refraktal.dev/refraktal/internal/acp/acptest"
)

var torture = flag.Bool("torture", false, "run heavier, still capped process and timeout torture tests")

func startTorture(t *testing.T, s acptest.TortureSpec, role string, sessions bool) *Child {
	t.Helper()
	argv, env, err := acptest.TortureCommand(s, role)
	if err != nil {
		t.Fatal(err)
	}
	sp := Spec{Argv: argv, Env: env, PollEvery: time.Hour, SessionPollEvery: time.Hour,
		LiveDir: filepath.Join(s.Dir, "live")}
	if role == "cycle" {
		// Exercise both background pollers at production defaults. Faster
		// tickers make four workers compete on hundreds of global ps scans
		// per second (an occupied session needs two), before shutdown's scans.
		sp.PollEvery, sp.SessionPollEvery = 0, 0
	}
	if sessions {
		sp.SessionsFile = filepath.Join(s.Dir, "sessions")
	}
	c, err := Start(sp)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Shutdown(0) })
	return c
}

func tortureReady(t *testing.T, s acptest.TortureSpec, count int) {
	t.Helper()
	acptest.WaitFor(t, 15*time.Second, "fake process inventory", func() bool {
		return len(acptest.TortureInventory(s)) == count
	})
	if err := acptest.TortureError(s); err != nil {
		t.Fatal(err)
	}
}

func tortureGone(t *testing.T, s acptest.TortureSpec) {
	t.Helper()
	// Scan only for this fixture's unique argv marker, including descendants
	// that raced their parent's death before writing their inventory record.
	// Never feed these results into Kill or SweepSession.
	var survivors []int
	var quietSince time.Time
	defer func() {
		if len(survivors) > 0 {
			t.Logf("surviving fixture PIDs: %v", survivors)
		}
	}()
	acptest.WaitFor(t, 3*time.Second, "zero surviving fixture processes", func() bool {
		all, err := List()
		if err != nil {
			t.Fatal(err)
		}
		survivors = survivors[:0]
		for pid, p := range all {
			if acptest.TortureMatches(s, p.Cmd) {
				survivors = append(survivors, pid)
			}
		}
		if len(survivors) != 0 {
			quietSince = time.Time{}
			return false
		}
		if quietSince.IsZero() {
			quietSince = time.Now()
		}
		return time.Since(quietSince) >= 100*time.Millisecond
	})
	if err := acptest.TortureError(s); err != nil {
		t.Fatal(err)
	}
	acptest.TortureSlots(t, s)
}

func tortureReport(t *testing.T, r ShutdownReport) {
	t.Helper()
	if r.Err != "" || len(r.Kill.Alive)+len(r.Kill.Unknown)+len(r.Swept.Alive)+len(r.Swept.Unknown) != 0 {
		t.Fatalf("incomplete shutdown: error %q, tree alive/unknown %d/%d, sweep alive/unknown %d/%d",
			r.Err, len(r.Kill.Alive), len(r.Kill.Unknown), len(r.Swept.Alive), len(r.Swept.Unknown))
	}
}

func fdCount(t *testing.T) int {
	t.Helper()
	dir := "/proc/self/fd"
	if runtime.GOOS == "darwin" {
		dir = "/dev/fd"
	}
	// Names only: os.ReadDir may lstat each entry, and on macOS the listing
	// includes the descriptor used to read it, already closed by then
	// ("lstat /dev/fd/4: bad file descriptor"). Both counts include that one.
	f, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil {
		t.Fatal(err)
	}
	return len(names)
}

// tortureCycleDiagnostics keeps command lines out of test output. A fresh
// listing distinguishes a still-live untracked leaf from an expired fixture,
// and its duration distinguishes slow process enumeration from enrollment.
func tortureCycleDiagnostics(t *testing.T, s acptest.TortureSpec, c *Child, leaf int, elapsed time.Duration) {
	t.Helper()
	exited, reaped := false, false
	select {
	case <-c.exited:
		exited = true
	default:
	}
	select {
	case <-c.reaped:
		reaped = true
	default:
	}
	t.Logf("cycle discovery: Poll elapsed=%s, tracker error=%v, root=%d exited=%t reaped=%t, fixture expiry in=%s, inventory=%+v",
		elapsed, c.tracker.Err(), c.PID(), exited, reaped, time.Until(s.Deadline), acptest.TortureInventory(s))
	logProc := func(source string, p Proc) {
		t.Logf("%s: pid=%d ppid=%d pgid=%d uid=%d start=%q", source, p.PID, p.PPID, p.PGID, p.UID, p.Start)
	}
	tracked := c.Tracked()
	t.Logf("tracker saw %d processes", len(tracked))
	for _, p := range tracked {
		logProc("tracked", p)
	}
	start := time.Now()
	all, err := List()
	t.Logf("fresh listing: elapsed=%s, processes=%d, error=%v, root present=%t, leaf %d present=%t",
		time.Since(start), len(all), err, all[c.PID()].PID != 0, leaf, all[leaf].PID != 0)
	for _, p := range all {
		if p.PID == c.PID() || p.PID == leaf || p.PPID == c.PID() || acptest.TortureMatches(s, p.Cmd) {
			logProc("listed", p)
		}
	}
}

func TestTortureStartKillCycles(t *testing.T) {
	checkSentinel := acptest.StartSentinel(t)
	cycles, workers := 20, 4
	if *torture {
		cycles, workers = 200, 8
	}
	// Initialize the runtime's persistent pipe poll descriptor before counting
	// FDs; this is infrastructure, not a per-child descriptor leak.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	r.SetReadDeadline(time.Now().Add(time.Second))
	r.Close()
	w.Close()
	fds, goroutines := fdCount(t), runtime.NumGoroutine()
	t.Run("workers", func(t *testing.T) {
		for worker := 0; worker < workers; worker++ {
			t.Run(fmt.Sprint(worker), func(t *testing.T) {
				t.Parallel()
				for i := worker; i < cycles; i += workers {
					s := acptest.NewTortureSpec(t, *torture)
					c := startTorture(t, s, "cycle", false)
					tortureReady(t, s, 2)
					inventory := acptest.TortureInventory(s)
					root := slices.IndexFunc(inventory, func(p acptest.TortureProcess) bool {
						return p.Role == "cycle" && p.PID == c.PID()
					})
					leaf := slices.IndexFunc(inventory, func(p acptest.TortureProcess) bool {
						return p.Role == "leaf" && p.Parent == c.PID()
					})
					if root < 0 || leaf < 0 {
						t.Fatalf("cycle %d: invalid root/leaf inventory: %+v", i, inventory)
					}
					// Inventory is written after exec. The leaf stays a direct
					// child until the shared safety expiry, so one successful
					// post-ready scan must enroll it. Repeated Poll calls only
					// add global ps load; List already bounds its own command.
					start := time.Now()
					err := c.tracker.Poll()
					elapsed := time.Since(start)
					tracked := c.Tracked()
					if err != nil || len(tracked) != 1 || tracked[0].PID != inventory[leaf].PID || tracked[0].PPID != c.PID() {
						tortureCycleDiagnostics(t, s, c, inventory[leaf].PID, elapsed)
						t.Fatalf("cycle %d: post-ready Poll took %s: error=%v, tracked PIDs=%v, want only leaf %d of root %d",
							i, elapsed, err, pids(tracked), inventory[leaf].PID, c.PID())
					}
					tortureReport(t, c.Shutdown(5*time.Millisecond))
					tortureGone(t, s)
					if entries, err := os.ReadDir(filepath.Join(s.Dir, "live")); err != nil || len(entries) != 0 {
						t.Errorf("cycle %d left live records: %d, error %v", i, len(entries), err)
					}
				}
			})
		}
	})
	var afterFDs, afterGoroutines int
	defer func() {
		t.Logf("%d cycles, %d workers: FDs %d -> %d, goroutines %d -> %d", cycles, workers, fds, afterFDs, goroutines, afterGoroutines)
	}()
	acptest.WaitFor(t, 5*time.Second, fmt.Sprintf("FDs <= %d and goroutines <= %d", fds, goroutines), func() bool {
		afterFDs, afterGoroutines = fdCount(t), runtime.NumGoroutine()
		return afterFDs <= fds && afterGoroutines <= goroutines
	})
	checkSentinel()
}

func TestTortureWideDeepRecordedTrees(t *testing.T) {
	checkSentinel := acptest.StartSentinel(t)
	s := acptest.NewTortureSpec(t, *torture)
	s.Width, s.Depth = 4, 4
	if *torture {
		s.Width, s.Depth = 12, 8
	}
	c := startTorture(t, s, "tree", true)
	// Harness + width chains + two double-fork daemon paths (3 each).
	tortureReady(t, s, 1+s.Width*s.Depth+6)
	var daemons []Proc
	acptest.WaitFor(t, 10*time.Second, "reparented daemons and a recorded escaped session", func() bool {
		all, err := List()
		if err != nil {
			t.Fatal(err)
		}
		daemons = nil
		for _, p := range acptest.TortureInventory(s) {
			if p.Role == "daemon" {
				q, ok := all[p.PID]
				if !ok || q.PPID == p.Parent || all[p.Parent].PID != 0 {
					return false
				}
				daemons = append(daemons, q)
			}
		}
		leaders, err := RecordedSessions(filepath.Join(s.Dir, "sessions"))
		return err == nil && len(leaders) == 1 && leaders[0].PID != c.PID() && len(daemons) == 2
	})
	var escaped bool
	for _, p := range daemons {
		sid, err := getsid(p.PID)
		if err != nil {
			t.Fatal(err)
		}
		escaped = escaped || sid != c.PID()
	}
	if !escaped {
		t.Fatal("fixture did not create a setsid escape")
	}
	tortureReport(t, c.Shutdown(10*time.Millisecond))
	tortureGone(t, s)
	checkSentinel()
}

func TestTortureClosedPipesIgnoringTERM(t *testing.T) {
	checkSentinel := acptest.StartSentinel(t)
	repeats := 1
	if *torture {
		repeats = 10
	}
	for i := 0; i < repeats; i++ {
		s := acptest.NewTortureSpec(t, *torture)
		c := startTorture(t, s, "closedio", false)
		tortureReady(t, s, 2)
		// A real pending ACP handshake must observe EOF, not wait for the
		// context deadline or for the helper's safety expiry.
		client := acp.New(c.Stdout, c.Stdin, acp.Options{})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		done := make(chan error, 1)
		go func() { _, err := client.Initialize(ctx); done <- err }()
		acptest.WaitFor(t, 5*time.Second, "ACP request read by the fake", func() bool { return acptest.TortureHas(s, "request-read") })
		acptest.TortureMark(t, s, "close-stdio")
		acptest.WaitFor(t, 5*time.Second, "closed stdin and stdout", func() bool { return acptest.TortureHas(s, "closed-stdio") })
		select {
		case err := <-done:
			if err == nil || errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("ACP returned %v, want prompt EOF/write error", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("ACP hung after stdout closed")
		}
		cancel()
		// Confirm SIGTERM is ignored by both fixture processes before asking
		// Shutdown to use its hard kill. Targets come only from our inventory.
		all, err := List()
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range acptest.TortureInventory(s) {
			rec := all[p.PID]
			now, err := listPID(p.PID)
			if err != nil || !now.Same(rec) || !acptest.TortureMatches(s, now.Cmd) {
				t.Fatal("owned TERM target changed identity")
			}
			if err := syscall.Kill(p.PID, syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
		}
		start := time.Now()
		rep := c.Shutdown(100 * time.Millisecond)
		if rep.ExitedOnItsOwn || time.Since(start) > 5*time.Second {
			t.Error("shutdown did not hard-kill within the grace plus bounded cleanup")
		}
		tortureReport(t, rep)
		tortureGone(t, s)
	}
	checkSentinel()
}

func TestTortureSessionSweepRacesCappedForkBomb(t *testing.T) {
	checkSentinel := acptest.StartSentinel(t)
	s := acptest.NewTortureSpec(t, *torture)
	c := startTorture(t, s, "bomb", false)
	// Stop both background pollers while the launcher waits at a gate.
	// Each consumes one stop message and exits. Shutdown can still close
	// the channel and join them normally; no timing guess is involved.
	for poller := 0; poller < 2; poller++ {
		select {
		case c.stop <- struct{}{}:
		case <-time.After(10 * time.Second):
			t.Fatal("poller did not stop before detachment")
		}
	}
	<-c.polling
	<-c.spoll
	acptest.TortureMark(t, s, "start")
	tortureReady(t, s, 3)
	select {
	case <-c.Exited():
	case <-time.After(10 * time.Second):
		t.Fatal("bomb launcher did not exit")
	}
	var seed int
	for _, p := range acptest.TortureInventory(s) {
		if p.Role == "bomb-seed" {
			seed = p.PID
		}
	}
	if seed == 0 {
		t.Fatal("no bomb seed")
	}
	// The hook only gates this fixture's actual session lookup. It never
	// substitutes a session or supplies unrelated processes to a kill.
	original := membersOf
	entered := false
	membersOf = func(sid int) ([]Proc, bool, error) {
		if sid == c.PID() && !entered {
			entered = true
			acptest.TortureMark(t, s, "fork")
			acptest.WaitFor(t, 10*time.Second, "forks during session sweep", func() bool {
				return len(acptest.TortureInventory(s)) >= 5
			})
		}
		return original(sid)
	}
	// Restore even if an assertion in the hook terminates this test.
	var rep ShutdownReport
	func() {
		defer func() { membersOf = original }()
		rep = c.Shutdown(0)
	}()
	if !entered || slices.Contains(pids(rep.Tracked), seed) {
		t.Fatal("fixture did not exercise the untracked session-sweep path")
	}
	tortureReport(t, rep)
	tortureGone(t, s)
	t.Logf("bomb: %d/%d permanent launch slots spent", acptest.TortureSlots(t, s), s.Cap)
	checkSentinel()
}
