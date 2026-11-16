package proc

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// sessionMarkers are set by a parent Claude Code session. A harness that
// inherits them behaves as if it were nested inside that session.
var sessionMarkers = []string{"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_SSE_PORT",
	// API keys: a harness or tool must not silently bill a key instead of the
	// subscription. A harness gets a key only by explicit opt-in (pass_env in
	// the config; a nested Refraktal's routing key by
	// REFRAKTAL_PASS_GEMINI_KEY=1; docs/DESIGN.md).
	"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY"}

// ScrubEnv returns env without the parent's session markers and API keys.
func ScrubEnv(env []string) []string {
	return slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return slices.Contains(sessionMarkers, name)
	})
}

// Spec says how to start a child.
type Spec struct {
	Argv []string
	Dir  string
	Env  []string // KEY=VALUE pairs added to the scrubbed parent environment
	// BaseEnv, if set, replaces the scrubbed parent environment, such as
	// safegit.Env() for check commands. Session markers and API keys are
	// scrubbed from it too.
	BaseEnv   []string
	Stderr    io.Writer     // nil discards it
	PollEvery time.Duration // how often to look for new descendants; default 200 ms

	// LiveDir, if set, is the shared folder of live records (see Record). The
	// child's record is written there at start, rewritten as tracking grows,
	// and removed after Shutdown kills everything.
	LiveDir string
	Labels  map[string]string // stored in the live record, such as the run ID and harness

	// SessionsFile, if set, is where the shell-prefix helper records the
	// sessions the child's tool commands run in; Shutdown sweeps them too.
	SessionsFile string
	// SessionPollEvery is how often recorded sessions are checked for being
	// empty; default 1 s.
	SessionPollEvery time.Duration
}

// Child is a running harness process in a process group of its own, with its
// descendants tracked from the start.
type Child struct {
	Stdin  io.WriteCloser
	Stdout io.ReadCloser

	cmd     *exec.Cmd
	tracker *Tracker
	exited  chan struct{} // closed when the child has exited (it may not be reaped yet)
	reaped  chan struct{} // closed when the child has been reaped: its PID is free
	reapOne sync.Once
	stop    chan struct{}
	polling chan struct{}
	spoll   chan struct{} // closed when the session poller has stopped

	smu     sync.Mutex
	dropped map[string]bool // sessions (by leader PID and start) seen empty: never swept
	members map[int]Proc    // session members seen by the poller, kept in the live record

	once   sync.Once
	report ShutdownReport

	leader   Proc   // the child as listed just after it started: the session leader
	sessions string // Spec.SessionsFile

	record     string // live record path, or ""
	recordBase Record
	recordErr  error // the last error writing the live record
}

// Start starts the child and begins tracking its descendants.
func Start(s Spec) (*Child, error) {
	if len(s.Argv) == 0 {
		return nil, errors.New("proc: empty command")
	}
	// Our own pipes, not cmd.StdoutPipe: Wait runs as soon as the child exits,
	// and it would close a StdoutPipe while the reader may still be draining it.
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, err
	}
	if err := checkTestLaunch(s.Argv[0]); err != nil {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		return nil, err
	}
	cmd := exec.Command(s.Argv[0], s.Argv[1:]...)
	cmd.Dir, cmd.Stdin, cmd.Stdout, cmd.Stderr = s.Dir, inR, outW, s.Stderr
	base := ScrubEnv(os.Environ())
	if s.BaseEnv != nil {
		base = ScrubEnv(s.BaseEnv)
	}
	cmd.Env = append(append(base, s.Env...), NestEnv+"="+strconv.Itoa(Nest()+1))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // a session of its own, and so a process group of its own
	cmd.WaitDelay = 2 * time.Second                      // a descendant holding stderr open must not hang Wait
	err = cmd.Start()
	inR.Close()
	outW.Close()
	if err != nil {
		inW.Close()
		outR.Close()
		return nil, err
	}
	c := &Child{Stdin: inW, Stdout: outR, cmd: cmd, exited: make(chan struct{}), reaped: make(chan struct{}),
		stop: make(chan struct{}), polling: make(chan struct{}), spoll: make(chan struct{}),
		dropped: map[string]bool{}, members: map[int]Proc{}}
	// Learn of the exit without reaping: an exited, unreaped child keeps its
	// PID, and so its session ID, reserved until Shutdown has swept it. Where
	// that is not possible, fall back to reaping at once.
	go func() {
		if err := waitExited(cmd.Process.Pid); err != nil {
			c.reap()
		}
		close(c.exited)
	}()
	c.tracker = NewTracker(cmd.Process.Pid, func() bool {
		select {
		case <-c.reaped:
			return false
		default:
			return true
		}
	})
	c.sessions = s.SessionsFile
	c.leader = Proc{PID: c.PID()}
	if all, err := List(); err == nil && all[c.PID()].PID == c.PID() {
		c.leader = all[c.PID()]
	}
	if s.LiveDir != "" {
		c.startRecord(s.LiveDir, s.Labels)
	}
	go c.pollSessions(cmp.Or(s.SessionPollEvery, time.Second))
	every := cmp.Or(s.PollEvery, 200*time.Millisecond)
	go func() {
		defer close(c.polling)
		t := time.NewTicker(every)
		defer t.Stop()
		written := 0
		for {
			c.tracker.Poll()
			if n := len(c.tracker.Tracked()); c.record != "" && n != written {
				c.writeRecord()
				written = n
			}
			select {
			case <-c.stop:
				return
			case <-t.C:
			}
		}
	}()
	return c, nil
}

