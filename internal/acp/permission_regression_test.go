package acp_test

import (
	"context"
	"encoding/json"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"refraktal.dev/refraktal/internal/acp"
	"refraktal.dev/refraktal/internal/policy"
)

type permissionMessages struct{ ch chan []byte }

func (w permissionMessages) Write(b []byte) (int, error) {
	w.ch <- append([]byte(nil), b...)
	return len(b), nil
}

func (w permissionMessages) next(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	select {
	case b := <-w.ch:
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m
	case <-time.After(time.Second):
		t.Fatal("no client message")
		return nil
	}
}

// A late request must not recover policy authority after Prompt has returned,
// whether the turn ended normally or acknowledged a cancellation.
func TestPermissionRequiresAnActiveUncancelledTurn(t *testing.T) {
	for _, state := range []string{"idle", "completed", "cancelled", "cancelling"} {
		t.Run(state, func(t *testing.T) {
			r, w := io.Pipe()
			out := permissionMessages{make(chan []byte, 8)}
			pol := policy.Policy{Worktree: t.TempDir()}
			var calls atomic.Int32
			c := acp.New(r, out, acp.Options{OnPermission: func(ctx context.Context, req acp.PermissionRequest) string {
				calls.Add(1)
				if ctx.Err() != nil {
					return ""
				}
				return pol.Decide(req).OptionID
			}})
			t.Cleanup(func() {
				w.Close()
				r.Close()
				<-c.Done()
			})
			var finish func()
			if state != "idle" {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan error, 1)
				go func() {
					_, err := c.Prompt(ctx, "s", "x")
					done <- err
				}()
				prompt := out.next(t)
				stop := "end_turn"
				if state == "cancelled" || state == "cancelling" {
					cancel()
					frame := out.next(t)
					if string(frame["method"]) != `"session/cancel"` {
						t.Fatalf("cancel frame = %s", frame["method"])
					}
					stop = "cancelled"
				}
				finish = func() {
					if _, err := io.WriteString(w, `{"jsonrpc":"2.0","id":`+string(prompt["id"])+`,"result":{"stopReason":"`+stop+`"}}`+"\n"); err != nil {
						t.Fatal(err)
					}
					select {
					case err := <-done:
						if err != nil {
							t.Fatal(err)
						}
					case <-time.After(time.Second):
						t.Fatal("prompt did not finish")
					}
				}
				if state != "cancelling" {
					finish()
				}
			}
			if _, err := io.WriteString(w, `{"jsonrpc":"2.0","id":"late","method":"session/request_permission","params":{"sessionId":"s","toolCall":{"kind":"edit","rawInput":{"file_path":"late.txt"}},"options":[{"kind":"allow_once","optionId":"yes"}]}}`+"\n"); err != nil {
				t.Fatal(err)
			}
			response := out.next(t)
			var result struct {
				Outcome struct{ Outcome, OptionID string }
			}
			if err := json.Unmarshal(response["result"], &result); err != nil {
				t.Fatal(err)
			}
			if result.Outcome.Outcome != "cancelled" || result.Outcome.OptionID != "" || calls.Load() != 0 {
				t.Fatalf("permission after %s turn = %s, policy calls = %d", state, response["result"], calls.Load())
			}
			if state == "cancelling" {
				finish()
			}
		})
	}
}

