package refraktal

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"refraktal.dev/refraktal/internal/acp"
	"refraktal.dev/refraktal/internal/budget"
)

// Environment variables a parent Refraktal gives a child.
const (
	// BudgetEnv is the child's grant: JSON {usd, turns, deadline (RFC 3339),
	// depth}. The child's budgets are its flags, shrunk to the grant.
	BudgetEnv = "REFRAKTAL_BUDGET"
	// ParentRunEnv is a folder in the parent's run folder where the child
	// makes its own run folder, so runs nest.
	ParentRunEnv = "REFRAKTAL_PARENT_RUN"
)

// ServeOptions configure ServeACP.
type ServeOptions struct {
	// Run is the template for every prompt's run: budgets (the acp flags),
	// harnesses and paths. Task and Dir come from the prompt and its session.
	Run RunOptions
	// Getenv reads REFRAKTAL_BUDGET and REFRAKTAL_PARENT_RUN. Default os.Getenv.
	Getenv func(string) string
	// Now is the clock for the grant's deadline. Default time.Now.
	Now func() time.Time
	// WriteTimeout bounds each write to the client. Default 10 s.
	WriteTimeout time.Duration
}

// grant is a parent's REFRAKTAL_BUDGET.
type grant struct {
	USD      *float64 `json:"usd"`
	Turns    *int     `json:"turns"`
	Deadline string   `json:"deadline"`
	Depth    *int     `json:"depth"`
}

// childLimits shrinks the flags to the parent's grant, if any: a child never
// gets more than its parent has left.
func childLimits(flags budget.Limits, env string, now time.Time) (budget.Limits, error) {
	l := flags
	if strings.TrimSpace(env) == "" {
		return l, nil
	}
	var g grant
	dec := json.NewDecoder(strings.NewReader(env))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&g); err != nil {
		return l, fmt.Errorf("%s: %w", BudgetEnv, err)
	}
	if g.USD != nil {
		if math.IsNaN(*g.USD) || math.IsInf(*g.USD, 0) || *g.USD < 0 {
			return l, fmt.Errorf("%s: usd must be a finite number, not negative", BudgetEnv)
		}
		l.USD = min(l.USD, *g.USD)
	}
	if g.Turns != nil {
		l.Turns = min(l.Turns, max(0, *g.Turns))
	}
	if g.Depth != nil {
		l.Depth = min(l.Depth, *g.Depth)
	}
	if g.Deadline != "" {
		d, err := time.Parse(time.RFC3339, g.Deadline)
		if err != nil {
			return l, fmt.Errorf("%s: deadline: %w", BudgetEnv, err)
		}
		left := d.Sub(now)
		if left <= 0 {
			return l, &budget.Exhausted{Budget: "mins"}
		}
		l.Wall = min(l.Wall, left)
	}
	return l, nil
}

// rpc is one JSON-RPC message.
type rpc struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// server is one ACP connection.
type server struct {
	o ServeOptions
	// lmu guards ledger: one per server, seeded from the inherited grant on
	// the first prompt and shared by every prompt and session after it.
	lmu    sync.Mutex
	ledger *budget.Ledger
	lerr   error
	ctx    context.Context
	w      *bufio.Writer
	// The transport: one writer goroutine drains out; a write that takes
	// longer than the timeout (a client that stopped reading) fails the
	// transport, which cancels every run.
	out      chan []byte
	dead     chan struct{} // closed when the transport has failed
	deadOnce sync.Once
	terr     error
	busy     atomic.Int64 // when the current write started (unix ns), 0 when idle
	wdone    chan struct{}
	quit     chan struct{}
	cancel   context.CancelFunc
	mu       sync.Mutex
	next     int
	sess     map[string]*serverSession
	wg       sync.WaitGroup
}

type serverSession struct {
	cwd    string
	cancel context.CancelFunc // the running prompt's, or nil
	spent  float64            // cumulative cost reported in this session
}

