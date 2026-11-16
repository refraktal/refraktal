package verify

import (
	"fmt"
	"os"
	"testing"
	"time"

	"refraktal.dev/refraktal/internal/budget"
)

// The ledger model uses two parallel grants, four turns and four integer USD
// units. A third (judge) grant may reserve returned resources after both finish.
// Turn costs are 0, 1 or 3 units; child 0 reports cumulative Claude cost and the
// others report per-turn cost. Each scalar represents an exact integer, avoiding
// floating-point key ambiguity. This is a bounded check, not a proof for arbitrary
// fan-out, arbitrary costs or floating-point rounding. The main exploration
// assumes truthful child reports within the passed turn allowance. A separate
// faithful finding exploration admits an oversized ChildReport.Turns, which
// acp.Client.Prompt (internal/acp/client.go) accepts without a grant-bound check.
// Claude reports are assumed cumulative and nondecreasing; decreasing reports
// reset sessionCost.prev in fanout.go and are outside this provider contract.
//
// There is no finite absolute USD overrun bound in the product: a harness reports
// only after a turn and that turn can cost any amount. The exact exception is
// O_i=max(0, pre-final-turn spend + final-turn cost - grant_i). Each child can
// cross its grant only once; spent+reserved <= limit+sum(settled O_i), and live
// exposure <= limit+sum(all O_i). With these integer bounds, O_i <= 3-1=2.
const (
	ledgerUSD   = 4
	ledgerTurns = 4
	ledgerCost  = 3
)

type ledgerChild struct {
	Phase                               uint8 // 0 unreserved, 1 ready, 2 in turn, 3 finished, 4 settled
	USD, Allowance, Turns, Cost, Actual int
	Previous, Last, Before              int
	Cancelled, Estimated, Retried       bool
	Settlements, Enforced               int
}

type ledgerState struct {
	Children                     [3]ledgerChild
	Spent, Reserved, Turns, Held int
	Started                      int
}

type ledgerMutation uint8

const (
	ledgerFaithful ledgerMutation = iota
	ledgerNoReservation
	ledgerNoTurnGuard
	ledgerNoCancelRetention
	ledgerNoSettlementGuard
	ledgerNoCumulativeDelta
	ledgerUntrustedReport // faithful input accepted by the ACP parser, not a mutation
)

