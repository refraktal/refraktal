package proc

import (
	"slices"
	"testing"
)

const trackerStart = "Sun Nov 15 15:45:51 2026"

func trackerSnapshot(procs ...Proc) map[int]Proc {
	all := make(map[int]Proc, len(procs))
	for _, p := range procs {
		all[p.PID] = p
	}
	return all
}

func TestTrackerRetiresProcessGroups(t *testing.T) {
	root := Proc{PID: 62000, PPID: 1, PGID: 62000, Start: trackerStart}
	leader := Proc{PID: 63000, PPID: root.PID, PGID: 63000, Start: trackerStart}
	member := Proc{PID: 63001, PPID: leader.PID, PGID: leader.PID, Start: trackerStart}
	stranger := Proc{PID: 64000, PPID: 1, PGID: leader.PID, Start: "Sun Nov 15 15:45:53 2026"}
	for _, reason := range []string{"empty", "leader changed", "no verified survivor"} {
		t.Run(reason, func(t *testing.T) {
			tr := NewTracker(root.PID, func() bool { return true })
			tr.ownPGID = 1 // independent of the test runner's real process group
			tr.add(trackerSnapshot(root, leader, member))
			switch reason {
			case "empty":
				tr.add(trackerSnapshot(root))
			case "leader changed":
				reused := leader
				reused.PPID, reused.Start = 1, stranger.Start
				tr.add(trackerSnapshot(root, reused))
			}
			// The replacement leader has already exited between snapshots.
			tr.add(trackerSnapshot(root, stranger))
			if slices.Contains(pids(tr.Tracked()), stranger.PID) {
				t.Fatal("enrolled an unrelated member through an unverified reused process group")
			}
		})
	}
}

func TestTrackerKeepsVerifiedSurvivingGroupMembers(t *testing.T) {
	root := Proc{PID: 62000, PPID: 1, PGID: 62000, Start: trackerStart}
	leader := Proc{PID: 63000, PPID: root.PID, PGID: 63000, Start: trackerStart}
	member := Proc{PID: 63001, PPID: leader.PID, PGID: leader.PID, Start: trackerStart}
	tr := NewTracker(root.PID, func() bool { return true })
	tr.ownPGID = 1 // independent of the test runner's real process group
	tr.add(trackerSnapshot(root, leader, member))
	member.PPID = 1
	newMember := Proc{PID: 63002, PPID: 1, PGID: leader.PID, Start: "Sun Nov 15 15:45:52 2026"}
	tr.add(trackerSnapshot(root, member, newMember))
	if !slices.Contains(pids(tr.Tracked()), newMember.PID) {
		t.Fatal("lost a continuously verified group after its leader exited")
	}
}

func TestTrackerKeepsUnreapedRootGroupReserved(t *testing.T) {
	root := Proc{PID: 62000, PPID: 1, PGID: 62000, Start: trackerStart}
	tr := NewTracker(root.PID, func() bool { return true })
	tr.ownPGID = 1 // independent of the test runner's real process group
	tr.add(trackerSnapshot(root))
	// List omits the zombie root, but rootUp says its PID remains reserved.
	tr.add(trackerSnapshot())
	member := Proc{PID: 63001, PPID: 1, PGID: root.PID, Start: trackerStart}
	tr.add(trackerSnapshot(member))
	if !slices.Contains(pids(tr.Tracked()), member.PID) {
		t.Fatal("lost the root's reserved group before the root was reaped")
	}
}

func TestTrackerRefreshesReusedDescendantIdentity(t *testing.T) {
	root := Proc{PID: 62000, PPID: 1, PGID: 62000, Start: trackerStart}
	parent := Proc{PID: 63000, PPID: root.PID, PGID: 63000, Start: trackerStart}
	old := Proc{PID: 63001, PPID: parent.PID, PGID: parent.PID, Start: trackerStart}
	for _, relation := range []string{"root child", "tracked parent", "verified group"} {
		t.Run(relation, func(t *testing.T) {
			tr := NewTracker(root.PID, func() bool { return true })
			tr.ownPGID = 1 // independent of the test runner's real process group
			tr.add(trackerSnapshot(root, parent, old))
			current := old
			current.Start = "Sun Nov 15 15:45:52 2026"
			switch relation {
			case "root child":
				current.PPID, current.PGID = root.PID, 64000
			case "tracked parent":
				current.PGID = 64000
			case "verified group":
				current.PPID = 1
			}
			tr.add(trackerSnapshot(root, parent, current))
			tracked := tr.Tracked()
			if len(tracked) != 2 || !slices.Contains(tracked, current) {
				t.Fatal("verified descendant PID retained its previous process identity")
			}
		})
	}
}

func TestTrackerDoesNotRefreshUnverifiedPIDReuse(t *testing.T) {
	root := Proc{PID: 62000, PPID: 1, PGID: 62000, Start: trackerStart}
	old := Proc{PID: 63000, PPID: root.PID, PGID: 64000, Start: trackerStart}
	tr := NewTracker(root.PID, func() bool { return true })
	tr.ownPGID = 1 // independent of the test runner's real process group
	tr.add(trackerSnapshot(root, old))
	stranger := old
	stranger.PPID, stranger.PGID, stranger.Start = 1, 64001, "Sun Nov 15 15:45:52 2026"
	strangerChild := Proc{PID: 63001, PPID: stranger.PID, PGID: stranger.PGID, Start: stranger.Start}
	tr.add(trackerSnapshot(root, stranger, strangerChild))
	if got := tr.Tracked(); len(got) != 1 || got[0] != old {
		t.Fatal("an unverified reused PID replaced the tracked identity or enrolled its child")
	}
}

