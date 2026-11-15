// Command livecheck runs M1's open tests against live harnesses. It is a
// developer script, not part of the CLI; run it through scripts/livecheck.sh.
//
//	livecheck -test kill   -harness claude -cmd "<acp server command>"
//	livecheck -test orphan -harness agy    -cmd "agy acp" -runs 3
//
// The kill test sends a prompt that starts `sleep 120 &`. After the turn it
// shuts down and records whether the sleep was tracked and killed.
//
// The orphan test sends a prompt that runs `sleep 20` and cancels 2 s after
// the sleep is first seen (30 s at most). It then shuts down and records
// whether the sleep outlived the harness.
//
// Every request for permission is approved: prompts are trivial and each run
// gets a fresh folder under .live/work. Output goes to .live/results, which is
// gitignored: it holds local paths and must never be committed.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"refraktal.dev/refraktal/internal/acp"
	"refraktal.dev/refraktal/internal/proc"
	"refraktal.dev/refraktal/internal/runlog"
)

const (
	killPrompt   = "Run this shell command exactly as written: sleep 120 &   Then reply DONE."
	rejectPrompt = "Run the shell command: touch marker.txt   Then reply DONE."
	orphanPrompt = "First run the shell command: sleep 20   After it finishes, create a file named hello.txt in the current directory containing the single word hello, then reply DONE."
)

// modes are the per-harness permission modes from docs/DESIGN.md.
var modes = map[string]string{"claude": "default", "codex": "workspace-write", "agy": "default"}

// gated are the modes in which each harness asks before `touch` (round 4).
var gated = map[string]string{"claude": "default", "codex": "read-only", "agy": "default"}

type result struct {
	Test             string `json:"test"`
	Harness          string `json:"harness"`
	Run              int    `json:"run"`
	Error            string `json:"error,omitempty"`
	StopReason       string `json:"stop_reason"`
	Cancelled        bool   `json:"cancelled"`
	CancelledMidTool bool   `json:"cancelled_mid_tool"` // orphan: the cancel came 2 s after the tool was first seen
	CancelMs         int64  `json:"cancel_to_response_ms,omitempty"`
	Perms            int    `json:"permission_requests"`
	TurnMs           int64  `json:"turn_ms"`

	ToolSeen        bool  `json:"tool_seen"`             // the sleep appeared on the machine
	ToolSeenPIDs    int   `json:"tool_seen_pids"`        // distinct PIDs seen running it
	ToolLifeMs      int64 `json:"tool_observed_life_ms"` // first to last sighting, by the watcher
	ToolTracked     bool  `json:"tool_tracked"`          // it was in the tracked set
	ToolAliveBefore bool  `json:"tool_alive_before"`     // running just before shutdown began
	ToolKilled      bool  `json:"tool_killed"`           // killed by the identity-checked tree kill
	ToolGone        bool  `json:"tool_gone"`             // had already exited at the kill
	ToolAliveAfter  int   `json:"tool_alive_after"`      // matching processes on the machine 1 s after shutdown

	ExitedOnItsOwn bool                `json:"harness_exited_on_its_own"`
	Shutdown       proc.ShutdownReport `json:"shutdown"` // local paths: results stay gitignored
}

