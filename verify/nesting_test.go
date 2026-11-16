package verify

import (
	"fmt"
	"strings"
	"testing"
)

// Nesting bounds: initial REFRAKTAL_NEST in {invalid, negative, 0..3},
// --depth in 0..3, and inherited ceilings in {missing, malformed, -1..4}.
// Each managed edge passes proc.Start, increments Nest, and preserves the
// ceiling. Child flags range over 0..3; their grant either shrinks depth,
// disappears (flags reset), or is malformed (startup refuses). These cover
// the boundary relations, not all integers: this is a bounded check, excluding
// integer overflow and deliberate environment replacement. Direct shell
// relaunches that bypass proc.Start are not counted managed edges. A manual
// invocation enters as an initial state and uses the no-ceiling fallback.
// No exploration cutoff is used: intact guards terminate every modeled chain.
// At most four runs are admitted per chain. Repeated prompts on one ACP
// server are not new managed edges; server.sharedLedger (acpserver.go) keeps
// their inherited ledger, which is outside this nesting-only state machine.
type nestingState struct {
	Phase                         uint8 // 0 configure, 1 guard, 2 accepted, 3 refused
	Nest, Depth, Ceiling          int
	Inherited                     bool
	RootNest, Bound, Accepted     int
	MaxAccepted                   int
	LostGrant, Lowered, Malformed bool
}

func nestingModel(removeGuard bool) model[nestingState] {
	name := "nesting FAITHFUL"
	if removeGuard {
		name = "nesting MUTATION inherited-ceiling refusal"
	}
	return model[nestingState]{
		Name: name, Key: func(s nestingState) string { return fmt.Sprintf("%v", s) },
		Invariant: func(s nestingState) error {
			if s.Accepted > 0 && (s.MaxAccepted > s.Bound || s.Accepted > s.Bound-s.RootNest+1) {
				return fmt.Errorf("managed chain exceeded ceiling %d: accepted=%d highest_nest=%d", s.Bound, s.Accepted, s.MaxAccepted)
			}
			return nil
		},
		Next: func(s nestingState) []transition[nestingState] {
			var out []transition[nestingState]
			add := func(action string, n nestingState) { out = append(out, transition[nestingState]{action, n}) }
			switch s.Phase {
			case 0:
				// proc.Nest (internal/proc/child.go) normalizes invalid/negative
				// values to zero; nestCeiling (run.go) treats missing/malformed
				// ceilings alike and applies the manual-invocation fallback.
				for raw := -2; raw <= 3; raw++ {
					for depth := 0; depth <= 3; depth++ {
						for ceiling := -3; ceiling <= 4; ceiling++ {
							n := nestingState{Phase: 1, Nest: max(0, raw), Depth: depth, Ceiling: ceiling, Inherited: ceiling >= -1}
							add(fmt.Sprintf("initial nest=%d depth=%d ceiling=%d (-3 absent, -2 malformed)", raw, depth, ceiling), n)
						}
					}
				}
			case 1:
				// nestCeiling (run.go), budget.New (internal/budget/budget.go),
				// and childLimits/server.sharedLedger (acpserver.go) refuse
				// malformed grants or a negative remaining grant depth.
				n := s
				if s.Malformed || s.Depth < 0 || s.Inherited && s.Nest > s.Ceiling && !removeGuard || !s.Inherited && s.Nest > s.Depth+1 {
					n.Phase = 3
					add("refuse nesting/depth/grant before starting a run", n)
					break
				}
				ceiling := s.Nest + s.Depth
				if s.Inherited {
					ceiling = min(ceiling, s.Ceiling)
					n.Lowered = n.Lowered || ceiling < s.Ceiling
				}
				if s.Accepted == 0 {
					n.RootNest, n.Bound = s.Nest, ceiling
				}
				n.Ceiling, n.Inherited, n.Phase = ceiling, true, 2
				n.Accepted++
				n.MaxAccepted = s.Nest
				add("nestCeiling admits run and passes min(own, inherited) ceiling", n)
			case 2:
				// proc.Start (internal/proc/child.go) increments REFRAKTAL_NEST;
				// childEnv/runEnv.child (run.go/fanout.go) preserve the ceiling;
				// Ledger.ReserveShare (internal/budget/budget.go) decrements depth;
				// childLimits (acpserver.go) intersects it with the child's flags.
				for flags := 0; flags <= 3; flags++ {
					n := s
					n.Phase, n.Nest, n.Depth = 1, s.Nest+1, min(flags, s.Depth-1)
					add(fmt.Sprintf("launch managed child flags_depth=%d grant_depth=%d", flags, s.Depth-1), n)
					// childLimits (acpserver.go): with no REFRAKTAL_BUDGET,
					// flags start afresh; the independent ceiling still applies.
					n.Depth, n.LostGrant = flags, true
					add(fmt.Sprintf("launch child with lost grant and reset flags_depth=%d", flags), n)
				}
				// childLimits (acpserver.go) rejects malformed grant JSON.
				n := s
				n.Phase, n.Nest, n.Malformed = 1, s.Nest+1, true
				add("launch child with malformed parent grant", n)
			}
			return out
		},
	}
}

func TestNesting(t *testing.T) {
	m := nestingModel(false)
	var deepest, refused, lost, lowered, malformed bool
	next := m.Next
	m.Next = func(s nestingState) []transition[nestingState] {
		deepest = deepest || s.Phase == 2 && s.Accepted == 4
		refused = refused || s.Phase == 3 && s.Nest > s.Bound && s.Accepted > 0
		lost = lost || s.Phase == 2 && s.Accepted > 1 && s.LostGrant
		lowered = lowered || s.Phase == 2 && s.Accepted > 1 && s.Lowered
		malformed = malformed || s.Phase == 3 && s.Malformed
		return next(s)
	}
	r := check(m)
	t.Log(r.String())
	if r.Violation != nil {
		t.Fatal(r.String())
	}
	if !deepest || !refused || !lost || !lowered || !malformed {
		t.Fatalf("missing witness: deepest=%t refused=%t lost=%t lowered=%t malformed=%t", deepest, refused, lost, lowered, malformed)
	}
}

func TestNestingMutation(t *testing.T) {
	m := nestingModel(true)
	// Start at a valid top run. Exceeding this bound requires a lost/reset
	// grant; ordinary decrementing grants still stop at negative depth.
	m.Initial = nestingState{Phase: 1, Depth: 1}
	r := check(m)
	t.Log(r.String())
	if r.Violation == nil || !strings.HasPrefix(r.Violation.Error(), "managed chain exceeded ceiling ") {
		t.Fatalf("removing inherited ceiling guard missed its intended counterexample: %s", r.String())
	}
	if !r.Trace[len(r.Trace)-1].State.LostGrant {
		t.Fatal("mutation did not exercise the independent guard after grant loss")
	}
}
