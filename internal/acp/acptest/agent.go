// Package acptest is a scripted ACP agent for tests: no network, no
// credentials.
//
// A test binary becomes the agent when started with ACPTEST_MODE set. Call
// RunIfAgent first in TestMain, and start the agent with Command.
//
// Modes:
//
//	turn      a full turn: message chunks, a usage_update, a tool call with a
//	          permission request, an fs/read_text_file request, a final
//	          usage_update with cost, and end_turn with token usage
//	hang      never finishes the turn, but answers session/cancel with cancelled
//	permhang  asks permission, waits for the answer, then waits for
//	          session/cancel and ends the turn cancelled
//	deaf      ignores session/cancel and stdin closing; must be killed
//	detach    as turn, and first starts `sleep 30` in a process group of its
//	          own, as harness tools do; its PID goes to detach.pid in the cwd
//	noauth    session/new fails with -32000 Authentication required
//	oldproto  initialize answers with protocol version 2
//	auth      as turn, and first sends an auth/status_update carrying an email
//	slowstart answers initialize only after 3 s
//	selfcancel ends the turn with stop reason cancelled on its own
//	noread    stops reading stdin when a prompt arrives
//	bigframe  writes 17 MiB without a newline when a prompt arrives
//	tools     asks permission for three tool calls, shaped as Claude sends
//	          them, and performs each only if allowed: write hello.txt in the
//	          cwd, write ../outside.txt, and run git push (simulated: it writes
//	          pushed.txt in the cwd); then end_turn
//
// In turn mode, ACPTEST_WRITE ("path=content|...") writes files in the cwd,
// each after an edit permission request it must be allowed; ACPTEST_RAWWRITE
// (same format, paths relative to the cwd) writes without asking. A prompt that
// starts "You are judging" is answered with ACPTEST_JUDGE when set: the reply
// text, or "hang" to wait for session/cancel; "stop=<reason>\n<text>" ends
// the turn with that stop reason, and stop=error with an ACP error.
//
// session/new offers the modes in ACPTEST_MODES (comma-separated; default
// "default,plan,read-only"), and session/set_mode appends the mode set to
// ACPTEST_PROMPTS plus ".modes".
//
// session/new reports the model in ACPTEST_MODEL (default fake-model). With
// ACPTEST_NOCOST set, the turn reports tokens but no cost, as Codex and
// Antigravity do. In hang mode the cancelled response carries usage for a
// model call that finished before the cancel.
//
// If ACPTEST_PROMPTS names a file, every session/prompt appends a line to it,
// so a test can prove that no prompt was sent, and every session/new appends
// its cwd to the same name plus ".cwd".
//
// Message chunks carry the client's answers, so tests can check them: the
// permission answer as "permission=<raw response>" and the fs answer as
// "fs=<raw response>".
package acptest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const envMode = "ACPTEST_MODE"

// AuthEmail is the account email the auth mode sends; it must never reach a log.
const AuthEmail = "someone@example.com"

// RunIfAgent runs the fake agent and exits if this process was started by
// Command. Otherwise it returns at once.
func RunIfAgent() {
	mode := os.Getenv(envMode)
	if mode == "" {
		return
	}
	Serve(mode, os.Stdin, os.Stdout)
	os.Exit(0)
}

// Command returns the argv and extra environment that start the fake agent
// in the given mode, from the current test binary.
func Command(mode string) (argv, env []string) {
	self, err := os.Executable()
	if err != nil {
		panic(err)
	}
	// A -race binary sleeps 1 s on exit by default, which would make the agent
	// look slow to exit; this setting has no effect on other builds.
	return []string{self}, []string{envMode + "=" + mode, "GORACE=atexit_sleep_ms=0"}
}

type obj = map[string]any

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   any             `json:"error,omitempty"`
}

type agent struct {
	mode    string
	mu      sync.Mutex
	out     *bufio.Writer
	waiting map[string]chan string
	nextID  int
	cancel  chan struct{}
	cwd     string
}

// Serve runs the agent until in closes (in deaf mode, forever).
func Serve(mode string, in io.Reader, out io.Writer) {
	a := &agent{mode: mode, out: bufio.NewWriter(out), waiting: map[string]chan string{}, cancel: make(chan struct{})}
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var m message
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		a.handle(m, sc.Text())
	}
	if mode == "deaf" {
		time.Sleep(time.Hour)
	}
}