// startRecord writes the child's first live record. Failing to write it does
// not stop the child; the error is reported by Shutdown.
func (c *Child) startRecord(dir string, labels map[string]string) {
	all, err := List()
	if err == nil {
		err = os.MkdirAll(dir, 0o700)
	}
	if err != nil {
		c.recordErr = err
		return
	}
	self := os.Getpid()
	c.recordBase = Record{Owner: all[self], Child: c.leader, Labels: labels}
	c.record = filepath.Join(dir, fmt.Sprintf("%d-%d.json", self, c.PID()))
	c.writeRecord()
}

func (c *Child) writeRecord() {
	r := c.recordBase
	r.Tracked = c.tracker.Tracked()
	c.smu.Lock()
	for pid, p := range c.members {
		if !slices.ContainsFunc(r.Tracked, func(q Proc) bool { return q.PID == pid }) {
			r.Tracked = append(r.Tracked, p)
		}
	}
	c.smu.Unlock()
	if err := writeRecord(c.record, r); err != nil {
		c.recordErr = err
	}
}

// reap waits for the child and frees its PID. It runs once.
func (c *Child) reap() {
	c.reapOne.Do(func() { c.cmd.Wait(); close(c.reaped) })
}

func sessionKey(leader Proc) string { return fmt.Sprintf("%d|%s", leader.PID, leader.Start) }

// pollSessions checks the child's session and every recorded session about
// once a second. A session seen empty is dropped for good: its ID may be
// reused, and a later process with that session ID is a stranger. Members
// seen are kept in the live record, so cleanup after a dead run can kill
// them by identity.
func (c *Child) pollSessions(every time.Duration) {
	defer close(c.spoll)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-t.C:
		}
		c.checkSessions()
	}
}

func (c *Child) sessionLeaders() []Proc {
	leaders := []Proc{c.leader}
	if c.sessions != "" {
		recorded, _ := RecordedSessions(c.sessions)
		leaders = append(leaders, recorded...)
	}
	return leaders
}

func (c *Child) checkSessions() {
	grew := false
	for _, l := range c.sessionLeaders() {
		c.smu.Lock()
		dropped := c.dropped[sessionKey(l)]
		c.smu.Unlock()
		if dropped {
			continue
		}
		members, occupied, err := sessionOf(l)
		if err == nil && !occupied {
			// Confirm with a fresh look: one check lists processes, then asks
			// each for its session, and a fork followed by an exit between the
			// two can make a live session look empty for a moment.
			time.Sleep(50 * time.Millisecond)
			members, occupied, err = sessionOf(l)
		}
		if err != nil {
			continue
		}
		c.smu.Lock()
		if !occupied {
			c.dropped[sessionKey(l)] = true
		}
		for _, m := range members {
			if _, ok := c.members[m.PID]; !ok {
				c.members[m.PID] = m
				grew = true
			}
		}
		c.smu.Unlock()
	}
	if grew && c.record != "" {
		c.writeRecord()
	}
}

