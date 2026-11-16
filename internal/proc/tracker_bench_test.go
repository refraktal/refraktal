package proc

import "testing"

func BenchmarkTrackedRecordSnapshot(b *testing.B) {
	for _, mode := range []string{"unchanged", "changed", "disabled"} {
		b.Run(mode, func(b *testing.B) {
			tracker := NewTracker(1, func() bool { return true })
			for pid := 2; pid < 258; pid++ {
				tracker.seen[pid] = Proc{PID: pid, PPID: 1, Start: "recorded"}
			}
			previous := tracker.Tracked()
			recording := mode != "disabled"
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				if mode == "changed" {
					tracker.seen[2] = Proc{PID: 2, PPID: 1, Start: "recorded", Cmd: string(rune(i))}
				}
				if recording {
					if tracked, changed := tracker.trackedIfChanged(previous); changed {
						previous = tracked
					}
				}
			}
		})
	}
}
