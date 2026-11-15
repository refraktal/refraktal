// Package acp is a client for the Agent Client Protocol, version 1: JSON-RPC
// 2.0 over a harness's stdin and stdout, one message per line.
//
// The client does not implement the fs/* or terminal/* methods (no harness
// uses them) and refuses them with "method not found".
package acp

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// ProtocolVersion is the ACP version the client speaks.
const ProtocolVersion = 1

// Error is a JSON-RPC error returned by the agent.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("acp error %d: %s", e.Code, e.Message) }

// JSON-RPC error codes the client sends.
const (
	codeMethodNotFound = -32601
	codeInternal       = -32603
)

var (
	// ErrAgentExited means the agent closed its stdout before responding.
	ErrAgentExited = errors.New("acp: agent exited")
	// ErrCancelIgnored means the agent did not answer session/prompt within
	// the grace period after session/cancel. The caller must shut it down.
	ErrCancelIgnored = errors.New("acp: no prompt response after session/cancel")
)

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Options configure a Client. Callbacks run on the client's read goroutine,
// except OnPermission, which runs on a goroutine of its own; they must not
// block for long.
type Options struct {
	ClientName, ClientVersion string

	// OnUpdate receives every session/update.
	OnUpdate func(Update)
	// OnNotification receives every other notification, such as a harness's
	// auth status (which carries account details: do not log it verbatim).
	OnNotification func(method string, params json.RawMessage)
	// OnPermission answers session/request_permission with an option ID, or
	// "" for the cancelled outcome. ctx is cancelled if the turn is cancelled
	// first; the client then answers "cancelled" itself, as ACP requires. If
	// nil, every request is rejected.
	OnPermission func(ctx context.Context, req PermissionRequest) string
	// CancelGrace is how long Prompt waits for the response after sending
	// session/cancel. Default 5 s.
	CancelGrace time.Duration
}

// Client speaks ACP to one agent.
type Client struct {
	w    io.Writer
	opts Options

	wmu     sync.Mutex
	mu      sync.Mutex
	nextID  int
	pending map[string]chan message
	turns   map[string]*turnState // active turn by session ID
	models  map[string]string     // current model by session ID
	done    chan struct{}
}

type turnState struct {
	ctx    context.Context
	cancel context.CancelFunc
	turn   Turn
}

// New starts a client reading the agent's stdout from r and writing to its
// stdin through w.
func New(r io.Reader, w io.Writer, opts Options) *Client {
	opts.CancelGrace = cmp.Or(opts.CancelGrace, 5*time.Second)
	opts.ClientName = cmp.Or(opts.ClientName, "refraktal")
	c := &Client{w: w, opts: opts, pending: map[string]chan message{},
		turns: map[string]*turnState{}, models: map[string]string{}, done: make(chan struct{})}
	go c.readLoop(r)
	return c
}

// Done is closed when the agent's stdout closes.
func (c *Client) Done() <-chan struct{} { return c.done }

func (c *Client) write(m message) error {
	m.JSONRPC = "2.0"
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.w.Write(append(b, '\n'))
	return err
}

func (c *Client) send(method string, params any) (chan message, string, error) {
	p, err := json.Marshal(params)
	if err != nil {
		return nil, "", err
	}
	ch := make(chan message, 1)
	c.mu.Lock()
	c.nextID++
	id := fmt.Sprint(c.nextID)
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.write(message{ID: json.RawMessage(id), Method: method, Params: p}); err != nil {
		c.forget(id)
		return nil, "", fmt.Errorf("%s: %w", method, err)
	}
	return ch, id, nil
}

func (c *Client) forget(id string) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func result(m message) (json.RawMessage, error) {
	if m.Error != nil {
		return nil, m.Error
	}
	return m.Result, nil
}

