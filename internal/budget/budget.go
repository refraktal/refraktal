// Package budget enforces Refraktal's four budgets, --usd, --turns, --mins
// and --depth, with reservation accounting.
//
//   - A child is granted at most what is left: a live child holds its whole
//     grant, and a finished child returns what it did not spend.
//   - --usd is checked before each turn, never inside one, so each grant can
//     cross its share once, by its final turn's cost: spent + reserved <=
//     --usd + the sum of each grant's final-turn overrun (N parallel
//     children: up to N final turns), not one turn for the whole run.
//   - --turns counts prompts sent to harnesses.
//   - --mins is wall-clock time from the start of the run.
//   - --depth is how many more levels of Refraktal may nest below this one;
//     each child gets one less.
//   - A cancelled child keeps its whole reservation for enforcement; for
//     reporting, its charges are recorded as they are.
package budget

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
)

// Limits are a run's budgets.
type Limits struct {
	USD   float64       `json:"usd"`
	Turns int           `json:"turns"`
	Wall  time.Duration `json:"-"`
	Depth int           `json:"depth"`
}

// MaxWall is the longest --mins accepted.
const MaxWall = 24 * time.Hour

// Defaults: --mins always has one and is never unlimited.
var Defaults = Limits{USD: 10, Turns: 20, Wall: 30 * time.Minute, Depth: 2}

// Exhausted names the budget that ran out.
type Exhausted struct{ Budget string } // usd, turns, mins or depth

func (e *Exhausted) Error() string { return "budget exhausted: --" + e.Budget }

// Source says where a dollar figure came from.
type Source string

const (
	Reported  Source = "reported"  // the harness reported the cost
	Computed  Source = "computed"  // tokens priced from the price table
	Unpriced  Source = "unpriced"  // no price for the model: counted as $0
	Estimated Source = "estimated" // a guess for a call a cancel cut off; reporting only
	// Retained is a child Refraktal's reservation kept by its own cancelled
	// children: held against --usd, but not reported as spent.
	Retained Source = "retained"
	// Forfeited is the unspent rest of a grant charged to a child whose
	// report could not be trusted (a turn count or cost out of range).
	Forfeited Source = "forfeited"
)

// Charge is one dollar figure for a child's turn.
type Charge struct {
	USD    float64 `json:"usd"`
	Source Source  `json:"source"`
	Model  string  `json:"model,omitempty"`
	Note   string  `json:"note,omitempty"`
}

// Grant is what a child may use.
type Grant struct {
	ID       int       `json:"id"`
	USD      float64   `json:"usd"`
	Turns    int       `json:"turns"`
	Deadline time.Time `json:"deadline"`
	Depth    int       `json:"depth"` // for a child Refraktal: Limits.Depth - 1
}

// Settlement is how a finished child's grant was closed.
type Settlement struct {
	Spent    float64 `json:"spent"`    // charged against --usd
	Returned float64 `json:"returned"` // given back to the run
	Overrun  float64 `json:"overrun"`  // spent beyond the grant (a turn cannot be stopped for cost)
}

type grant struct {
	spent        float64
	turns        int // turns used
	open         bool
	forfeited    bool    // charged its whole grant (an untrusted report)
	forfeitedUSD float64 // the unspent rest charged then
}

// Ledger tracks one run's budgets.
type Ledger struct {
	mu       sync.Mutex
	lim      Limits
	deadline time.Time
	now      func() time.Time
	spent    float64 // settled, for enforcement
	reserved float64 // held by live grants
	turns    int     // turns used
	held     int     // turns reserved by live grants and not used yet
	nextID   int
	grants   map[int]*grant
	charges  []Charge
}

