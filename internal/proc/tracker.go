package proc

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"sync"
	"syscall"
	"time"
)

// Tracker records the descendants of one root process, whatever their
// process group.
//
// A process is tracked when its parent is the root or a tracked process that
// is still alive, or when it is in a process group whose leader (the process
// with PID == PGID) is the root or is tracked. The second rule catches a tool's
// background child that was reparented to PID 1 after its shell exited,
// provided the shell, as group leader, was seen first. Membership alone never
// widens tracking: a tracked process in someone else's group does not pull in
// that group's other members.
//
// A leader's group counts while the leader or an already verified member
// still holds it. Once that continuity is lost, or the leader's identity
// changes, the group is retired permanently: its ID may have been reused.
// The unreaped root keeps its own group ID reserved even when List omits it
// as a zombie. A process that detaches and is reparented between two polls,
// with its group leader never seen, escapes.
type Tracker struct {
	root    int
	rootUp  func() bool // reports whether the root has not yet been reaped (its PID cannot be reused until then)
	ownPGID int

	mu      sync.Mutex
	seen    map[int]Proc
	groups  map[int]Proc // group leaders verified while they were tracked
	retired map[int]bool // a retired group ID never widens tracking again
	err     error
}

// NewTracker tracks the descendants of root. rootUp reports whether root is
// still unreaped; while it is, root's PID cannot belong to another process.
func NewTracker(root int, rootUp func() bool) *Tracker {
	return &Tracker{root: root, rootUp: rootUp, ownPGID: syscall.Getpgrp(), seen: map[int]Proc{},
		groups: map[int]Proc{}, retired: map[int]bool{}}
}

// Poll lists the machine's processes once and adds any new descendants.
func (t *Tracker) Poll() error {
	all, err := List()
	t.mu.Lock()
	defer t.mu.Unlock()
	if err != nil {
		t.err = err
		return err
	}
	t.add(all)
	return nil
}

func (t *Tracker) add(all map[int]Proc) {
	rootUp := t.rootUp()
	for pgid, leader := range t.groups {
		now, present := all[pgid]
		continuous := present && now.Same(leader) && now.PGID == pgid
		if !present {
			continuous = pgid == t.root && rootUp
			for pid, p := range t.seen {
				if q, ok := all[pid]; ok && p.Same(q) && p.PGID == pgid && q.PGID == pgid {
					continuous = true
					break
				}
			}
		}
		if !continuous {
			t.retired[pgid] = true
			delete(t.groups, pgid)
		}
	}
	alive := func(pid int) bool { // a tracked process still running as recorded
		p, ok := t.seen[pid]
		return ok && p.Same(all[pid])
	}
	for changed := true; changed; {
		changed = false
		// A previously unseen group needs a currently verified tracked
		// leader. An old record of an exited leader cannot establish trust.
		for pid, p := range t.seen {
			if now, running := all[pid]; running && now.Same(p) && now.PGID == pid && pid != t.ownPGID && !t.retired[pid] {
				t.groups[pid] = now
			}
		}
		if q, ok := all[t.root]; ok && rootUp && q.PGID == t.root && q.PGID != t.ownPGID && !t.retired[t.root] {
			t.groups[t.root] = q
		}
		for pid, p := range all {
			if pid == t.root {
				continue
			}
			if previous, ok := t.seen[pid]; ok && previous.Same(p) {
				if previous != p {
					t.seen[pid] = p
					changed = true
				}
				continue
			}
			_, group := t.groups[p.PGID]
			if (p.PPID == t.root && rootUp) || alive(p.PPID) || group {
				t.seen[pid] = p
				changed = true
			}
		}
	}
}

// Tracked returns every process seen so far, by PID, including those that
// have since exited.
func (t *Tracker) Tracked() []Proc {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Proc, 0, len(t.seen))
	for _, p := range t.seen {
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b Proc) int { return cmp.Compare(a.PID, b.PID) })
	return out
}

// Err returns the last error from listing processes, if any.
func (t *Tracker) Err() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.err
}

// KillReport says what Kill did with each process it was given.
type KillReport struct {
	Killed  []Proc `json:"killed"`  // matched its record and was sent SIGKILL
	Gone    []Proc `json:"gone"`    // had already exited
	Changed []Proc `json:"changed"` // its PID now belongs to a different process: left alone
	Alive   []Proc `json:"alive"`   // sent SIGKILL but still running killConfirm (5 s) later
	Unknown []Proc `json:"unknown"` // lookup failed: identity or exit could not be confirmed
}

// Kill sends SIGKILL to each process that is still running as recorded: same
// PID and start time, checked again immediately before its own signal, so the
// window for a PID reuse is as small as one ps call. A process whose PID has
// been reused is not touched. Kill never targets this process or PID 1.
// killConfirm is how long Kill waits for killed processes to disappear.
const killConfirm = 5 * time.Second

// listForKill lists processes for Kill's confirmation (a variable for tests).
var listForKill = List

func Kill(procs []Proc) (KillReport, error) {
	r := KillReport{Killed: []Proc{}, Gone: []Proc{}, Changed: []Proc{}, Alive: []Proc{}, Unknown: []Proc{}}
	var lookupErr error
	self := syscall.Getpid()
	for _, p := range procs {
		if p.PID <= 1 || p.PID == self {
			r.Changed = append(r.Changed, p)
			continue
		}
		now, err := recheckPID(p.PID)
		switch {
		case errors.Is(err, ErrProcessGone):
			r.Gone = append(r.Gone, p)
		case err != nil:
			r.Unknown = append(r.Unknown, p)
			lookupErr = errors.Join(lookupErr, fmt.Errorf("process %d: %w", p.PID, err))
		case !now.Same(p):
			r.Changed = append(r.Changed, p)
		default:
			signal(p.PID, syscall.SIGKILL)
			r.Killed = append(r.Killed, p)
		}
	}
	if len(r.Killed) == 0 {
		return r, lookupErr
	}
	// A killed process must still be scheduled to exit, which on a loaded
	// machine can take more than a second: wait up to killConfirm before
	// calling it alive (a false "alive" keeps a live record for a process
	// that is about to be gone).
	for deadline := time.Now().Add(killConfirm); ; time.Sleep(50 * time.Millisecond) {
		after, err := listForKill()
		if err != nil {
			r.Alive = r.Alive[:0]
			r.Unknown = append(r.Unknown, r.Killed...)
			return r, errors.Join(lookupErr, err)
		}
		r.Alive = r.Alive[:0]
		for _, p := range r.Killed {
			if now, ok := after[p.PID]; ok && now.Same(p) {
				r.Alive = append(r.Alive, p)
			}
		}
		if len(r.Alive) == 0 || time.Now().After(deadline) {
			return r, lookupErr
		}
	}
}

// recheckPID and signal are variables so tests can simulate a PID that
// changes identity between the check and the signal.
var (
	recheckPID = listPID
	signal     = syscall.Kill
)
