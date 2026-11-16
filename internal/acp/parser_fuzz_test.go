package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Measure only the parser/application, not fixture construction or the oracle.
// The fixed allowance covers JSON's nesting stack and bufio's buffers. No
// parallel tests run in a fuzz worker, so TotalAlloc includes no other target.
func acpBound(t *testing.T, n int, parse func()) {
	t.Helper()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	deadline := 2*time.Second + time.Duration(n/(1<<20))*100*time.Millisecond
	watchdog := time.AfterFunc(deadline, func() { panic("ACP parser exceeded work deadline") })
	defer watchdog.Stop()
	start := time.Now()
	parse()
	elapsed := time.Since(start)
	watchdog.Stop()
	runtime.ReadMemStats(&after)
	if used, limit := after.TotalAlloc-before.TotalAlloc, uint64(n)*64+(1<<20); used > limit {
		t.Fatalf("parser allocated %d bytes for %d input bytes (limit %d)", used, n, limit)
	}
	if elapsed > deadline {
		t.Fatalf("parser took %s for %d bytes", elapsed, n)
	}
}

func acpHostile(f *testing.F) {
	for _, s := range []string{"", "null", "{", `{"sessionId":"s","update":`, `1e9999`, `"NaN"`, `"Inf"`, "\xff\xfe\x00", strings.Repeat("[", 10001) + strings.Repeat("]", 10001), strings.Repeat("x", 200<<10), `{"sessionId":"other","sessionId":"s","update":{}}`} {
		f.Add([]byte(s))
	}
}

