package verify

import (
	"fmt"
	"strings"
	"testing"
)

// Lifecycle bounds: one or two candidate children, one optional judge, one
// collect and check result per candidate, two files in one patch, and monotonic
// cancel/deadline events. Two children expose a missing wait barrier; two files
// distinguish a whole apply from a partial apply. These are bounded checks, not
// a proof for arbitrary fan-out, filesystems, or Git failures. A stopped child
// means its shutdown call returned; the separate process model covers signals.
// Repository checks are applicable only to fan-out, as DESIGN.md specifies.
// The mandatory final git apply --check is modeled for both widths.
//
// Parent cancellation/deadline cannot cut apply off because runEnv.opCtx uses
// context.WithoutCancel. Apply snapshots both paths before sequential writes
// and restores them on write failure. Atomicity is checked when Apply returns,
// not between its writes. Restoration succeeds in this model; rollback I/O
// failure (reported by ApplyError.Unrestored with the snapshot kept), concurrent
// edits, process death, and opCtx's independent timeout are outside the bounds.
type lifecycleState struct {
	Count        uint8
	NoApply      bool
	Child        [2]uint8 // 0 running, 1 stopped, 2 swept/shutdown returned
	Collect      [2]uint8 // 0 pending, 1 changes, 2 failed collect with changes, 3 failed turn with changes, 4 no changes, 5 failed collect with unknown changes
	Check        [2]uint8 // 0 pending, 1 passed, 2 failed, 3 inapplicable
	Kept         [2]bool
	Deleted      [2]bool
	Judge        uint8 // 0 none, 1 running, 2 stopped, 3 swept
	Stage        uint8 // 0 configure, 1 collect, 2 check, 3 choose, 4 admit, 5 apply-check, 6 apply operation, 7 finish, 8 done
	Winner       uint8 // 0 none, candidate index + 1 otherwise
	Cancelled    bool
	Expired      bool
	Admitted     bool
	AdmissionOK  bool
	ApplyChecked bool
	Tree         uint8 // 0 unchanged, 1 first file written, 3 both files applied
	WritePhase   uint8 // 0 snapshot, 1 first write, 2 second write, 3 rollback
	Snapshot     bool
	RolledBack   bool
	ApplyFailed  bool
	Interrupted  bool
	Outcome      uint8 // 0 pending, 1 applied, 2 kept winner, 3 failure, 4 no changes/no winner
	EarlyCollect bool
}

type lifecycleMutation string

const (
	lifecycleFaithful        lifecycleMutation = ""
	lifecycleNoCancel        lifecycleMutation = "remove cancel guard"
	lifecycleNoTime          lifecycleMutation = "remove deadline guard"
	lifecycleNoWait          lifecycleMutation = "remove all-children wait"
	lifecycleNoKeep          lifecycleMutation = "remove failed-collect retention"
	lifecycleNoCheck         lifecycleMutation = "remove failed-check exclusion"
	lifecycleCutApply        lifecycleMutation = "remove cancellation-independent apply"
	lifecycleNoRollback      lifecycleMutation = "apply without rollback"
	lifecycleDeleteFailed    lifecycleMutation = "delete failed turn changes"
	lifecycleNoFailedOutcome lifecycleMutation = "report unused failed changes as no changes"
)