// call sends a request and waits for its response, the agent exiting, or ctx.
func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	ch, id, err := c.send(method, params)
	if err != nil {
		return nil, err
	}
	select {
	case m := <-ch:
		res, err := result(m)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", method, err)
		}
		return res, nil
	case <-c.done:
		return nil, fmt.Errorf("%s: %w", method, ErrAgentExited)
	case <-ctx.Done():
		c.forget(id)
		return nil, fmt.Errorf("%s: %w", method, ctx.Err())
	}
}

func (c *Client) readLoop(r io.Reader) {
	defer close(c.done)
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		var m message
		if len(line) > 1 && json.Unmarshal(line, &m) == nil {
			switch {
			case m.Method == "" && m.ID != nil:
				c.mu.Lock()
				ch := c.pending[string(m.ID)]
				delete(c.pending, string(m.ID))
				c.mu.Unlock()
				if ch != nil {
					ch <- m
				}
			case m.ID == nil:
				c.notification(m)
			default:
				c.request(m)
			}
		}
		if err != nil {
			return
		}
	}
}

func (c *Client) notification(m message) {
	if m.Method != "session/update" {
		if c.opts.OnNotification != nil {
			c.opts.OnNotification(m.Method, m.Params)
		}
		return
	}
	u := parseUpdate(m.Params)
	c.mu.Lock()
	if u.Kind == "config_option_update" {
		var co struct {
			ConfigOptions []ConfigOption `json:"configOptions"`
		}
		if json.Unmarshal(u.Raw, &co) == nil {
			if model := modelOf(co.ConfigOptions); model != "" {
				c.models[u.SessionID] = model
			}
		}
	}
	if ts := c.turns[u.SessionID]; ts != nil && u.Kind == "usage_update" {
		if u.Used != nil {
			ts.turn.LastUsed = u.Used
		}
		if u.Cost != nil {
			ts.turn.Cost = u.Cost
		}
	}
	c.mu.Unlock()
	if c.opts.OnUpdate != nil {
		c.opts.OnUpdate(u)
	}
}

func (c *Client) request(m message) {
	if m.Method != "session/request_permission" {
		c.write(message{ID: m.ID, Error: &Error{Code: codeMethodNotFound, Message: "method not found: " + m.Method}})
		return
	}
	var req PermissionRequest
	if err := json.Unmarshal(m.Params, &req); err != nil {
		c.write(message{ID: m.ID, Error: &Error{Code: codeInternal, Message: "bad params"}})
		return
	}
	req.Raw = m.Params
	c.mu.Lock()
	ctx := context.Background()
	if ts := c.turns[req.SessionID]; ts != nil {
		ctx = ts.ctx
		ts.turn.Permissions++
	}
	c.mu.Unlock()
	go func() {
		answer := make(chan string, 1)
		go func() {
			if c.opts.OnPermission == nil {
				answer <- req.reject()
				return
			}
			answer <- c.opts.OnPermission(ctx, req)
		}()
		var id string
		select {
		case id = <-answer:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			id = "" // once the turn is cancelled, every pending request is answered "cancelled"
		}
		outcome := map[string]any{"outcome": "cancelled"}
		if id != "" {
			outcome = map[string]any{"outcome": "selected", "optionId": id}
		}
		b, _ := json.Marshal(map[string]any{"outcome": outcome})
		c.write(message{ID: m.ID, Result: b})
	}()
}

// Initialize performs the handshake and checks the protocol version.
func (c *Client) Initialize(ctx context.Context) (Agent, error) {
	res, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion":    ProtocolVersion,
		"clientInfo":         map[string]any{"name": c.opts.ClientName, "version": c.opts.ClientVersion},
		"clientCapabilities": map[string]any{"fs": map[string]any{"readTextFile": false, "writeTextFile": false}, "terminal": false},
	})
	if err != nil {
		return Agent{}, err
	}
	var a Agent
	if err := json.Unmarshal(res, &a); err != nil {
		return Agent{}, fmt.Errorf("initialize: %w", err)
	}
	if a.ProtocolVersion != ProtocolVersion {
		return a, fmt.Errorf("initialize: agent speaks protocol version %d, want %d", a.ProtocolVersion, ProtocolVersion)
	}
	return a, nil
}

