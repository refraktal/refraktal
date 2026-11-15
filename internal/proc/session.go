package proc

import (
	"errors"
	"os"
	"time"
)

// Each harness starts as the leader of a new session, and a session ID is
// inherited by every descendant: through reparenting, through process
// groups of their own, after the leader exits. Only setsid() leaves a session
// (pseudo-terminal launchers do that). So at shutdown, the processes of this
// user still in the harness's session are the harness's leftovers, including
// any that detached and escaped the parent-PID tracking.

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
		if s, err := getsid(pid); err == nil && s == sid {
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

// SweepSession kills, with the identity check, every process of this user
// still in the session that leader started. leader is the session leader as
// recorded; it is matched on PID and start time only, since its command line
// may change when it execs. If its PID now belongs to a process with another
// start time, the ID may name a new session and nothing is killed. A member
// that started before the leader cannot be in the leader's session and is
// skipped.
//
// SweepSession cannot tell a session that emptied, and whose ID was then
// reused, from the original: callers sweep only sessions they have never seen
// empty (Child polls them while it runs).
func SweepSession(leader Proc) (KillReport, error) {
	r := KillReport{Killed: []Proc{}, Gone: []Proc{}, Changed: []Proc{}, Alive: []Proc{}}
	if leader.PID <= 1 {
		return r, nil
	}
	for pass := 0; pass < 3; pass++ { // a member may fork while the sweep runs
		all, err := List()
		if err != nil {
			return r, err
		}
		if now, ok := all[leader.PID]; ok && now.Start != leader.Start {
			return r, ErrSessionReused
		}
		members, _, err := sessionOf(leader)
		if err != nil {
			return r, err
		}
		if len(members) == 0 {
			break
		}
		kr, err := Kill(members)
		r.Killed = append(r.Killed, kr.Killed...)
		r.Gone = append(r.Gone, kr.Gone...)
		r.Changed = append(r.Changed, kr.Changed...)
		r.Alive = kr.Alive
		if err != nil {
			return r, err
		}
		time.Sleep(50 * time.Millisecond)
	}
	return r, nil
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