func FuzzReadFrame(f *testing.F) {
	acpHostile(f)
	f.Add([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}` + "\n"))
	f.Add([]byte("\n{}\n"))
	// Exercise both sides of the actual 16 MiB limit without storing giant
	// corpus files. The newline counts toward MaxFrame.
	for _, n := range []int{MaxFrame - 1, MaxFrame, MaxFrame + 1} {
		f.Add(bytes.Repeat([]byte{'x'}, n))
		f.Add(append(bytes.Repeat([]byte{'x'}, n-1), '\n'))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		r := bytes.NewReader(b)
		br := bufio.NewReaderSize(r, 64<<10)
		var got []byte
		var err error
		acpBound(t, len(b), func() { got, err = readFrame(br) })
		end := bytes.IndexByte(b, '\n') + 1
		if end == 0 {
			end = len(b)
		}
		if end > MaxFrame {
			if !errors.Is(err, ErrFrameTooLarge) || got != nil {
				t.Fatal("oversize frame was accepted or returned partial data")
			}
			if consumed := len(b) - r.Len(); consumed > MaxFrame+(64<<10) {
				t.Fatalf("oversize read consumed %d bytes", consumed)
			}
			return
		}
		if !bytes.Equal(got, b[:end]) || (end > 0 && b[end-1] == '\n' && err != nil) || ((end == 0 || b[end-1] != '\n') && !errors.Is(err, io.EOF)) {
			t.Fatalf("frame boundary/EOF mismatch: got %d bytes, err %v", len(got), err)
		}
		if err == nil {
			rest, readErr := io.ReadAll(br)
			if readErr != nil || !bytes.Equal(rest, b[end:]) {
				t.Fatal("reader lost bytes after first frame")
			}
		}
	})
}

// A deadline-capable in-memory writer avoids writeBy's background goroutine.
type fuzzWriter struct{ ch chan []byte }

func (w fuzzWriter) SetWriteDeadline(time.Time) error { return nil }
func (w fuzzWriter) Write(b []byte) (int, error) {
	w.ch <- bytes.Clone(b)
	return len(b), nil
}

func fuzzClient() *Client {
	return &Client{pending: map[string]chan message{}, turns: map[string]*turnState{}, models: map[string]string{}, done: make(chan struct{})}
}

func FuzzMessageDecoder(f *testing.F) {
	acpHostile(f)
	for _, s := range []string{`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}`, `{"id":1,"error":{"code":-32601,"message":"method not found"}}`, `{"method":"auth/status_update","params":{}}`, `{"method":"session/request_permission","id":2,"params":{}}`, `{"id":1,"id":2,"method":"unknown"}`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		// NDJSON decodes one frame at a time; framing has its own target.
		line, _, _ := bytes.Cut(b, []byte{'\n'})
		c := fuzzClient()
		w := fuzzWriter{make(chan []byte, 1)}
		c.w = w
		notifications := 0
		c.opts.OnNotification = func(string, json.RawMessage) { notifications++ }
		pending := make(chan message, 1)
		c.pending["1"] = pending
		var decoded message
		decodeErr := json.Unmarshal(line, &decoded)
		request := len(line) > 1 && len(line)+1 <= MaxFrame && decodeErr == nil && decoded.ID != nil && decoded.Method != ""
		acpBound(t, len(line), func() {
			c.readLoop(bytes.NewReader(append(bytes.Clone(line), '\n')))
			if request {
				select {
				case reply := <-w.ch:
					var m message
					if json.Unmarshal(reply, &m) != nil || !bytes.Equal(m.ID, decoded.ID) || (m.Result == nil && m.Error == nil) {
						t.Fatal("request response lost its ID or outcome")
					}
				case <-time.After(time.Second):
					t.Fatal("in-memory request did not complete")
				}
			}
		})
		valid := len(line) > 1 && len(line)+1 <= MaxFrame && decodeErr == nil
		wantDelivered := valid && decoded.Method == "" && string(decoded.ID) == "1"
		if (len(pending) == 1) != wantDelivered || (c.pending["1"] == nil) != wantDelivered {
			t.Fatal("invalid response changed pending state or valid response was lost")
		}
		if wantDelivered && !reflect.DeepEqual(<-pending, decoded) {
			t.Fatal("response delivery changed decoded fields")
		}
		wantNotification := valid && decoded.ID == nil && decoded.Method != "session/update"
		if (notifications == 1) != wantNotification || (!request && len(w.ch) != 0) {
			t.Fatal("invalid message dispatched a callback or write")
		}
	})
}

const usageSeed = `{"sessionId":"s","update":{"sessionUpdate":"usage_update","used":1500,"size":200000,"cost":{"amount":0.0123,"currency":"USD"}}}`

func FuzzUsageUpdate(f *testing.F) {
	acpHostile(f)
	for _, s := range []string{usageSeed, `{"sessionId":"s","update":{"sessionUpdate":"usage_update","used":1200,"size":200000}}`, `{"sessionId":"s","update":{"sessionUpdate":"usage_update","used":5,"cost":{"amount":"NaN"}}}`, `{"sessionId":"s","update":{"sessionUpdate":"usage_update","used":-1}}`} {
		f.Add([]byte(s))
	}
	f.Fuzz(checkUsageUpdate)
}

func checkUsageUpdate(t *testing.T, b []byte) {
	c := fuzzClient()
	used := int64(77)
	ts := &turnState{turn: Turn{LastUsed: &used, Cost: &Cost{Amount: 3, Currency: "USD"}}}
	c.turns["s"] = ts
	before := ts.turn
	beforeUsed, beforeCost := *before.LastUsed, *before.Cost
	before.LastUsed, before.Cost = &beforeUsed, &beforeCost
	var got Update
	acpBound(t, len(b), func() {
		got = parseUpdate(b)
		c.notification(message{Method: "session/update", Params: b})
	})
	// Decode the raw envelope separately: duplicate update keys replace the
	// raw object, rather than merging two typed update structs.
	sessionID, _, u, valid := fuzzUpdate(b)
	if !valid {
		// Accounting is all or nothing; the update may still reach callbacks
		// and the log with its kind and raw payload.
		if got.Used != nil || got.Size != nil || got.Cost != nil || !reflect.DeepEqual(ts.turn, before) {
			t.Fatal("rejected usage update was partially applied")
		}
		return
	}
	want := before
	if sessionID == "s" && u.Kind == "usage_update" {
		if u.Used != nil {
			want.LastUsed = u.Used
		}
		if u.Cost != nil {
			want.Cost = u.Cost
		}
	}
	if !reflect.DeepEqual(ts.turn, want) {
		t.Fatal("valid usage update changed unrelated state or lost usage")
	}
}

const configSeed = `{"sessionId":"s","update":{"sessionUpdate":"config_option_update","configOptions":[{"id":"model","category":"model","currentValue":"gpt-6.1-sol"}]}}`

func FuzzConfigOptions(f *testing.F) {
	acpHostile(f)
	f.Add([]byte(configSeed))
	f.Add([]byte(`{"sessionId":"s","update":{"sessionUpdate":"config_option_update","configOptions":[{"id":"other","category":"model","currentValue":"gemini-4-carbon"},{"id":"model","currentValue":42}]}}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		c := fuzzClient()
		c.models["s"] = "original"
		acpBound(t, len(b), func() { c.notification(message{Method: "session/update", Params: b}) })
		want := map[string]string{"s": "original"}
		sessionID, raw, update, valid := fuzzUpdate(b)
		var co struct {
			ConfigOptions []ConfigOption `json:"configOptions"`
		}
		if valid && update.Kind == "config_option_update" && json.Unmarshal(raw, &co) == nil {
			for _, option := range co.ConfigOptions {
				if option.ID != "model" && option.Category != "model" {
					continue
				}
				var model string
				if json.Unmarshal(option.CurrentValue, &model) == nil {
					if model != "" {
						want[sessionID] = model
					}
					break
				}
			}
		}
		if !reflect.DeepEqual(c.models, want) {
			t.Fatal("config update was partially applied or selected the wrong model")
		}
	})
}