func ledgerModel(mutation ledgerMutation) model[ledgerState] {
	return model[ledgerState]{
		Name: "budget ledger",
		Key:  func(s ledgerState) string { return fmt.Sprintf("%v", s) },
		Next: func(s ledgerState) []transition[ledgerState] {
			var out []transition[ledgerState]
			add := func(action string, n ledgerState) {
				out = append(out, transition[ledgerState]{Action: action, State: n})
			}
			for i, c := range s.Children {
				if c.Phase == 0 && (i < 2 || s.Children[0].Phase == 4 && s.Children[1].Phase == 4) {
					// Ledger.ReserveShare (internal/budget/budget.go) and runEnv.fanOut
					// (fanout.go): counting remaining candidates and reserving are atomic.
					available := ledgerUSD - s.Spent - s.Reserved
					if mutation == ledgerNoReservation {
						available = ledgerUSD - s.Spent
					}
					if available > 0 && s.Turns+s.Held < ledgerTurns {
						n := s
						share := 1
						if i < 2 {
							share = 2 - s.Started
							n.Started++
						}
						g := &n.Children[i]
						g.Phase, g.USD = 1, available/share
						g.Allowance = max(1, (ledgerTurns-s.Turns-s.Held)/share)
						n.Reserved += g.USD
						n.Held += g.Allowance
						add(fmt.Sprintf("reserve child %d share %d", i, share), n)
					}
				}
				if c.Phase == 1 && !c.Cancelled && c.Cost < c.USD && s.Spent+c.Cost < ledgerUSD {
					// Ledger.BeginTurn (internal/budget/budget.go): grant and run turn
					// limits are checked in the same locked step that consumes a turn.
					if mutation == ledgerNoTurnGuard || c.Turns < c.Allowance && s.Turns < ledgerTurns {
						n := s
						g := &n.Children[i]
						g.Phase, g.Before = 2, g.Cost
						g.Turns++
						n.Turns++
						n.Held--
						add(fmt.Sprintf("begin turn child %d", i), n)
					}
				}
				if c.Phase == 2 {
					for _, cost := range []int{0, 1, ledgerCost} {
						// sessionCost.charges (fanout.go), charges (run.go), and
						// Ledger.Charge (internal/budget/budget.go): cumulative reported
						// USD becomes a nonnegative delta; computed USD is per turn.
						n := s
						g := &n.Children[i]
						g.Actual += cost
						charged := cost
						if i == 0 {
							total := g.Previous + cost
							charged = max(0, total-g.Previous)
							if mutation == ledgerNoCumulativeDelta {
								charged = total
							}
							g.Previous = total
						}
						g.Cost, g.Last, g.Phase = g.Cost+charged, cost, 1
						add(fmt.Sprintf("charge child %d cost %d", i, cost), n)
					}
				}
				if (c.Phase == 1 || c.Phase == 2) && !c.Cancelled {
					// runEnv.child (fanout.go): a sent session/cancel or cancelled
					// stop reason retains the grant, including already reported cost.
					n := s
					n.Children[i].Cancelled = true
					add(fmt.Sprintf("cancel child %d", i), n)
				}
				if c.Phase == 1 && c.Cancelled && !c.Estimated {
					// charges (run.go) and Ledger.Charge (internal/budget/budget.go):
					// cancellation estimates are report-only, not enforcement spend.
					n := s
					n.Children[i].Estimated = true
					add(fmt.Sprintf("report estimate child %d", i), n)
				}
				if c.Phase == 1 && !c.Cancelled && c.Turns > 0 && c.Turns < c.Allowance {
					// runEnv.child (fanout.go) passes grant.Turns-1 to Refraktal;
					// Ledger.AddTurns (internal/budget/budget.go) counts a truthful
					// descendant report within the remaining allowance, once here.
					n := s
					n.Children[i].Turns++
					n.Turns++
					n.Held--
					add(fmt.Sprintf("count nested prompt child %d", i), n)
				}
				if mutation == ledgerUntrustedReport && c.Phase == 1 && c.Turns > 0 {
					// Client.Prompt (internal/acp/client.go) accepts ChildReport.Turns
					// without checking the grant. runEnv.child (fanout.go) passes it
					// to Ledger.AddTurns (internal/budget/budget.go), even past limits.
					n := s
					n.Children[i].Turns += ledgerTurns
					n.Turns += ledgerTurns
					n.Held -= max(0, c.Allowance-c.Turns)
					add(fmt.Sprintf("accept oversized nested report child %d turns %d", i, ledgerTurns), n)
				}
				if c.Phase == 1 {
					// runEnv.child (fanout.go): the prompt/session is finished before
					// its grant is settled; startup failure may use zero turns.
					n := s
					n.Children[i].Phase = 3
					add(fmt.Sprintf("finish child %d", i), n)
				}
				if c.Phase == 3 || c.Phase == 4 && !c.Retried {
					// Ledger.Settle (internal/budget/budget.go) closes once, retains
					// max(grant, cost) on cancellation and returns unused resources.
					n := s
					g := &n.Children[i]
					if c.Phase == 4 {
						g.Retried = true
					}
					if c.Phase == 3 || mutation == ledgerNoSettlementGuard {
						g.Phase, g.Settlements = 4, g.Settlements+1
						g.Enforced = g.Cost
						if c.Cancelled && mutation != ledgerNoCancelRetention {
							g.Enforced = max(g.USD, g.Cost)
						}
						n.Spent += g.Enforced
						n.Reserved -= g.USD
						n.Held -= max(0, g.Allowance-g.Turns)
					}
					add(fmt.Sprintf("settle child %d", i), n)
				}
			}
			return out
		},
		Invariant: func(s ledgerState) error {
			if s.Turns > ledgerTurns || s.Held < 0 || s.Turns+s.Held > ledgerTurns {
				return fmt.Errorf("turn budget violated: used=%d held=%d limit=%d", s.Turns, s.Held, ledgerTurns)
			}
			overrun, settledOverrun, exposure, reserved, spent, turns, held := 0, 0, s.Spent, 0, 0, 0, 0
			for i, c := range s.Children {
				if c.Cost != c.Actual {
					return fmt.Errorf("child %d cumulative cost charged %d, actual %d", i, c.Cost, c.Actual)
				}
				if c.Turns > c.Allowance {
					return fmt.Errorf("child %d exceeded its turn grant", i)
				}
				if c.Settlements > 1 || c.Phase == 4 && c.Settlements != 1 {
					return fmt.Errorf("child %d grant settled %d times", i, c.Settlements)
				}
				if c.Cancelled && c.Phase == 4 && c.Enforced < c.USD {
					return fmt.Errorf("cancelled child %d returned part of grant %d", i, c.USD)
				}
				o := max(0, c.Cost-c.USD)
				if o > 0 && (c.Before >= c.USD || o != c.Before+c.Last-c.USD || o > ledgerCost-1) {
					return fmt.Errorf("child %d overrun %d is not exactly its final turn crossing", i, o)
				}
				overrun += o
				turns += c.Turns
				if c.Phase == 4 {
					settledOverrun += o
					spent += c.Enforced
				} else if c.Phase != 0 {
					reserved += c.USD
					exposure += max(c.USD, c.Cost)
					held += max(0, c.Allowance-c.Turns)
				}
			}
			if s.Reserved != reserved || s.Spent != spent || s.Turns != turns || s.Held != held {
				return fmt.Errorf("ledger totals disagree with grants")
			}
			if s.Spent+s.Reserved > ledgerUSD+settledOverrun || exposure > ledgerUSD+overrun {
				return fmt.Errorf("USD reservation/exposure exceeds limit plus exact overruns")
			}
			return nil
		},
	}
}