func (a *agent) send(m message) {
	m.JSONRPC = "2.0"
	b, _ := json.Marshal(m)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.out.Write(append(b, '\n'))
	a.out.Flush()
}

func (a *agent) update(u obj) {
	p, _ := json.Marshal(obj{"sessionId": "fake-1", "update": u})
	a.send(message{Method: "session/update", Params: p})
}

func (a *agent) chunk(text string) {
	a.update(obj{"sessionUpdate": "agent_message_chunk", "content": obj{"type": "text", "text": text}})
}

// ask sends a request to the client and returns its raw response line.
func (a *agent) ask(method string, params obj) string {
	a.mu.Lock()
	a.nextID++
	id := fmt.Sprintf("%q", fmt.Sprint("a", a.nextID)) // string IDs, to exercise the client
	ch := make(chan string, 1)
	a.waiting[id] = ch
	a.mu.Unlock()
	params["sessionId"] = "fake-1"
	p, _ := json.Marshal(params)
	a.send(message{ID: json.RawMessage(id), Method: method, Params: p})
	return <-ch
}

func (a *agent) handle(m message, line string) {
	switch m.Method {
	case "": // a response to one of our requests
		a.mu.Lock()
		ch := a.waiting[string(m.ID)]
		a.mu.Unlock()
		if ch != nil {
			ch <- line
		}
	case "initialize":
		if a.mode == "slowstart" { // answers initialize only after 3 s
			go func(id json.RawMessage) {
				time.Sleep(3 * time.Second)
				a.send(message{ID: id, Result: obj{"protocolVersion": 1, "agentInfo": obj{"name": "acptest", "version": "1.0.0"}, "agentCapabilities": obj{}}})
			}(m.ID)
			return
		}
		version := 1
		if a.mode == "oldproto" {
			version = 2
		}
		a.send(message{ID: m.ID, Result: obj{"protocolVersion": version,
			"agentInfo": obj{"name": "acptest", "version": "1.0.0"}, "agentCapabilities": obj{}, "authMethods": []any{}}})
	case "session/new":
		if a.mode == "noauth" {
			a.send(message{ID: m.ID, Error: obj{"code": -32000, "message": "Authentication required for " + AuthEmail}})
			return
		}
		var p struct {
			Cwd string `json:"cwd"`
		}
		json.Unmarshal(m.Params, &p)
		a.cwd = p.Cwd
		if f := os.Getenv("ACPTEST_PROMPTS"); f != "" { // the shell-prefix variables this agent was given, for tests
			os.WriteFile(f+".env", []byte("prefix="+os.Getenv("CLAUDE_CODE_SHELL_PREFIX")+" helper="+os.Getenv("REFRAKTAL_HELPER")+
				" nest="+os.Getenv("REFRAKTAL_NEST")+" keys="+keysSet()+"\n"), 0o600)
		}
		if f := os.Getenv("ACPTEST_PROMPTS"); f != "" { // also record each session folder, for tests
			if fh, err := os.OpenFile(f+".cwd", os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600); err == nil {
				fh.WriteString(p.Cwd + "\n")
				fh.Close()
			}
		}
		model := os.Getenv("ACPTEST_MODEL")
		if model == "" {
			model = "fake-model"
		}
		modes := []obj{}
		for _, id := range strings.Split(cmpOr(os.Getenv("ACPTEST_MODES"), "default,plan,read-only"), ",") {
			modes = append(modes, obj{"id": id, "name": id})
		}
		a.send(message{ID: m.ID, Result: obj{"sessionId": "fake-1",
			"modes":         obj{"currentModeId": "default", "availableModes": modes},
			"configOptions": []obj{{"id": "model", "category": "model", "currentValue": model}}}})
		a.update(obj{"sessionUpdate": "available_commands_update", "availableCommands": []obj{{"name": "review", "description": "Review"}}})
	case "session/set_mode":
		var p struct {
			ModeID string `json:"modeId"`
		}
		json.Unmarshal(m.Params, &p)
		if f := os.Getenv("ACPTEST_PROMPTS"); f != "" { // record each mode set, for tests
			if fh, err := os.OpenFile(f+".modes", os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600); err == nil {
				fh.WriteString(p.ModeID + "\n")
				fh.Close()
			}
		}
		a.send(message{ID: m.ID, Result: obj{}})
		a.update(obj{"sessionUpdate": "current_mode_update", "currentModeId": p.ModeID})
	case "session/prompt":
		if a.mode == "bigframe" { // writes past the frame limit without a newline, then waits
			chunk := make([]byte, 1<<20)
			for i := range chunk {
				chunk[i] = 'x'
			}
			a.mu.Lock()
			for i := 0; i < 17; i++ {
				a.out.Write(chunk)
			}
			a.out.Flush()
			a.mu.Unlock()
			time.Sleep(time.Hour)
		}
		if a.mode == "noread" { // stops reading stdin for good: the client's writes back up
			time.Sleep(time.Hour)
		}
		if f := os.Getenv("ACPTEST_PROMPTS"); f != "" {
			if fh, err := os.OpenFile(f, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600); err == nil {
				fh.WriteString("prompt\n")
				fh.Close()
			}
		}
		var pp struct {
			Prompt []struct {
				Text string `json:"text"`
			} `json:"prompt"`
		}
		json.Unmarshal(m.Params, &pp)
		judging := len(pp.Prompt) > 0 && strings.HasPrefix(pp.Prompt[0].Text, "You are judging")
		if j := os.Getenv("ACPTEST_JUDGE"); judging && j != "" {
			go a.judge(m.ID, j)
			return
		}
		go a.prompt(m.ID)
	case "session/cancel":
		if a.mode != "deaf" {
			select {
			case <-a.cancel:
			default:
				close(a.cancel)
			}
		}
	default:
		if m.ID != nil {
			a.send(message{ID: m.ID, Error: obj{"code": -32601, "message": "method not found"}})
		}
	}
}

