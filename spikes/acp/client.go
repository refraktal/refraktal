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
	"strconv"
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
	Turn   int             `json:"turn"` // 1-based; several turns share one session with --turns
	seq    int
}

// Sample is the process group at a moment after the leader exited.
type Sample struct {
	AtMs  int64  `json:"at_ms"` // ms after the leader exited (or after the wait for it gave up)
	Procs []Proc `json:"procs"`
}

// Step is one turn of a scripted session: a prompt and its own time limit.
type Step struct {
	Limit  time.Duration
	Prompt string
}

// Proc is one process in the harness's process group, from ps.
type Proc struct {
	PID int    `json:"pid"`
	Cmd string `json:"cmd"` // command line, truncated; may contain local paths, so never publish raw
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

	// Follow-up measurements.
	ModeSet          string   `json:"mode_set,omitempty"`             // mode applied with session/set_mode
	TurnsDone        int      `json:"turns_done"`                     // prompt responses received
	TurnWallMs       []int64  `json:"turn_wall_ms"`                   // per turn: prompt sent -> response
	TurnStops        []string `json:"turn_stop_reasons"`              // per turn
	CancelAtMs       *int64   `json:"cancel_at_ms,omitempty"`         // when session/cancel was sent, ms into the turn
	StopAfterMs      *int64   `json:"stop_after_cancel_ms,omitempty"` // cancel -> prompt response; absent if killed
	ProcsLive        []Proc   `json:"procs_live"`                     // process group at the end of the session, before shutdown
	ProcsAtCancel    []Proc   `json:"procs_at_cancel"`                // process group just before session/cancel was sent (last cancel)
	ProcsAfterCancel []Proc   `json:"procs_after_cancel"`             // process group 1 s after the cancel response (last cancel)
	Survivors        []Proc   `json:"survivors"`                      // first survivor sample: still in the group after stdin closed and the leader was reaped
	SurvivorSamples  []Sample `json:"survivor_samples"`               // every survivor sample, before the group kill

	// With --kill-tree: every descendant of the harness seen while it ran, in any
	// process group; those still alive at shutdown and killed by PID; and any of
	// those still alive 300 ms after the kill.
	TreeTracked    []Proc `json:"tree_tracked"`
	TreeKilled     []Proc `json:"tree_killed"`
	TreeAliveAfter []Proc `json:"tree_alive_after_kill"`
	LeaderExited   bool   `json:"leader_exited"` // the spawned process exited on its own after stdin closed
	ProcsError     string `json:"procs_error,omitempty"`
}

type config struct {
	harness, cmd, cwd, prompt string
	limit, grace, linger      time.Duration
	handshake                 bool            // stop after session/new: no prompt is sent, no tokens are spent
	mode                      string          // if set, session/set_mode to this mode id before prompting
	turns                     int             // prompts to send in the same session (default 1)
	steps                     []Step          // if set, replaces prompt/turns/limit; a cancelled step does not end the session
	sessionMeta               json.RawMessage // if set, sent as _meta on session/new
	survivorSamples           []time.Duration // when to list survivors after the leader exits (default 200 ms)
	killTree                  bool            // track descendants by parent PID and kill them all at shutdown
	pollEvery                 time.Duration   // how often to record descendants with killTree (default 500 ms)
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
	sentAt    time.Time    // zero until session/prompt is sent
	pgid      int          // process group of the spawned harness (also its PID)
	tracked   map[int]Proc // with killTree: descendants seen so far, by PID
	phase     string       // setup | turn | after
	turn      int          // current turn, 1-based
	lastOther map[int]int  // per turn: seq of the last non-usage update seen during it
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
					c.sum.TurnWallMs = append(c.sum.TurnWallMs, c.sum.WallMs)
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
		c.lastOther[c.turn] = c.sum.Updates
	}
}

func (c *client) observe(u Usage) { // caller holds c.mu
	u.seq, u.When, u.Turn = c.sum.Updates, c.phase, c.turn
	if !c.sentAt.IsZero() {
		u.TMs = time.Since(c.sentAt).Milliseconds()
	}
	c.sum.Usage = append(c.sum.Usage, u)
}