// sweepSessions sweeps the child's session and every recorded session never
// seen empty. The sessions file is read twice, the second time after a short
// delay, so a record a tool command appended late is not missed.
func (c *Child) sweepSessions() (KillReport, error) {
	r := KillReport{Killed: []Proc{}, Gone: []Proc{}, Changed: []Proc{}, Alive: []Proc{}, Unknown: []Proc{}}
	done := map[string]bool{}
	var firstErr error
	pass := func() {
		for _, l := range c.sessionLeaders() {
			k := sessionKey(l)
			c.smu.Lock()
			skip := c.dropped[k] || done[k]
			c.smu.Unlock()
			if skip {
				continue
			}
			done[k] = true
			kr, err := SweepSession(l)
			if err != nil && !errors.Is(err, ErrSessionReused) && firstErr == nil {
				firstErr = err
			}
			r.Killed = append(r.Killed, kr.Killed...)
			r.Gone = append(r.Gone, kr.Gone...)
			r.Changed = append(r.Changed, kr.Changed...)
			r.Alive = append(r.Alive, kr.Alive...)
			r.Unknown = append(r.Unknown, kr.Unknown...)
		}
	}
	if c.sessions != "" {
		// From now on a helper that records its session refuses to run its
		// command, so every command that runs was recorded before the reads.
		if err := os.WriteFile(ShutdownMarker(c.sessions), nil, 0o600); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	pass()
	if c.sessions != "" {
		betweenRecordedReads()
		time.Sleep(300 * time.Millisecond)
		pass()
	}
	return r, firstErr
}

// NestEnv counts how many Refraktal-started processes a process descends
// from. Every child gets one more than its parent; Run and Doctor refuse to
// start past a limit, so no recursion can fan out without bound, whatever
// the budgets say.
const NestEnv = "REFRAKTAL_NEST"

// Nest is this process's nesting level (0 if unset or invalid).
func Nest() int {
	n, err := strconv.Atoi(os.Getenv(NestEnv))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// LiveEnv, set to 1, lets a test binary start real executables. Only the live
// scripts set it; without it, go test can never start a real harness.
const LiveEnv = "REFRAKTAL_LIVE"

// checkTestLaunch refuses, inside a test binary, any executable other than
// the test binary itself (which is how fakes and helpers run), unless
// REFRAKTAL_LIVE=1.
func checkTestLaunch(argv0 string) error {
	if !testing.Testing() || os.Getenv(LiveEnv) == "1" {
		return nil
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("proc: inside a test, cannot find the test binary: %w", err)
	}
	path := argv0
	if !filepath.IsAbs(path) {
		if p, err := exec.LookPath(argv0); err == nil {
			path = p
		}
	}
	if !SameFile(path, self) {
		return fmt.Errorf("proc: refusing to start %s inside a test binary: only fakes may run (set %s=1 for live checks)", filepath.Base(argv0), LiveEnv)
	}
	return nil
}

// PID is the child's process ID, which is also its process group ID.
func (c *Child) PID() int { return c.cmd.Process.Pid }

// Exited is closed once the child has exited. Its PID stays reserved until
// Shutdown reaps it, so always call Shutdown.
func (c *Child) Exited() <-chan struct{} { return c.exited }

// ExitCode is the child's exit status once Shutdown has reaped it: -1 if it
// was killed by a signal or is not reaped yet.
func (c *Child) ExitCode() int {
	select {
	case <-c.reaped:
		return c.cmd.ProcessState.ExitCode()
	default:
		return -1
	}
}

// Tracked returns every descendant seen so far.
func (c *Child) Tracked() []Proc { return c.tracker.Tracked() }

// ShutdownReport says how a child was shut down.
type ShutdownReport struct {
	ExitedOnItsOwn bool       `json:"exited_on_its_own"` // exited within the grace period after stdin closed
	Tracked        []Proc     `json:"tracked"`           // every descendant seen while it ran
	Kill           KillReport `json:"kill"`              // the identity-checked kill of the child and its descendants
	Swept          KillReport `json:"swept"`             // processes still in the child's session, or in a session its tool commands recorded, killed by the session sweep
	Err            string     `json:"error,omitempty"`   // listing processes failed at some point
}

// Shutdown stops the child and everything it started. It looks at the tree a
// last time while lineage is still visible, closes stdin, gives the child
// grace to exit, then kills its process group and every tracked process that
// is still running as recorded, and finally sweeps the child's session for
// anything that detached. Later calls return the first call's report.
func (c *Child) Shutdown(grace time.Duration) ShutdownReport {
	c.once.Do(func() { c.report = c.shutdown(grace) })
	return c.report
}

func (c *Child) shutdown(grace time.Duration) ShutdownReport {
	close(c.stop)
	<-c.polling
	<-c.spoll
	c.tracker.Poll()
	rootRec, rootErr := c.rootRecord()

	var r ShutdownReport
	c.Stdin.Close()
	select {
	case <-c.exited:
		r.ExitedOnItsOwn = true
	case <-time.After(grace):
	}
	c.tracker.Poll() // a process that joined a tracked group during shutdown
	if c.record != "" {
		c.writeRecord() // retain the final targets too if cleanup needs a retry
	}

	tracked := c.tracker.Tracked()
	targets := slices.Clone(tracked)
	if rootErr == nil {
		targets = append(targets, rootRec)
	}
	groupErr := c.killGroup(targets)
	kr, err := Kill(targets)
	err = errors.Join(err, groupErr)
	if err == nil && !errors.Is(rootErr, errNotRunning) {
		err = rootErr
	}
	if err == nil {
		err = c.tracker.Err()
	}
	// The child is not reaped yet, so its session ID cannot have been reused.
	swept, serr := c.sweepSessions()
	if err == nil {
		err = serr
	}
	if c.record != "" {
		// The final sweep can discover processes the pollers never recorded.
		// Preserve every known identity whose cleanup needs another attempt.
		c.smu.Lock()
		for _, pending := range [][]Proc{kr.Alive, kr.Unknown, swept.Alive, swept.Unknown} {
			for _, p := range pending {
				if p.Start != "" {
					c.members[p.PID] = p
				}
			}
		}
		c.smu.Unlock()
		c.writeRecord()
	}
	select {
	case <-c.exited:
	case <-time.After(5 * time.Second):
	}
	reaped := make(chan struct{})
	go func() { c.reap(); close(reaped) }()
	select {
	case <-reaped:
	case <-time.After(5 * time.Second):
	}
	c.Stdout.Close()
	r.Swept = swept
	if err == nil {
		err = c.recordErr
	}
	if c.record != "" && err == nil && len(kr.Alive) == 0 && len(kr.Unknown) == 0 && len(swept.Alive) == 0 && len(swept.Unknown) == 0 {
		os.Remove(c.record) // clean exit; otherwise the record stays for CleanLeftovers
	}
	r.Tracked, r.Kill = tracked, kr
	if err != nil {
		r.Err = err.Error()
	}
	return r
}

// rootRecord returns the child's own identity while it is unreaped.
func (c *Child) rootRecord() (Proc, error) {
	all, err := List()
	if err != nil {
		return Proc{}, err
	}
	p, ok := all[c.PID()]
	select {
	case <-c.exited:
		ok = false
	default:
	}
	if !ok {
		return Proc{}, errNotRunning
	}
	return p, nil
}

var errNotRunning = errors.New("not running")

// killGroup kills the child's process group, but only while a member of it is
// still running as recorded, checked again immediately before the signal. A
// group ID cannot be reused while the group has a member, and the leader (our
// child) is not reaped until after this, so its zombie also keeps the ID.
func (c *Child) killGroup(targets []Proc) error {
	pgid := c.PID()
	all, err := List()
	if err != nil {
		return err
	}
	for _, p := range targets {
		if now, ok := all[p.PID]; ok && now.Same(p) && now.PGID == pgid {
			again, err := recheckPID(p.PID)
			if err != nil && !errors.Is(err, ErrProcessGone) {
				return err
			}
			if err == nil && again.Same(p) && again.PGID == pgid {
				signal(-pgid, syscall.SIGKILL)
				return nil
			}
		}
	}
	return nil
}