func (a *agent) prompt(id json.RawMessage) {
	switch a.mode {
	case "agent-judge":
		var n tortureNode
		if json.Unmarshal([]byte(os.Getenv(tortureEnv)), &n) != nil {
			return
		}
		if err := n.readyTree(); err != nil {
			n.mark("error")
			return
		}
		n.mark("judge-started")
		<-a.cancel
		n.mark("judge-cancelled")
		if err := n.await("release-judge"); err != nil {
			n.mark("error")
			return
		}
		a.send(message{ID: id, Result: obj{"stopReason": "cancelled"}})
		return
	case "agent-tool", "agent-tool-change", "agent-edit", "agent-edit-tree":
		var n tortureNode
		if json.Unmarshal([]byte(os.Getenv(tortureEnv)), &n) != nil {
			return // the parent observes the missing tool-started marker
		}
		if a.mode != "agent-tool" {
			if os.WriteFile(filepath.Join(a.cwd, "README"), []byte("agent result\n"), 0o600) != nil ||
				os.WriteFile(filepath.Join(a.cwd, "tool-output.txt"), []byte("complete result\n"), 0o600) != nil {
				n.mark("error")
				return
			}
		}
		if a.mode == "agent-edit-tree" {
			if err := n.readyTree(); err != nil {
				n.mark("error")
				return
			}
		}
		if a.mode == "agent-edit" || a.mode == "agent-edit-tree" {
			a.send(message{ID: id, Result: obj{"stopReason": "end_turn"}})
			return
		}
		a.update(obj{"sessionUpdate": "tool_call", "toolCallId": "slow-tool", "title": "pwd", "kind": "execute", "status": "pending"})
		a.ask("session/request_permission", obj{"toolCall": obj{"toolCallId": "slow-tool", "kind": "execute", "rawInput": obj{"command": "pwd"}},
			"options": []obj{{"optionId": "allow", "kind": "allow_once"}, {"optionId": "reject", "kind": "reject_once"}}})
		if err := n.launch("cycle", 0, &syscall.SysProcAttr{Setpgid: true}, false); err != nil {
			n.mark("error")
			return
		}
		a.update(obj{"sessionUpdate": "tool_call_update", "toolCallId": "slow-tool", "status": "in_progress"})
		n.mark("tool-started")
		<-a.cancel
		n.mark("cancelled")
		a.send(message{ID: id, Result: obj{"stopReason": "cancelled"}})
		return
	case "selfcancel": // ends the turn as cancelled on its own, as Codex does after a denial
		a.update(obj{"sessionUpdate": "usage_update", "used": 4000, "size": 200000})
		a.send(message{ID: id, Result: obj{"stopReason": "cancelled", "usage": obj{"totalTokens": 300, "inputTokens": 200, "outputTokens": 100}}})
		return
	case "hang":
		a.chunk("working")
		a.update(obj{"sessionUpdate": "usage_update", "used": 5000, "size": 200000})
		<-a.cancel
		// A model call finished before the cancel; the one in flight is not reported.
		a.send(message{ID: id, Result: obj{"stopReason": "cancelled",
			"usage": obj{"totalTokens": 300, "inputTokens": 200, "outputTokens": 100}}})
		return
	case "deaf":
		a.chunk("working")
		time.Sleep(time.Hour)
	case "permhang":
		a.update(obj{"sessionUpdate": "tool_call", "toolCallId": "t1", "title": "touch marker.txt", "kind": "execute", "status": "pending"})
		a.chunk("permission=" + a.permission())
		<-a.cancel
		a.send(message{ID: id, Result: obj{"stopReason": "cancelled"}})
		return
	case "detach":
		kid := exec.Command("sleep", "30")
		kid.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if kid.Start() == nil {
			os.WriteFile(filepath.Join(a.cwd, "detach.pid"), []byte(fmt.Sprint(kid.Process.Pid)), 0o644)
		}
	case "auth":
		p, _ := json.Marshal(obj{"status": "authenticated", "account": obj{"email": AuthEmail, "plan": "max"}})
		a.send(message{Method: "auth/status_update", Params: p})
	case "tools":
		inside := filepath.Join(a.cwd, "hello.txt")
		outside := filepath.Join(a.cwd, "..", "outside.txt")
		steps := []struct {
			tool obj
			do   func()
		}{
			{obj{"toolCallId": "w1", "kind": "edit", "title": "Write hello.txt", "rawInput": obj{"file_path": inside, "content": "hello"},
				"locations": []obj{{"path": inside}}}, func() { os.WriteFile(inside, []byte("hello\n"), 0o644) }},
			{obj{"toolCallId": "w2", "kind": "edit", "title": "Write ../outside.txt", "rawInput": obj{"file_path": outside, "content": "x"},
				"locations": []obj{{"path": outside}}}, func() { os.WriteFile(outside, []byte("x\n"), 0o644) }},
			{obj{"toolCallId": "x1", "kind": "execute", "title": "git push origin HEAD", "rawInput": obj{"command": "git push origin HEAD"}},
				func() { os.WriteFile(filepath.Join(a.cwd, "pushed.txt"), []byte("pushed\n"), 0o644) }},
		}
		for _, st := range steps {
			a.update(obj{"sessionUpdate": "tool_call", "toolCallId": st.tool["toolCallId"], "title": st.tool["title"], "kind": st.tool["kind"], "status": "pending"})
			answer := a.ask("session/request_permission", obj{"toolCall": st.tool, "options": []obj{
				{"optionId": "allow", "name": "Allow", "kind": "allow_once"},
				{"optionId": "reject", "name": "Reject", "kind": "reject_once"}}})
			if strings.Contains(answer, `"optionId":"allow"`) {
				st.do()
			}
			a.chunk(fmt.Sprintf(" %v=%s", st.tool["toolCallId"], answer))
		}
		a.update(obj{"sessionUpdate": "usage_update", "used": 900, "size": 200000, "cost": obj{"amount": 0.01, "currency": "USD"}})
		a.send(message{ID: id, Result: obj{"stopReason": "end_turn", "usage": obj{"totalTokens": 900, "inputTokens": 800, "outputTokens": 100}}})
		return
	}
	a.chunk("Hello")
	a.writes()
	for _, f := range strings.Split(os.Getenv("ACPTEST_RAWDELETE"), "|") { // deletes without asking
		if f != "" {
			os.Remove(filepath.Join(a.cwd, f))
		}
	}
	if w := os.Getenv("ACPTEST_RAWWRITE"); w != "" { // writes without asking: a sandbox that failed
		for _, kv := range strings.Split(w, "|") {
			path, content, _ := strings.Cut(kv, "=")
			os.MkdirAll(filepath.Dir(filepath.Join(a.cwd, path)), 0o755)
			os.WriteFile(filepath.Join(a.cwd, path), []byte(content+"\n"), 0o644)
		}
	}
	a.update(obj{"sessionUpdate": "usage_update", "used": 1200, "size": 200000})
	a.update(obj{"sessionUpdate": "tool_call", "toolCallId": "t1", "title": "Write note.txt", "kind": "edit", "status": "pending"})
	a.chunk(" permission=" + a.permission())
	a.chunk(" fs=" + a.ask("fs/read_text_file", obj{"path": filepath.Join(a.cwd, "note.txt")}))
	a.update(obj{"sessionUpdate": "tool_call_update", "toolCallId": "t1", "status": "completed"})
	if os.Getenv("ACPTEST_NOCOST") != "" { // like Codex and Antigravity: tokens only
		a.update(obj{"sessionUpdate": "usage_update", "used": 1500, "size": 200000})
	} else {
		a.update(obj{"sessionUpdate": "usage_update", "used": 1500, "size": 200000, "cost": obj{"amount": 0.0123, "currency": "USD"}})
	}
	a.send(message{ID: id, Result: obj{"stopReason": "end_turn",
		"usage": obj{"totalTokens": 1500, "inputTokens": 1400, "outputTokens": 100, "cachedReadTokens": 1000}}})
}

