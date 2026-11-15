package main

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

type rpcErr struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcErr) Error() string { return fmt.Sprintf("rpc error %d: %s %s", e.Code, e.Message, e.Data) }

type msg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcErr         `json:"error,omitempty"`
}

type obj = map[string]any

// Usage is one usage observation: a usage_update, or usage on the prompt response.
type Usage struct {
	TMs    int64           `json:"t_ms"`   // ms since session/prompt was sent
	Source string          `json:"source"` // "usage_update" | "prompt_response"
	When   string          `json:"when"`   // "mid_turn" | "at_end" | "after_end"
	Used   *int64          `json:"used,omitempty"`
	Size   *int64          `json:"size,omitempty"`
	Cost   json.RawMessage `json:"cost,omitempty"` // {amount, currency}
	Tokens json.RawMessage `json:"tokens,omitempty"`
	seq    int
}

type Summary struct {
	Type        string          `json:"type"`
	Harness     string          `json:"harness"`
	Cmd         string          `json:"cmd"`
	Stage       string          `json:"stage"` // last step that succeeded: spawn|initialize|session/new|session/prompt
	Error       string          `json:"error,omitempty"`
	StopReason  string          `json:"stop_reason"`
	TimedOut    bool            `json:"timed_out"`
	Killed      bool            `json:"killed"`
	WallMs      int64           `json:"wall_ms"` // prompt sent -> prompt response (or kill)
	Updates     int             `json:"updates"`
	UpdateKinds map[string]int  `json:"update_kinds"`
	Permissions int             `json:"permission_requests"`
	FSRequests  int             `json:"fs_requests"`
	Refused     map[string]int  `json:"refused_requests"`
	Usage       []Usage         `json:"usage"`
	FirstUsage  string          `json:"first_usage"` // mid_turn | at_end | after_end | none
	FirstCost   string          `json:"first_cost"`
	PromptMeta  json.RawMessage `json:"prompt_response_meta,omitempty"`
}

type config struct {
	harness, cmd, cwd, prompt string
	limit, grace, linger      time.Duration
}

type client struct {
	root      string // symlink-resolved session cwd; the only tree fs requests may touch
	out       io.Writer
	in        io.Writer
	wmu, mu   sync.Mutex
	pending   map[string]chan msg
	nextID    int
	dead      chan struct{}
	spawned   time.Time
	sentAt    time.Time // zero until session/prompt is sent
	phase     string    // setup | turn | after
	lastOther int       // seq of the last non-usage update seen during the turn
	sum       Summary
}

func (c *client) write(m msg) {
	m.JSONRPC = "2.0"
	b, _ := json.Marshal(m)
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.in.Write(append(b, '\n'))
}

func (c *client) emit(v obj) { // caller holds c.mu
	if c.sentAt.IsZero() {
		v["setup_ms"] = time.Since(c.spawned).Milliseconds()
	} else {
		v["t_ms"] = time.Since(c.sentAt).Milliseconds()
	}
	b, _ := json.Marshal(v)
	c.out.Write(append(b, '\n'))
}

func (c *client) call(method string, params any) chan msg {
	p, _ := json.Marshal(params)
	ch := make(chan msg, 1)
	c.mu.Lock()
	c.nextID++
	id := fmt.Sprint(c.nextID)
	c.pending[id] = ch
	c.mu.Unlock()
	c.write(msg{ID: json.RawMessage(id), Method: method, Params: p})
	return ch
}

// await waits for a response, the agent dying, or the timer.
func (c *client) await(ch chan msg, d time.Duration) (json.RawMessage, error) {
	select {
	case m := <-ch:
		if m.Error != nil {
			return nil, m.Error
		}
		return m.Result, nil
	case <-c.dead:
		return nil, errors.New("agent exited before responding")
	case <-time.After(d):
		return nil, errTimeout
	}
}

var errTimeout = errors.New("timeout")

// readLoop dispatches every line the agent writes: responses, notifications, requests.
func (c *client) readLoop(r io.Reader) {
	defer close(c.dead)
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		var m msg
		if len(line) > 1 && json.Unmarshal(line, &m) == nil {
			switch {
			case m.Method == "" && m.ID != nil: // response to one of our calls
				c.mu.Lock()
				ch := c.pending[string(m.ID)]
				delete(c.pending, string(m.ID))
				if ch != nil && c.phase == "turn" {
					c.phase = "after" // only session/prompt is outstanding during the turn
					c.sum.WallMs = time.Since(c.sentAt).Milliseconds()
				}
				c.mu.Unlock()
				if ch != nil {
					ch <- m
				}
			case m.ID == nil: // notification
				c.notification(m)
			default: // agent -> client request
				res, e := c.request(m)
				c.write(msg{ID: m.ID, Result: res, Error: e})
			}
		}
		if err != nil {
			return
		}
	}
}

func (c *client) notification(m msg) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if m.Method != "session/update" {
		c.emit(obj{"type": "notification", "method": m.Method, "params": m.Params})
		return
	}
	var p struct {
		Update json.RawMessage `json:"update"`
	}
	var u struct {
		Kind string          `json:"sessionUpdate"`
		Used *int64          `json:"used"`
		Size *int64          `json:"size"`
		Cost json.RawMessage `json:"cost"`
	}
	json.Unmarshal(m.Params, &p)
	json.Unmarshal(p.Update, &u)
	c.sum.Updates++
	c.sum.UpdateKinds[u.Kind]++
	c.emit(obj{"type": "update", "phase": c.phase, "update": p.Update})
	switch {
	case u.Kind == "usage_update":
		if string(u.Cost) == "null" {
			u.Cost = nil
		}
		c.observe(Usage{Source: "usage_update", Used: u.Used, Size: u.Size, Cost: u.Cost})
	case c.phase == "turn":
		c.lastOther = c.sum.Updates
	}
}

