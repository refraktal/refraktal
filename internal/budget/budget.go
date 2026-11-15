// Package budget enforces Refraktal's four budgets, --usd, --turns, --mins
// and --depth, with reservation accounting.
//
//   - A child is granted at most what is left: a live child holds its whole
//     grant, and a finished child returns what it did not spend.
//   - --usd is checked before each turn, never inside one.
//   - --turns counts prompts sent to harnesses.
//   - --mins is wall-clock time from the start of the run.
//   - --depth is how many more levels of Refraktal may nest below this one;
//     each child gets one less.
//   - A cancelled child keeps its whole reservation for enforcement; for
//     reporting, its charges are recorded as they are.
package budget

import (
	"fmt"
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
	g     Grant
	spent float64
	open  bool
}

// Ledger tracks one run's budgets.
type Ledger struct {
	mu       sync.Mutex
	lim      Limits
	deadline time.Time
	now      func() time.Time
	spent    float64 // settled, for enforcement
	reserved float64 // held by live grants
	turns    int
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
	case l.USD < 0 || l.Turns < 0 || l.Wall < 0:
		return nil, fmt.Errorf("budgets must not be negative")
	case l.Wall == 0:
		return nil, fmt.Errorf("--mins must be more than 0: it is never unlimited")
	case l.Depth < 0:
		return nil, &Exhausted{"depth"}
	}
	return &Ledger{lim: l, deadline: now().Add(l.Wall), now: now, grants: map[int]*grant{}}, nil
}

// Deadline is when --mins runs out.
func (l *Ledger) Deadline() time.Time { return l.deadline }

// Reserve grants a child everything that is left.
func (l *Ledger) Reserve() (Grant, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	usd := l.lim.USD - l.spent - l.reserved
	switch {
	case !l.now().Before(l.deadline):
		return Grant{}, &Exhausted{"mins"}
	case l.turns >= l.lim.Turns:
		return Grant{}, &Exhausted{"turns"}
	case usd <= 0:
		return Grant{}, &Exhausted{"usd"}
	}
	l.nextID++
	g := Grant{ID: l.nextID, USD: usd, Turns: l.lim.Turns - l.turns, Deadline: l.deadline, Depth: l.lim.Depth - 1}
	l.grants[g.ID] = &grant{g: g, open: true}
	l.reserved += usd
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
	case l.turns >= l.lim.Turns:
		return &Exhausted{"turns"}
	case st.spent >= g.USD:
		return &Exhausted{"usd"}
	}
	l.turns++
	return nil
}

// Charge records a figure for a child's turn. Estimated figures are for
// reporting only.
func (l *Ledger) Charge(g Grant, c Charge) {
	l.mu.Lock()
	defer l.mu.Unlock()
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
	Spent     float64  `json:"spent_usd"`     // charged against --usd, including kept reservations
	Reported  float64  `json:"reported_usd"`  // sum of reported and computed charges
	Estimated float64  `json:"estimated_usd"` // sum of estimated charges, reporting only
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
		default:
			s.Reported += c.USD
		}
	}
	return s
}
