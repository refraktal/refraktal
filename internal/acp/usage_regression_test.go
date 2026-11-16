package acp

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestUsageUpdateRejectsMissingOrNullCostAmount(t *testing.T) {
	for _, cost := range []string{`{}`, `{"currency":"USD"}`, `{"amount":null,"currency":"USD"}`} {
		t.Run(cost, func(t *testing.T) {
			c := fuzzClient()
			used := int64(77)
			previous := Cost{Amount: 3, Currency: "USD"}
			ts := &turnState{turn: Turn{LastUsed: &used, Cost: &previous}}
			c.turns["s"] = ts
			before := ts.turn
			var seen []Update
			c.opts.OnUpdate = func(u Update) { seen = append(seen, u) }
			params := json.RawMessage(`{"sessionId":"s","update":{"sessionUpdate":"usage_update","used":5,"size":100,"cost":` + cost + `}}`)
			c.notification(message{Method: "session/update", Params: params})
			if !reflect.DeepEqual(ts.turn, before) {
				t.Fatalf("invalid cost changed previous accounting: got %+v, want %+v", ts.turn, before)
			}
			if len(seen) != 1 || seen[0].Kind != "usage_update" || seen[0].Cost != nil || seen[0].Used != nil || seen[0].Size != nil {
				t.Fatalf("invalid accounting was applied or update was hidden: %+v", seen)
			}
		})
	}
}

func TestUsageUpdateCostAmountMustBeNumericAndNonnegative(t *testing.T) {
	for _, amount := range []string{`"0"`, `true`, `{}`, `[]`, `-1`, `1e9999`, `"NaN"`, `"Infinity"`} {
		t.Run(amount, func(t *testing.T) {
			u := parseUpdate(json.RawMessage(`{"sessionId":"s","update":{"sessionUpdate":"usage_update","used":5,"cost":{"amount":` + amount + `,"currency":"USD"}}}`))
			if u.Cost != nil || u.Used != nil {
				t.Fatalf("invalid cost accepted: %+v", u)
			}
		})
	}
	for _, amount := range []string{`0`, `0.125`, `1e2`} {
		t.Run(amount, func(t *testing.T) {
			u := parseUpdate(json.RawMessage(`{"sessionId":"s","update":{"sessionUpdate":"usage_update","used":5,"cost":{"amount":` + amount + `,"currency":"USD"}}}`))
			if u.Cost == nil || u.Used == nil || *u.Used != 5 {
				t.Fatalf("valid numeric cost rejected: %+v", u)
			}
		})
	}
}

func TestUsageUpdateOracleRejectsMissingOrNullCostAmount(t *testing.T) {
	for _, cost := range []string{`{}`, `{"currency":"USD"}`, `{"amount":null,"currency":"USD"}`} {
		_, _, _, valid := fuzzUpdate([]byte(`{"sessionId":"s","update":{"sessionUpdate":"usage_update","cost":` + cost + `}}`))
		if valid {
			t.Errorf("oracle accepted cost without a numeric amount: %s", cost)
		}
	}
}
