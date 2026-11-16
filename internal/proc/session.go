package proc

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// Each harness starts as the leader of a new session, and a session ID is
// inherited by every descendant: through reparenting, through process
// groups of their own, after the leader exits. Only setsid() leaves a session
// (pseudo-terminal launchers do that). So at shutdown, the processes of this
// user still in the harness's session can include leftovers. A recorded
// session may empty and be reused between polls, so membership needs proof.

// membersOf lists a session's members; a variable so tests can simulate a
// session whose ID is reused.
var membersOf = sessionMembers

// ErrSessionReused means the session leader's PID now belongs to another
// process, so the session ID may name a different session: nothing is swept.
var ErrSessionReused = errors.New("proc: the session leader's PID is now another process; session not swept")

// sessionMembers returns this user's processes in session sid, except this
// process, each listed the same before and after the session lookup (so a PID
// reused during the lookup is left out). occupied reports whether any process
// held the session ID at all: a process that execs between the two listings
// is left out of members but still counts, so a live session never looks
// empty.
func sessionMembers(sid int) (members []Proc, occupied bool, err error) {
	before, err := List()
	if err != nil {
		return nil, false, err
	}
	uid, self := os.Getuid(), os.Getpid()
	var cand []int
	for pid, p := range before {
		if p.UID != uid || pid == self || pid <= 1 {
			continue
		}
		s, err := getsid(pid)
		if err != nil && !errors.Is(err, syscall.ESRCH) {
			return nil, true, fmt.Errorf("session lookup for process %d: %w", pid, err)
		}
		if err == nil && s == sid {
			cand = append(cand, pid)
		}
	}
	if len(cand) == 0 {
		return nil, false, nil
	}
	after, err := List()
	if err != nil {
		return nil, true, err
	}
	var out []Proc
	for _, pid := range cand {
		if a, ok := after[pid]; ok && a.Same(before[pid]) {
			out = append(out, a)
		}
	}
	return out, true, nil
}

// SweepSession kills only provable members of the recorded session. A matching
// live leader proves that current membership belongs to its session. After the
// leader exits, membership alone is not proof: the ID may have been reused.
// Child supplies identities recorded while the session was known to be ours.
func SweepSession(leader Proc) (KillReport, error) {
	return sweepSession(leader, []Proc{leader}, nil, nil)
}

func sweepSession(leader Proc, known []Proc, pinned func() bool, remember func([]Proc)) (KillReport, error) {
	r := KillReport{Killed: []Proc{}, Gone: []Proc{}, Changed: []Proc{}, Alive: []Proc{}, Unknown: []Proc{}}
	if leader.PID <= 1 {
		return r, nil
	}
	deadline := time.Now().Add(sweepFor)
	for pass := 0; ; pass++ {
		members, _, all, err := sessionSnapshot(leader)
		if err != nil {
			r.Unknown = append(r.Unknown, leader)
			return r, err
		}
		if len(members) == 0 {
			r.Alive = r.Alive[:0]
			break
		}
		owned, unknown := provenMembers(leader, members, all, known, pinned != nil && pinned())
		if remember != nil {
			remember(owned)
		}
		known = provenDescendants(all, append(known, owned...))
		if remember != nil {
			remember(known)
		}
		r.Unknown = append(r.Unknown, unknown...)
		if pass > 0 && time.Now().After(deadline) {
			r.Alive = append(r.Alive[:0], owned...)
			break
		}
		kr, err := Kill(owned)
		r.Killed = append(r.Killed, kr.Killed...)
		r.Gone = append(r.Gone, kr.Gone...)
		r.Changed = append(r.Changed, kr.Changed...)
		r.Alive = kr.Alive
		r.Unknown = append(r.Unknown, kr.Unknown...)
		if err != nil {
			return r, err
		}
		if len(unknown) != 0 {
			return r, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return r, nil
}

// sessionSnapshot checks the leader after membership was sampled. A matching
// leader then existed throughout the sample; a different birth retires the ID.
func sessionSnapshot(leader Proc) ([]Proc, bool, map[int]Proc, error) {
	members, occupied, err := sessionOf(leader)
	if err != nil {
		return nil, occupied, nil, err
	}
	all, err := List()
	if err != nil {
		return nil, occupied, nil, err
	}
	if now, ok := all[leader.PID]; ok && !now.Same(leader) {
		return nil, occupied, all, ErrSessionReused
	}
	return members, occupied, all, nil
}

// provenMembers never treats continuity between polls as proof of ownership.
// Birth identities survive leader exit; new identities need a verified parent
// chain in this snapshot, or a leader that still reserves the session ID.
func provenMembers(leader Proc, members []Proc, all map[int]Proc, known []Proc, pinned bool) (owned, unknown []Proc) {
	if now, ok := all[leader.PID]; pinned || (ok && now.Same(leader)) {
		return members, nil
	}
	trusted := map[int]Proc{}
	for _, p := range provenDescendants(all, known) {
		if now, ok := all[p.PID]; ok && now.Same(p) {
			trusted[p.PID] = p
		}
	}
	for _, m := range members {
		proved := false
		for _, k := range known {
			if k.Same(m) {
				proved = true
				break
			}
		}
		if p, ok := trusted[m.PID]; ok && p.Same(m) {
			proved = true
		}
		if proved {
			owned = append(owned, m)
		} else {
			unknown = append(unknown, m)
		}
	}
	return owned, unknown
}

// provenDescendants follows PPID links only from identities that still match
// in one snapshot. A stale parent PID alone never establishes ownership.
func provenDescendants(all map[int]Proc, known []Proc) []Proc {
	trusted := map[int]Proc{}
	for _, p := range known {
		if now, ok := all[p.PID]; ok && now.Same(p) {
			trusted[p.PID] = now
		}
	}
	for changed := true; changed; {
		changed = false
		for pid, p := range all {
			if _, ok := trusted[pid]; ok {
				continue
			}
			if _, ok := trusted[p.PPID]; ok {
				trusted[pid] = p
				changed = true
			}
		}
	}
	// Preserve old birth identities too, so a later PPID reparenting cannot
	// erase an identity that was proved while its parent was still visible.
	identities := map[string]Proc{}
	for _, p := range known {
		identities[sessionKey(p)] = p
	}
	for _, p := range trusted {
		identities[sessionKey(p)] = p
	}
	out := make([]Proc, 0, len(identities))
	for _, p := range identities {
		out = append(out, p)
	}
	return out
}

// sessionOf lists the members of leader's session that started no earlier
// than the leader, and whether the session ID is held by any process.
func sessionOf(leader Proc) ([]Proc, bool, error) {
	members, occupied, err := membersOf(leader.PID)
	if err != nil {
		return nil, occupied, err
	}
	since, ok := startTime(leader.Start)
	if !ok {
		return members, occupied, nil
	}
	out := members[:0]
	for _, m := range members {
		if t, ok := startTime(m.Start); ok && t.Before(since) {
			continue
		}
		out = append(out, m)
	}
	return out, occupied, nil
}

// startTime parses a start time as ps prints it (lstart).
func startTime(s string) (time.Time, bool) {
	t, err := time.ParseInLocation("Mon Jan 2 15:04:05 2006", s, time.Local)
	return t, err == nil
}

// sweepFor bounds how long one session sweep keeps going while members fork
// (a variable for tests).
var sweepFor = 5 * time.Second