func TestPermissionDecisionCannotOutliveItsTurn(t *testing.T) {
	for _, finish := range []string{"active", "complete", "cancel"} {
		t.Run(finish, func(t *testing.T) {
			r, w := io.Pipe()
			out := permissionMessages{make(chan []byte, 8)}
			entered, release := make(chan struct{}), make(chan struct{})
			c := acp.New(r, out, acp.Options{OnPermission: func(context.Context, acp.PermissionRequest) string {
				close(entered)
				<-release
				return "yes" // Deliberately ignores cancellation; the client must enforce it.
			}})
			defer close(release)
			defer w.Close()
			defer r.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := c.Prompt(ctx, "s", "x")
				done <- err
			}()
			prompt := out.next(t)
			if _, err := io.WriteString(w, `{"jsonrpc":"2.0","id":"permission","method":"session/request_permission","params":{"sessionId":"s","options":[{"kind":"allow_once","optionId":"yes"}]}}`+"\n"); err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("active request did not reach policy")
			}
			stop := "end_turn"
			if finish == "active" {
				release <- struct{}{}
			} else if finish == "cancel" {
				cancel()
				stop = "cancelled"
			}
			if finish != "active" {
				if _, err := io.WriteString(w, `{"jsonrpc":"2.0","id":`+string(prompt["id"])+`,"result":{"stopReason":"`+stop+`"}}`+"\n"); err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(time.Second):
					t.Fatal("prompt did not finish")
				}
			}
			response := out.next(t)
			if string(response["method"]) == `"session/cancel"` {
				response = out.next(t)
			}
			var result struct {
				Outcome struct{ Outcome, OptionID string }
			}
			if err := json.Unmarshal(response["result"], &result); err != nil {
				t.Fatal(err)
			}
			want := "cancelled"
			if finish == "active" {
				want = "selected"
			}
			if result.Outcome.Outcome != want || (finish != "active" && result.Outcome.OptionID != "") {
				t.Fatalf("permission while %s = %s", finish, response["result"])
			}
			if finish == "active" {
				if _, err := io.WriteString(w, `{"jsonrpc":"2.0","id":`+string(prompt["id"])+`,"result":{"stopReason":"end_turn"}}`+"\n"); err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(time.Second):
					t.Fatal("prompt did not finish")
				}
			}
		})
	}
}

type permissionPromptGate struct {
	permissionMessages
	entered chan struct{}
	release chan struct{}
}

func (w permissionPromptGate) Write(b []byte) (int, error) {
	var frame struct{ Method string }
	if err := json.Unmarshal(b, &frame); err != nil {
		return 0, err
	}
	if frame.Method == "session/prompt" {
		close(w.entered)
		<-w.release
	}
	return w.permissionMessages.Write(b)
}

func TestPermissionRejectsCallerCancellationWhilePromptWriteIsBlocked(t *testing.T) {
	r, w := io.Pipe()
	out := permissionPromptGate{permissionMessages{make(chan []byte, 8)}, make(chan struct{}), make(chan struct{})}
	barrier := make(chan struct{}, 1)
	var calls atomic.Int32
	c := acp.New(r, out, acp.Options{
		OnPermission: func(context.Context, acp.PermissionRequest) string {
			calls.Add(1)
			return "yes"
		},
		OnNotification: func(string, json.RawMessage) { barrier <- struct{}{} },
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		_, err := c.Prompt(ctx, "s", "x")
		done <- err
		close(finished)
	}()
	released := false
	t.Cleanup(func() {
		cancel()
		if !released {
			close(out.release)
		}
		w.Close()
		r.Close()
		<-c.Done()
		<-finished
	})
	select {
	case <-out.entered:
	case <-time.After(time.Second):
		t.Fatal("prompt write did not block")
	}
	cancel() // Prompt is still inside sendBy and cannot select ctx.Done yet.
	if _, err := io.WriteString(w, `{"jsonrpc":"2.0","id":"permission","method":"session/request_permission","params":{"sessionId":"s","options":[{"kind":"allow_once","optionId":"yes"}]}}`+"\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, `{"jsonrpc":"2.0","method":"barrier"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-barrier: // The permission request captured its turn before this frame.
	case <-time.After(time.Second):
		t.Fatal("permission request was not read")
	}
	close(out.release)
	released = true
	prompt := out.next(t)
	if _, err := io.WriteString(w, `{"jsonrpc":"2.0","id":`+string(prompt["id"])+`,"result":{"stopReason":"cancelled"}}`+"\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("prompt did not finish")
	}
	response := out.next(t)
	if string(response["method"]) == `"session/cancel"` {
		response = out.next(t)
	}
	if string(response["result"]) != `{"outcome":{"outcome":"cancelled"}}` || calls.Load() != 0 {
		t.Fatalf("permission after caller cancel = %s, policy calls = %d", response["result"], calls.Load())
	}
}