func (c *client) observe(u Usage) { // caller holds c.mu
	u.seq, u.When = c.sum.Updates, c.phase
	if !c.sentAt.IsZero() {
		u.TMs = time.Since(c.sentAt).Milliseconds()
	}
	c.sum.Usage = append(c.sum.Usage, u)
}

// run drives one harness through one prompt. The Summary is always filled in;
// err reports the step that failed, if any.
func run(cfg config, stdout, stderr io.Writer) (Summary, error) {
	c := &client{out: stdout, pending: map[string]chan msg{}, dead: make(chan struct{}), phase: "setup"}
	c.sum = Summary{Type: "summary", Harness: cfg.harness, Stage: "none", UpdateKinds: map[string]int{},
		Refused: map[string]int{}, Usage: []Usage{}, FirstUsage: "none", FirstCost: "none"}
	err := c.drive(cfg, stderr)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.sum.Error = err.Error()
	}
	slices.SortStableFunc(c.sum.Usage, func(a, b Usage) int { return cmp.Compare(a.TMs, b.TMs) })
	for i := range c.sum.Usage {
		u := &c.sum.Usage[i]
		switch {
		case u.When == "turn" && u.seq < c.lastOther:
			u.When = "mid_turn" // other updates followed it before the turn ended
		case u.When == "turn" || u.Source == "prompt_response":
			u.When = "at_end"
		default:
			u.When = "after_end"
		}
		if c.sum.FirstUsage == "none" {
			c.sum.FirstUsage = u.When
		}
		if c.sum.FirstCost == "none" && u.Cost != nil {
			c.sum.FirstCost = u.When
		}
	}
	b, _ := json.Marshal(c.sum)
	stdout.Write(append(b, '\n'))
	return c.sum, err
}

func (c *client) drive(cfg config, stderr io.Writer) error {
	argv, err := resolveCmd(cfg)
	if err != nil {
		return err
	}
	c.sum.Cmd = strings.Join(argv, " ")
	abs, err := filepath.Abs(cfg.cwd)
	if err == nil {
		c.root, err = filepath.EvalSymlinks(abs)
	}
	if err != nil {
		return fmt.Errorf("cwd: %w", err)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir, cmd.Stderr = c.root, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // so the adapter's own children die with it
	stdin, _ := cmd.StdinPipe()
	stdoutPipe, _ := cmd.StdoutPipe()
	c.in, c.spawned = stdin, time.Now()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn: %w", err)
	}
	c.sum.Stage = "spawn"
	go c.readLoop(stdoutPipe)
	defer func() { // close stdin, give the agent a moment to exit, then kill the group
		if c.sum.Killed { // the turn outlived session/cancel plus the grace period
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		stdin.Close()
		select {
		case <-c.dead:
		case <-time.After(cfg.grace):
			c.sum.Killed = true
		}
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		cmd.Wait()
	}()

	const setup = 90 * time.Second // npx may have to download the adapter
	res, err := c.await(c.call("initialize", obj{
		"protocolVersion":    1,
		"clientInfo":         obj{"name": "refraktal-acp-spike", "version": "0.1.0"},
		"clientCapabilities": obj{"fs": obj{"readTextFile": true, "writeTextFile": true}, "terminal": false},
	}), setup)
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	c.sum.Stage = "initialize"
	c.mu.Lock()
	c.emit(obj{"type": "initialize", "result": res})
	c.mu.Unlock()

	res, err = c.await(c.call("session/new", obj{"cwd": c.root, "mcpServers": []any{}}), setup)
	if err != nil {
		return fmt.Errorf("session/new: %w", err)
	}
	var s struct {
		ID string `json:"sessionId"`
	}
	json.Unmarshal(res, &s)
	c.sum.Stage = "session/new"

	c.mu.Lock()
	c.emit(obj{"type": "session", "result": res})
	c.sentAt, c.phase = time.Now(), "turn"
	c.mu.Unlock()
	turn := c.call("session/prompt", obj{"sessionId": s.ID,
		"prompt": []obj{{"type": "text", "text": cfg.prompt}}})
	res, err = c.await(turn, cfg.limit)
	if err == errTimeout {
		c.mu.Lock()
		c.sum.TimedOut = true
		c.mu.Unlock()
		c.write(msg{Method: "session/cancel", Params: json.RawMessage(fmt.Sprintf(`{"sessionId":%q}`, s.ID))})
		if res, err = c.await(turn, cfg.grace); err != nil {
			c.mu.Lock()
			c.sum.Killed, c.sum.StopReason, c.sum.WallMs = true, "killed", time.Since(c.sentAt).Milliseconds()
			c.mu.Unlock()
			return fmt.Errorf("session/prompt: no response within %s of session/cancel: %w", cfg.grace, err)
		}
	}
	if err != nil {
		return fmt.Errorf("session/prompt: %w", err)
	}
	var pr struct {
		StopReason string          `json:"stopReason"`
		Usage      json.RawMessage `json:"usage"` // unstable in ACP v1; captured if present
		Meta       json.RawMessage `json:"_meta"`
	}
	json.Unmarshal(res, &pr)
	c.mu.Lock()
	c.sum.Stage, c.sum.StopReason, c.sum.PromptMeta = "session/prompt", pr.StopReason, pr.Meta
	if len(pr.Usage) > 0 && string(pr.Usage) != "null" {
		c.observe(Usage{Source: "prompt_response", Tokens: pr.Usage})
		c.sum.Usage[len(c.sum.Usage)-1].TMs = c.sum.WallMs
	}
	c.mu.Unlock()
	time.Sleep(cfg.linger) // catch updates that trail the response
	return nil
}
