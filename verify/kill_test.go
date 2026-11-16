package verify

import (
	"fmt"
	"testing"
)

// Three PID slots represent an unreaped harness, a recorded tool-session
// leader and a member. Each reusable PID has two distinct births. Group and
// session IDs can be reused after their final holder exits. These bounds expose
// both reuse windows without claiming a cutoff theorem for arbitrary trees.
// Births are valid, distinct timestamps; the one-second ps alias is excluded.
// Lookup errors, signal failures, root-reaping fallback and arbitrary setsid
// chains are abstracted. Group signals remain PID-based, with the root pinned.
// Known retains historical births like sweepSession's local known slice; across
// polls it overapproximates Child.members (which retains the latest birth per
// PID). With monotonic, nonreturning births this cannot trust a new stranger.
type killProc struct {
	Birth, Parent, Group, Session int
	Live, Owned                   bool
}
type killRecord struct{ Birth, Group int }
type killState struct {
	P                                                                   [3]killProc
	Seen                                                                [3]killRecord
	Known                                                               [3]uint8 // birth identities proved while ours, including historical births
	Groups                                                              [3]int
	Retired, Dropped                                                    [3]bool
	Empty                                                               [3]uint8 // consecutive unoccupied snapshots, saturated at two
	Phase, At                                                           int      // 0 track, 1 recheck, 2 signal, 3 sweep, 4 done, 5 group check, 6 group signal, 7 handle
	Targets                                                             [3]int
	Handle                                                              int // birth pinned by FindProcess before recheck; zero means no process
	Checked, Bad, PrematureEmpty, RetiredInspected, Unknown, LiveRecord bool
	Signals, GroupSignals                                               bool
}
type killOptions struct {
	NoPIDFD                   bool // documented macOS/old-kernel limit, also the old PID-signal mutation
	NoIdentityGuard           bool
	NoEmptyGuard              bool
	UnsafeSessionMembers      bool
	SingleEmpty               bool
	LegacyRetirementIsolation bool // only the explicitly pre-fix retirement guard fixture
}

func killInitial() killState {
	return killState{LiveRecord: true, P: [3]killProc{
		{Birth: 1, Parent: -1, Group: 0, Session: 0, Live: true, Owned: true},
		{Birth: 1, Parent: 0, Group: 1, Session: 1, Live: true, Owned: true},
		{Birth: 1, Parent: 1, Group: 1, Session: 1, Live: true, Owned: true},
	}, Known: [3]uint8{1, 0, 0}}
}