// run drives one harness through one prompt. The Summary is always filled in;
// err reports the step that failed, if any.
func run(cfg config, stdout, stderr io.Writer) (Summary, error) {
	c := &client{out: stdout, pending: map[string]chan msg{}, dead: make(chan struct{}), phase: "setup",
		lastOther: map[int]int{}}
	c.sum = Summary{Type: "summary", Harness: cfg.harness, Stage: "none", UpdateKinds: map[string]int{},
		Refused: map[string]int{}, Usage: []Usage{}, FirstUsage: "none", FirstCost: "none",
		TurnWallMs: []int64{}, TurnStops: []string{}, ProcsLive: []Proc{}, Survivors: []Proc{},
		ProcsAtCancel: []Proc{}, ProcsAfterCancel: []Proc{}, SurvivorSamples: []Sample{},
		TreeTracked: []Proc{}, TreeKilled: []Proc{}, TreeAliveAfter: []Proc{}}
	c.tracked = map[int]Proc{}
	err := c.drive(cfg, stderr)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.sum.Error = err.Error()
	}
	slices.SortStableFunc(c.sum.Usage, func(a, b Usage) int {
		return cmp.Or(cmp.Compare(a.Turn, b.Turn), cmp.Compare(a.TMs, b.TMs))
	})
	for i := range c.sum.Usage {
		u := &c.sum.Usage[i]
		switch {
		case u.When == "turn" && u.seq < c.lastOther[u.Turn]:
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
	cmd.WaitDelay = 2 * time.Second // a child holding stderr open must not hang Wait
	pgid := cmd.Process.Pid
	c.mu.Lock()
	c.pgid = pgid
	c.mu.Unlock()
	stopPoll := make(chan struct{})
	if cfg.killTree {
		every := cmp.Or(cfg.pollEvery, 500*time.Millisecond)
		go func() {
			t := time.NewTicker(every)
			defer t.Stop()
			for {
				c.pollTree()
				select {
				case <-stopPoll:
					return
				case <-t.C:
				}
			}
		}()
	}
	defer func() {
		if cfg.killTree {
			close(stopPoll)
			c.pollTree() // last look while the harness is still alive: an orphan's lineage is lost once its parent dies
		}
		// Close stdin and give the agent a moment to exit. Before the group kill,
		// record what is still in the process group, so the run shows whether the
		// kill was needed at all. A process that called setsid has left the group
		// and is not seen here.
		killedEarly := c.sum.Killed // the turn outlived session/cancel plus the grace period
		if killedEarly {
			syscall.Kill(-pgid, syscall.SIGKILL)
		}
		stdin.Close()
		select {
		case <-c.dead:
		case <-time.After(cfg.grace):
			c.sum.Killed = true
		}
		exited := make(chan struct{})
		go func() { cmd.Wait(); close(exited) }() // reaps the leader so it is not listed as a zombie
		select {
		case <-exited:
			c.mu.Lock()
			c.sum.LeaderExited = !killedEarly
			c.mu.Unlock()
		case <-time.After(cfg.grace):
		}
		if !killedEarly {
			samples := cfg.survivorSamples
			if len(samples) == 0 {
				samples = []time.Duration{200 * time.Millisecond} // let children notice the parent is gone
			}
			start := time.Now()
			for i, d := range samples {
				time.Sleep(time.Until(start.Add(d)))
				procs, perr := groupProcs(pgid)
				c.mu.Lock()
				c.sum.SurvivorSamples = append(c.sum.SurvivorSamples, Sample{AtMs: time.Since(start).Milliseconds(), Procs: procs})
				if i == 0 {
					c.sum.Survivors = procs
				}
				if perr != nil {
					c.sum.ProcsError = perr.Error()
				}
				c.mu.Unlock()
			}
		}
		syscall.Kill(-pgid, syscall.SIGKILL)
		if cfg.killTree {
			c.killTracked()
		}
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
		}
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

	newParams := obj{"cwd": c.root, "mcpServers": []any{}}
	if len(cfg.sessionMeta) > 0 {
		newParams["_meta"] = cfg.sessionMeta
	}
	res, err = c.await(c.call("session/new", newParams), setup)
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
	c.mu.Unlock()

	if cfg.mode != "" {
		if _, err := c.await(c.call("session/set_mode", obj{"sessionId": s.ID, "modeId": cfg.mode}), setup); err != nil {
			return fmt.Errorf("session/set_mode %q: %w", cfg.mode, err)
		}
		c.mu.Lock()
		c.sum.ModeSet = cfg.mode
		c.emit(obj{"type": "mode_set", "mode": cfg.mode})
		c.mu.Unlock()
	}

	if cfg.handshake {
		time.Sleep(cfg.linger) // let the adapter start whatever it starts for a session
		c.snapshot()
		return nil
	}

	steps, scripted := cfg.steps, len(cfg.steps) > 0
	if !scripted {
		for i := 0; i < max(cfg.turns, 1); i++ {
			steps = append(steps, Step{Limit: cfg.limit, Prompt: cfg.prompt})
		}
	}
	for i, st := range steps {
		if i > 0 {
			time.Sleep(cfg.linger) // catch updates that trail the previous response
		}
		done, err := c.prompt(cfg, s.ID, i+1, st)
		if err != nil {
			return err
		}
		if !done && !scripted { // cancelled: with --turns, do not start another turn
			break
		}
	}
	c.snapshot()
	time.Sleep(cfg.linger) // catch updates that trail the response
	return nil
}

// prompt runs one turn. done is false when the turn was cancelled.
func (c *client) prompt(cfg config, sessionID string, turn int, st Step) (done bool, err error) {
	c.mu.Lock()
	c.sentAt, c.phase, c.turn = time.Now(), "turn", turn
	c.mu.Unlock()
	call := c.call("session/prompt", obj{"sessionId": sessionID,
		"prompt": []obj{{"type": "text", "text": st.Prompt}}})
	res, err := c.await(call, st.Limit)
	cancelled := false
	if err == errTimeout {
		cancelled = true
		atCancel, _ := groupProcs(c.pgid) // the harness is mid-turn: its child processes are running now
		c.mu.Lock()
		c.sum.ProcsAtCancel = atCancel
		c.sum.TimedOut = true
		at := time.Since(c.sentAt).Milliseconds()
		c.sum.CancelAtMs = &at
		c.mu.Unlock()
		cancelSent := time.Now()
		c.write(msg{Method: "session/cancel", Params: json.RawMessage(fmt.Sprintf(`{"sessionId":%q}`, sessionID))})
		if res, err = c.await(call, cfg.grace); err != nil {
			c.mu.Lock()
			c.sum.Killed, c.sum.StopReason, c.sum.WallMs = true, "killed", time.Since(c.sentAt).Milliseconds()
			c.mu.Unlock()
			return false, fmt.Errorf("session/prompt: no response within %s of session/cancel: %w", cfg.grace, err)
		}
		stop := time.Since(cancelSent).Milliseconds()
		c.mu.Lock()
		c.sum.StopAfterMs = &stop
		c.mu.Unlock()
		time.Sleep(time.Second) // did the harness stop the tool it was running?
		after, _ := groupProcs(c.pgid)
		c.mu.Lock()
		c.sum.ProcsAfterCancel = after
		c.mu.Unlock()
	}
	if err != nil {
		return false, fmt.Errorf("session/prompt (turn %d): %w", turn, err)
	}
	var pr struct {
		StopReason string          `json:"stopReason"`
		Usage      json.RawMessage `json:"usage"` // unstable in ACP v1; captured if present
		Meta       json.RawMessage `json:"_meta"`
	}
	json.Unmarshal(res, &pr)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sum.Stage, c.sum.StopReason, c.sum.PromptMeta = "session/prompt", pr.StopReason, pr.Meta
	c.sum.TurnsDone++
	c.sum.TurnStops = append(c.sum.TurnStops, pr.StopReason)
	if len(pr.Usage) > 0 && string(pr.Usage) != "null" {
		c.observe(Usage{Source: "prompt_response", Tokens: pr.Usage})
		c.sum.Usage[len(c.sum.Usage)-1].TMs = c.sum.WallMs
	}
	return !cancelled, nil
}

// snapshot records the harness's process group while the session is live.
func (c *client) snapshot() {
	c.mu.Lock()
	pgid := c.pgid
	c.mu.Unlock()
	procs, err := groupProcs(pgid)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sum.ProcsLive = procs
	if err != nil {
		c.sum.ProcsError = err.Error()
	}
}

// groupProcs lists the processes in process group pgid. It uses ps, which
// accepts these flags on both macOS and Linux.
func groupProcs(pgid int) ([]Proc, error) {
	procs := []Proc{}
	if pgid <= 0 {
		return procs, nil
	}
	out, err := exec.Command("ps", "-A", "-o", "pid=", "-o", "pgid=", "-o", "command=").Output()
	if err != nil {
		return procs, fmt.Errorf("ps: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		g, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil || g != pgid {
			continue
		}
		cmdline := strings.Join(f[2:], " ")
		if len(cmdline) > 300 {
			cmdline = cmdline[:300] + "..."
		}
		procs = append(procs, Proc{PID: pid, Cmd: cmdline})
	}
	return procs, nil
}

type psEntry struct {
	ppid      int
	stat, cmd string
}

// psAll lists every process with its parent, state and command line. These ps
// flags work on both macOS and Linux.
func psAll() (map[int]psEntry, error) {
	out, err := exec.Command("ps", "-A", "-o", "pid=", "-o", "ppid=", "-o", "stat=", "-o", "command=").Output()
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	all := map[int]psEntry{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			continue
		}
		all[pid] = psEntry{ppid: ppid, stat: f[2], cmd: clip(strings.Join(f[3:], " "))}
	}
	return all, nil
}

