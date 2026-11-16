package verify

import (
	"fmt"
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
// context.WithoutCancel. Git writes files sequentially, however: with
// filesystemErrors=true, a write failure after the first file violates the
// half-apply invariant. The control-flow check excludes filesystem failures,
// process death and opCtx's independent five-minute timeout explicitly.
type lifecycleState struct {
	Count        uint8
	NoApply      bool
	Child        [2]uint8 // 0 running, 1 stopped, 2 swept/shutdown returned
	Collect      [2]uint8 // 0 pending, 1 success with changes, 2 failed
	Check        [2]uint8 // 0 pending, 1 passed, 2 failed, 3 inapplicable
	Kept         [2]bool
	Deleted      [2]bool
	Judge        uint8 // 0 none, 1 running, 2 stopped, 3 swept
	Stage        uint8 // 0 configure, 1 collect, 2 check, 3 choose, 4 admit, 5 apply-check, 6 write, 7 finish, 8 done
	Winner       uint8 // 0 none, candidate index + 1 otherwise
	Cancelled    bool
	Expired      bool
	Admitted     bool
	AdmissionOK  bool
	ApplyChecked bool
	Tree         uint8 // 0 unchanged, 3 both files applied, 1 partial (mutation only)
	EarlyCollect bool
}

type lifecycleMutation string

const (
	lifecycleFaithful lifecycleMutation = ""
	lifecycleNoCancel lifecycleMutation = "remove cancel guard"
	lifecycleNoTime   lifecycleMutation = "remove deadline guard"
	lifecycleNoWait   lifecycleMutation = "remove all-children wait"
	lifecycleNoKeep   lifecycleMutation = "remove failed-collect retention"
	lifecycleNoCheck  lifecycleMutation = "remove failed-check exclusion"
	lifecycleCutApply lifecycleMutation = "remove cancellation-independent apply"
)

func lifecycleModel(mutation lifecycleMutation, filesystemErrors bool) model[lifecycleState] {
	name := "run lifecycle control flow " + string(mutation)
	if filesystemErrors {
		name = "run lifecycle faithful filesystem-error environment"
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
			if s.Tree != 0 && s.Tree != 3 {
				return fmt.Errorf("tree half-applied")
			}
			if s.Tree != 0 && !s.ApplyChecked {
				return fmt.Errorf("tree changed without final git apply --check")
			}
			for i := 0; i < int(s.Count); i++ {
				if s.Collect[i] == 2 && s.Deleted[i] {
					return fmt.Errorf("candidate %d deleted after failed collect", i)
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
					// precede runEnv.collect (fanout.go); Collect may fail.
					for _, result := range []uint8{1, 2} {
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
					}
					add(fmt.Sprintf("final git apply --check passes=%t", pass), n)
				}
			case 6:
				if filesystemErrors {
					// Worktree.Apply (internal/worktree/worktree.go) runs git
					// apply --binary without rollback. A later file creation
					// can fail after an earlier file was already replaced.
					n := s
					n.Tree, n.Stage = 1, 7
					add("git writes first file; second file write fails", n)
				}
				// Worktree.Apply (internal/worktree/worktree.go) uses opCtx
				// (fanout.go), so parent cancel/deadline never interrupts writes.
				n := s
				n.Tree, n.Stage = 3, 7
				if mutation == lifecycleCutApply && (s.Cancelled || s.Expired) {
					n.Tree = 1
				}
				add("write complete patch with cancellation-independent context", n)
			case 7:
				// Run's final retention (run.go) and fanOut.finish with
				// Candidate.keep/drop (fanout.go): failed collects always survive.
				n := s
				n.Stage = 8
				for i := 0; i < int(s.Count); i++ {
					keep := s.Collect[i] == 2 || s.Tree == 0 || s.NoApply
					if mutation == lifecycleNoKeep && s.Collect[i] == 2 {
						keep = false
					}
					n.Kept[i], n.Deleted[i] = keep, !keep
				}
				add("retain candidates or remove successful worktrees", n)
			}
			return out
		},
	}
}

func TestRunLifecycleControlFlow(t *testing.T) {
	m := lifecycleModel(lifecycleFaithful, false)
	var applied, failedCollectKept, cancelDuringApply, judged, single, fan, noApply bool
	next := m.Next
	m.Next = func(s lifecycleState) []transition[lifecycleState] {
		applied = applied || s.Tree == 3
		cancelDuringApply = cancelDuringApply || s.Tree == 3 && (s.Cancelled || s.Expired)
		judged = judged || s.Judge == 3
		single, fan = single || s.Count == 1, fan || s.Count == 2
		noApply = noApply || s.Stage == 8 && s.NoApply && s.Winner != 0 && s.Tree == 0
		for i := range s.Collect {
			failedCollectKept = failedCollectKept || s.Collect[i] == 2 && s.Kept[i]
		}
		return next(s)
	}
	r := check(m)
	t.Log(r.String())
	if r.Violation != nil {
		t.Fatal(r.String())
	}
	if !applied || !failedCollectKept || !cancelDuringApply || !judged || !single || !fan || !noApply {
		t.Fatalf("missing witness: apply=%t retention=%t during_apply=%t judge=%t single=%t fan=%t no_apply=%t", applied, failedCollectKept, cancelDuringApply, judged, single, fan, noApply)
	}
}

func TestRunLifecycleMutations(t *testing.T) {
	for _, mutation := range []lifecycleMutation{lifecycleNoCancel, lifecycleNoTime, lifecycleNoWait, lifecycleNoKeep, lifecycleNoCheck, lifecycleCutApply} {
		t.Run(string(mutation), func(t *testing.T) {
			r := check(lifecycleModel(mutation, false))
			t.Log(r.String())
			if r.Violation == nil {
				t.Fatal("guard removal did not produce a counterexample")
			}
		})
	}
}

// This is an expected counterexample, not a proof of the requested invariant.
// Keep it visible until Worktree.Apply has transactional failure behavior.
func TestRunLifecycleFilesystemFinding(t *testing.T) {
	r := check(lifecycleModel(lifecycleFaithful, true))
	t.Log(r.String())
	if r.Violation == nil || r.Violation.Error() != "tree half-applied" {
		t.Fatalf("expected faithful half-apply finding: %s", r.String())
	}
}