func lifecycleModel(mutation lifecycleMutation, filesystemErrors bool) model[lifecycleState] {
	name := "run lifecycle control flow " + string(mutation)
	if filesystemErrors {
		name = "run lifecycle filesystem errors " + string(mutation)
	}
	return model[lifecycleState]{
		Name: name,
		Key:  func(s lifecycleState) string { return fmt.Sprintf("%v", s) },
		Invariant: func(s lifecycleState) error {
			if s.EarlyCollect {
				return fmt.Errorf("collected before every candidate stopped and was swept")
			}
			if s.Admitted && !s.AdmissionOK {
				return fmt.Errorf("apply admitted without winner, applicable checks, live deadline, or no cancel")
			}
			if s.Stage >= 7 && s.Tree != 0 && s.Tree != 3 {
				return fmt.Errorf("tree half-applied")
			}
			if s.Interrupted {
				return fmt.Errorf("parent cancellation interrupted an admitted apply")
			}
			if s.Tree != 0 && !s.Snapshot {
				return fmt.Errorf("tree changed without snapshot")
			}
			if s.Tree != 0 && !s.ApplyChecked {
				return fmt.Errorf("tree changed without final git apply --check")
			}
			for i := 0; i < int(s.Count); i++ {
				if (s.Collect[i] == 2 || s.Collect[i] == 5) && s.Deleted[i] {
					return fmt.Errorf("candidate %d deleted after failed collect", i)
				}
				if s.Collect[i] == 3 && s.Deleted[i] {
					return fmt.Errorf("candidate %d deleted after failed turn with changes", i)
				}
				if s.Stage == 8 && (s.Collect[i] == 2 || s.Collect[i] == 3) && s.Winner == 0 && s.Outcome != 3 {
					return fmt.Errorf("unused failed changes did not make run fail")
				}
			}
			return nil
		},
		Next: func(s lifecycleState) []transition[lifecycleState] {
			var out []transition[lifecycleState]
			add := func(action string, n lifecycleState) { out = append(out, transition[lifecycleState]{action, n}) }
			if s.Stage == 8 {
				return out
			}
			if s.Stage == 0 {
				// Run (run.go) selects a lone candidate or runEnv.fanOut (fanout.go).
				for _, count := range []uint8{1, 2} {
					for _, noApply := range []bool{false, true} {
						n := s
						n.Count, n.NoApply, n.Stage = count, noApply, 1
						add(fmt.Sprintf("configure %d candidates no_apply=%t", count, noApply), n)
					}
				}
				return out
			}
			// Run contexts (run.go), runEnv.child and runEnv.apply (fanout.go):
			// cancellation and deadline may arrive at every nonterminal stage.
			if !s.Cancelled {
				n := s
				n.Cancelled = true
				add("parent cancel arrives", n)
			}
			if !s.Expired {
				n := s
				n.Expired = true
				add("run deadline passes", n)
			}
			allSwept, allCollected, allChecked := true, true, true
			for i := 0; i < int(s.Count); i++ {
				allSwept = allSwept && s.Child[i] == 2
				allCollected = allCollected && s.Collect[i] != 0
				allChecked = allChecked && (s.Collect[i] != 1 || s.Check[i] != 0)
				if s.Child[i] < 2 {
					// runEnv.child (fanout.go), session.close (session.go), and
					// Child.shutdown (internal/proc/child.go): stop, then sweep.
					n := s
					n.Child[i]++
					add(fmt.Sprintf("child %d shutdown step %d", i, n.Child[i]), n)
				}
			}
			switch s.Stage {
			case 1:
				for i := 0; i < int(s.Count); i++ {
					if s.Collect[i] != 0 || s.Child[i] != 2 || (!allSwept && mutation != lifecycleNoWait) {
						continue
					}
					// Run (run.go) and runEnv.fanOut's wg.Wait (fanout.go)
					// precede runEnv.collect (fanout.go). A successful collect
					// preserves a failed turn's outcome even when files changed;
					// a failed collect retains work even without known filenames.
					for _, result := range []uint8{1, 2, 3, 4, 5} {
						n := s
						n.Collect[i], n.EarlyCollect = result, !allSwept
						add(fmt.Sprintf("collect candidate %d result=%d", i, result), n)
					}
				}
				if allCollected {
					// Run (run.go) skips repository checks for a lone candidate;
					// runEnv.fanOut (fanout.go) enters parallel checks.
					n := s
					n.Stage = 2
					if s.Count == 1 {
						n.Check[0] = 3
					}
					add("collection barrier complete", n)
				}
			case 2:
				for i := 0; i < int(s.Count); i++ {
					if s.Collect[i] != 1 || s.Check[i] != 0 {
						continue
					}
					// runEnv.fanOut (fanout.go), Worktree.CheckApply
					// (internal/worktree/worktree.go), runCheck (judge.go).
					for _, result := range []uint8{1, 2, 3} {
						n := s
						n.Check[i] = result // 3: no repository check detected
						add(fmt.Sprintf("candidate %d checks result=%d", i, result), n)
					}
				}
				if allChecked {
					// runEnv.fanOut waits for checks before choose (judge.go).
					n := s
					n.Stage = 3
					add("check barrier complete", n)
				}
			case 3:
				var eligible []int
				for i := 0; i < int(s.Count); i++ {
					if s.Collect[i] == 1 && (s.Check[i] == 1 || s.Check[i] == 3 || mutation == lifecycleNoCheck) {
						eligible = append(eligible, i)
					}
				}
				if len(eligible) > 1 && s.Judge < 3 {
					// runEnv.fanOut invokes runEnv.child(judge=true)
					// (fanout.go), whose session.close (session.go) sweeps it.
					n := s
					n.Judge++
					add(fmt.Sprintf("judge start/stop/sweep step %d", n.Judge), n)
					break
				}
				// choose and parseWinner (judge.go), plus runEnv.fanOut's
				// failed-judge fallback (fanout.go): only eligible candidates win.
				for _, i := range eligible {
					n := s
					n.Winner, n.Stage = uint8(i+1), 4
					add(fmt.Sprintf("choose candidate %d", i), n)
				}
				if len(eligible) == 0 || s.Judge == 3 {
					// runEnv.fanOut (fanout.go): no winner keeps candidates.
					n := s
					n.Stage = 7
					add("no winner", n)
				}
			case 4:
				// Run and runEnv.fanOut (run.go, fanout.go): --no-apply keeps work.
				if s.NoApply {
					n := s
					n.Stage = 7
					add("no-apply keeps winner", n)
					break
				}
				// runEnv.apply (fanout.go): deadline and cancel guards immediately
				// precede admission; opCtx detaches the admitted apply afterward.
				if (s.Cancelled && mutation != lifecycleNoCancel) || (s.Expired && mutation != lifecycleNoTime) {
					n := s
					n.Stage = 7
					add("apply refused by cancel/deadline", n)
					break
				}
				n := s
				n.Admitted, n.Stage = true, 5
				i := int(s.Winner) - 1
				n.AdmissionOK = i >= 0 && !s.Cancelled && !s.Expired && (s.Check[i] == 1 || s.Check[i] == 3)
				add("admit winner apply", n)
			case 5:
				// Worktree.Apply (internal/worktree/worktree.go): final
				// git apply --check either passes or rejects the whole patch.
				for _, pass := range []bool{false, true} {
					n := s
					n.ApplyChecked, n.Stage = pass, 7
					if pass {
						n.Stage = 6
					} else {
						n.ApplyFailed = true
					}
					add(fmt.Sprintf("final git apply --check passes=%t", pass), n)
				}
			case 6:
				switch s.WritePhase {
				case 0:
					// Worktree.Apply (internal/worktree/worktree.go) calls
					// takeSnapshot (internal/worktree/rollback.go) before writes.
					// A snapshot error returns without changing the user's tree.
					for _, pass := range []bool{false, true} {
						n := s
						n.Snapshot, n.WritePhase = pass, 1
						if !pass {
							n.ApplyFailed, n.Stage = true, 7
						}
						add(fmt.Sprintf("snapshot both paths succeeds=%t", pass), n)
					}
				case 1:
					// Worktree.Apply (internal/worktree/worktree.go): git apply
					// --binary writes the first path while the snapshot is held.
					n := s
					n.Tree, n.WritePhase = 1, 2
					add("git writes first file", n)
				case 2:
					// runEnv.opCtx (fanout.go) detaches parent cancellation and
					// deadlines. Removing it interrupts writes; rollback still runs.
					if mutation == lifecycleCutApply && (s.Cancelled || s.Expired) {
						n := s
						n.Interrupted, n.ApplyFailed, n.WritePhase = true, true, 3
						add("parent interrupts admitted git apply", n)
					}
					if filesystemErrors {
						// Worktree.Apply (internal/worktree/worktree.go): a later
						// write failure calls snapshot.rollback (rollback.go).
						n := s
						n.ApplyFailed, n.WritePhase = true, 3
						if mutation == lifecycleNoRollback {
							n.Stage = 7
						}
						add("second file write fails", n)
					}
					// Worktree.Apply (internal/worktree/worktree.go) clears its
					// snapshot after successful git apply. A later LFS lookup may
					// fail, but the whole patch is already applied in that case.
					for _, postCheck := range []bool{false, true} {
						n := s
						n.Tree, n.Stage, n.ApplyFailed = 3, 7, !postCheck
						add(fmt.Sprintf("write complete patch; LFS lookup succeeds=%t", postCheck), n)
					}
				case 3:
					// snapshot.rollback/restore (internal/worktree/rollback.go)
					// restores both touched paths before Apply returns its error.
					n := s
					n.Tree, n.Stage, n.RolledBack = 0, 7, true
					add("restore both paths from snapshot and return apply error", n)
				}
			case 7:
				// Run (run.go), runEnv.fanOut.finish and Candidate.keep/drop
				// (fanout.go): retain failed collects and failed turns' changes,
				// even when a different candidate was successfully applied.
				n := s
				n.Stage, n.Outcome = 8, 4
				if s.Tree == 3 && !s.ApplyFailed {
					n.Outcome = 1
				} else if s.NoApply && s.Winner != 0 {
					n.Outcome = 2
				} else if s.ApplyFailed || s.Cancelled || s.Expired {
					n.Outcome = 3
				}
				// Run (run.go) propagates the lone failed candidate's outcome;
				// fanOut (fanout.go) also fails when every candidate failed.
				allFailed := true
				for i := 0; i < int(s.Count); i++ {
					allFailed = allFailed && (s.Collect[i] == 2 || s.Collect[i] == 3 || s.Collect[i] == 5)
				}
				if allFailed {
					n.Outcome = 3
				}
				for i := 0; i < int(s.Count); i++ {
					failedCollect := s.Collect[i] == 2 || s.Collect[i] == 5
					failedChanges := s.Collect[i] == 2 || s.Collect[i] == 3
					hasChanges := s.Collect[i] >= 1 && s.Collect[i] <= 3
					keep := failedCollect || failedChanges || (s.Tree == 0 || s.ApplyFailed || s.NoApply) && (hasChanges || s.NoApply)
					if mutation == lifecycleNoKeep && failedCollect || mutation == lifecycleDeleteFailed && s.Collect[i] == 3 && s.Tree == 3 && !s.ApplyFailed && s.Count == 2 {
						keep = false
					}
					n.Kept[i], n.Deleted[i] = keep, !keep
					// runEnv.fanOut.finish (fanout.go) upgrades no_changes and
					// no_winner to failed if failed changes remain unused.
					if failedChanges && s.Winner == 0 && mutation != lifecycleNoFailedOutcome {
						n.Outcome = 3
					}
				}
				add("retain work and classify final run outcome", n)
			}
			return out
		},
	}
}

