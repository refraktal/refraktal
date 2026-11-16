package acp

import (
	"strings"
	"testing"
)

// Run-6 #1: a prompt response is decoded field by field. A usage or
// _meta.refraktal that is present but undecodable is flagged, never read as
// absent, and never hides the fields that did decode.
func TestDecodePromptResultFlagsAPresentUndecodableReport(t *testing.T) {
	for _, tc := range []struct {
		res                 string
		child, childInvalid bool
		usage, usageInvalid bool
		stop, errSubstr     string
	}{
		{res: `{"stopReason":"end_turn","usage":{"inputTokens":3},"_meta":{"refraktal":{"turns":2,"outcome":"applied"}}}`, child: true, usage: true, stop: "end_turn"},
		{res: `{"stopReason":"end_turn"}`, stop: "end_turn"},
		{res: `null`},
		{res: `{"stopReason":"end_turn","usage":null,"_meta":{"refraktal":null}}`, stop: "end_turn"},
		{res: `{"stopReason":"end_turn","_meta":{"other":1}}`, stop: "end_turn"},
		// Present but undecodable: flagged, the rest still decoded.
		{res: `{"stopReason":"end_turn","_meta":{"refraktal":{"turns":9223372036854775808}}}`, childInvalid: true, stop: "end_turn", errSubstr: "_meta.refraktal"},
		{res: `{"stopReason":"end_turn","_meta":{"refraktal":{"turns":1.5}}}`, childInvalid: true, stop: "end_turn", errSubstr: "_meta.refraktal"},
		{res: `{"stopReason":"end_turn","_meta":{"refraktal":{"exit":"0"}}}`, childInvalid: true, stop: "end_turn", errSubstr: "_meta.refraktal"},
		{res: `{"stopReason":"end_turn","_meta":{"refraktal":{"retained_usd":1e999}}}`, childInvalid: true, stop: "end_turn", errSubstr: "_meta.refraktal"},
		{res: `{"stopReason":"end_turn","_meta":{"refraktal":"x"}}`, childInvalid: true, stop: "end_turn", errSubstr: "_meta.refraktal"},
		{res: `{"stopReason":"end_turn","usage":{"inputTokens":9223372036854775808},"_meta":{"refraktal":{"turns":1}}}`, child: true, usageInvalid: true, stop: "end_turn", errSubstr: "usage"},
		{res: `{"stopReason":7,"_meta":{"refraktal":{"turns":1}}}`, child: true, errSubstr: "cannot unmarshal"},
		// Keys match as encoding/json matches them: "_META" is "_meta".
		{res: `{"stopReason":"end_turn","_META":{"Refraktal":{"turns":"1"}}}`, childInvalid: true, stop: "end_turn", errSubstr: "_meta.refraktal"},
		// No report can be seen: only the error.
		{res: `{"stopReason":"end_turn","_meta":5}`, stop: "end_turn", errSubstr: "_meta"},
		{res: `[1]`, errSubstr: "cannot unmarshal"},
	} {
		pr, err := decodePromptResult([]byte(tc.res))
		if (pr.Child != nil) != tc.child || pr.ChildInvalid != tc.childInvalid || (pr.Usage != nil) != tc.usage || pr.UsageInvalid != tc.usageInvalid || pr.StopReason != tc.stop {
			t.Errorf("%s: child %v invalid %t usage %v invalid %t stop %q", tc.res, pr.Child, pr.ChildInvalid, pr.Usage, pr.UsageInvalid, pr.StopReason)
		}
		if tc.errSubstr == "" && err != nil || tc.errSubstr != "" && (err == nil || !strings.Contains(err.Error(), tc.errSubstr)) {
			t.Errorf("%s: error %v, want one containing %q", tc.res, err, tc.errSubstr)
		}
	}
}
