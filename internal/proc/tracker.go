package proc

import (
	"cmp"
	"slices"
	"sync"
	"syscall"
	"time"
)

// Tracker records the descendants of one root process, whatever their
// process group.
//
// A process is tracked when its parent is the root or a tracked process that
// is still alive, or when it is in the process group of a tracked process that
// is still alive. The second rule catches a tool's background child that was
// reparented to PID 1 after its shell exited, provided some member of its
// group was seen first. A process that detaches and is reparented between two
// polls, with no tracked member left in its group, escapes.
type Tracker struct {
	root    int
	rootUp  func() bool // reports whether the root has not yet been reaped (its PID cannot be reused until then)
	ownPGID int

	mu   sync.Mutex
	seen map[int]Proc
	err  error
}

// NewTracker tracks the descendants of root. rootUp reports whether root is
// still unreaped; while it is, root's PID cannot belong to another process.
func NewTracker(root int, rootUp func() bool) *Tracker {
	return &Tracker{root: root, rootUp: rootUp, ownPGID: syscall.Getpgrp(), seen: map[int]Proc{}}
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
	alive := func(pid int) bool { // a tracked process still running as recorded
		p, ok := t.seen[pid]
		return ok && p.Same(all[pid])
	}
	for changed := true; changed; {
		changed = false
		groups := map[int]bool{}
		for pid, p := range t.seen {
			if alive(pid) && p.PGID != t.ownPGID {
				groups[p.PGID] = true
			}
		}
		if q, ok := all[t.root]; ok && rootUp && q.PGID != t.ownPGID {
			groups[q.PGID] = true
		}
		for pid, p := range all {
			if _, ok := t.seen[pid]; ok || pid == t.root {
				continue
			}
			if (p.PPID == t.root && rootUp) || alive(p.PPID) || groups[p.PGID] {
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
	Alive   []Proc `json:"alive"`   // sent SIGKILL but still running a second later
}

// Kill sends SIGKILL to each process that is still running as recorded: same
// PID, start time and command line. A process whose PID has been reused is not
// touched. Kill never targets this process or PID 1.
func Kill(procs []Proc) (KillReport, error) {
	r := KillReport{Killed: []Proc{}, Gone: []Proc{}, Changed: []Proc{}, Alive: []Proc{}}
	all, err := List()
	if err != nil {
		return r, err
	}
	self := syscall.Getpid()
	for _, p := range procs {
		now, ok := all[p.PID]
		switch {
		case p.PID <= 1 || p.PID == self:
			r.Changed = append(r.Changed, p)
		case !ok:
			r.Gone = append(r.Gone, p)
		case !now.Same(p):
			r.Changed = append(r.Changed, p)
		default:
			syscall.Kill(p.PID, syscall.SIGKILL)
			r.Killed = append(r.Killed, p)
		}
	}
	if len(r.Killed) == 0 {
		return r, nil
	}
	for deadline := time.Now().Add(time.Second); ; time.Sleep(50 * time.Millisecond) {
		after, err := List()
		if err != nil {
			return r, err
		}
		r.Alive = r.Alive[:0]
		for _, p := range r.Killed {
			if now, ok := after[p.PID]; ok && now.Same(p) {
				r.Alive = append(r.Alive, p)
			}
		}
		if len(r.Alive) == 0 || time.Now().After(deadline) {
			return r, nil
		}
	}
}
