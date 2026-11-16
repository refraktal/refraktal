package refraktal

import (
	"fmt"
	"testing"
	"time"

	"refraktal.dev/refraktal/internal/budget"
	"refraktal.dev/refraktal/internal/proc"
)

// Source witnesses anchor the nesting model to real guards without starting
// harnesses, accessing the network, or changing any global configuration.
func TestVerifyNestingSourceWitness(t *testing.T) {
	for _, raw := range []string{"", "invalid", "-1", "0", "3"} {
		t.Setenv(proc.NestEnv, raw)
		want := 0
		if raw == "3" {
			want = 3
		}
		if got := proc.Nest(); got != want {
			t.Fatalf("Nest(%q)=%d want %d", raw, got, want)
		}
	}
	for _, tc := range []struct {
		ceiling           string
		nest, depth, want int
		refused           bool
	}{
		{"", 0, 3, 3, false}, {"invalid", 0, 3, 3, false},
		{"", 4, 2, 0, true}, {"", 3, 2, 5, false},
		{"3", 3, 0, 3, false}, {"3", 4, 3, 0, true},
		{"3", 1, 0, 1, false}, {"-1", 0, 3, 0, true},
	} {
		t.Setenv(NestCeilingEnv, tc.ceiling)
		got, err := nestCeiling(tc.nest, tc.depth)
		if (err != nil) != tc.refused || err == nil && got != tc.want {
			t.Fatalf("nestCeiling(%d,%d) inherited=%q: ceiling=%d err=%v", tc.nest, tc.depth, tc.ceiling, got, err)
		}
	}
	flags := budget.Limits{USD: 4, Turns: 4, Wall: time.Minute, Depth: 3}
	for depth := -1; depth <= 4; depth++ {
		lim, err := childLimits(flags, fmt.Sprintf(`{"depth":%d}`, depth), time.Unix(1000, 0))
		if err != nil || lim.Depth != min(flags.Depth, depth) {
			t.Fatalf("childLimits depth=%d: %+v %v", depth, lim, err)
		}
		_, err = budget.New(lim, nil)
		if (err != nil) != (depth < 0) {
			t.Fatalf("budget.New depth=%d: %v", depth, err)
		}
	}
	if lim, err := childLimits(flags, "", time.Unix(1000, 0)); err != nil || lim.Depth != flags.Depth {
		t.Fatalf("lost grant: %+v %v", lim, err)
	}
	if _, err := childLimits(flags, "{", time.Unix(1000, 0)); err == nil {
		t.Fatal("malformed grant accepted")
	}
}
