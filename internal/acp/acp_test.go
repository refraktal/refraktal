package acp_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"refraktal.dev/refraktal/internal/acp"
	"refraktal.dev/refraktal/internal/acp/acptest"
	"refraktal.dev/refraktal/internal/proc"
	"refraktal.dev/refraktal/internal/runlog"
)

func TestMain(m *testing.M) {
	// A fake agent is named explicitly (ACPTEST_MODE) and plays the harness,
	// which carries the helper switch for its shell prefix: check it first.
	acptest.RunIfAgent()
	proc.MaybeShellPrefix()
	os.Exit(m.Run())
}

type rig struct {
	child  *proc.Child
	client *acp.Client
	cwd    string
	log    string

	mu     sync.Mutex
	chunks []string
}

func (r *rig) text() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.chunks, "")
}

// start runs the fake agent in mode under process control, with a client and
// a run log wired to it.
func start(t *testing.T, mode string, opts acp.Options) *rig {
	t.Helper()
	argv, env := acptest.Command(mode)
	r := &rig{cwd: t.TempDir()}
	r.log = filepath.Join(r.cwd, "events.jsonl")
	l, err := runlog.Create(r.log)
	if err != nil {
		t.Fatal(err)
	}
	r.child, err = proc.Start(proc.Spec{Argv: argv, Env: env, Dir: r.cwd, PollEvery: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	opts.OnUpdate = func(u acp.Update) {
		l.Update("fake", u.SessionID, u.Kind, u.Raw)
		if u.Kind == "agent_message_chunk" {
			var c struct {
				Content struct{ Text string } `json:"content"`
			}
			json.Unmarshal(u.Raw, &c)
			r.mu.Lock()
			r.chunks = append(r.chunks, c.Content.Text)
			r.mu.Unlock()
		}
	}
	opts.OnNotification = func(method string, params json.RawMessage) { l.Notification("fake", method, params) }
	r.client = acp.New(r.child.Stdout, r.child.Stdin, opts)
	t.Cleanup(func() {
		r.child.Shutdown(500 * time.Millisecond)
		l.Close()
	})
	return r
}

func (r *rig) session(t *testing.T) acp.Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := r.client.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	s, err := r.client.NewSession(ctx, r.cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func prompt(t *testing.T, r *rig, s acp.Session, limit time.Duration) (acp.Turn, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	return r.client.Prompt(ctx, s.ID, "hi")
}

func TestFullTurn(t *testing.T) {
	var asked []acp.PermissionRequest
	r := start(t, "turn", acp.Options{OnPermission: func(_ context.Context, req acp.PermissionRequest) string {
		asked = append(asked, req)
		return req.Option("allow_once")
	}})
	s := r.session(t)
	if err := r.client.SetMode(context.Background(), s.ID, "default"); err != nil {
		t.Fatal(err)
	}
	turn, err := prompt(t, r, s, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if turn.StopReason != "end_turn" || turn.Cancelled || turn.Permissions != 1 || len(asked) != 1 {
		t.Fatalf("turn %+v, asked %d", turn, len(asked))
	}
	if turn.Usage == nil || turn.Usage.TotalTokens != 1500 || turn.Usage.CachedReadTokens == nil || *turn.Usage.CachedReadTokens != 1000 {
		t.Fatalf("usage %+v", turn.Usage)
	}
	if turn.LastUsed == nil || *turn.LastUsed != 1500 || turn.Cost == nil || turn.Cost.Amount != 0.0123 || turn.Cost.Currency != "USD" {
		t.Fatalf("last used %v, cost %+v", turn.LastUsed, turn.Cost)
	}
	text := r.text()
	if !strings.Contains(text, `{"optionId":"once","outcome":"selected"}`) {
		t.Errorf("permission answer not seen by the agent: %s", text)
	}
	if !strings.Contains(text, `-32601`) {
		t.Errorf("fs request not refused with method not found: %s", text)
	}
	if asked[0].ToolCall.ID != "t1" || len(asked[0].Options) != 3 {
		t.Errorf("permission request parsed as %+v", asked[0])
	}
}

func TestPermissionsAreRejectedWithoutAHandler(t *testing.T) {
	r := start(t, "turn", acp.Options{})
	if _, err := prompt(t, r, r.session(t), 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.text(), `{"optionId":"no","outcome":"selected"}`) {
		t.Fatalf("want reject_once selected: %s", r.text())
	}
}

func TestCancel(t *testing.T) {
	r := start(t, "hang", acp.Options{CancelGrace: 5 * time.Second})
	turn, err := prompt(t, r, r.session(t), 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !turn.Cancelled || turn.StopReason != "cancelled" {
		t.Fatalf("turn %+v", turn)
	}
}

// ACP requires the client to answer a pending permission request "cancelled"
// once it has cancelled the turn.
func TestCancelAnswersAPendingPermissionRequest(t *testing.T) {
	r := start(t, "permhang", acp.Options{OnPermission: func(ctx context.Context, _ acp.PermissionRequest) string {
		<-ctx.Done() // a policy that has not decided yet
		return "once"
	}})
	turn, err := prompt(t, r, r.session(t), 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !turn.Cancelled || turn.StopReason != "cancelled" {
		t.Fatalf("turn %+v", turn)
	}
	if !strings.Contains(r.text(), `"outcome":{"outcome":"cancelled"}`) {
		t.Fatalf("pending request not answered cancelled: %s", r.text())
	}
}

func TestKillWhenCancelIsIgnored(t *testing.T) {
	r := start(t, "deaf", acp.Options{CancelGrace: 200 * time.Millisecond})
	turn, err := prompt(t, r, r.session(t), 200*time.Millisecond)
	if !errors.Is(err, acp.ErrCancelIgnored) || !turn.Cancelled {
		t.Fatalf("err %v, turn %+v", err, turn)
	}
	rep := r.child.Shutdown(200 * time.Millisecond)
	if rep.ExitedOnItsOwn {
		t.Fatal("deaf agent exited on its own")
	}
	select {
	case <-r.child.Exited():
	case <-time.After(3 * time.Second):
		t.Fatal("agent still running after shutdown")
	}
	select {
	case <-r.client.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("client did not see the agent go")
	}
}

func TestTreeKillStopsADetachedTool(t *testing.T) {
	r := start(t, "detach", acp.Options{})
	if _, err := prompt(t, r, r.session(t), 10*time.Second); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(r.cwd, "detach.pid"))
	pid, _ := strconv.Atoi(string(b))
	if pid == 0 {
		t.Fatal("no detach.pid")
	}
	defer killIfRunning(pid)
	for deadline := time.Now().Add(3 * time.Second); !tracked(r.child, pid); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("detached tool %d never tracked", pid)
		}
	}
	rep := r.child.Shutdown(time.Second)
	if !rep.ExitedOnItsOwn {
		t.Error("agent did not exit when stdin closed")
	}
	killed := false
	for _, p := range rep.Kill.Killed {
		killed = killed || p.PID == pid
	}
	if !killed || len(rep.Kill.Alive) != 0 {
		t.Fatalf("detached tool %d not killed: %+v", pid, rep.Kill)
	}
	if all, _ := proc.List(); all[pid].PID == pid {
		t.Fatalf("detached tool %d still running", pid)
	}
}

func tracked(c *proc.Child, pid int) bool {
	return slices.ContainsFunc(c.Tracked(), func(p proc.Proc) bool { return p.PID == pid })
}

// killIfRunning cleans up after a failed test. The PID was just read from the
// fake agent, but check its command anyway.
func killIfRunning(pid int) {
	all, _ := proc.List()
	if p, ok := all[pid]; ok && p.Cmd == "sleep 30" {
		proc.Kill([]proc.Proc{p})
	}
}

func TestAuthNotificationIsNotLogged(t *testing.T) {
	r := start(t, "auth", acp.Options{})
	if _, err := prompt(t, r, r.session(t), 10*time.Second); err != nil {
		t.Fatal(err)
	}
	r.child.Shutdown(time.Second)
	b, _ := os.ReadFile(r.log)
	if strings.Contains(string(b), acptest.AuthEmail) {
		t.Fatal("account email reached the run log")
	}
	if !strings.Contains(string(b), `"method":"auth/status_update"`) || !strings.Contains(string(b), `"kind":"agent_message_chunk"`) {
		t.Fatalf("expected events missing:\n%s", b)
	}
	if strings.Contains(string(b), `"availableCommands"`) {
		t.Fatal("available_commands_update logged verbatim")
	}
}

func TestSessionNewError(t *testing.T) {
	r := start(t, "noauth", acp.Options{})
	ctx := context.Background()
	if _, err := r.client.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	_, err := r.client.NewSession(ctx, r.cwd, nil)
	var e *acp.Error
	if !errors.As(err, &e) || e.Code != -32000 {
		t.Fatalf("want ACP error -32000, got %v", err)
	}
}

func TestProtocolVersionIsChecked(t *testing.T) {
	r := start(t, "oldproto", acp.Options{})
	if _, err := r.client.Initialize(context.Background()); err == nil || !strings.Contains(err.Error(), "version 2") {
		t.Fatalf("want a version error, got %v", err)
	}
}

func TestAgentExitEndsAPrompt(t *testing.T) {
	r := start(t, "hang", acp.Options{})
	s := r.session(t)
	done := make(chan error, 1)
	go func() {
		_, err := prompt(t, r, s, 10*time.Second)
		done <- err
	}()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for !strings.Contains(r.text(), "working") {
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("the agent did not start its prompt")
		case err := <-done:
			t.Fatalf("prompt ended before shutdown: %v", err)
		}
	}
	r.child.Shutdown(100 * time.Millisecond)
	err := <-done
	if !errors.Is(err, acp.ErrAgentExited) {
		t.Fatalf("want ErrAgentExited, got %v", err)
	}
}

// 18. A harness that stopped reading must not block a cancel: writes are
// bounded, and the timeout path never waits on a blocked writer.
func TestCancelDoesNotBlockOnAHarnessThatStoppedReading(t *testing.T) {
	r := start(t, "noread", acp.Options{CancelGrace: 200 * time.Millisecond})
	s := r.session(t)
	// The first prompt makes the harness stop reading for good.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	_, err := r.client.Prompt(ctx, s.ID, "go")
	cancel()
	if !errors.Is(err, acp.ErrCancelIgnored) {
		t.Fatalf("first prompt: %v", err)
	}
	// A 4 MB prompt cannot fit in the pipe: its write blocks.
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, err := r.client.Prompt(ctx, s.ID, strings.Repeat("y", 4<<20))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a prompt to a harness that is not reading succeeded")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Prompt blocked on a harness that stopped reading")
	}
}

// Hardening 1. A harness that writes past MaxFrame without a newline gets its
// connection closed with a clear error, instead of growing the heap.
func TestOversizedFrameClosesTheConnection(t *testing.T) {
	r := start(t, "bigframe", acp.Options{CancelGrace: 200 * time.Millisecond})
	s := r.session(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := r.client.Prompt(ctx, s.ID, "go")
	if !errors.Is(err, acp.ErrFrameTooLarge) {
		t.Fatalf("want ErrFrameTooLarge, got %v", err)
	}
}