// ServeACP speaks ACP (protocol 1) as an agent on in and out until in ends
// or ctx is done. Each session/prompt is one Refraktal run in the session's
// folder, with the result applied there. Progress and notices arrive as
// message chunks; the run's cost (US dollars, cumulative for the session)
// as a usage_update; the prompt response carries the run's outcome and the
// prompts it sent in _meta.refraktal. Harness auth notifications and
// permission requests never cross to the client: the run answers them.
func ServeACP(ctx context.Context, in io.Reader, out io.Writer, o ServeOptions) error {
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	parent := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s := &server{o: o, ctx: ctx, cancel: cancel, w: bufio.NewWriter(out), sess: map[string]*serverSession{}}
	s.start()
	type frame struct {
		line []byte
		err  error
	}
	frames := make(chan frame)
	go func() { // a pending read never holds the server: it is abandoned on exit
		r := bufio.NewReaderSize(in, 64<<10)
		for {
			line, err := readFrame(r)
			select {
			case frames <- frame{line, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	var err error
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-s.dead:
			break loop
		case f := <-frames:
			if f.err != nil {
				err = f.err
				break loop
			}
			if len(bytes.TrimSpace(f.line)) == 0 {
				continue
			}
			var m rpc
			if json.Unmarshal(f.line, &m) != nil {
				s.send(rpc{Error: &rpcError{-32700, "parse error"}})
				continue
			}
			s.handle(m)
		}
	}
	cancel() // stdin closed, the client stopped reading, or Ctrl-C: stop every run
	s.wg.Wait()
	s.stop()
	switch {
	case s.failed() != nil:
		return s.failed()
	case errors.Is(err, io.EOF), err == nil && parent.Err() != nil:
		return nil
	}
	return err
}

// readFrame reads one newline-terminated message of at most acp.MaxFrame.
func readFrame(r *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > acp.MaxFrame {
			return nil, acp.ErrFrameTooLarge
		}
		if err == nil {
			return buf, nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			if len(buf) > 0 && errors.Is(err, io.EOF) {
				return buf, nil
			}
			return nil, err
		}
	}
}

func (s *server) timeout() time.Duration {
	if s.o.WriteTimeout > 0 {
		return s.o.WriteTimeout
	}
	return 10 * time.Second
}

// start begins the writer and its watchdog.
func (s *server) start() {
	s.out, s.dead, s.wdone, s.quit = make(chan []byte, 1024), make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(s.wdone)
		for b := range s.out {
			select {
			case <-s.dead:
				continue // the transport failed: drop what is left
			default:
			}
			s.busy.Store(time.Now().UnixNano())
			_, err := s.w.Write(b)
			if err == nil {
				err = s.w.Flush()
			}
			s.busy.Store(0)
			if err != nil {
				s.broken(fmt.Errorf("acp: writing to the client: %w", err))
			}
		}
	}()
	go func() {
		t := time.NewTicker(max(min(s.timeout()/4, 250*time.Millisecond), time.Millisecond))
		defer t.Stop()
		for {
			select {
			case <-s.quit:
				return
			case <-s.dead:
				return
			case <-t.C:
				if b := s.busy.Load(); b != 0 && time.Since(time.Unix(0, b)) > s.timeout() {
					s.broken(errors.New("acp: the client stopped reading"))
				}
			}
		}
	}()
}

// stop flushes what is queued (bounded: a stuck writer is abandoned). The
// watchdog stays on through the drain, and a drain that cannot finish is a
// transport failure: an undelivered response is never reported as success.
func (s *server) stop() {
	close(s.out)
	defer close(s.quit)
	select {
	case <-s.wdone:
	case <-s.dead:
	case <-time.After(s.timeout()):
		s.broken(errors.New("acp: the client stopped reading; the final responses were not delivered"))
	}
}

// broken marks the transport failed, once, and cancels every run: their
// harnesses are shut down as on any cancel.
func (s *server) broken(err error) {
	s.deadOnce.Do(func() {
		s.lmu.Lock()
		s.terr = err
		s.lmu.Unlock()
		close(s.dead)
		if s.cancel != nil {
			s.cancel()
		}
	})
}

func (s *server) failed() error {
	s.lmu.Lock()
	defer s.lmu.Unlock()
	return s.terr
}

func (s *server) send(m rpc) {
	m.JSONRPC = "2.0"
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	b = append(b, '\n')
	t := time.NewTimer(s.timeout())
	defer t.Stop()
	select {
	case <-s.dead:
	case s.out <- b:
	case <-t.C:
		s.broken(errors.New("acp: the client stopped reading"))
	}
}

// maxChunkBytes is the most text one message chunk carries: its frame,
// even with every character escaped, stays far below acp.MaxFrame.
var maxChunkBytes = 256 << 10