// New starts a ledger now. It fails if a limit is invalid or already spent.
func New(l Limits, now func() time.Time) (*Ledger, error) {
	if now == nil {
		now = time.Now
	}
	switch {
	case math.IsNaN(l.USD) || math.IsInf(l.USD, 0):
		return nil, fmt.Errorf("--usd must be a finite number")
	case l.Wall > MaxWall:
		return nil, fmt.Errorf("--mins must be at most %.0f (%s)", MaxWall.Minutes(), MaxWall)
	case l.USD < 0 || l.Turns < 0 || l.Wall < 0:
		return nil, fmt.Errorf("budgets must not be negative")
	case l.Wall == 0:
		return nil, fmt.Errorf("--mins must be more than 0: it is never unlimited")
	case l.Depth < 0:
		return nil, &Exhausted{"depth"}
	}
	return &Ledger{lim: l, deadline: now().Add(l.Wall), now: now, grants: map[int]*grant{}}, nil
}

// Limits are the ledger's limits.
func (l *Ledger) Limits() Limits { return l.lim }

// Deadline is when --mins runs out.
func (l *Ledger) Deadline() time.Time { return l.deadline }

// Reserve grants a child everything that is left.
func (l *Ledger) Reserve() (Grant, error) { return l.ReserveShare(1) }

// Check reports whether a child could be granted anything now.
func (l *Ledger) Check() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.check(l.lim.USD - l.spent - l.reserved)
}

func (l *Ledger) check(usd float64) error {
	switch {
	case !l.now().Before(l.deadline):
		return &Exhausted{"mins"}
	case usd <= 0:
		return &Exhausted{"usd"}
	case l.turns+l.held >= l.lim.Turns:
		return &Exhausted{"turns"}
	}
	return nil
}

// ReserveShare grants a child what is left divided by n, the children still
// to start (this one included), so children started in parallel split it:
// dollars, and a turn allowance (at least one) that no other grant can use.
// Unused dollars and turns return at settlement. --mins is not split: it
// belongs to the whole run.
func (l *Ledger) ReserveShare(n int) (Grant, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	usd := l.lim.USD - l.spent - l.reserved
	if err := l.check(usd); err != nil {
		return Grant{}, err
	}
	turns := l.lim.Turns - l.turns - l.held
	if n > 1 {
		usd /= float64(n)
		turns = max(1, turns/n)
	}
	l.nextID++
	g := Grant{ID: l.nextID, USD: usd, Turns: turns, Deadline: l.deadline, Depth: l.lim.Depth - 1}
	l.grants[g.ID] = &grant{open: true}
	l.reserved += usd
	l.held += turns
	return g, nil
}

// BeginTurn is called before each prompt: it checks the grant still has
// dollars, turns and time, then counts the turn.
func (l *Ledger) BeginTurn(g Grant) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.grants[g.ID]
	switch {
	case st == nil || !st.open:
		return fmt.Errorf("budget: grant %d is not open", g.ID)
	case !l.now().Before(g.Deadline):
		return &Exhausted{"mins"}
	case st.turns >= g.Turns || l.turns >= l.lim.Turns:
		return &Exhausted{"turns"}
	case st.spent >= g.USD:
		return &Exhausted{"usd"}
	case l.spent+st.spent >= l.lim.USD:
		// The run as a whole is spent (another child overran): a held
		// reservation does not keep this child going.
		return &Exhausted{"usd"}
	}
	l.turns++
	st.turns++
	l.held--
	return nil
}

// AddTurns counts prompts a child sent on its own (a child Refraktal's
// harness prompts), so --turns covers the whole tree. The report is
// untrusted: a count outside 0..the grant's remaining allowance (it was told
// that allowance) forfeits the whole grant (Forfeit) and returns Exhausted
// turns, so no count can wrap or reach past the grant.
func (l *Ledger) AddTurns(g Grant, n int) error {
	if n == 0 {
		return nil
	}
	l.mu.Lock()
	st := l.grants[g.ID]
	if st == nil || !st.open {
		l.mu.Unlock()
		return fmt.Errorf("budget: grant %d is not open", g.ID)
	}
	if n < 0 || n > g.Turns-st.turns {
		l.forfeit(g, st, fmt.Sprintf("a child reported %d turns, outside 0..%d", n, max(0, g.Turns-st.turns)))
		l.mu.Unlock()
		return &Exhausted{"turns"}
	}
	l.turns = satAdd(l.turns, n)
	st.turns += n
	l.held -= n // within the allowance: no longer held
	l.mu.Unlock()
	return nil
}