func TestBudgetLedger(t *testing.T) {
	m := ledgerModel(ledgerFaithful)
	next := m.Next
	witnesses := map[string]bool{}
	m.Next = func(s ledgerState) []transition[ledgerState] {
		out := next(s)
		live := 0
		for _, c := range s.Children {
			if c.Phase > 0 && c.Phase < 4 {
				live++
			}
			witnesses["overrun"] = witnesses["overrun"] || c.Phase == 4 && c.Cost > c.USD
			witnesses["cancel-retained"] = witnesses["cancel-retained"] || c.Phase == 4 && c.Cancelled && c.Cost < c.USD && c.Enforced == c.USD
			if len(out) == 0 && c.Phase > 0 && (c.Phase != 4 || c.Settlements != 1) {
				t.Errorf("terminal state left a grant unsettled: %+v", s)
			}
		}
		witnesses["parallel"] = witnesses["parallel"] || live >= 2
		witnesses["judge-reuses-returned"] = witnesses["judge-reuses-returned"] || s.Children[2].Phase > 0
		witnesses["cumulative-second-turn"] = witnesses["cumulative-second-turn"] || s.Children[0].Turns == 2 && s.Children[0].Before > 0 && s.Children[0].Cost > s.Children[0].Before
		witnesses["terminal"] = witnesses["terminal"] || len(out) == 0
		return out
	}
	r := check(m)
	t.Log(r.String())
	if r.Violation != nil {
		t.Fatal(r.String())
	}
	for _, name := range []string{"overrun", "cancel-retained", "parallel", "judge-reuses-returned", "cumulative-second-turn", "terminal"} {
		if !witnesses[name] {
			t.Errorf("missing reachable witness %q", name)
		}
	}
}

// This does not classify a real oversized report as a guard-removal mutation.
// It preserves the faithful counterexample alongside the honest-child bounds.
func TestBudgetUntrustedTurnsFinding(t *testing.T) {
	r := check(ledgerModel(ledgerUntrustedReport))
	t.Log(r.String())
	if r.Violation == nil {
		t.Fatal("known accepted-input finding no longer has a counterexample")
	}
}