// NewSession starts a session in cwd, an absolute path. meta, if set, is sent
// as _meta (Claude's MCP isolation option goes here).
func (c *Client) NewSession(ctx context.Context, cwd string, meta json.RawMessage) (Session, error) {
	params := map[string]any{"cwd": cwd, "mcpServers": []any{}}
	if len(meta) > 0 {
		params["_meta"] = meta
	}
	res, err := c.call(ctx, "session/new", params)
	if err != nil {
		return Session{}, err
	}
	var s Session
	if err := json.Unmarshal(res, &s); err != nil || s.ID == "" {
		return Session{}, fmt.Errorf("session/new: no session ID in %d-byte result", len(res))
	}
	if model := s.Model(); model != "" {
		c.mu.Lock()
		if c.models[s.ID] == "" {
			c.models[s.ID] = model
		}
		c.mu.Unlock()
	}
	return s, nil
}

// SetMode sets the session's permission mode.
func (c *Client) SetMode(ctx context.Context, sessionID, mode string) error {
	_, err := c.call(ctx, "session/set_mode", map[string]any{"sessionId": sessionID, "modeId": mode})
	return err
}

// Prompt sends one prompt and waits for the turn to end.
//
// If ctx ends first, Prompt sends session/cancel, answers any pending
// permission request "cancelled", and waits up to CancelGrace for the
// response. The returned Turn then has Cancelled set. If no response comes,
// the error is ErrCancelIgnored and the caller must shut the agent down.
func (c *Client) Prompt(ctx context.Context, sessionID, text string) (Turn, error) {
	tctx, tcancel := context.WithCancel(context.Background())
	ts := &turnState{ctx: tctx, cancel: tcancel}
	c.mu.Lock()
	if c.turns[sessionID] != nil {
		c.mu.Unlock()
		tcancel()
		return Turn{}, fmt.Errorf("session/prompt: session %s already has a turn running", sessionID)
	}
	c.turns[sessionID] = ts
	c.mu.Unlock()
	start := time.Now()
	finish := func() Turn {
		tcancel()
		c.mu.Lock()
		defer c.mu.Unlock()
		delete(c.turns, sessionID)
		ts.turn.Wall = time.Since(start)
		ts.turn.Model = c.models[sessionID]
		return ts.turn
	}

	ch, id, err := c.send("session/prompt", map[string]any{"sessionId": sessionID,
		"prompt": []map[string]any{{"type": "text", "text": text}}})
	if err != nil {
		return finish(), err
	}
	var m message
	select {
	case m = <-ch:
	case <-c.done:
		return finish(), fmt.Errorf("session/prompt: %w", ErrAgentExited)
	case <-ctx.Done():
		c.mu.Lock()
		ts.turn.Cancelled = true
		c.mu.Unlock()
		tcancel() // pending permission requests are answered "cancelled"
		c.write(message{Method: "session/cancel", Params: mustJSON(map[string]any{"sessionId": sessionID})})
		select {
		case m = <-ch:
		case <-c.done:
			return finish(), fmt.Errorf("session/prompt: %w", ErrAgentExited)
		case <-time.After(c.opts.CancelGrace):
			c.forget(id)
			return finish(), ErrCancelIgnored
		}
	}
	res, err := result(m)
	if err != nil {
		return finish(), fmt.Errorf("session/prompt: %w", err)
	}
	var pr struct {
		StopReason string `json:"stopReason"`
		Usage      *Usage `json:"usage"`
	}
	if err := json.Unmarshal(res, &pr); err != nil {
		return finish(), fmt.Errorf("session/prompt: %w", err)
	}
	c.mu.Lock()
	ts.turn.StopReason, ts.turn.Usage = pr.StopReason, pr.Usage
	c.mu.Unlock()
	return finish(), nil
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
