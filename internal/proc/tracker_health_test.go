package proc

import (
	"slices"
	"testing"
)

func TestTrackedIfChangedPreservesSnapshot(t *testing.T) {
	tracker := NewTracker(1, func() bool { return true })
	previous := tracker.Tracked()
	if _, changed := tracker.trackedIfChanged(previous); changed {
		t.Fatal("empty tracker changed")
	}
	check := func(want bool) {
		t.Helper()
		tracked, changed := tracker.trackedIfChanged(previous)
		if changed != want {
			t.Fatalf("changed = %t, want %t", changed, want)
		}
		if changed {
			if !slices.Equal(tracked, tracker.Tracked()) {
				t.Fatal("changed snapshot differs from Tracked")
			}
			previous = tracked
		}
	}
	tracker.seen[3] = Proc{PID: 3, PPID: 1, Start: "old", Cmd: "before"}
	tracker.seen[2] = Proc{PID: 2, PPID: 1, Start: "first"}
	check(true)
	check(false)
	tracker.seen[3] = Proc{PID: 3, PPID: 1, Start: "new", Cmd: "before"}
	check(true)
	tracker.seen[3] = Proc{PID: 3, PPID: 1, Start: "new", Cmd: "after"}
	check(true)
	tracker.seen[3] = Proc{PID: 3, PPID: 2, Start: "new", Cmd: "after"}
	check(true)
	check(false)
}