func TestBudgetLedgerMutations(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind ledgerMutation
	}{
		{"ignore-held-dollars", ledgerNoReservation},
		{"remove-turn-guards", ledgerNoTurnGuard},
		{"return-cancelled-reservation", ledgerNoCancelRetention},
		{"settle-twice", ledgerNoSettlementGuard},
		{"charge-cumulative-total", ledgerNoCumulativeDelta},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := check(ledgerModel(tc.kind))
			t.Log(r.String())
			if r.Violation == nil {
				t.Fatal("mutation produced no counterexample")
			}
		})
	}
}

// This source-level witness exercises the exported ledger with exactly the
// model's two parallel shares, cumulative deltas, a one-turn overrun, cancelled
// retention and a repeated settlement. It also anchors the model's unit scale.
func TestBudgetLedgerRealWitness(t *testing.T) {
	now := time.Unix(1000, 0)
	l, err := budget.New(budget.Limits{USD: ledgerUSD, Turns: ledgerTurns, Wall: time.Minute}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	a, err := l.ReserveShare(2)
	if err != nil {
		t.Fatal(err)
	}
	b, err := l.ReserveShare(1)
	if err != nil || a.USD != 2 || b.USD != 2 || a.Turns+b.Turns != ledgerTurns {
		t.Fatalf("parallel shares: a=%+v b=%+v err=%v", a, b, err)
	}
	for _, g := range []budget.Grant{a, b} {
		if err := l.BeginTurn(g); err != nil {
			t.Fatal(err)
		}
	}
	l.Charge(a, budget.Charge{USD: 1, Source: budget.Reported})
	if err := l.BeginTurn(a); err != nil {
		t.Fatal(err)
	}
	l.Charge(a, budget.Charge{USD: 4 - 1, Source: budget.Reported})
	l.Charge(b, budget.Charge{USD: 1, Source: budget.Estimated})
	if st := l.Settle(a, false); st.Spent != 4 || st.Overrun != 2 {
		t.Fatalf("exact final-turn overrun: %+v", st)
	}
	if st := l.Settle(b, true); st.Spent != b.USD || st.Returned != 0 {
		t.Fatalf("cancelled retention: %+v", st)
	}
	if st := l.Settle(a, false); st != (budget.Settlement{}) {
		t.Fatalf("repeated settlement: %+v", st)
	}
	if s := l.Summary(); s.Spent != 6 || s.Reported != 4 || s.Estimated != 1 || s.Turns != 3 {
		t.Fatalf("summary: %+v", s)
	}
	if err := l.BeginTurn(b); err == nil {
		t.Fatal("settled grant began a turn")
	}
}

// The parser accepts ChildReport.Turns without checking its grant; runEnv.child
// (fanout.go) forwards it to Ledger.AddTurns (internal/budget/budget.go). Keep the
// strict desired invariant as an opt-in failing reproduction until that protocol
// trust boundary is resolved. Counting an overreport is documented by AddTurns;
// clamping it would merely hide the report, not prevent already executed prompts.
func TestBudgetUntrustedTurnsFindingReproduction(t *testing.T) {
	if os.Getenv("REFRAKTAL_VERIFY_FINDINGS") != "1" {
		t.Skip("known finding; set REFRAKTAL_VERIFY_FINDINGS=1 to reproduce")
	}
	l, err := budget.New(budget.Limits{USD: ledgerUSD, Turns: ledgerTurns, Wall: time.Minute}, nil)
	if err != nil {
		t.Fatal(err)
	}
	g, err := l.ReserveShare(2)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.BeginTurn(g); err != nil {
		t.Fatal(err)
	}
	l.AddTurns(g, ledgerTurns)
	if s := l.Summary(); s.Turns > ledgerTurns {
		t.Fatalf("accepted nested report exceeded --turns: used=%d limit=%d child allowance=%d", s.Turns, ledgerTurns, g.Turns-1)
	}
}