func TestRunLifecycleFaithful(t *testing.T) {
	for _, filesystemErrors := range []bool{false, true} {
		t.Run(fmt.Sprintf("filesystem_errors=%t", filesystemErrors), func(t *testing.T) {
			m := lifecycleModel(lifecycleFaithful, filesystemErrors)
			var applied, failedCollectKept, failedTurnKept, unusedFailed, cancelDuringApply, judged, single, fan, noApply, rollback bool
			next := m.Next
			m.Next = func(s lifecycleState) []transition[lifecycleState] {
				applied = applied || s.Tree == 3
				cancelDuringApply = cancelDuringApply || s.Tree == 3 && (s.Cancelled || s.Expired)
				judged = judged || s.Judge == 3
				single, fan = single || s.Count == 1, fan || s.Count == 2
				noApply = noApply || s.Stage == 8 && s.NoApply && s.Winner != 0 && s.Tree == 0
				rollback = rollback || s.RolledBack && s.Tree == 0
				for i := range s.Collect {
					failedCollectKept = failedCollectKept || s.Collect[i] == 2 && s.Kept[i]
					failedTurnKept = failedTurnKept || s.Collect[i] == 3 && s.Kept[i] && s.Tree == 3
					unusedFailed = unusedFailed || s.Collect[i] == 3 && s.Winner == 0 && s.Outcome == 3
				}
				return next(s)
			}
			r := check(m)
			t.Log(r.String())
			if r.Violation != nil {
				t.Fatal(r.String())
			}
			if !applied || !failedCollectKept || !failedTurnKept || !unusedFailed || !cancelDuringApply || !judged || !single || !fan || !noApply || filesystemErrors && !rollback {
				t.Fatalf("missing witness: apply=%t collect_keep=%t failed_turn_keep=%t unused_failure=%t during_apply=%t judge=%t single=%t fan=%t no_apply=%t rollback=%t", applied, failedCollectKept, failedTurnKept, unusedFailed, cancelDuringApply, judged, single, fan, noApply, rollback)
			}
		})
	}
}

func TestRunLifecycleMutations(t *testing.T) {
	for _, tc := range []struct {
		mutation lifecycleMutation
		want     string
	}{
		{lifecycleNoCancel, "apply admitted without winner, applicable checks, live deadline, or no cancel"},
		{lifecycleNoTime, "apply admitted without winner, applicable checks, live deadline, or no cancel"},
		{lifecycleNoWait, "collected before every candidate stopped and was swept"},
		{lifecycleNoKeep, "deleted after failed collect"},
		{lifecycleNoCheck, "apply admitted without winner, applicable checks, live deadline, or no cancel"},
		{lifecycleCutApply, "parent cancellation interrupted an admitted apply"},
		{lifecycleNoRollback, "tree half-applied"},
		{lifecycleDeleteFailed, "deleted after failed turn with changes"},
		{lifecycleNoFailedOutcome, "unused failed changes did not make run fail"},
	} {
		t.Run(string(tc.mutation), func(t *testing.T) {
			r := check(lifecycleModel(tc.mutation, true))
			t.Log(r.String())
			if r.Violation == nil || !strings.Contains(r.Violation.Error(), tc.want) {
				t.Fatalf("want counterexample for %q: %s", tc.want, r.String())
			}
		})
	}
}
