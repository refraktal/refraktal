package budget

import (
	"errors"
	"math"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func exhausted(err error, budget string) bool {
	var e *Exhausted
	return errors.As(err, &e) && e.Budget == budget
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestValidation(t *testing.T) {
	for _, l := range []Limits{{USD: -1, Turns: 1, Wall: time.Minute}, {USD: 1, Turns: -1, Wall: time.Minute}, {USD: 1, Turns: 1}} {
		if _, err := New(l, nil); err == nil {
			t.Errorf("%+v: want an error", l)
		}
	}
	if _, err := New(Limits{USD: 1, Turns: 1, Wall: time.Minute, Depth: -1}, nil); !exhausted(err, "depth") {
		t.Errorf("depth -1: %v", err)
	}
}

func TestEachBudget(t *testing.T) {
	c := &clock{time.Unix(1000, 0)}
	// --usd: nothing left means no grant.
	l, _ := New(Limits{USD: 0, Turns: 5, Wall: time.Minute}, c.now)
	if _, err := l.Reserve(); !exhausted(err, "usd") {
		t.Fatalf("usd 0: %v", err)
	}
	// --turns: counts prompts; the next one is refused.
	l, _ = New(Limits{USD: 1, Turns: 1, Wall: time.Minute}, c.now)
	g, err := l.Reserve()
	if err != nil || l.BeginTurn(g) != nil {
		t.Fatalf("first turn: %v", err)
	}
	if err := l.BeginTurn(g); !exhausted(err, "turns") {
		t.Fatalf("second turn: %v", err)
	}
	// --usd is checked before a turn: once the grant is spent, no new turn.
	l, _ = New(Limits{USD: 1, Turns: 5, Wall: time.Minute}, c.now)
	g, _ = l.Reserve()
	l.BeginTurn(g)
	l.Charge(g, Charge{USD: 1.2, Source: Reported})
	if err := l.BeginTurn(g); !exhausted(err, "usd") {
		t.Fatalf("after spending: %v", err)
	}
	if s := l.Settle(g, false); !near(s.Spent, 1.2) || !near(s.Overrun, 0.2) || s.Returned != 0 {
		t.Fatalf("overrun settlement %+v", s)
	}
	// --mins: wall clock from the start.
	l, _ = New(Limits{USD: 1, Turns: 5, Wall: time.Minute}, c.now)
	g, _ = l.Reserve()
	c.t = c.t.Add(time.Minute)
	if err := l.BeginTurn(g); !exhausted(err, "mins") {
		t.Fatalf("after the deadline: %v", err)
	}
	if _, err := l.Reserve(); !exhausted(err, "mins") {
		t.Fatalf("reserve after the deadline: %v", err)
	}
	// --depth: a child gets one less.
	l, _ = New(Limits{USD: 1, Turns: 1, Wall: time.Minute, Depth: 2}, time.Now)
	if g, _ := l.Reserve(); g.Depth != 1 {
		t.Fatalf("child depth %d", g.Depth)
	}
}

func TestReservationAccounting(t *testing.T) {
	l, _ := New(Limits{USD: 10, Turns: 10, Wall: time.Minute}, nil)
	a, _ := l.Reserve()
	if a.USD != 10 {
		t.Fatalf("first grant %v", a.USD)
	}
	if _, err := l.Reserve(); !exhausted(err, "usd") {
		t.Fatalf("a live child holds its whole grant: %v", err)
	}
	l.BeginTurn(a)
	l.Charge(a, Charge{USD: 0.75, Source: Computed, Model: "m"})
	l.Charge(a, Charge{USD: 3, Source: Estimated}) // reporting only
	s := l.Settle(a, false)
	if !near(s.Spent, 0.75) || !near(s.Returned, 9.25) {
		t.Fatalf("settlement %+v", s)
	}
	b, err := l.Reserve()
	if err != nil || !near(b.USD, 9.25) {
		t.Fatalf("returned dollars not available: %v %v", b.USD, err)
	}
	sum := l.Summary()
	if !near(sum.Spent, 0.75) || !near(sum.Reported, 0.75) || !near(sum.Estimated, 3) || sum.Turns != 1 {
		t.Fatalf("summary %+v", sum)
	}
}

// A cancelled child keeps its whole reservation for enforcement; its charges
// are still reported as they are.
func TestCancelledChildKeepsItsReservation(t *testing.T) {
	l, _ := New(Limits{USD: 4, Turns: 10, Wall: time.Minute}, nil)
	g, _ := l.Reserve()
	l.BeginTurn(g)
	l.Charge(g, Charge{USD: 0.5, Source: Computed, Model: "m"})
	l.Charge(g, Charge{USD: 0.1, Source: Estimated, Note: "last used priced as input"})
	s := l.Settle(g, true)
	if !near(s.Spent, 4) || s.Returned != 0 {
		t.Fatalf("settlement %+v", s)
	}
	if _, err := l.Reserve(); !exhausted(err, "usd") {
		t.Fatalf("after a cancelled child: %v", err)
	}
	sum := l.Summary()
	if !near(sum.Spent, 4) || !near(sum.Reported, 0.5) || !near(sum.Estimated, 0.1) {
		t.Fatalf("summary %+v", sum)
	}
}

func TestUnpricedCountsAsZero(t *testing.T) {
	l, _ := New(Limits{USD: 1, Turns: 3, Wall: time.Minute}, nil)
	g, _ := l.Reserve()
	l.BeginTurn(g)
	l.Charge(g, Charge{USD: 0, Source: Unpriced, Model: "mystery-model"})
	if err := l.BeginTurn(g); err != nil {
		t.Fatalf("an unpriced turn must not use up --usd: %v", err)
	}
	if sum := l.Summary(); len(sum.Unpriced) != 1 || sum.Unpriced[0] != "mystery-model" {
		t.Fatalf("summary %+v", sum)
	}
}
