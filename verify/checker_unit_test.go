package verify

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func graphModel(edges map[int][]int, bad int, expanded map[int]int) model[int] {
	return model[int]{
		Name: "checker unit graph", Initial: 0,
		Key: func(s int) string { return fmt.Sprint(s) },
		Next: func(s int) []transition[int] {
			if expanded != nil {
				expanded[s]++
			}
			var out []transition[int]
			for _, n := range edges[s] {
				out = append(out, transition[int]{Action: fmt.Sprintf("%d -> %d", s, n), State: n})
			}
			return out
		},
		Invariant: func(s int) error {
			if s == bad {
				return errors.New("bad state")
			}
			return nil
		},
	}
}

func TestCheckerExhaustsAndDeduplicates(t *testing.T) {
	expanded := make(map[int]int)
	r := check(graphModel(map[int][]int{0: {0, 1, 2}, 1: {3}, 2: {3}, 3: {0, 4}, 4: {4}}, -1, expanded))
	if r.Violation != nil || r.States != 5 || r.MaxDepth != 3 || len(r.Trace) != 0 {
		t.Fatalf("unexpected result: %s", r.String())
	}
	if !reflect.DeepEqual(expanded, map[int]int{0: 1, 1: 1, 2: 1, 3: 1, 4: 1}) {
		t.Fatalf("each reachable state must expand once: %v", expanded)
	}
	for _, field := range []string{"states=5", "max_depth=3", "elapsed="} {
		if !strings.Contains(r.String(), field) {
			t.Fatalf("missing metric %q in %s", field, r.String())
		}
	}
}

func TestCheckerFindsShortestTrace(t *testing.T) {
	// DFS through the first edge would find 0,1,3,4,5. BFS must find
	// 0,2,5 even though that branch is listed second.
	r := check(graphModel(map[int][]int{0: {1, 2}, 1: {3}, 3: {4}, 4: {5}, 2: {5}}, 5, nil))
	if r.Violation == nil || r.MaxDepth != 2 || len(r.Trace) != 3 {
		t.Fatalf("unexpected result: %s", r.String())
	}
	got := []int{r.Trace[0].State, r.Trace[1].State, r.Trace[2].State}
	if !reflect.DeepEqual(got, []int{0, 2, 5}) {
		t.Fatalf("trace is not shortest: %v", got)
	}
	for _, numbered := range []string{"0. initial", "1. 0 -> 2", "2. 2 -> 5"} {
		if !strings.Contains(r.String(), numbered) {
			t.Fatalf("missing numbered step %q in %s", numbered, r.String())
		}
	}
}

func TestCheckerChecksInitialState(t *testing.T) {
	expanded := make(map[int]int)
	r := check(graphModel(map[int][]int{0: {1}}, 0, expanded))
	if r.Violation == nil || r.States != 1 || r.MaxDepth != 0 || len(r.Trace) != 1 || len(expanded) != 0 {
		t.Fatalf("initial violation was not returned immediately: %s", r.String())
	}
}