func TestTrackerDoesNotUseANewMemberToReviveAGroup(t *testing.T) {
	root := Proc{PID: 62000, PPID: 1, PGID: 62000, Start: trackerStart}
	leader := Proc{PID: 63000, PPID: root.PID, PGID: 63000, Start: trackerStart}
	moved := Proc{PID: 64000, PPID: root.PID, PGID: root.PID, Start: trackerStart}
	tr := NewTracker(root.PID, func() bool { return true })
	tr.ownPGID = 1 // independent of the test runner's real process group
	tr.add(trackerSnapshot(root, leader, moved))
	moved.PPID, moved.PGID = 1, leader.PID
	stranger := Proc{PID: 64001, PPID: 1, PGID: leader.PID, Start: "Sun Nov 15 15:45:52 2026"}
	tr.add(trackerSnapshot(root, moved, stranger))
	if slices.Contains(pids(tr.Tracked()), stranger.PID) {
		t.Fatal("a tracked process newly joining the group revived an unverified group")
	}
}

func TestTrackerRefreshesVerifiedGroupMembership(t *testing.T) {
	root := Proc{PID: 62000, PPID: 1, PGID: 62000, Start: trackerStart}
	leader := Proc{PID: 63000, PPID: root.PID, PGID: 63000, Start: trackerStart}
	moved := Proc{PID: 64000, PPID: root.PID, PGID: root.PID, Start: trackerStart}
	tr := NewTracker(root.PID, func() bool { return true })
	tr.ownPGID = 1 // independent of the test runner's real process group
	tr.add(trackerSnapshot(root, leader, moved))
	moved.PGID = leader.PID
	tr.add(trackerSnapshot(root, leader, moved))
	moved.PPID = 1
	member := Proc{PID: 64001, PPID: 1, PGID: leader.PID, Start: "Sun Nov 15 15:45:52 2026"}
	tr.add(trackerSnapshot(root, moved, member))
	if !slices.Contains(pids(tr.Tracked()), member.PID) {
		t.Fatal("a verified change of process group was not retained for continuity")
	}
}

func TestReviewTrackerStalePPIDTargetsUnrelatedProcess(t *testing.T) {
	root := Proc{PID: 62000, PPID: 1, PGID: 62000, Start: "Sun Nov 15 15:45:50 2026"}
	parent := Proc{PID: 63000, PPID: root.PID, PGID: 63000, Start: "Sun Nov 15 15:45:54 2026"}
	stranger := Proc{PID: 64000, PPID: parent.PID, PGID: 64000, Start: "Sun Nov 15 15:45:52 2026"}
	tr := NewTracker(root.PID, func() bool { return true })
	tr.ownPGID = 1
	snapshot := map[int]Proc{root.PID: root, parent.PID: parent, stranger.PID: stranger}
	tr.add(snapshot)
	sent := interceptSignals(t)
	oldCheck, oldList := recheckPID, listForKill
	t.Cleanup(func() { recheckPID, listForKill = oldCheck, oldList })
	recheckPID = func(pid int) (Proc, error) { return snapshot[pid], nil }
	listForKill = func() (map[int]Proc, error) { return map[int]Proc{}, nil }
	report, err := Kill(tr.Tracked())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("tracked=%v signalled=%v killed=%v", pids(tr.Tracked()), *sent, pids(report.Killed))
	if slices.Contains(*sent, stranger.PID) {
		t.Fatal("unrelated older child was enrolled through a reused PPID and passed the kill identity check")
	}
}

func TestTrackerPPIDEnrollmentNeedsBirthOrder(t *testing.T) {
	for _, relation := range []string{"root", "tracked parent"} {
		for _, tc := range []struct {
			name, parentStart, childStart string
			present, want                 bool
		}{
			{"equal births", trackerStart, trackerStart, true, true},
			{"later child", trackerStart, "Sun Nov 15 15:45:52 2026", true, true},
			{"older child", "Sun Nov 15 15:45:52 2026", trackerStart, true, false},
			{"missing child birth", trackerStart, "", true, false},
			{"invalid child birth", trackerStart, "invalid", true, false},
			{"missing parent birth", "", trackerStart, true, false},
			{"invalid parent birth", "invalid", trackerStart, true, false},
			{"missing parent row", trackerStart, trackerStart, false, false},
		} {
			t.Run(relation+"/"+tc.name, func(t *testing.T) {
				root := Proc{PID: 62000, PPID: 1, PGID: 62000, Start: trackerStart}
				parent := root
				parent.Start = tc.parentStart
				tr := NewTracker(root.PID, func() bool { return true })
				tr.ownPGID = 1
				if relation == "tracked parent" {
					parent.PID, parent.PPID, parent.PGID = 63000, root.PID, 63000
					// A historical identity alone cannot prove a PPID edge without
					// a current, valid and chronologically possible parent row.
					tr.seen[parent.PID] = parent
				}
				child := Proc{PID: 64000, PPID: parent.PID, PGID: 64000, Start: tc.childStart}
				all := trackerSnapshot(child)
				if relation == "tracked parent" {
					all[root.PID] = root
				}
				if tc.present {
					all[parent.PID] = parent
				}
				tr.add(all)
				if got := slices.Contains(pids(tr.Tracked()), child.PID); got != tc.want {
					t.Fatalf("child enrolled=%v, want %v", got, tc.want)
				}
			})
		}
	}
}