func main() {
	test := flag.String("test", "", "kill or orphan")
	harness := flag.String("harness", "", "claude, codex or agy")
	cmdline := flag.String("cmd", "", "the harness's ACP server command")
	runs := flag.Int("runs", 1, "how many runs")
	root := flag.String("root", ".live", "folder for work dirs and results (never under $TMPDIR)")
	every := flag.Duration("watch", 200*time.Millisecond, "how often the watcher looks for the tool on the machine")
	flag.Parse()
	if *test == "reject" && modes[*harness] != "" && *cmdline != "" {
		r := reject(*harness, strings.Fields(*cmdline), *root)
		fmt.Printf("reject %s: mode=%s stop=%s perms=%d marker_created=%v tool_statuses=%v err=%q\n   reply: %q\n",
			r.Harness, r.Mode, r.StopReason, r.Perms, r.Marker, r.ToolStatuses, r.Error, r.Reply)
		return
	}
	if (*test != "kill" && *test != "orphan") || modes[*harness] == "" || *cmdline == "" {
		flag.Usage()
		os.Exit(2)
	}
	tool := map[string]string{"kill": "sleep 120", "orphan": "sleep 20"}[*test]
	if n := matching(tool); n > 0 {
		fmt.Fprintf(os.Stderr, "%d %q already running on this machine: results would be ambiguous\n", n, tool)
		os.Exit(1)
	}
	for i := 1; i <= *runs; i++ {
		r := once(*test, *harness, strings.Fields(*cmdline), *root, i, tool, *every)
		fmt.Printf("%s %s run %d: stop=%s cancelled=%v midtool=%v perms=%d seen=%v(pids=%d life=%dms) tracked=%v alive_before=%v killed=%v gone=%v alive_after=%d exited_on_own=%v err=%q\n",
			r.Test, r.Harness, r.Run, r.StopReason, r.Cancelled, r.CancelledMidTool, r.Perms, r.ToolSeen, r.ToolSeenPIDs, r.ToolLifeMs, r.ToolTracked, r.ToolAliveBefore,
			r.ToolKilled, r.ToolGone, r.ToolAliveAfter, r.ExitedOnItsOwn, r.Error)
		if r.ToolAliveAfter > 0 { // never kill by pattern: wait for it to end on its own
			fmt.Fprintf(os.Stderr, "waiting for the leftover %q to end on its own\n", tool)
			for matching(tool) > 0 {
				time.Sleep(2 * time.Second)
			}
		}
	}
}

// matching counts processes on the machine whose command line is exactly cmd.
// It only reads; nothing is ever killed by name.
func matching(cmd string) int {
	all, err := proc.List()
	if err != nil {
		return 0
	}
	n := 0
	for _, p := range all {
		if p.Cmd == cmd {
			n++
		}
	}
	return n
}

func once(test, harness string, argv []string, root string, run int, tool string, every time.Duration) (r result) {
	r = result{Test: test, Harness: harness, Run: run}
	label := fmt.Sprintf("%s-%s-%d-%s", test, harness, run, time.Now().Format("150405"))
	results := filepath.Join(root, "results")
	work, err := filepath.Abs(filepath.Join(root, "work", label))
	if err == nil {
		err = os.MkdirAll(work, 0o755)
	}
	if err == nil {
		err = os.MkdirAll(results, 0o755)
	}
	if err != nil {
		r.Error = err.Error()
		return r
	}
	defer os.RemoveAll(work)
	defer func() {
		b, _ := json.MarshalIndent(r, "", "  ")
		os.WriteFile(filepath.Join(results, label+".json"), b, 0o600)
	}()

	log, err := runlog.Create(filepath.Join(results, label+".events.jsonl"))
	if err != nil {
		r.Error = err.Error()
		return r
	}
	defer log.Close()
	stderr, _ := os.Create(filepath.Join(results, label+".stderr"))
	defer stderr.Close()

	child, err := proc.Start(proc.Spec{Argv: argv, Dir: work, Stderr: stderr})
	if err != nil {
		r.Error = err.Error()
		return r
	}
	w := &watcher{cmd: tool, every: every, stop: make(chan struct{}), done: make(chan struct{}), pids: map[int]bool{}, seen: make(chan struct{})}
	go w.run()
	client := acp.New(child.Stdout, child.Stdin, acp.Options{
		ClientName: "refraktal-livecheck", ClientVersion: "m1",
		CancelGrace:    15 * time.Second,
		OnUpdate:       func(u acp.Update) { log.Update(harness, u.SessionID, u.Kind, u.Raw) },
		OnNotification: func(m string, p json.RawMessage) { log.Notification(harness, m, p) },
		OnPermission: func(_ context.Context, req acp.PermissionRequest) string {
			log.Event("permission", map[string]any{"tool_kind": req.ToolCall.Kind, "options": len(req.Options)})
			if id := req.Option("allow_once"); id != "" {
				return id
			}
			return req.Option("allow_always")
		},
	})

	err = drive(client, test, harness, work, w.seen, &r)
	if err != nil {
		r.Error = err.Error()
	}
	time.Sleep(2 * time.Second) // let the harness settle after the turn
	r.ToolAliveBefore = matchingTracked(child, tool)
	r.Shutdown = child.Shutdown(10 * time.Second)
	r.ExitedOnItsOwn = r.Shutdown.ExitedOnItsOwn
	close(w.stop)
	<-w.done
	r.ToolSeen, r.ToolSeenPIDs = len(w.pids) > 0, len(w.pids)
	if r.ToolSeen {
		r.ToolLifeMs = w.last.Sub(w.first).Milliseconds()
	}
	for _, p := range r.Shutdown.Tracked {
		r.ToolTracked = r.ToolTracked || p.Cmd == tool
	}
	for _, p := range r.Shutdown.Kill.Killed {
		r.ToolKilled = r.ToolKilled || p.Cmd == tool
	}
	for _, p := range r.Shutdown.Kill.Gone {
		r.ToolGone = r.ToolGone || p.Cmd == tool
	}
	time.Sleep(time.Second)
	r.ToolAliveAfter = matching(tool)
	return r
}

