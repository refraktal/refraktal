package verify

import (
	"fmt"
	"testing"
)

// Kill safety is a bounded check over three PID slots: the pinned harness,
// a recorded tool-session leader, and one member. Tool/member PIDs have two
// distinct birth times; group/session IDs can be reused only after their last
// holder exits. Three slots suffice to exhibit reuse with a replacement
// leader that forks and exits, leaving a stranger in the reused session.
// This is not a proof for arbitrary trees, ps start-time collisions, lookup
// errors, arbitrary setsid chains, or OS scheduling. Unlike an atomic lookup
// abstraction, the faithful model permits exit/reuse between recheck and
// signal. Both that race and a recorded session reused entirely between polls
// violate the requested absolute invariant in the current implementation.

type killProc struct {
	Birth, Parent, Group, Session int
	Live, Owned                   bool
}
type killRecord struct{ Birth, Group int }
type killState struct {
	P                [3]killProc
	Seen             [3]killRecord
	Groups           [3]int
	Retired, Dropped [3]bool
	Phase, At        int // 0 tracking; 1 PID check; 2 PID signal; 3 session snapshot; 4 done; 5 group check; 6 group signal
	Targets          [3]int
	Checked          bool
	Bad              bool
	Signals          int
	GroupSignals     int
}
type killOptions struct {
	AtomicSignal    bool // explicitly stronger abstraction for guard mutation tests
	NoIdentityGuard bool
	NoEmptyGuard    bool
	NoSessionReuse  bool // exclude only the separately reported session finding
}

func killInitial() killState {
	return killState{P: [3]killProc{
		{Birth: 1, Parent: -1, Group: 0, Session: 0, Live: true, Owned: true},
		{Birth: 1, Parent: 0, Group: 1, Session: 1, Live: true, Owned: true},
		{Birth: 1, Parent: 1, Group: 1, Session: 1, Live: true, Owned: true},
	}}
}