func TestUsageUpdateRejectsPartialCost(t *testing.T) {
	checkUsageUpdate(t, []byte(`{"sessionId":"s","update":{"sessionUpdate":"usage_update","used":5,"cost":{"amount":"NaN"}}}`))
}

func TestUsageUpdateRejectsNegativeTokens(t *testing.T) {
	checkUsageUpdate(t, []byte(`{"sessionId":"s","update":{"sessionUpdate":"usage_update","used":-1}}`))
}

func FuzzToolCallUpdate(f *testing.F) {
	acpHostile(f)
	for _, s := range []string{`{"sessionId":"s","update":{"sessionUpdate":"tool_call","toolCallId":"t1","title":"Write note.txt","kind":"edit","status":"pending"}}`, `{"sessionId":"s","update":{"sessionUpdate":"tool_call_update","toolCallId":"t1","status":"completed"}}`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		c := fuzzClient()
		var delivered []Update
		c.opts.OnUpdate = func(u Update) { delivered = append(delivered, u) }
		var u Update
		acpBound(t, len(b), func() { u = parseUpdate(b); c.notification(message{Method: "session/update", Params: b}) })
		sessionID, raw, expected, valid := fuzzUpdate(b)
		if valid && (expected.Kind == "tool_call" || expected.Kind == "tool_call_update") {
			if u.Kind != expected.Kind || u.SessionID != sessionID || !bytes.Equal(raw, u.Raw) || len(delivered) != 1 || !reflect.DeepEqual(delivered[0], u) {
				t.Fatal("tool call payload was altered or lost")
			}
		}
		if (!valid || expected.Kind == "") && len(delivered) != 0 {
			t.Fatal("rejected tool update reached a callback")
		}
		if len(c.models) != 0 || len(c.turns) != 0 {
			t.Fatal("tool update created client state")
		}
	})
}