// finish ends a prompt: the accounting first in its own small frame (the
// session's cumulative cost), then the summary in pieces, then the
// response with the child report.
func (s *server) finish(sessionID string, id json.RawMessage, text string, spent float64, rep acp.ChildReport, stop string) {
	s.update(sessionID, map[string]any{"sessionUpdate": "usage_update", "cost": map[string]any{"amount": spent, "currency": "USD"}})
	for text != "" {
		n := min(len(text), maxChunkBytes)
		for n < len(text) && n > 0 && !utf8.RuneStart(text[n]) {
			n-- // never split a character
		}
		if n == 0 {
			n = min(len(text), maxChunkBytes)
		}
		s.chunk(sessionID, text[:n])
		text = text[n:]
	}
	s.reply(id, map[string]any{"stopReason": stop, "_meta": map[string]any{"refraktal": rep}})
}

func (s *server) reply(id json.RawMessage, result any) { s.send(rpc{ID: id, Result: result}) }

func (s *server) fail(id json.RawMessage, code int, msg string) {
	if id != nil {
		s.send(rpc{ID: id, Error: &rpcError{code, msg}})
	}
}

func (s *server) update(sessionID string, u map[string]any) {
	p, _ := json.Marshal(map[string]any{"sessionId": sessionID, "update": u})
	s.send(rpc{Method: "session/update", Params: p})
}

