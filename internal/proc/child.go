package proc

import (
	"cmp"
	"errors"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// sessionMarkers are set by a parent Claude Code session. A harness that
// inherits them behaves as if it were nested inside that session.
var sessionMarkers = []string{"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_SSE_PORT"}

// ScrubEnv returns env without the parent's session markers.
func ScrubEnv(env []string) []string {
	return slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return slices.Contains(sessionMarkers, name)
	})
}

// Spec says how to start a child.
type Spec struct {
	Argv      []string
	Dir       string
	Env       []string      // KEY=VALUE pairs added to the scrubbed parent environment
	Stderr    io.Writer     // nil discards it
	PollEvery time.Duration // how often to look for new descendants; default 200 ms
}

// Child is a running harness process in a process group of its own, with its
// descendants tracked from the start.
type Child struct {
	Stdin  io.WriteCloser
	Stdout io.ReadCloser

	cmd     *exec.Cmd
	tracker *Tracker
	exited  chan struct{}
	stop    chan struct{}
	polling chan struct{}

	once   sync.Once
	report ShutdownReport
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
	cmd := exec.Command(s.Argv[0], s.Argv[1:]...)
	cmd.Dir, cmd.Stdin, cmd.Stdout, cmd.Stderr = s.Dir, inR, outW, s.Stderr
	cmd.Env = append(ScrubEnv(os.Environ()), s.Env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 2 * time.Second // a descendant holding stderr open must not hang Wait
	err = cmd.Start()
	inR.Close()
	outW.Close()
	if err != nil {
		inW.Close()
		outR.Close()
		return nil, err
	}
	c := &Child{Stdin: inW, Stdout: outR, cmd: cmd, exited: make(chan struct{}),
		stop: make(chan struct{}), polling: make(chan struct{})}
	go func() { cmd.Wait(); close(c.exited) }()
	c.tracker = NewTracker(cmd.Process.Pid, func() bool {
		select {
		case <-c.exited:
			return false
		default:
			return true
		}
	})
	every := cmp.Or(s.PollEvery, 200*time.Millisecond)
	go func() {
		defer close(c.polling)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			c.tracker.Poll()
			select {
			case <-c.stop:
				return
			case <-t.C:
			}
		}
	}()
	return c, nil
}

// PID is the child's process ID, which is also its process group ID.
func (c *Child) PID() int { return c.cmd.Process.Pid }

// Exited is closed once the child has exited and been reaped.
func (c *Child) Exited() <-chan struct{} { return c.exited }

// Tracked returns every descendant seen so far.
func (c *Child) Tracked() []Proc { return c.tracker.Tracked() }

// ShutdownReport says how a child was shut down.
type ShutdownReport struct {
	ExitedOnItsOwn bool       `json:"exited_on_its_own"` // exited within the grace period after stdin closed
	Tracked        []Proc     `json:"tracked"`           // every descendant seen while it ran
	Kill           KillReport `json:"kill"`              // the identity-checked kill of the child and its descendants
	Err            string     `json:"error,omitempty"`   // listing processes failed at some point
}

// Shutdown stops the child and everything it started. It looks at the tree a
// last time while lineage is still visible, closes stdin, gives the child
// grace to exit, then kills its process group and every tracked process that
// is still running as recorded. Later calls return the first call's report.
func (c *Child) Shutdown(grace time.Duration) ShutdownReport {
	c.once.Do(func() { c.report = c.shutdown(grace) })
	return c.report
}

func (c *Child) shutdown(grace time.Duration) ShutdownReport {
	close(c.stop)
	<-c.polling
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

	tracked := c.tracker.Tracked()
	targets := slices.Clone(tracked)
	if rootErr == nil {
		targets = append(targets, rootRec)
	}
	c.killGroup(targets)
	kr, err := Kill(targets)
	if err == nil && !errors.Is(rootErr, errNotRunning) {
		err = rootErr
	}
	if err == nil {
		err = c.tracker.Err()
	}
	select {
	case <-c.exited:
	case <-time.After(5 * time.Second):
	}
	c.Stdout.Close()
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
// still running as recorded. A group ID cannot be reused while the group has a
// member, so this never reaches an unrelated group.
func (c *Child) killGroup(targets []Proc) {
	pgid := c.PID()
	all, err := List()
	if err != nil {
		return
	}
	for _, p := range targets {
		if now, ok := all[p.PID]; ok && now.Same(p) && now.PGID == pgid {
			syscall.Kill(-pgid, syscall.SIGKILL)
			return
		}
	}
}