// Forfeit charges a child its whole grant: the dollars it has not spent (as
// a Forfeited charge) and the turns it has not used. It is for a child whose
// report cannot be trusted; the grant can start nothing more. Real charges
// are made first and stand in full (an overrun included): only the unspent
// rest is forfeited. It charges once; every call returns the dollars
// forfeited for the grant, whichever call (or AddTurns) forfeited it.
func (l *Ledger) Forfeit(g Grant, why string) float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	if st := l.grants[g.ID]; st != nil && st.open {
		return l.forfeit(g, st, why)
	}
	return 0
}

func (l *Ledger) forfeit(g Grant, st *grant, why string) (usd float64) {
	if st.forfeited {
		return st.forfeitedUSD
	}
	st.forfeited = true
	if left := g.USD - st.spent; left > 0 {
		l.charges = append(l.charges, Charge{USD: left, Source: Forfeited, Note: why})
		st.spent, st.forfeitedUSD, usd = g.USD, left, left
	}
	if left := g.Turns - st.turns; left > 0 {
		l.turns = satAdd(l.turns, left)
		l.held -= left
		st.turns = g.Turns
	}
	return usd
}

// satAdd adds two non-negative counts, saturating at math.MaxInt.
func satAdd(a, b int) int {
	if b > math.MaxInt-a {
		return math.MaxInt
	}
	return a + b
}

// Charge records a figure for a child's turn. Estimated figures are for
// reporting only.
func (l *Ledger) Charge(g Grant, c Charge) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if math.IsNaN(c.USD) || math.IsInf(c.USD, 0) || c.USD < 0 {
		// Never lower or poison spend: a bad figure counts as $0, flagged.
		c.Note = strings.TrimPrefix(c.Note+"; invalid amount counted as $0", "; ")
		c.USD = 0
	}
	l.charges = append(l.charges, c)
	if st := l.grants[g.ID]; st != nil && c.Source != Estimated {
		st.spent += c.USD
	}
}

// Settle closes a finished child's grant. A cancelled child keeps its whole
// reservation; any other returns what it did not spend.
func (l *Ledger) Settle(g Grant, cancelled bool) Settlement {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.grants[g.ID]
	if st == nil || !st.open {
		return Settlement{}
	}
	st.open = false
	l.reserved -= g.USD
	l.held -= max(0, g.Turns-st.turns) // unused turns return
	s := Settlement{Spent: st.spent}
	if cancelled && s.Spent < g.USD {
		s.Spent = g.USD
	}
	if s.Spent > g.USD {
		s.Overrun = s.Spent - g.USD
	} else {
		s.Returned = g.USD - s.Spent
	}
	l.spent += s.Spent
	return s
}

// Summary is the ledger's state for reports.
type Summary struct {
	Limits    Limits   `json:"limits"`
	Mins      float64  `json:"mins"`
	Spent     float64  `json:"spent_usd"`              // charged against --usd, including kept reservations
	Reported  float64  `json:"reported_usd"`           // sum of reported and computed charges
	Estimated float64  `json:"estimated_usd"`          // sum of estimated charges, reporting only
	Retained  float64  `json:"retained_usd,omitempty"` // held for children's kept reservations, not spent
	Turns     int      `json:"turns"`
	Charges   []Charge `json:"charges"`
	Unpriced  []string `json:"unpriced,omitempty"` // models that had no price
}

// Summary returns the ledger's state.
func (l *Ledger) Summary() Summary {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := Summary{Limits: l.lim, Mins: l.lim.Wall.Minutes(), Spent: l.spent, Turns: l.turns,
		Charges: append([]Charge{}, l.charges...)}
	for _, c := range l.charges {
		switch c.Source {
		case Estimated:
			s.Estimated += c.USD
		case Unpriced:
			s.Unpriced = append(s.Unpriced, c.Model)
		case Retained:
			s.Retained += c.USD
		default:
			s.Reported += c.USD
		}
	}
	return s
}
