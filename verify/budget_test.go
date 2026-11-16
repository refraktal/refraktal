package verify

import (
	"fmt"
	"strings"
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
// models reports admitted by runEnv.child (fanout.go). A separate exhaustive
// report-boundary model explores valid and invalid inputs before admission,
// including Ledger.AddTurns/Forfeit (internal/budget/budget.go).
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
	ledgerUntrustedReport // pre-fix: accept a report without validating the allowance
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
					// MUTATION of runEnv.child (fanout.go) and Ledger.AddTurns
					// (internal/budget/budget.go): accept the pre-fix oversized
					// report instead of rejecting it and forfeiting the grant.
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
			for i, c := range s.Children {
				if c.Settlements > 1 || c.Phase == 4 && c.Settlements != 1 {
					return fmt.Errorf("child %d grant settled %d times", i, c.Settlements)
				}
			}
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

func TestBudgetLedgerMutations(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind ledgerMutation
		want string
	}{
		{"ignore-held-dollars", ledgerNoReservation, "USD reservation/exposure"},
		{"remove-turn-guards", ledgerNoTurnGuard, "turn budget violated"},
		{"return-cancelled-reservation", ledgerNoCancelRetention, "cancelled child"},
		{"settle-twice", ledgerNoSettlementGuard, "grant settled 2 times"},
		{"charge-cumulative-total", ledgerNoCumulativeDelta, "cumulative cost charged"},
		{"unvalidated-child-report", ledgerUntrustedReport, "turn budget violated: used=5 held=0 limit=4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := check(ledgerModel(tc.kind))
			t.Log(r.String())
			if r.Violation == nil || !strings.Contains(r.Violation.Error(), tc.want) {
				t.Fatalf("mutation did not reproduce %q: %s", tc.want, r.String())
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

// runEnv.child (fanout.go) forwards validated ChildReport.Turns to
// Ledger.AddTurns (internal/budget/budget.go), which independently refuses a
// report outside 0..the grant's remaining allowance and forfeits
// the whole grant instead. This reproduction is enforced: --turns can no longer
// be exceeded by a report.
func TestBudgetUntrustedTurnsFindingReproduction(t *testing.T) {
	for _, turns := range []int{-1, ledgerTurns} {
		for _, cost := range []float64{1, 3} {
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
			l.Charge(g, budget.Charge{USD: cost, Source: budget.Reported})
			if err := l.AddTurns(g, turns); err == nil {
				t.Fatalf("accepted untrusted nested turn count %d", turns)
			}
			wantForfeited := max(0, g.USD-cost)
			for range 2 {
				if got := l.Forfeit(g, "untrusted nested report"); got != wantForfeited {
					t.Fatalf("forfeit=%v want=%v", got, wantForfeited)
				}
			}
			if err := l.BeginTurn(g); err == nil {
				t.Fatal("forfeited grant began another turn")
			}
			if st := l.Settle(g, false); st.Spent != max(g.USD, cost) || st.Returned != 0 {
				t.Fatalf("untrusted report did not retain full grant and real cost: %+v", st)
			}
			if s := l.Summary(); s.Turns != g.Turns || s.Reported != max(g.USD, cost) {
				t.Fatalf("forfeiture failed: summary=%+v allowance=%d", s, g.Turns)
			}
		}
	}
}

// The response boundary is explored separately from concurrent reservations:
// USD and turn grants 1..4, report turns -1..5, finite costs {0,1,3,6},
// retained amounts {0,1,3,5}, and negative/NaN/infinite/non-USD cost classes,
// plus the wire forms acp.parseUpdate sees: a non-numeric or non-object cost
// and an absent cost (priced from tokens, abstracted here as $0), and the
// forms acp.decodePromptResult sees: a decodable report, a report with a
// field it cannot decode (a number outside the field's range, a wrong type,
// not an object) and an undecodable usage beside a report. A refraktal child
// may also end without a valid report at all: after end_turn, or after a
// failed turn (an ACP error, a crash). The parse step maps the wire classes
// to what runEnv.child sees.
// A child gets allowance-1 turns after the parent's prompt (runEnv.child,
// fanout.go; childLimits and server.prompt/finish, acpserver.go). Costs larger
// than the grant remain valid: final-turn overrun is never clamped. Classes
// encode invalid floating-point values without NaN state-key ambiguity.
// This is a bounded response-boundary check, not arbitrary wire/protocol input.
type reportState struct {
	Phase                                    uint8
	WireCost                                 int  // the cost class as sent; CostClass is what the parser hands on
	WireReport                               int  // a response class below
	NoReport                                 bool // the parse step handed on no report at all
	Kept                                     bool // whole grant kept without a forfeit outcome (a failed turn)
	USD, Allowance, ReportTurns              int
	CostClass, RetainedClass                 int
	Turns, Held, Charged, Retained           int
	ForfeitedUSD, Spent, Reserved            int
	Cancelled, Untrusted, Exhausted, Retried bool
}

// Cost classes: 0..3 finite amounts; 4 negative, 5 NaN, 6 infinite, 7 not
// USD, 8 not a number or not an object (on the wire only); 9 absent.
const (
	costNonNumeric = 8
	costAbsent     = 9
	costClasses    = 10
)

// Response classes: what the prompt response carries besides the cost.
const (
	reportDecodable   = 0
	reportUndecodable = 1 // _meta.refraktal present, a field not decodable
	usageUndecodable  = 2 // usage present and not decodable, beside a report
	noReportNormal    = 3 // a refraktal child ended its turn (end_turn) without a valid report
	noReportFailed    = 4 // a refraktal child's turn failed (ACP error, crash) without a report
	reportWires       = 5
)

func reportCost(class int) (int, bool) {
	switch class {
	case 0, 1, 2, 3:
		return [...]int{0, 1, 3, 6}[class], true
	case costAbsent: // no cost field: token pricing, abstracted as $0
		return 0, true
	default: // negative, NaN, infinity, wrong currency, non-numeric
		return 0, false
	}
}

type reportMutation string

const (
	reportFaithful reportMutation = ""
	// Pre-run-5 acp.parseUpdate: a negative or non-numeric cost was dropped
	// as if absent, so the child's report was priced from tokens.
	reportDropMalformedCost reportMutation = "parser drops a malformed cost as absent"
	// Pre-run-6 acp.Client.Prompt: one json.Unmarshal over the whole
	// response, so an undecodable report or usage failed it and the report
	// was read as absent: no validation, the unused grant returned.
	reportDropUndecodable reportMutation = "client reads an undecodable report as absent"
	// Pre-run-6 runEnv.child: a refraktal child that sent no report settled
	// like a plain harness and returned its unused grant.
	reportReturnUnusedGrant reportMutation = "child without a report returns its unused grant"
)

func reportRetained(class int) (int, bool) {
	switch class {
	case 0, 1, 2, 3:
		return [...]int{0, 1, 3, 5}[class], true
	default: // negative, NaN, infinity
		return 0, false
	}
}

func reportModel(mutation reportMutation) model[reportState] {
	return model[reportState]{
		Name: "budget child-report validation " + string(mutation),
		Key:  func(s reportState) string { return fmt.Sprintf("%v", s) },
		Next: func(s reportState) []transition[reportState] {
			var out []transition[reportState]
			add := func(action string, n reportState) {
				out = append(out, transition[reportState]{Action: action, State: n})
			}
			switch s.Phase {
			case 0:
				// Ledger.ReserveShare/BeginTurn (internal/budget/budget.go),
				// runEnv.child (fanout.go), server.prompt/finish (acpserver.go):
				// select a response after one parent prompt consumed a turn.
				for usd := 1; usd <= ledgerUSD; usd++ {
					for allowance := 1; allowance <= ledgerTurns; allowance++ {
						for turns := -1; turns <= ledgerTurns+1; turns++ {
							for costClass := 0; costClass < costClasses; costClass++ {
								for retainedClass := 0; retainedClass < 7; retainedClass++ {
									for wire := 0; wire < reportWires; wire++ {
										for _, cancelled := range []bool{false, true} {
											n := reportState{Phase: 1, USD: usd, Allowance: allowance, WireCost: costClass, WireReport: wire,
												ReportTurns: turns, CostClass: costClass, RetainedClass: retainedClass,
												Cancelled: cancelled, Turns: 1, Held: allowance - 1, Reserved: usd}
											action := "reserve, begin prompt and receive child response"
											if wire == noReportNormal || wire == noReportFailed {
												action = "reserve, begin prompt and child ends without report"
											}
											add(action, n)
										}
									}
								}
							}
						}
					}
				}
			case 1:
				// sessionCost.charges (fanout.go), charges/validCost (run.go),
				// Ledger.Charge (internal/budget/budget.go): real charges precede
				// forfeiture and keep the entire overrun. Invalid cost uses the
				// modeled unpriced fallback ($0); token pricing is abstracted.
				// acp.parseUpdate (internal/acp/types.go) hands a present but
				// unusable cost on as CostInvalid, never as absent.
				n := s
				n.Phase = 2
				if mutation == reportDropMalformedCost && (s.WireCost == 4 || s.WireCost == costNonNumeric) {
					n.CostClass = costAbsent
				}
				// acp.decodePromptResult (internal/acp/types.go) decodes the
				// response field by field and flags an undecodable report or
				// usage (Turn.ChildInvalid, Turn.UsageInvalid).
				if mutation == reportDropUndecodable && (s.WireReport == reportUndecodable || s.WireReport == usageUndecodable) {
					n.NoReport = true
				}
				if mutation == reportReturnUnusedGrant && (s.WireReport == noReportNormal || s.WireReport == noReportFailed) {
					n.NoReport = true
				}
				n.Charged, _ = reportCost(n.CostClass)
				add("parse the wire cost and response, then charge a valid cost in full before validating report", n)
			case 2:
				// runEnv.child (fanout.go), validCost (run.go), Ledger.AddTurns
				// and Ledger.forfeit (internal/budget/budget.go): validate first,
				// including the whole remaining turn allowance; bad counts
				// forfeit immediately without incrementing by the invalid value.
				n := s
				n.Phase = 3
				if s.NoReport {
					// No report: a plain harness's turn, nothing to validate.
					add("validate child cost, retained amount and turn count", n)
					break
				}
				switch s.WireReport {
				case noReportNormal:
					// runEnv.child (fanout.go): end_turn without a report is
					// untrusted; forfeiture follows.
					n.Untrusted = true
					add("validate child cost, retained amount and turn count", n)
					return out
				case noReportFailed:
					// runEnv.child (fanout.go) and Ledger.Forfeit: the turn
					// failed and stays failed, the whole grant is kept,
					// whatever its cost (an invalid one charged as $0).
					n.Kept = true
					n.ForfeitedUSD = max(0, s.USD-s.Charged)
					n.Charged += n.ForfeitedUSD
					n.Turns, n.Held = s.Allowance, 0
					add("validate child cost, retained amount and turn count", n)
					return out
				}
				_, validCost := reportCost(s.CostClass)
				retained, validRetained := reportRetained(s.RetainedClass)
				decodable := s.WireReport == reportDecodable
				n.Untrusted = !decodable || !validCost || !validRetained || retained > s.USD || s.ReportTurns < 0 || s.ReportTurns > s.Allowance-s.Turns
				if !n.Untrusted {
					n.Turns += s.ReportTurns
					n.Held -= s.ReportTurns
					n.Retained = retained
					n.Charged += retained
				} else if decodable && validCost && validRetained && retained <= s.USD {
					// Ledger.AddTurns invokes forfeit on an invalid count;
					// the later runEnv.child Forfeit must be idempotent.
					n.ForfeitedUSD = max(0, s.USD-s.Charged)
					n.Charged += n.ForfeitedUSD
					n.Turns, n.Held = s.Allowance, 0
				}
				add("validate child cost, retained amount and turn count", n)
			case 3:
				// runEnv.child (fanout.go) and Ledger.Forfeit/forfeit
				// (internal/budget/budget.go): every untrusted report consumes
				// the unused grant, marks exhaustion, and charges only once.
				n := s
				n.Phase = 4
				if s.Untrusted {
					n.ForfeitedUSD += max(0, s.USD-s.Charged)
					n.Charged = max(s.USD, s.Charged)
					n.Turns, n.Held, n.Exhausted = s.Allowance, 0, true
				}
				add("forfeit untrusted report and mark budget exhausted", n)
			case 4:
				// Ledger.Forfeit/forfeit (internal/budget/budget.go): a duplicate
				// call returns the prior forfeiture without charging it again.
				if !s.Retried {
					n := s
					n.Retried = true
					add("retry forfeiture without changing accounting", n)
				}
				// Ledger.Settle (internal/budget/budget.go), runEnv.child
				// (fanout.go): settle exactly once, retaining cancelled grants.
				n := s
				n.Phase, n.Reserved, n.Held = 5, 0, 0
				n.Spent = s.Charged
				if s.Cancelled {
					n.Spent = max(s.USD, s.Charged)
				}
				add("settle report after validation or forfeiture", n)
			}
			return out
		},
		Invariant: func(s reportState) error {
			if s.Phase == 0 {
				return nil
			}
			// Kept charges the whole grant, so nothing is returned on that cost.
			if _, ok := reportCost(s.WireCost); s.Phase >= 3 && !ok && !s.Untrusted && !s.Kept {
				return fmt.Errorf("a present but malformed cost was accepted as absent: wire class %d", s.WireCost)
			}
			if s.Phase >= 3 && (s.WireReport == reportUndecodable || s.WireReport == usageUndecodable) && !s.Untrusted {
				return fmt.Errorf("a present but undecodable report was read as absent: response class %d", s.WireReport)
			}
			if s.Phase >= 3 && (s.WireReport == noReportNormal || s.WireReport == noReportFailed) && !s.Untrusted && !s.Kept {
				return fmt.Errorf("a refraktal child without a report returned its unused grant: response class %d", s.WireReport)
			}
			if s.Kept && (s.Untrusted || s.Exhausted || s.Turns != s.Allowance || s.Charged < s.USD) {
				return fmt.Errorf("a failed child without a report did not keep its whole grant, or was marked exhausted")
			}
			if s.Turns < 1 || s.Turns > s.Allowance || s.Held < 0 || s.Phase < 5 && s.Turns+s.Held != s.Allowance {
				return fmt.Errorf("reported turns escaped grant: turns=%d held=%d allowance=%d", s.Turns, s.Held, s.Allowance)
			}
			cost, _ := reportCost(s.CostClass)
			if s.Phase >= 2 && s.Charged != cost+s.Retained+s.ForfeitedUSD {
				return fmt.Errorf("cost, retained and forfeiture accounting disagree")
			}
			if s.Phase >= 4 && s.Untrusted && (!s.Exhausted || s.Charged != max(s.USD, cost) || s.ForfeitedUSD != max(0, s.USD-cost) || s.Turns != s.Allowance || s.Retained != 0) {
				return fmt.Errorf("untrusted report did not forfeit its whole grant")
			}
			retained, _ := reportRetained(s.RetainedClass)
			if s.Phase >= 3 && !s.Untrusted && !s.Kept && (s.Turns != 1+s.ReportTurns || s.ForfeitedUSD != 0 || s.Retained != retained) {
				return fmt.Errorf("valid report was not counted exactly")
			}
			if s.Phase == 5 {
				want := s.Charged
				if s.Cancelled {
					want = max(s.USD, want)
				}
				if s.Spent != want || s.Reserved != 0 {
					return fmt.Errorf("settlement lost real costs, forfeiture or cancellation retention")
				}
			}
			return nil
		},
	}
}

func TestBudgetChildReportValidation(t *testing.T) {
	m := reportModel(reportFaithful)
	next := m.Next
	var valid, invalidTurns, invalidCost, invalidRetained, overrunForfeited, retained, nonNumeric, absent, undecodable, usage, noReportForfeit, noReportKept bool
	m.Next = func(s reportState) []transition[reportState] {
		if s.Phase == 5 {
			_, costOK := reportCost(s.CostClass)
			r, retainedOK := reportRetained(s.RetainedClass)
			valid = valid || !s.Untrusted && !s.Kept
			invalidTurns = invalidTurns || s.Untrusted && (s.ReportTurns < 0 || s.ReportTurns >= s.Allowance)
			invalidCost = invalidCost || s.Untrusted && !costOK
			invalidRetained = invalidRetained || s.Untrusted && (!retainedOK || r > s.USD)
			overrunForfeited = overrunForfeited || s.Untrusted && s.Charged > s.USD && s.ForfeitedUSD == 0
			retained = retained || !s.Untrusted && s.Retained > 0
			nonNumeric = nonNumeric || s.Untrusted && s.WireCost == costNonNumeric
			absent = absent || !s.Untrusted && s.WireCost == costAbsent
			// Undecodable alone: cost, retained amount and turns all in range.
			inRange := costOK && retainedOK && r <= s.USD && s.ReportTurns >= 0 && s.ReportTurns < s.Allowance
			undecodable = undecodable || s.Untrusted && s.Exhausted && s.ForfeitedUSD > 0 && s.WireReport == reportUndecodable && inRange
			usage = usage || s.Untrusted && s.WireReport == usageUndecodable
			noReportForfeit = noReportForfeit || s.Untrusted && s.Exhausted && s.WireReport == noReportNormal && costOK && s.ForfeitedUSD > 0
			noReportKept = noReportKept || s.Kept && !s.Exhausted && !s.Cancelled && s.WireReport == noReportFailed && s.Spent == max(s.USD, s.Charged) && s.ForfeitedUSD > 0
		}
		return next(s)
	}
	r := check(m)
	t.Log(r.String())
	if r.Violation != nil {
		t.Fatal(r.String())
	}
	if !valid || !invalidTurns || !invalidCost || !invalidRetained || !overrunForfeited || !retained || !nonNumeric || !absent || !undecodable || !usage || !noReportForfeit || !noReportKept {
		t.Fatalf("missing report witness: valid=%v invalidTurns=%v invalidCost=%v invalidRetained=%v overrunForfeited=%v retained=%v nonNumeric=%v absent=%v undecodable=%v usage=%v noReportForfeit=%v noReportKept=%v",
			valid, invalidTurns, invalidCost, invalidRetained, overrunForfeited, retained, nonNumeric, absent, undecodable, usage, noReportForfeit, noReportKept)
	}
}

// Run-5 #4: the old parser dropped a negative or non-numeric cost, so a
// nested report with one was accepted and priced from tokens.
func TestBudgetChildReportMalformedCostMutation(t *testing.T) {
	r := check(reportModel(reportDropMalformedCost))
	t.Log(r.String())
	if r.Violation == nil || !strings.Contains(r.Violation.Error(), "a present but malformed cost was accepted as absent") {
		t.Fatalf("want the malformed-cost counterexample: %s", r.String())
	}
}

// Run-6 #1: the old client decoded the prompt response in one step, so a
// child report with turns one past MaxInt64 (or any undecodable field, or an
// undecodable usage beside it) was read as absent and its grant returned.
func TestBudgetChildReportUndecodableMutation(t *testing.T) {
	r := check(reportModel(reportDropUndecodable))
	t.Log(r.String())
	if r.Violation == nil || !strings.Contains(r.Violation.Error(), "a present but undecodable report was read as absent") {
		t.Fatalf("want the undecodable-report counterexample: %s", r.String())
	}
}

// Run-6 #1 follow-up: a refraktal child that ended without a report (after
// end_turn, an ACP error or a crash) settled like a plain harness and
// returned its unused grant, though it may have spent it on its own
// harnesses.
func TestBudgetChildReportNoReportMutation(t *testing.T) {
	r := check(reportModel(reportReturnUnusedGrant))
	t.Log(r.String())
	if r.Violation == nil || !strings.Contains(r.Violation.Error(), "a refraktal child without a report returned its unused grant") {
		t.Fatalf("want the no-report counterexample: %s", r.String())
	}
}
