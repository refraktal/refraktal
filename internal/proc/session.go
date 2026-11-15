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

// ErrSessionReused means the session leader's PID now belongs to another
// process, so the session ID may name a different session: nothing is swept.
var ErrSessionReused = errors.New("proc: the session leader's PID is now another process; session not swept")

// sessionMembers returns this user's processes in session sid, except this
// process, each listed the same before and after the session lookup (so a PID
// reused during the lookup is left out).
func sessionMembers(sid int) ([]Proc, error) {
	before, err := List()
	if err != nil {
		return nil, err
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
		return nil, nil
	}
	after, err := List()
	if err != nil {
		return nil, err
	}
	var out []Proc
	for _, pid := range cand {
		if a, ok := after[pid]; ok && a.Same(before[pid]) {
			out = append(out, a)
		}
	}
	return out, nil
}

// SweepSession kills, with the identity check, every process of this user
// still in the session that leader started. leader is the session leader as
// recorded when it started. If its PID now belongs to another process, the
// ID may name a new session and nothing is killed. (While any member of the
// original session is alive, the ID cannot be handed out again.)
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
		if now, ok := all[leader.PID]; ok && !now.Same(leader) {
			return r, ErrSessionReused
		}
		members, err := sessionMembers(leader.PID)
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
