package acp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"refraktal.dev/refraktal/internal/acp"
)

type gateWriter struct {
	blockEntered chan struct{}
	release      chan struct{}
	frames       chan map[string]json.RawMessage
	once         sync.Once
}

func (w *gateWriter) Write(b []byte) (int, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return 0, err
	}
	if string(m["id"]) == `"blocker"` {
		w.once.Do(func() { close(w.blockEntered) })
		<-w.release
	}
	w.frames <- m
	return len(b), nil
}
func TestQueuedPermissionCannotApproveAfterCancellation(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()
	out := &gateWriter{make(chan struct{}), make(chan struct{}), make(chan map[string]json.RawMessage, 16), sync.Once{}}
	decision := make(chan struct{})
	c := acp.New(r, out, acp.Options{CancelGrace: time.Second, OnPermission: func(_ context.Context, req acp.PermissionRequest) string {
		if req.ToolCall.ID == "target" {
			close(decision)
		}
		return "yes"
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	turnDone := make(chan error, 1)
	go func() { _, err := c.Prompt(ctx, "s", "task"); turnDone <- err }()
	prompt := <-out.frames
	fmt.Fprintln(w, `{"jsonrpc":"2.0","id":"blocker","method":"session/request_permission","params":{"sessionId":"s","toolCall":{"toolCallId":"blocker"},"options":[{"kind":"allow_once","optionId":"yes"}]}}`)
	<-out.blockEntered
	fmt.Fprintln(w, `{"jsonrpc":"2.0","id":"permission","method":"session/request_permission","params":{"sessionId":"s","toolCall":{"toolCallId":"target"},"options":[{"kind":"allow_once","optionId":"yes"}]}}`)
	<-decision
	// Allow the decision goroutine to queue behind the blocked first permission response.
	time.Sleep(50 * time.Millisecond)
	cancel()
	time.Sleep(50 * time.Millisecond)
	close(out.release)
	var permission map[string]json.RawMessage
	timer := time.After(2 * time.Second)
	for permission == nil {
		select {
		case m := <-out.frames:
			if string(m["id"]) == `"permission"` {
				permission = m
			}
		case <-timer:
			t.Fatal("no permission response")
		}
	}
	fmt.Fprintf(w, "{\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"stopReason\":\"cancelled\"}}\n", prompt["id"])
	<-turnDone
	t.Logf("permission response after cancellation: %s", permission["result"])
	var result struct{ Outcome struct{ Outcome string } }
	if err := json.Unmarshal(permission["result"], &result); err != nil {
		t.Fatal(err)
	}
	if result.Outcome.Outcome != "cancelled" {
		t.Errorf("permission approval escaped after cancellation; want cancelled, got %s", result.Outcome.Outcome)
	}
}
