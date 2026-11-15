// Command summarize reads a decision log on stdin and prints one TSV line per
// record. It exists so run-live.sh needs nothing but Go (no jq).
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"

	routing "refraktal.dev/spikes/routing"
)

func main() {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(nil, 4<<20)
	for sc.Scan() {
		var r routing.Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			fmt.Fprintln(os.Stderr, "summarize:", err)
			os.Exit(1)
		}
		tokens, cost := 0, 0.0
		if u := r.Decision.Usage; u != nil {
			tokens, cost = u.InputTokens, u.CostUSD
		}
		fmt.Printf("%s\t%s\t%.1f\t%d\t%.8f\t%s\t%s\n", r.ID, r.Decision.Decider, r.Decision.LatencyMS, tokens, cost, r.Outcome.Action, r.Decision.FallbackReason)
	}
}
