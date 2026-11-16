package acp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"
)

// gatedACPWriter lets a prompt through, then holds writes until released or
// their installed deadline expires. entered is proof that a write owns the
// client's writer lock, rather than a delay intended to arrange contention.
type gatedACPWriter struct {
	mu       sync.Mutex
	deadline time.Time
	prompt   chan message
	entered  chan message
	release  chan struct{}
}

func newGatedACPWriter() *gatedACPWriter {
	return &gatedACPWriter{prompt: make(chan message, 1), entered: make(chan message, 8), release: make(chan struct{})}
}

func (w *gatedACPWriter) SetWriteDeadline(deadline time.Time) error {
	w.mu.Lock()
	w.deadline = deadline
	w.mu.Unlock()
	return nil
}

func (w *gatedACPWriter) Write(b []byte) (int, error) {
	var m message
	if err := json.Unmarshal(b, &m); err != nil {
		return 0, err
	}
	if m.Method == "session/prompt" {
		w.prompt <- m
		return len(b), nil
	}
	w.mu.Lock()
	deadline := w.deadline
	w.mu.Unlock()
	w.entered <- m
	var expiry <-chan time.Time
	if !deadline.IsZero() {
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		expiry = timer.C
	}
	select {
	case <-w.release:
		return len(b), nil
	case <-expiry:
		return 0, os.ErrDeadlineExceeded
	}
}

type refusingACPDeadline struct{ io.Writer }

func (refusingACPDeadline) SetWriteDeadline(time.Time) error {
	return errors.New("write deadlines are unsupported")
}

func TestWriteDeadlineIncludesWriterLock(t *testing.T) {
	for _, kind := range []string{"deadline", "no_deadline", "deadline_rejected"} {
		t.Run(kind, func(t *testing.T) { checkContendedWriteDeadline(t, kind) })
	}
}

func checkContendedWriteDeadline(t *testing.T, kind string) {
	t.Helper()
	w := newGatedACPWriter()
	var writer io.Writer = w
	switch kind {
	case "no_deadline":
		writer = struct{ io.Writer }{w}
	case "deadline_rejected":
		writer = refusingACPDeadline{w}
	}
	c := &Client{w: writer}
	firstDone := make(chan error, 1)
	var queuedFinished <-chan struct{}
	go func() {
		firstDone <- c.writeBy(message{ID: json.RawMessage(`1`), Result: json.RawMessage(`{}`)}, time.Now().Add(5*time.Second))
	}()
	t.Cleanup(func() {
		close(w.release)
		<-firstDone
		if queuedFinished != nil {
			<-queuedFinished
		}
		select {
		case <-w.entered:
			t.Error("expired queued message reached the writer after it was released")
		default:
		}
	})
	select {
	case <-w.entered:
	case <-time.After(time.Second):
		t.Fatal("first write did not acquire the writer")
	}
	queuedDone := make(chan error, 1)
	queuedEnd := make(chan struct{})
	queuedFinished = queuedEnd
	go func() {
		queuedDone <- c.writeBy(message{Method: "session/cancel"}, time.Now().Add(50*time.Millisecond))
		close(queuedEnd)
	}()
	select {
	case err := <-queuedDone:
		if err == nil {
			t.Fatal("contended write succeeded after its deadline")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("write deadline did not include waiting for the writer lock")
	}
}

func TestPromptCancelDeadlineUnderPermissionContention(t *testing.T) {
	agentOut, toClient := io.Pipe()
	w := newGatedACPWriter()
	c := New(agentOut, w, Options{CancelGrace: 20 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	type outcome struct {
		turn Turn
		err  error
	}
	result := make(chan outcome, 1)
	finished := make(chan struct{})
	go func() {
		turn, err := c.Prompt(ctx, "s", "x")
		result <- outcome{turn, err}
		close(finished)
	}()
	t.Cleanup(func() {
		cancel()
		close(w.release)
		toClient.Close()
		agentOut.Close()
		<-finished
		<-c.Done()
	})
	select {
	case <-w.prompt:
	case <-time.After(time.Second):
		t.Fatal("prompt was not written")
	}
	if _, err := io.WriteString(toClient, `{"jsonrpc":"2.0","id":"permission","method":"session/request_permission","params":{"sessionId":"s"}}`+"\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-w.entered:
		if string(m.ID) != `"permission"` {
			t.Fatalf("expected the permission response to own the writer, got %s", m.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("permission response did not acquire the writer")
	}
	cancel()
	select {
	case got := <-result:
		if !got.turn.Cancelled || !errors.Is(got.err, ErrCancelIgnored) {
			t.Fatalf("cancelled prompt = %+v, %v", got.turn, got.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("2 s cancel waited for the permission response's 10 s write deadline")
	}
}
