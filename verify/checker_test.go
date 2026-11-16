// Package verify contains bounded explicit-state checks. It has no non-test Go
// files, so neither the checker nor its models can enter a production build.
package verify

import (
	"fmt"
	"strings"
	"time"
)

type transition[S any] struct {
	Action string
	State  S
}

type model[S any] struct {
	Name      string
	Initial   S
	Key       func(S) string
	Next      func(S) []transition[S]
	Invariant func(S) error
}

type step[S any] struct {
	Action string
	State  S
}

type result[S any] struct {
	Name      string
	States    int
	MaxDepth  int
	Elapsed   time.Duration
	Violation error
	Trace     []step[S]
}

// check explores the entire reachable graph, stopping only at an invariant
// violation. There is no search-depth or state-count cutoff. Model bounds must
// make the graph finite; a bound must never silently truncate the search.
//
// Key is the model's canonical state serialization, not a hash. Equal keys must
// mean equal invariant results and equivalent future behavior. Every field that
// affects either belongs in the key. The checker cannot detect an unsound key.
// Model values must also be immutable: Next must not mutate an earlier state
// through shared pointers, slices, or maps. Value structs and arrays are simplest.
//
// The FIFO frontier and first-discovery predecessor give a shortest trace by
// transition count. MaxDepth is the maximum shortest-path depth discovered;
// with a violation this describes the explored prefix, not the whole graph.
func check[S any](m model[S]) (r result[S]) {
	started := time.Now()
	defer func() { r.Elapsed = time.Since(started) }()
	r.Name = m.Name
	type node struct {
		state  S
		parent int
		action string
		depth  int
	}
	nodes := []node{{state: m.Initial, parent: -1, action: "initial"}}
	seen := map[string]struct{}{m.Key(m.Initial): {}}
	r.States = 1
	trace := func(index int) []step[S] {
		out := make([]step[S], nodes[index].depth+1)
		for index >= 0 {
			n := nodes[index]
			out[n.depth] = step[S]{Action: n.action, State: n.state}
			index = n.parent
		}
		return out
	}
	if err := m.Invariant(m.Initial); err != nil {
		r.Violation, r.Trace = err, trace(0)
		return r
	}
	for head := 0; head < len(nodes); head++ {
		current := nodes[head]
		for _, next := range m.Next(current.state) {
			key := m.Key(next.State)
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			depth := current.depth + 1
			nodes = append(nodes, node{state: next.State, parent: head, action: next.Action, depth: depth})
			r.States++
			if depth > r.MaxDepth {
				r.MaxDepth = depth
			}
			if err := m.Invariant(next.State); err != nil {
				r.Violation, r.Trace = err, trace(len(nodes)-1)
				return r
			}
		}
	}
	return r
}

func (r result[S]) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: states=%d max_depth=%d elapsed=%s", r.Name, r.States, r.MaxDepth, r.Elapsed)
	if r.Violation != nil {
		fmt.Fprintf(&b, "\ninvariant violated: %s\nshortest trace:", r.Violation)
		for i, s := range r.Trace {
			fmt.Fprintf(&b, "\n%d. %s: %+v", i, s.Action, s.State)
		}
	}
	return b.String()
}