// writes performs ACPTEST_WRITE ("path=content|path=content"), each only if
// an edit permission for it is allowed.
func (a *agent) writes() {
	w := os.Getenv("ACPTEST_WRITE")
	if w == "" {
		return
	}
	for i, kv := range strings.Split(w, "|") {
		path, content, _ := strings.Cut(kv, "=")
		full := filepath.Join(a.cwd, path)
		tool := obj{"toolCallId": fmt.Sprintf("e%d", i), "kind": "edit", "title": "Write " + path,
			"rawInput": obj{"file_path": full, "content": content}, "locations": []obj{{"path": full}}}
		answer := a.ask("session/request_permission", obj{"toolCall": tool, "options": []obj{
			{"optionId": "allow", "name": "Allow", "kind": "allow_once"},
			{"optionId": "reject", "name": "Reject", "kind": "reject_once"}}})
		if strings.Contains(answer, `"optionId":"allow"`) {
			os.MkdirAll(filepath.Dir(full), 0o755)
			os.WriteFile(full, []byte(content+"\n"), 0o644)
		}
	}
}

// judge answers a judging prompt: "hang" waits for session/cancel; anything
// else is the reply. It tries ACPTEST_WRITE first, which a read-only judge
// must be denied.
func (a *agent) judge(id json.RawMessage, reply string) {
	a.writes()
	if f := os.Getenv("ACPTEST_JUDGE_RAWWRITE"); f != "" { // writes without asking: a sandbox that failed
		os.WriteFile(filepath.Join(a.cwd, f), []byte("judge was here\n"), 0o644)
	}
	if reply == "hang" {
		a.chunk("thinking")
		<-a.cancel
		a.send(message{ID: id, Result: obj{"stopReason": "cancelled"}})
		return
	}
	stop := "end_turn"
	if rest, ok := strings.CutPrefix(reply, "stop="); ok { // "stop=<reason>\n<reply>"
		stop, reply, _ = strings.Cut(rest, "\n")
	}
	a.chunk(reply)
	if stop == "error" {
		a.send(message{ID: id, Error: obj{"code": -32603, "message": "internal error"}})
		return
	}
	a.update(obj{"sessionUpdate": "usage_update", "used": 700, "size": 200000, "cost": obj{"amount": 0.002, "currency": "USD"}})
	a.send(message{ID: id, Result: obj{"stopReason": stop, "usage": obj{"totalTokens": 700, "inputTokens": 600, "outputTokens": 100}}})
}

func (a *agent) permission() string {
	return a.ask("session/request_permission", obj{
		"toolCall": obj{"toolCallId": "t1"},
		"options": []obj{
			{"optionId": "always", "name": "Always allow", "kind": "allow_always"},
			{"optionId": "once", "name": "Allow", "kind": "allow_once"},
			{"optionId": "no", "name": "Reject", "kind": "reject_once"},
		}})
}

// keysSet names the provider API keys present in the agent's environment.
func keysSet() string {
	var set []string
	for _, k := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY"} {
		if os.Getenv(k) != "" {
			set = append(set, k)
		}
	}
	return strings.Join(set, ",")
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