func FuzzPermissionRequest(f *testing.F) {
	acpHostile(f)
	f.Add([]byte(`{"sessionId":"s","toolCall":{"toolCallId":"t1","kind":"edit","title":"Write note.txt","rawInput":{"file_path":"note.txt","content":"hello"}},"options":[{"optionId":"always","kind":"allow_always"},{"optionId":"once","kind":"allow_once"},{"optionId":"no","kind":"reject_once"}]}`))
	f.Add([]byte(`{"sessionId":"s","toolCall":{"toolCallId":"x1","kind":"execute","rawInput":{"command":"git push origin HEAD"}},"options":[{"optionId":"no","kind":"reject_always"}]}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		c := fuzzClient()
		w := fuzzWriter{make(chan []byte, 1)}
		c.w = w
		c.turns["s"] = &turnState{ctx: context.Background()}
		var callback *PermissionRequest
		c.opts.OnPermission = func(_ context.Context, r PermissionRequest) string { callback = &r; return r.reject() }
		var reply []byte
		acpBound(t, len(b), func() {
			c.request(message{ID: json.RawMessage("7"), Method: "session/request_permission", Params: b})
			select {
			case reply = <-w.ch:
			case <-time.After(time.Second):
				t.Fatal("permission request hung")
			}
		})
		var req PermissionRequest
		err := json.Unmarshal(b, &req)
		var response message
		if json.Unmarshal(reply, &response) != nil || string(response.ID) != "7" {
			t.Fatal("invalid permission response")
		}
		if err != nil {
			if callback != nil || c.turns["s"].turn.Permissions != 0 || response.Error == nil {
				t.Fatal("invalid permission request reached policy or changed turn")
			}
			return
		}
		req.Raw = b
		if callback == nil || !reflect.DeepEqual(*callback, req) || response.Error != nil {
			t.Fatal("permission decode lost tool input or options")
		}
		wantCount := 0
		if req.SessionID == "s" {
			wantCount = 1
		}
		if c.turns["s"].turn.Permissions != wantCount {
			t.Fatal("permission counted in wrong session")
		}
		var outcome struct {
			Outcome struct{ Outcome, OptionID string }
		}
		if json.Unmarshal(response.Result, &outcome) != nil {
			t.Fatal("invalid outcome")
		}
		want := ""
		for _, kind := range []string{"reject_once", "reject_always"} {
			for _, o := range req.Options {
				if o.Kind == kind {
					want = o.ID
					break
				}
			}
			if want != "" {
				break
			}
		}
		if outcome.Outcome.OptionID != want || (want == "" && outcome.Outcome.Outcome != "cancelled") || (want != "" && outcome.Outcome.Outcome != "selected") {
			t.Fatal("permission selected an unoffered rejection option")
		}
	})
}

// This oracle follows JSON's envelope/duplicate-key semantics independently of
// parseUpdate and checks validity before inspecting any returned state.
type fuzzUpdateFields struct {
	Kind string `json:"sessionUpdate"`
	Used *int64 `json:"used"`
	Size *int64 `json:"size"`
	Cost *Cost  `json:"cost"`
}

func fuzzUpdate(b []byte) (string, json.RawMessage, fuzzUpdateFields, bool) {
	var envelope struct {
		SessionID string          `json:"sessionId"`
		Update    json.RawMessage `json:"update"`
	}
	var u fuzzUpdateFields
	if json.Unmarshal(b, &envelope) != nil || json.Unmarshal(envelope.Update, &u) != nil {
		return "", nil, u, false
	}
	valid := (u.Used == nil || *u.Used >= 0) && (u.Size == nil || *u.Size >= 0) &&
		(u.Cost == nil || (u.Cost.Amount >= 0 && !math.IsNaN(u.Cost.Amount) && !math.IsInf(u.Cost.Amount, 0)))
	return envelope.SessionID, envelope.Update, u, valid
}

// A usage_update with malformed accounting still reaches OnUpdate (and so the
// log), with none of its accounting fields applied.
func TestMalformedUsageUpdateStaysVisible(t *testing.T) {
	c := fuzzClient()
	var seen []Update
	c.opts.OnUpdate = func(u Update) { seen = append(seen, u) }
	c.notification(message{Method: "session/update", Params: []byte(`{"sessionId":"s","update":{"sessionUpdate":"usage_update","used":5,"size":1.5e5}}`)})
	if len(seen) != 1 || seen[0].Kind != "usage_update" || seen[0].Used != nil || seen[0].Size != nil || seen[0].Cost != nil {
		t.Fatalf("seen = %+v", seen)
	}
}