func (s *server) chunk(sessionID, text string) {
	s.update(sessionID, map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": text}})
}

func (s *server) handle(m rpc) {
	switch m.Method {
	case "initialize":
		s.reply(m.ID, map[string]any{"protocolVersion": acp.ProtocolVersion,
			"agentInfo":         map[string]any{"name": "refraktal", "version": Version},
			"agentCapabilities": map[string]any{"loadSession": false, "promptCapabilities": map[string]any{}},
			"authMethods":       []any{}})
	case "session/new":
		var p struct {
			Cwd string `json:"cwd"`
		}
		if err := json.Unmarshal(m.Params, &p); err != nil {
			s.fail(m.ID, -32602, "session/new: invalid params")
			return
		}
		if fi, err := os.Stat(p.Cwd); err != nil || !fi.IsDir() || !filepath.IsAbs(p.Cwd) {
			s.fail(m.ID, -32602, "session/new: cwd must be an absolute path to a folder")
			return
		}
		s.mu.Lock()
		s.next++
		id := fmt.Sprintf("refraktal-%d", s.next)
		s.sess[id] = &serverSession{cwd: p.Cwd}
		s.mu.Unlock()
		s.reply(m.ID, map[string]any{"sessionId": id,
			"modes": map[string]any{"currentModeId": "default", "availableModes": []any{map[string]any{"id": "default", "name": "Default"}}}})
	case "session/set_mode":
		var p struct {
			SessionID string `json:"sessionId"`
			ModeID    string `json:"modeId"`
		}
		if err := json.Unmarshal(m.Params, &p); err != nil {
			s.fail(m.ID, -32602, "session/set_mode: invalid params")
			return
		}
		if s.session(p.SessionID) == nil {
			s.fail(m.ID, -32602, "unknown session")
			return
		}
		if p.ModeID != "default" {
			s.fail(m.ID, -32602, fmt.Sprintf("refraktal has only the default mode, not %q", p.ModeID))
			return
		}
		s.reply(m.ID, map[string]any{})
	case "session/prompt":
		s.prompt(m)
	case "session/cancel":
		var p struct {
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal(m.Params, &p); err != nil {
			s.fail(m.ID, -32602, "session/cancel: invalid params")
			return
		}
		s.mu.Lock()
		if ss := s.sess[p.SessionID]; ss != nil && ss.cancel != nil {
			ss.cancel()
		}
		s.mu.Unlock()
	default:
		s.fail(m.ID, -32601, "method not found: "+m.Method)
	}
}

func (s *server) session(id string) *serverSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sess[id]
}

// Stop reasons for a child run's outcome.
func stopReason(r RunResult, cancelled bool) string {
	switch {
	case cancelled:
		return "cancelled"
	case r.Outcome == RunExhausted || r.Outcome == RunTimedOut:
		return "max_turn_requests" // a budget ran out
	case r.ExitCode() == ExitOK:
		return "end_turn"
	}
	return "refusal" // the run produced nothing usable
}

func (s *server) prompt(m rpc) {
	var p struct {
		SessionID string `json:"sessionId"`
		Prompt    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"prompt"`
	}
	if err := json.Unmarshal(m.Params, &p); err != nil {
		s.fail(m.ID, -32602, "session/prompt: invalid params")
		return
	}
	var task []string
	for _, b := range p.Prompt {
		if b.Type == "text" {
			task = append(task, b.Text)
		}
	}
	s.mu.Lock()
	ss := s.sess[p.SessionID]
	if ss == nil || ss.cancel != nil {
		s.mu.Unlock()
		s.fail(m.ID, -32602, "unknown session, or a prompt is already running in it")
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	ss.cancel = cancel
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		res, err := s.run(ctx, p.SessionID, ss.cwd, strings.Join(task, "\n\n"))
		cancelled := ctx.Err() != nil
		s.mu.Lock()
		ss.cancel = nil
		ss.spent += res.Charged // what the run actually spent, not its kept reservations
		spent := ss.spent
		s.mu.Unlock()
		if err != nil {
			res.Error = err.Error()
		}
		// The cost is cumulative for the session, as Claude reports it: the
		// parent charges the difference from the previous report.
		s.finish(p.SessionID, m.ID, summary(res), spent, acp.ChildReport{Turns: res.RunTurns, Outcome: res.Outcome, Exit: res.ExitCode(),
			Run: res.Run, Retained: res.Retained}, stopReason(res, cancelled))
	}()
}

// run is one prompt's Refraktal run, with the budgets shrunk to the
// parent's grant and the run folder nested in the parent's.
func (s *server) run(ctx context.Context, sessionID, cwd, task string) (RunResult, error) {
	o := s.o.Run
	ledger, lim, err := s.sharedLedger()
	if err != nil {
		var ex *budget.Exhausted
		if errors.As(err, &ex) {
			return RunResult{Outcome: RunExhausted, Error: err.Error()}, nil
		}
		return RunResult{Outcome: RunUnusable}, err
	}
	o.Budget, o.Ledger, o.Task, o.Dir, o.NoApply, o.Dry = &lim, ledger, task, cwd, false, false
	o.Confine = true // the session's folder, not the whole repository
	if pr := s.o.Getenv(ParentRunEnv); pr != "" {
		o.ParentRun = pr
	}
	// Only the safe form of each notice crosses: no raw OS error, URL or path.
	o.OnNotice, o.OnSafeNotice = nil, func(n string) { s.chunk(sessionID, "notice: "+n+"\n") }
	return Run(ctx, o)
}

// sharedLedger is the server's one ledger when a parent granted a budget:
// the acp flags shrunk to the inherited grant, made on the first prompt.
// Every prompt and session draws on it, so the grant never resets. Without
// a grant it returns no ledger: each run makes its own from the flags.
func (s *server) sharedLedger() (*budget.Ledger, budget.Limits, error) {
	s.lmu.Lock()
	defer s.lmu.Unlock()
	flags := budget.Defaults
	if s.o.Run.Budget != nil {
		flags = *s.o.Run.Budget
	}
	if strings.TrimSpace(s.o.Getenv(BudgetEnv)) == "" {
		// No parent: a standalone server (an editor's) gives each prompt the
		// flags afresh, so --mins is per prompt, not per server lifetime.
		return nil, flags, nil
	}
	if s.ledger == nil && s.lerr == nil {
		lim, err := childLimits(flags, s.o.Getenv(BudgetEnv), s.o.Now())
		if err == nil {
			s.ledger, s.lerr = budget.New(lim, nil)
		} else {
			s.lerr = err
		}
		flags = lim
	}
	if s.lerr != nil {
		return nil, flags, s.lerr
	}
	return s.ledger, s.ledger.Limits(), nil
}

// summary is the final message of a prompt: only Refraktal's structured
// outcome, harness and files, never the child's raw reply.
func summary(r RunResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "refraktal: %s", r.Outcome)
	if r.Harness != "" {
		fmt.Fprintf(&b, " (%s)", r.Harness)
	}
	if len(r.Files) > 0 {
		fmt.Fprintf(&b, ", %d files: %s", len(r.Files), strings.Join(r.Files, " "))
	}
	if r.Error != "" {
		// Never the text: harness errors can carry account details, and
		// nothing from a child's harness crosses to the client verbatim.
		b.WriteString("; the error is in the child's run log")
	}
	return b.String()
}
