// fakeagent: a scripted ACP agent for testing the client with no network and
// no credentials. The script is chosen with FAKE_MODE (or the first argument):
//
//	(default)  two message chunks, a usage_update mid-turn, one permission
//	           request, a usage_update with cost at the end, stopReason end_turn
//	fs         as default, and also asks the client to write and read a file in
//	           the cwd, read a file outside it, and create a terminal
//	endusage   only one usage_update (with cost), as the last update of the turn
//	late       the only usage_update arrives after the prompt response, and the
//	           response carries the unstable `usage` token counts
//	hang       never finishes, but answers session/cancel with stopReason cancelled
//	deaf       never finishes and ignores session/cancel (must be killed)
//	noauth     session/new fails with -32000 Authentication required
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type msg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   any             `json:"error,omitempty"`
}

type obj = map[string]any

var (
	mu      sync.Mutex
	out     = bufio.NewWriter(os.Stdout)
	waiting = map[string]chan json.RawMessage{} // our requests to the client, by id
	nextID  int
	cancel  = make(chan struct{})
	cwd     string
)

func send(m msg) {
	m.JSONRPC = "2.0"
	b, _ := json.Marshal(m)
	mu.Lock()
	defer mu.Unlock()
	out.Write(append(b, '\n'))
	out.Flush()
}

func update(u obj) {
	p, _ := json.Marshal(obj{"sessionId": "fake-1", "update": u})
	send(msg{Method: "session/update", Params: p})
}

func chunk(text string) {
	update(obj{"sessionUpdate": "agent_message_chunk", "content": obj{"type": "text", "text": text}})
}

// ask sends a request to the client and returns the raw response line.
func ask(method string, params obj) string {
	mu.Lock()
	nextID++
	id := fmt.Sprintf("%q", fmt.Sprint("a", nextID)) // string ids, to exercise the client
	ch := make(chan json.RawMessage, 1)
	waiting[id] = ch
	mu.Unlock()
	params["sessionId"] = "fake-1"
	p, _ := json.Marshal(params)
	send(msg{ID: json.RawMessage(id), Method: method, Params: p})
	return string(<-ch)
}

func prompt(id json.RawMessage, mode string) {
	chunk("Hello")
	switch mode {
	case "hang":
		<-cancel
		send(msg{ID: id, Result: obj{"stopReason": "cancelled"}})
		return
	case "deaf":
		os.WriteFile(filepath.Join(cwd, "pid"), []byte(fmt.Sprint(os.Getpid())), 0o644)
		time.Sleep(time.Hour)
	case "endusage":
		chunk(" world")
		update(obj{"sessionUpdate": "usage_update", "used": 900, "size": 200000, "cost": obj{"amount": 0.01, "currency": "USD"}})
		send(msg{ID: id, Result: obj{"stopReason": "end_turn"}})
		return
	case "late":
		send(msg{ID: id, Result: obj{"stopReason": "end_turn", "usage": obj{"totalTokens": 30, "inputTokens": 20, "outputTokens": 10}}})
		time.Sleep(50 * time.Millisecond)
		update(obj{"sessionUpdate": "usage_update", "used": 30, "size": 200000})
		return
	}
	update(obj{"sessionUpdate": "usage_update", "used": 1200, "size": 200000})
	update(obj{"sessionUpdate": "tool_call", "toolCallId": "t1", "title": "Write note.txt", "kind": "edit", "status": "pending"})
	chunk(" permission=" + ask("session/request_permission", obj{
		"toolCall": obj{"toolCallId": "t1"},
		"options": []obj{
			{"optionId": "always", "name": "Always allow", "kind": "allow_always"},
			{"optionId": "once", "name": "Allow", "kind": "allow_once"},
			{"optionId": "no", "name": "Reject", "kind": "reject_once"},
		}}))
	if mode == "fs" {
		note := filepath.Join(cwd, "note.txt")
		chunk(" write=" + ask("fs/write_text_file", obj{"path": note, "content": "one\ntwo\nthree\n"}))
		chunk(" read=" + ask("fs/read_text_file", obj{"path": note, "line": 2, "limit": 1}))
		chunk(" outside=" + ask("fs/read_text_file", obj{"path": "/etc/hostname"}))
		chunk(" escape=" + ask("fs/write_text_file", obj{"path": filepath.Join(cwd, "..", "escaped.txt"), "content": "x"}))
		chunk(" terminal=" + ask("terminal/create", obj{"command": "id"}))
	}
	update(obj{"sessionUpdate": "tool_call_update", "toolCallId": "t1", "status": "completed"})
	update(obj{"sessionUpdate": "usage_update", "used": 1500, "size": 200000, "cost": obj{"amount": 0.0123, "currency": "USD"}})
	send(msg{ID: id, Result: obj{"stopReason": "end_turn"}})
}

func main() {
	mode := os.Getenv("FAKE_MODE")
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<24)
	for in.Scan() {
		var m msg
		var raw struct {
			Result json.RawMessage `json:"result"`
			Cwd    string          `json:"cwd"`
		}
		if json.Unmarshal(in.Bytes(), &m) != nil {
			continue
		}
		json.Unmarshal(m.Params, &raw)
		switch m.Method {
		case "": // a response to one of our requests: hand the whole line back
			mu.Lock()
			ch := waiting[string(m.ID)]
			mu.Unlock()
			if ch != nil {
				ch <- append(json.RawMessage(nil), in.Bytes()...)
			}
		case "initialize":
			send(msg{ID: m.ID, Result: obj{"protocolVersion": 1,
				"agentInfo":         obj{"name": "fakeagent", "version": "0.1.0"},
				"agentCapabilities": obj{"loadSession": false},
				"authMethods":       []any{}}})
		case "session/new":
			if mode == "noauth" {
				send(msg{ID: m.ID, Error: obj{"code": -32000, "message": "Authentication required"}})
				continue
			}
			cwd = raw.Cwd
			send(msg{ID: m.ID, Result: obj{"sessionId": "fake-1"}})
			update(obj{"sessionUpdate": "available_commands_update", "availableCommands": []any{}})
		case "session/prompt":
			go prompt(m.ID, mode)
		case "session/cancel":
			if mode != "deaf" {
				close(cancel)
			}
		default:
			if m.ID != nil {
				send(msg{ID: m.ID, Error: obj{"code": -32601, "message": "Method not found"}})
			}
		}
	}
	if mode == "deaf" {
		time.Sleep(time.Hour) // not even closing stdin stops it
	}
}