func drive(c *acp.Client, test, harness, work string, toolSeen <-chan struct{}, r *result) error {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := c.Initialize(ctx); err != nil {
		return err
	}
	s, err := c.NewSession(ctx, work, nil)
	if err != nil {
		return err
	}
	if err := c.SetMode(ctx, s.ID, modes[harness]); err != nil {
		return err
	}
	text, limit := killPrompt, 90*time.Second
	if test == "orphan" {
		text, limit = orphanPrompt, 30*time.Second
	}
	tctx, tcancel := context.WithTimeout(context.Background(), limit)
	defer tcancel()
	var cancelAt atomic.Int64 // unix nanoseconds; set by the goroutine below
	if test == "orphan" {     // cancel mid-tool: 2 s after the sleep is first seen
		go func() {
			select {
			case <-toolSeen:
				time.Sleep(2 * time.Second)
				cancelAt.Store(time.Now().UnixNano())
				tcancel()
			case <-tctx.Done():
			}
		}()
	}
	turn, err := c.Prompt(tctx, s.ID, text)
	r.StopReason, r.Cancelled, r.Perms, r.TurnMs = turn.StopReason, turn.Cancelled, turn.Permissions, turn.Wall.Milliseconds()
	if at := cancelAt.Load(); at != 0 && turn.Cancelled {
		r.CancelledMidTool = true
		r.CancelMs = time.Since(time.Unix(0, at)).Milliseconds()
	}
	if errors.Is(err, acp.ErrCancelIgnored) {
		return fmt.Errorf("harness ignored session/cancel: %w", err)
	}
	return err
}

// watcher records every sighting of a process running cmd anywhere on the
// machine, so a tool that escaped tracking, or lived only briefly, still shows
// up in the result. It only reads.
type watcher struct {
	cmd         string
	every       time.Duration
	stop, done  chan struct{}
	pids        map[int]bool
	first, last time.Time
	seen        chan struct{} // closed at the first sighting
}

func (w *watcher) run() {
	defer close(w.done)
	for {
		if all, err := proc.List(); err == nil {
			for _, p := range all {
				if p.Cmd == w.cmd {
					if len(w.pids) == 0 {
						w.first = time.Now()
						close(w.seen)
					}
					w.pids[p.PID], w.last = true, time.Now()
				}
			}
		}
		select {
		case <-w.stop:
			return
		case <-time.After(w.every):
		}
	}
}