func clip(s string) string {
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}

// pollTree records every current descendant of the harness, whatever its
// process group.
func (c *client) pollTree() {
	all, err := psAll()
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.sum.ProcsError = err.Error()
		return
	}
	children := map[int][]int{}
	for pid, e := range all {
		children[e.ppid] = append(children[e.ppid], pid)
	}
	queue := []int{c.pgid}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		for _, kid := range children[pid] {
			if _, seen := c.tracked[kid]; !seen && !strings.HasPrefix(all[kid].stat, "Z") {
				c.tracked[kid] = Proc{PID: kid, Cmd: all[kid].cmd}
			}
			queue = append(queue, kid)
		}
	}
}

// killTracked kills every tracked descendant that is still alive. A process is
// only killed if its command line still matches what was recorded, so a PID
// reused by an unrelated process is left alone.
func (c *client) killTracked() {
	c.mu.Lock()
	tracked := make([]Proc, 0, len(c.tracked))
	for _, p := range c.tracked {
		tracked = append(tracked, p)
	}
	c.mu.Unlock()
	slices.SortFunc(tracked, func(a, b Proc) int { return cmp.Compare(a.PID, b.PID) })
	all, err := psAll()
	var killed []Proc
	if err == nil {
		for _, p := range tracked {
			if e, ok := all[p.PID]; ok && !strings.HasPrefix(e.stat, "Z") && e.cmd == p.Cmd {
				syscall.Kill(p.PID, syscall.SIGKILL)
				killed = append(killed, p)
			}
		}
	}
	time.Sleep(300 * time.Millisecond)
	alive := []Proc{}
	if after, err2 := psAll(); err2 == nil {
		for _, p := range killed {
			if e, ok := after[p.PID]; ok && !strings.HasPrefix(e.stat, "Z") && e.cmd == p.Cmd {
				alive = append(alive, p)
			}
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sum.TreeTracked = tracked
	if killed != nil {
		c.sum.TreeKilled = killed
	}
	c.sum.TreeAliveAfter = alive
	if err != nil {
		c.sum.ProcsError = err.Error()
	}
}