// killPoll mirrors Tracker.add in internal/proc/tracker.go, including stored
// group membership, a verified survivor, permanent retirement and root pinning.
func killPoll(s killState) killState {
	for g, birth := range s.Groups {
		if birth == 0 {
			continue
		}
		p := s.P[g]
		continuous := p.Live && p.Birth == birth && p.Group == g
		if !p.Live {
			continuous = g == 0 // Child.Start/shutdown pin the unreaped harness PID.
			for i, q := range s.P {
				old := s.Seen[i]
				if q.Live && old.Birth == q.Birth && old.Group == g && q.Group == g {
					continuous = true
				}
			}
		}
		if !continuous {
			s.Groups[g] = 0
			s.Retired[g] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for i, p := range s.P {
			tracked := i == 0 || s.Seen[i].Birth == p.Birth
			if p.Live && tracked && p.Group == i && !s.Retired[i] {
				s.Groups[i] = p.Birth
			}
		}
		for i, p := range s.P {
			if i == 0 || !p.Live {
				continue
			}
			knownParent := p.Parent == 0 || p.Parent > 0 && s.P[p.Parent].Live && s.Seen[p.Parent].Birth == s.P[p.Parent].Birth
			if s.Seen[i].Birth == p.Birth || knownParent || s.Groups[p.Group] != 0 {
				next := killRecord{p.Birth, p.Group}
				if s.Seen[i] != next {
					s.Seen[i] = next
					changed = true
				}
			}
		}
	}
	return s
}

func killSend(s killState) killState {
	if s.Checked && s.P[s.At].Live {
		s.Signals++
		if !s.P[s.At].Owned {
			s.Bad = true
		}
		// Kill waits for confirmed exit; here scheduling the death is immediate.
		s.P[s.At].Live = false
		for i, p := range s.P {
			if p.Parent == s.At {
				s.P[i].Parent = -1
			}
		}
	}
	s.At++
	s.Phase = 1
	return s
}

func killModel(o killOptions) model[killState] {
	return model[killState]{Name: "kill safety", Initial: killInitial(), Key: func(s killState) string { return fmt.Sprintf("%v", s) },
		Invariant: func(s killState) error {
			if s.Bad {
				return fmt.Errorf("signalled an unrelated process")
			}
			return nil
		}, Next: func(s killState) []transition[killState] {
			var out []transition[killState]
			add := func(action string, n killState) {
				if n != s {
					out = append(out, transition[killState]{Action: action, State: n})
				}
			}
			if s.Phase == 4 || s.Bad {
				return out
			}
			// Environment exits/reparenting consumed by Tracker.Poll (tracker.go),
			// Kill (tracker.go), and sessionOf (session.go), all in internal/proc/.
			for i, p := range s.P {
				if p.Live {
					n := s
					n.P[i].Live = false
					for j, q := range n.P {
						if q.Parent == i {
							n.P[j].Parent = -1
						}
					}
					add(fmt.Sprintf("exit PID %d; reparent children", i+2), n)
				}
			}
			// Environment PID reuse consumed by Kill in internal/proc/tracker.go.
			// Reuse has a distinct birth, so the finding does not use ps's accepted
			// one-second identity alias. A stranger's own session uses its own PID.
			if !s.P[2].Live && s.P[2].Birth == 1 {
				n := s
				n.P[2] = killProc{Birth: 2, Parent: -1, Group: 2, Session: 2, Live: true}
				add("reuse member PID 4 for unrelated process", n)
				if s.P[0].Live {
					n.P[2] = killProc{Birth: 2, Parent: 0, Group: 0, Session: 0, Live: true, Owned: true}
					add("reuse member PID 4 for a fresh descendant", n)
				}
			}
			// Environment group/session reuse consumed by Tracker.add (tracker.go)
			// and SweepSession (session.go): setsid is legal only after ID 3 is free.
			empty := true
			for _, p := range s.P {
				if p.Live && (p.Group == 1 || p.Session == 1) {
					empty = false
				}
			}
			if !o.NoSessionReuse && !s.P[1].Live && s.P[1].Birth == 1 && empty {
				n := s
				n.P[1] = killProc{Birth: 2, Parent: -1, Group: 1, Session: 1, Live: true}
				add("reuse PID/group/session 3 for stranger leader", n)
			}
			// Environment fork consumed by sessionOf in internal/proc/session.go;
			// a replacement session's child is unrelated to the original harness.
			if s.P[1].Live && s.P[1].Birth == 2 && !s.P[2].Live && s.P[2].Birth == 1 {
				n := s
				n.P[2] = killProc{Birth: 2, Parent: 1, Group: 1, Session: 1, Live: true}
				add("replacement session leader forks stranger member PID 4", n)
			}
			switch s.Phase {
			case 0:
				// Tracker.Poll/add, internal/proc/tracker.go.
				add("poll descendant identities and group continuity", killPoll(s))
				// Child.checkSessions, internal/proc/child.go; confirmed empty observations
				// retire a session permanently. Errors are excluded from these bounds.
				n := s
				for _, sid := range []int{0, 1} {
					occupied := false
					for _, p := range s.P {
						if p.Live && p.Session == sid {
							occupied = true
						}
					}
					if !occupied {
						n.Dropped[sid] = true
					}
				}
				add("poll sessions; permanently drop confirmed empty IDs", n)
				// Child.shutdown, internal/proc/child.go: final tracker poll then capture
				// targets while the harness PID is still pinned (not yet reaped).
				n = killPoll(s)
				n.Phase = 5
				n.At = 0
				n.Targets[0] = 1
				for i := 1; i < 3; i++ {
					n.Targets[i] = n.Seen[i].Birth
				}
				add("shutdown: final poll and capture tracked targets", n)
			case 5:
				// Child.killGroup, internal/proc/child.go: require one target's
				// fresh identity and current PGID before signalling the group.
				n := s
				n.Checked = false
				n.Phase = 6
				for i, p := range s.P {
					if p.Live && p.Group == 0 && s.Targets[i] != 0 && p.Birth == s.Targets[i] {
						n.Checked = true
					}
				}
				add("recheck a tracked member of the pinned harness group", n)
			case 6:
				// Child.killGroup signal(-pgid, SIGKILL), internal/proc/child.go.
				// The unreaped harness pins PGID 2; no stranger can acquire it.
				n := s
				n.Phase = 1
				n.At = 0
				if s.Checked {
					n.GroupSignals++
					for i, p := range n.P {
						if p.Live && p.Group == 0 {
							if !p.Owned {
								n.Bad = true
							}
							n.P[i].Live = false
							for j, q := range n.P {
								if q.Parent == i {
									n.P[j].Parent = -1
								}
							}
						}
					}
				}
				n.Checked = false
				add("signal the pinned harness process group", n)
			case 1:
				if s.At == 3 {
					n := s
					n.Phase = 3
					add("finish tree kill; begin session sweep", n)
					break
				}
				// Kill, internal/proc/tracker.go: EACH target receives a fresh identity
				// recheck, rather than reusing the pre-shutdown or previous-target row.
				n := s
				n.Checked = n.Targets[n.At] != 0 && n.P[n.At].Live && (o.NoIdentityGuard || n.P[n.At].Birth == n.Targets[n.At])
				if o.AtomicSignal {
					n = killSend(n)
					add(fmt.Sprintf("identity recheck and atomic signal PID %d (explicit abstraction)", s.At+2), n)
				} else {
					n.Phase = 2
					add(fmt.Sprintf("identity recheck PID %d", s.At+2), n)
				}
			case 2:
				// Kill's signal(p.PID, SIGKILL), internal/proc/tracker.go. This is a
				// separate syscall from recheckPID, so environment events can intervene.
				add(fmt.Sprintf("signal PID %d using successful recheck", s.At+2), killSend(s))
			case 3:
				// Child.sweepSessions, internal/proc/child.go, skips every dropped ID;
				// SweepSession/sessionOf, internal/proc/session.go, reject a changed
				// live leader. A replacement leader already gone cannot be checked.
				n := s
				n.Targets = [3]int{}
				n.At = 0
				n.Phase = 1
				for _, sid := range []int{0, 1} {
					if s.Dropped[sid] && !o.NoEmptyGuard {
						continue
					}
					if s.P[sid].Live && s.P[sid].Birth != 1 {
						continue
					}
					for i, p := range s.P {
						if p.Live && p.Session == sid {
							n.Targets[i] = p.Birth
						}
					}
				}
				// The sweep reuses Kill; a second completed kill must terminate rather
				// than re-snapshot forever. Phase 3 with no targets is the fixed point.
				any := false
				for _, b := range n.Targets {
					any = any || b != 0
				}
				if !any {
					n.Phase = 4
				}
				add("sweep never-seen-empty sessions; capture current member identities", n)
			}
			return out
		}}
}

func TestKillSafety(t *testing.T) {
	// These are faithful findings, not a passing safety proof. Their failing
	// production-code reproductions live in internal/proc/verify_findings_test.go.
	for _, tc := range []struct {
		name    string
		options killOptions
	}{
		{"unrestricted_faithful_model", killOptions{}},
		{"PID_reused_between_recheck_and_signal", killOptions{NoSessionReuse: true}},
		{"recorded_session_reused_between_polls", killOptions{AtomicSignal: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := check(killModel(tc.options))
			t.Log(r.String())
			if r.Violation == nil {
				t.Fatal("faithful finding disappeared; review production reproducer and model")
			}
		})
	}
}

func TestKillSafetyIdentityMutation(t *testing.T) {
	// The strong abstraction excludes both separately reproduced races. Within
	// that stated scope, removing Kill's identity guard must introduce a bug.
	good := killOptions{AtomicSignal: true, NoSessionReuse: true}
	m := killModel(good)
	var signalled, groupSignalled, retired, refreshed, finished bool
	invariant := m.Invariant
	m.Invariant = func(s killState) error {
		signalled = signalled || s.Signals > 0
		groupSignalled = groupSignalled || s.GroupSignals > 0
		retired = retired || s.Retired[1]
		refreshed = refreshed || s.P[2].Owned && s.Seen[2].Birth == 2
		finished = finished || s.Phase == 4
		return invariant(s)
	}
	r := check(m)
	t.Log(r.String())
	if !signalled || !groupSignalled || !retired || !refreshed || !finished {
		t.Fatalf("missing reachable witness: signal=%v group=%v retire=%v refresh=%v finish=%v", signalled, groupSignalled, retired, refreshed, finished)
	}
	if r.Violation != nil {
		t.Fatal(r.String())
	}
	good.NoIdentityGuard = true
	r = check(killModel(good))
	t.Log(r.String())
	if r.Violation == nil {
		t.Fatal("identity guard mutation was not detected")
	}
}

// A state reachable after an empty observation and complete session-ID reuse
// isolates the empty-session guard from the separate unobserved-reuse finding.
func TestKillSafetyEmptySessionMutation(t *testing.T) {
	m := killModel(killOptions{AtomicSignal: true})
	seed := killInitial()
	seed.Phase = 3
	seed.Dropped[1] = true
	seed.P[1] = killProc{Birth: 2, Parent: -1, Group: 1, Session: 1}
	seed.P[2] = killProc{Birth: 2, Parent: -1, Group: 1, Session: 1, Live: true}
	m.Initial = seed
	r := check(m)
	t.Log(r.String())
	if r.Violation != nil {
		t.Fatal(r.String())
	}
	m = killModel(killOptions{AtomicSignal: true, NoEmptyGuard: true})
	m.Initial = seed
	r = check(m)
	t.Log(r.String())
	if r.Violation == nil {
		t.Fatal("removing permanently dropped-session guard did not find a counterexample")
	}
}