// killPoll mirrors Tracker.add in internal/proc/tracker.go, including verified
// group continuity, permanent retirement and the unreaped root's reservation.
func killPoll(s killState) killState {
	for g, birth := range s.Groups {
		if birth == 0 {
			continue
		}
		p := s.P[g]
		continuous := p.Live && p.Birth == birth && p.Group == g
		if !p.Live {
			continuous = g == 0
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
			parent := p.Parent == 0 || p.Parent > 0 && s.P[p.Parent].Live && s.Seen[p.Parent].Birth == s.P[p.Parent].Birth
			if s.Seen[i].Birth == p.Birth || parent || s.Groups[p.Group] != 0 {
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

// killProven mirrors provenMembers/provenDescendants in internal/proc/session.go:
// seed only matching identities in this snapshot, then follow age-checked PPIDs.
// A live matching leader or the pinned root also reserves session membership.
func killProven(s killState, sid int, pinned bool) [3]bool {
	var trusted, proved [3]bool
	for i, p := range s.P {
		known := s.Known[i]&(1<<uint(p.Birth-1)) != 0 || s.Seen[i].Birth == p.Birth
		trusted[i] = p.Live && (known || i == 0)
	}
	for changed := true; changed; {
		changed = false
		for i, p := range s.P {
			if p.Live && !trusted[i] && p.Parent >= 0 && trusted[p.Parent] && p.Birth >= s.P[p.Parent].Birth {
				trusted[i] = true
				changed = true
			}
		}
	}
	reserved := pinned || s.P[sid].Live && s.P[sid].Birth == 1
	for i, p := range s.P {
		proved[i] = p.Live && p.Session == sid && (reserved || trusted[i])
	}
	return proved
}

// killExit is the environment exit/reparenting observed by Tracker.Poll
// (internal/proc/tracker.go) and sessionSnapshot (internal/proc/session.go).
func killExit(s killState, i int) killState {
	s.P[i].Live = false
	for j, p := range s.P {
		if p.Parent == i {
			s.P[j].Parent = -1
		}
	}
	return s
}

// killSend mirrors Kill's Process.Signal and Release in internal/proc/tracker.go.
// With pidfd, an exited handle cannot signal the replacement occupying its PID.
func killSend(s killState, o killOptions) killState {
	if s.Checked && s.P[s.At].Live && (o.NoPIDFD || s.P[s.At].Birth == s.Handle) {
		s.Signals = true
		if !s.P[s.At].Owned {
			s.Bad = true
		}
		s = killExit(s, s.At)
	}
	s.At++
	s.Phase = 7
	s.Checked = false
	s.Handle = 0
	return s
}

// killObserveEmpty mirrors Child.checkSessions (internal/proc/child.go) and
// sweepSession (internal/proc/session.go): occupied resets the empty sequence;
// only two consecutive unoccupied observations retire/finish a session.
func killObserveEmpty(s killState, sid int, occupied bool, o killOptions) killState {
	if occupied {
		s.Empty[sid] = 0
		return s
	}
	if s.Empty[sid] < 2 {
		s.Empty[sid]++
	}
	if s.Empty[sid] == 2 || o.SingleEmpty {
		s.Dropped[sid] = true
		if s.Empty[sid] < 2 {
			s.PrematureEmpty = true
		}
	}
	return s
}

func killModel(o killOptions) model[killState] {
	return model[killState]{Name: "kill safety", Initial: killInitial(), Key: func(s killState) string { return fmt.Sprintf("%v", s) }, Invariant: func(s killState) error {
		if s.Bad {
			return fmt.Errorf("signalled an unrelated process")
		}
		if s.PrematureEmpty {
			return fmt.Errorf("declared empty without two consecutive unoccupied snapshots")
		}
		if s.RetiredInspected {
			return fmt.Errorf("inspected a permanently retired session")
		}
		if s.Unknown && !s.LiveRecord {
			return fmt.Errorf("discarded an unresolved live record")
		}
		return nil
	}, Next: func(s killState) []transition[killState] {
		var out []transition[killState]
		add := func(action string, n killState) {
			if n != s {
				out = append(out, transition[killState]{Action: action, State: n})
			}
		}
		if s.Phase == 4 || s.Bad || s.PrematureEmpty {
			return out
		}
		// Environment exits/reparenting observed by Tracker.Poll (tracker.go), Kill
		// (tracker.go), and sessionSnapshot (session.go), all in internal/proc/.
		for i, p := range s.P {
			if p.Live {
				add(fmt.Sprintf("exit PID %d; reparent children", i+2), killExit(s, i))
			}
		}
		// Environment PID reuse observed by Kill in internal/proc/tracker.go. New
		// births distinguish reuse from the accepted one-second start-time alias.
		if !s.P[2].Live && s.P[2].Birth == 1 {
			n := s
			n.P[2] = killProc{Birth: 2, Parent: -1, Group: 2, Session: 2, Live: true}
			add("reuse member PID 4 for stranger", n)
			if s.P[0].Live {
				n.P[2] = killProc{Birth: 2, Parent: 0, Group: 0, Session: 0, Live: true, Owned: true}
				add("reuse member PID 4 for fresh descendant", n)
			}
		}
		// Environment session/group reuse observed by sessionSnapshot in
		// internal/proc/session.go; an ID cannot be reused while any holder lives.
		empty := true
		for _, p := range s.P {
			if p.Live && (p.Group == 1 || p.Session == 1) {
				empty = false
			}
		}
		if !s.P[1].Live && s.P[1].Birth == 1 && empty {
			n := s
			n.P[1] = killProc{Birth: 2, Parent: -1, Group: 1, Session: 1, Live: true}
			add("reuse tool leader PID/group/session 3", n)
		}
		// Environment fork observed by sessionSnapshot in internal/proc/session.go.
		if s.P[1].Live && s.P[1].Birth == 2 && !s.P[2].Live && s.P[2].Birth == 1 {
			n := s
			n.P[2] = killProc{Birth: 2, Parent: 1, Group: 1, Session: 1, Live: true}
			add("stranger session leader forks member PID 4", n)
		}
		switch s.Phase {
		case 0:
			// Tracker.Poll/add, internal/proc/tracker.go.
			add("poll descendants and group continuity", killPoll(s))
			// Child.checkSessions and rememberMembers, internal/proc/child.go;
			// provenMembers/provenDescendants, internal/proc/session.go.
			for sid := 0; sid < 2; sid++ {
				if s.Dropped[sid] {
					continue
				}
				n := s
				occupied := false
				for _, p := range s.P {
					occupied = occupied || p.Live && p.Session == sid
				}
				n = killObserveEmpty(n, sid, occupied, o)
				if !(s.P[sid].Live && s.P[sid].Birth != 1) {
					proved := killProven(s, sid, sid == 0)
					for i, p := range s.P {
						if proved[i] {
							n.Known[i] |= 1 << uint(p.Birth-1)
						}
					}
				}
				add(fmt.Sprintf("poll session %d; retain only proven identities", sid+2), n)
			}
			// Child.shutdown final tracker poll, internal/proc/child.go.
			n := killPoll(s)
			n.Phase = 5
			n.At = 0
			n.Targets[0] = 1
			for i := 1; i < 3; i++ {
				n.Targets[i] = n.Seen[i].Birth
			}
			add("shutdown: final tracker poll and targets", n)
		case 5:
			// Child.killGroup recheckPID, internal/proc/child.go.
			n := s
			n.Checked = false
			n.Phase = 6
			for i, p := range s.P {
				if p.Live && p.Group == 0 && s.Targets[i] != 0 && p.Birth == s.Targets[i] {
					n.Checked = true
				}
			}
			add("recheck a tracked member of pinned harness group", n)
		case 6:
			// Child.killGroup signal(-pgid, SIGKILL), internal/proc/child.go. The
			// signal remains PID-based; the unreaped root reserves its ID in bounds.
			n := s
			n.Phase = 7
			n.At = 0
			if s.Checked {
				n.GroupSignals = true
				for i, p := range s.P {
					if p.Live && p.Group == 0 {
						if !p.Owned {
							n.Bad = true
						}
						n = killExit(n, i)
					}
				}
			}
			n.Checked = false
			add("signal pinned harness process group by PGID", n)
		case 7:
			if s.At == 3 {
				// Child.shutdown after Kill, internal/proc/child.go.
				n := s
				n.Phase = 3
				add("tree targets complete; sweep sessions", n)
				break
			}
			// Kill iterates only recorded targets, internal/proc/tracker.go.
			if s.Targets[s.At] == 0 {
				n := s
				n.At++
				add("skip absent target", n)
				break
			}
			// Kill findProcessForKill/os.FindProcess, internal/proc/tracker.go.
			n := s
			n.Handle = 0
			if s.P[s.At].Live {
				n.Handle = s.P[s.At].Birth
			}
			n.Phase = 1
			add(fmt.Sprintf("open process handle for PID %d before recheck", s.At+2), n)
		case 1:
			// Kill recheckPID and Proc.Same, internal/proc/tracker.go.
			n := s
			n.Checked = s.Targets[s.At] != 0 && s.P[s.At].Live && (o.NoIdentityGuard || s.P[s.At].Birth == s.Targets[s.At])
			n.Phase = 2
			add(fmt.Sprintf("recheck target birth at PID %d", s.At+2), n)
		case 2:
			// Kill signalProcessForKill/process.Release, internal/proc/tracker.go.
			add(fmt.Sprintf("signal and release process handle for PID %d", s.At+2), killSend(s, o))
		case 3:
			// Child.sweepSessions and Child.shutdown, internal/proc/child.go;
			// sweepSession/sessionSnapshot/provenMembers, internal/proc/session.go.
			// Each session snapshot is a separate transition, so environment actions
			// can occur between the two empty observations.
			sid := s.At - 3
			if sid < 0 || sid > 1 {
				sid = 0
			}
			if sid == 1 && (s.Dropped[1] && !o.NoEmptyGuard) {
				n := s
				n.Phase = 4
				n.LiveRecord = s.Unknown
				add("finish cleanup; keep unknown live record", n)
				break
			}
			n := s
			if s.Dropped[sid] && o.NoEmptyGuard && !o.LegacyRetirementIsolation {
				n.RetiredInspected = true
				add("inspect permanently retired session", n)
				break
			}
			occupied := false
			for _, p := range s.P {
				occupied = occupied || p.Live && p.Session == sid
			}
			if s.P[sid].Live && s.P[sid].Birth != 1 {
				n.Unknown = true
				n.Phase = 4
				n.LiveRecord = true
				add("changed session leader: unknown, keep record", n)
				break
			}
			n = killObserveEmpty(n, sid, occupied, o)
			if n.Dropped[sid] && !o.NoEmptyGuard {
				if sid == 0 {
					n.At = 4
				} else {
					n.Phase = 4
					n.LiveRecord = n.Unknown
				}
				add(fmt.Sprintf("confirmed empty session %d; retire permanently", sid+2), n)
				break
			}
			proved := killProven(s, sid, sid == 0)
			n.Targets = [3]int{}
			any := false
			unknown := false
			for i, p := range s.P {
				if p.Live && p.Session == sid {
					if proved[i] || o.UnsafeSessionMembers {
						n.Targets[i] = p.Birth
						n.Known[i] |= 1 << uint(p.Birth-1)
						any = true
					} else {
						unknown = true
					}
				}
			}
			n.Unknown = n.Unknown || unknown
			if any {
				n.Phase = 7
				n.At = 0
			} else if unknown {
				n.Phase = 4
				n.LiveRecord = true
			}
			add(fmt.Sprintf("snapshot session %d: signal proven members, report strangers unknown", sid+2), n)
			// sessionMembers may see an occupied session but no stable identities
			// (fork/exec/exit during sampling), internal/proc/session.go. This resets
			// empty confirmation and can time out unknown; it is never evidence empty.
			if occupied {
				n = s
				n.Empty[sid] = 0
				n.Unknown = true
				n.Phase = 4
				n.LiveRecord = true
				add(fmt.Sprintf("session %d occupied with no stable members; deadline: unknown", sid+2), n)
			}
		}
		return out
	}}
}

func TestKillSafety(t *testing.T) {
	m := killModel(killOptions{})
	var signalled, group, retired, refreshed, unknown, finished, pinnedReuse bool
	invariant := m.Invariant
	m.Invariant = func(s killState) error {
		signalled = signalled || s.Signals
		group = group || s.GroupSignals
		retired = retired || s.Retired[1]
		refreshed = refreshed || s.P[2].Owned && s.Seen[2].Birth == 2
		unknown = unknown || s.Unknown && s.LiveRecord
		finished = finished || s.Phase == 4
		if s.Phase == 2 {
			pinnedReuse = pinnedReuse || s.Checked && s.P[s.At].Live && s.P[s.At].Birth != s.Handle
		}
		return invariant(s)
	}
	r := check(m)
	t.Log(r.String())
	if r.Violation != nil {
		t.Fatal(r.String())
	}
	if !signalled || !group || !retired || !refreshed || !unknown || !finished || !pinnedReuse {
		t.Fatalf("missing witnesses: signal=%v group=%v retire=%v refresh=%v unknown=%v finish=%v pinned-reuse=%v", signalled, group, retired, refreshed, unknown, finished, pinnedReuse)
	}
}

func TestKillSafetyMutations(t *testing.T) {
	for _, tc := range []struct {
		name     string
		options  killOptions
		expected string
	}{
		{"PID_signal_after_recheck", killOptions{NoPIDFD: true}, "signalled an unrelated process"},
		{"identity_guard", killOptions{NoIdentityGuard: true}, "signalled an unrelated process"},
		{"sweep_current_members_of_never_seen_empty_session", killOptions{UnsafeSessionMembers: true}, "signalled an unrelated process"},
		{"single_empty_snapshot", killOptions{SingleEmpty: true}, "declared empty without two consecutive unoccupied snapshots"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := check(killModel(tc.options))
			t.Log(r.String())
			if r.Violation == nil || r.Violation.Error() != tc.expected {
				t.Fatalf("expected %q counterexample, got %v", tc.expected, r.Violation)
			}
		})
	}
}

func TestKillSafetyNoPIDFDPlatformLimit(t *testing.T) {
	m := killModel(killOptions{NoPIDFD: true})
	m.Name = "documented macOS / no-pidfd platform limit"
	r := check(m)
	t.Log(r.String())
	if r.Violation == nil || r.Violation.Error() != "signalled an unrelated process" {
		t.Fatal("no-pidfd check-to-signal window was not reached")
	}
}

// The existing permanent-retirement mutation now checks the retirement
// contract directly. Fixed provenance prevents the old stranger-signal trace
// even without this guard; retirement still forbids inspecting a retired ID.
func TestKillSafetyEmptySessionMutation(t *testing.T) {
	seed := killInitial()
	seed.Phase = 3
	seed.At = 4
	seed.Dropped[1] = true
	seed.Empty[1] = 2
	seed.P[1] = killProc{Birth: 2, Parent: -1, Group: 1, Session: 1}
	seed.P[2] = killProc{Birth: 2, Parent: -1, Group: 1, Session: 1, Live: true}
	for _, mutated := range []bool{false, true} {
		m := killModel(killOptions{NoEmptyGuard: mutated})
		m.Initial = seed
		r := check(m)
		t.Log(r.String())
		if !mutated && r.Violation != nil {
			t.Fatal(r.String())
		}
		if mutated && (r.Violation == nil || r.Violation.Error() != "inspected a permanently retired session") {
			t.Fatal("permanent-retirement mutation was not detected")
		}
	}
}

// A separate exhaustive snapshot graph exposes stale PPID rows and child age.
// It has three rows: absent session leader, parent outside the session and child
// in the session. Parent/child births are 1..2, the parent's recorded birth is
// 1..2, the child record is absent or birth 1..2, parent presence and child PPID
// link are independent booleans. This complements the concrete exit/reuse graph.
func TestKillSessionProvenanceSnapshots(t *testing.T) {
	m := model[killState]{Name: "kill same-snapshot provenance", Initial: killState{Phase: -1}, Key: func(s killState) string { return fmt.Sprintf("%v", s) }, Invariant: func(s killState) error {
		if s.Phase == 1 && s.Checked != s.P[2].Owned {
			return fmt.Errorf("snapshot ownership differs from matching birth and age-checked ancestry")
		}
		return nil
	}, Next: func(s killState) []transition[killState] {
		if s.Phase == 1 {
			return nil
		}
		if s.Phase == 0 {
			// provenMembers/provenDescendants, internal/proc/session.go: this leader
			// is absent and unpinned, so membership requires identity or ancestry.
			n := s
			n.Phase = 1
			n.Checked = killProven(s, 0, false)[2]
			return []transition[killState]{{Action: "prove member from matching snapshot identities and age-checked PPID", State: n}}
		}
		var out []transition[killState]
		// Environment snapshots consumed by provenDescendants/provenMembers,
		// internal/proc/session.go. Independent record/live birth values cover a
		// reused parent, and childBirth < parentBirth covers stale sampled PPIDs.
		for recorded := 1; recorded <= 2; recorded++ {
			for parentBirth := 1; parentBirth <= 2; parentBirth++ {
				for childBirth := 1; childBirth <= 2; childBirth++ {
					for childRecord := 0; childRecord <= 2; childRecord++ {
						for present := 0; present <= 1; present++ {
							for linked := 0; linked <= 1; linked++ {
								n := killState{P: [3]killProc{{Birth: 1, Parent: -1}, {Birth: parentBirth, Parent: -1, Session: 2, Live: present == 1}, {Birth: childBirth, Parent: -1, Session: 0, Live: true}}}
								n.Known[1] = 1 << uint(recorded-1)
								if childRecord != 0 {
									n.Known[2] = 1 << uint(childRecord-1)
								}
								if linked == 1 {
									n.P[2].Parent = 1
								}
								n.P[2].Owned = childRecord == childBirth || present == 1 && linked == 1 && recorded == parentBirth && childBirth >= parentBirth
								out = append(out, transition[killState]{Action: "sample parent/child and recorded births", State: n})
							}
						}
					}
				}
			}
		}
		return out
	}}
	r := check(m)
	t.Log(r.String())
	if r.Violation != nil {
		t.Fatal(r.String())
	}
}

// Retain the original guard mutation's stranger-signal counterexample under
// explicitly restricted pre-fix sweep selection. This is not a faithful model:
// fixed provenMembers independently prevents the stranger from becoming a target.
func TestKillSafetyLegacyRetirementGuardIsolation(t *testing.T) {
	seed := killInitial()
	seed.Phase = 3
	seed.At = 4
	seed.Dropped[1] = true
	seed.Empty[1] = 2
	seed.P[1] = killProc{Birth: 2, Parent: -1, Group: 1, Session: 1}
	seed.P[2] = killProc{Birth: 2, Parent: -1, Group: 1, Session: 1, Live: true}
	for _, mutated := range []bool{false, true} {
		m := killModel(killOptions{UnsafeSessionMembers: true, NoEmptyGuard: mutated, LegacyRetirementIsolation: true})
		m.Name = "restricted legacy retirement guard isolation"
		m.Initial = seed
		r := check(m)
		t.Log(r.String())
		if !mutated && r.Violation != nil {
			t.Fatal(r.String())
		}
		if mutated && (r.Violation == nil || r.Violation.Error() != "signalled an unrelated process") {
			t.Fatal("legacy retirement mutation failed to reproduce stranger signal")
		}
	}
}
