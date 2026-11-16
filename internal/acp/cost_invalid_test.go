package acp

import "testing"

// Run-5 #4: a cost field that is present but cannot be applied is marked
// CostInvalid, so a caller can tell it from an absent cost.
func TestCostInvalidMarksAPresentUnusableCost(t *testing.T) {
	for _, tc := range []struct {
		update      string
		invalid     bool
		appliedCost bool
	}{
		{`{"sessionUpdate":"usage_update","cost":{"amount":0.5,"currency":"USD"}}`, false, true},
		{`{"sessionUpdate":"usage_update","used":5}`, false, false},
		{`{"sessionUpdate":"usage_update","cost":null}`, false, false},
		{`{"sessionUpdate":"usage_update","cost":{"amount":-1,"currency":"USD"}}`, true, false},
		{`{"sessionUpdate":"usage_update","cost":{"amount":"malformed","currency":"USD"}}`, true, false},
		{`{"sessionUpdate":"usage_update","cost":"malformed"}`, true, false},
		{`{"sessionUpdate":"usage_update","cost":{"currency":"USD"}}`, true, false},
		{`{"sessionUpdate":"usage_update","cost":{"amount":1e999,"currency":"USD"}}`, true, false},
		// All or nothing: a malformed used drops a well-formed cost too.
		{`{"sessionUpdate":"usage_update","used":-1,"cost":{"amount":0.5,"currency":"USD"}}`, true, false},
	} {
		u := parseUpdate([]byte(`{"sessionId":"s","update":` + tc.update + `}`))
		if u.Kind != "usage_update" || u.CostInvalid != tc.invalid || (u.Cost != nil) != tc.appliedCost {
			t.Errorf("%s: kind %q CostInvalid %t cost %v, want invalid %t applied %t", tc.update, u.Kind, u.CostInvalid, u.Cost, tc.invalid, tc.appliedCost)
		}
	}
}