func matchingTracked(c *proc.Child, cmd string) bool {
	all, err := proc.List()
	if err != nil {
		return false
	}
	for _, p := range c.Tracked() {
		if p.Cmd == cmd && all[p.PID].Same(p) {
			return true
		}
	}
	return false
}

type rejectResult struct {
	Harness      string   `json:"harness"`
	Mode         string   `json:"mode"`
	StopReason   string   `json:"stop_reason"`
	Perms        int      `json:"permission_requests"`
	Marker       bool     `json:"marker_created"`
	ToolStatuses []string `json:"tool_statuses"` // statuses of tool_call and tool_call_update, in order
	Reply        string   `json:"reply"`
	Error        string   `json:"error,omitempty"`
}

// reject runs the touch prompt in the harness's gated mode and rejects every
// permission request, to see how the harness reports a rejection.
func reject(harness string, argv []string, root string) (r rejectResult) {
	r = rejectResult{Harness: harness, Mode: gated[harness], ToolStatuses: []string{}}
	label := fmt.Sprintf("reject-%s-%s", harness, time.Now().Format("150405"))
	results := filepath.Join(root, "results")
	work, err := filepath.Abs(filepath.Join(root, "work", label))
	if err == nil {
		err = os.MkdirAll(work, 0o755)
	}
	os.MkdirAll(results, 0o755)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	defer os.RemoveAll(work)
	defer func() {
		b, _ := json.MarshalIndent(r, "", "  ")
		os.WriteFile(filepath.Join(results, label+".json"), b, 0o600)
	}()
	log, err := runlog.Create(filepath.Join(results, label+".events.jsonl"))
	if err != nil {
		r.Error = err.Error()
		return r
	}
	defer log.Close()
	stderr, _ := os.Create(filepath.Join(results, label+".stderr"))
	defer stderr.Close()
	child, err := proc.Start(proc.Spec{Argv: argv, Dir: work, Stderr: stderr})
	if err != nil {
		r.Error = err.Error()
		return r
	}
	defer child.Shutdown(10 * time.Second)
	var mu sync.Mutex
	var reply strings.Builder
	c := acp.New(child.Stdout, child.Stdin, acp.Options{ClientName: "refraktal-livecheck", ClientVersion: "m3",
		OnUpdate: func(u acp.Update) {
			log.Update(harness, u.SessionID, u.Kind, u.Raw)
			var v struct {
				Status  string `json:"status"`
				Content struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			}
			json.Unmarshal(u.Raw, &v)
			mu.Lock()
			defer mu.Unlock()
			switch u.Kind {
			case "agent_message_chunk":
				reply.WriteString(v.Content.Text)
			case "tool_call", "tool_call_update":
				if v.Status != "" {
					r.ToolStatuses = append(r.ToolStatuses, u.Kind+":"+v.Status)
				}
			}
		},
		OnNotification: func(m string, p json.RawMessage) { log.Notification(harness, m, p) },
		OnPermission: func(_ context.Context, req acp.PermissionRequest) string {
			log.Event("permission", map[string]any{"tool_kind": req.ToolCall.Kind, "decision": "reject"})
			if id := req.Option("reject_once"); id != "" {
				return id
			}
			return req.Option("reject_always")
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if _, err := c.Initialize(ctx); err != nil {
		r.Error = err.Error()
		return r
	}
	s, err := c.NewSession(ctx, work, nil)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	if err := c.SetMode(ctx, s.ID, r.Mode); err != nil {
		r.Error = err.Error()
		return r
	}
	turn, err := c.Prompt(ctx, s.ID, rejectPrompt)
	r.StopReason, r.Perms = turn.StopReason, turn.Permissions
	if err != nil {
		r.Error = err.Error()
	}
	time.Sleep(2 * time.Second)
	_, statErr := os.Stat(filepath.Join(work, "marker.txt"))
	r.Marker = statErr == nil
	mu.Lock()
	r.Reply = strings.TrimSpace(reply.String())
	mu.Unlock()
	return r
}
