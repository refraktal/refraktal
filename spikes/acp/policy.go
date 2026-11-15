package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// request applies the no-human policy to an agent-to-client request.
func (c *client) request(m msg) (json.RawMessage, *rpcErr) {
	var p struct {
		Path    string  `json:"path"`
		Content *string `json:"content"`
		Line    int     `json:"line"`
		Limit   int     `json:"limit"`
		Options []struct {
			ID   string `json:"optionId"`
			Kind string `json:"kind"`
		} `json:"options"`
	}
	json.Unmarshal(m.Params, &p)
	c.mu.Lock()
	defer c.mu.Unlock()
	refuse := func(code int, text string) (json.RawMessage, *rpcErr) {
		c.sum.Refused[m.Method]++
		c.emit(obj{"type": "request", "method": m.Method, "params": m.Params, "refused": text})
		return nil, &rpcErr{Code: code, Message: text}
	}
	reply := func(v any) (json.RawMessage, *rpcErr) {
		c.emit(obj{"type": "request", "method": m.Method, "params": m.Params, "reply": v})
		b, _ := json.Marshal(v)
		return b, nil
	}
	switch m.Method {
	case "session/request_permission":
		c.sum.Permissions++
		if c.sum.TimedOut { // the spec requires this outcome once session/cancel is sent
			return reply(obj{"outcome": obj{"outcome": "cancelled"}})
		}
		for _, kind := range []string{"allow_once", "allow_always"} {
			for _, o := range p.Options {
				if o.Kind == kind {
					return reply(obj{"outcome": obj{"outcome": "selected", "optionId": o.ID}})
				}
			}
		}
		return refuse(-32602, "no allow option offered")
	case "fs/read_text_file", "fs/write_text_file":
		path, err := c.confine(p.Path)
		if err != nil {
			return refuse(-32602, err.Error())
		}
		c.sum.FSRequests++
		if m.Method == "fs/write_text_file" {
			if p.Content == nil {
				return refuse(-32602, "missing content")
			}
			if err := os.WriteFile(path, []byte(*p.Content), 0o644); err != nil {
				return refuse(-32603, err.Error())
			}
			return reply(obj{})
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return refuse(-32002, err.Error())
		}
		text := string(b)
		if p.Line > 1 || p.Limit > 0 { // line is 1-based
			lines := strings.SplitAfter(text, "\n")
			lines = lines[min(max(p.Line-1, 0), len(lines)):]
			if p.Limit > 0 {
				lines = lines[:min(p.Limit, len(lines))]
			}
			text = strings.Join(lines, "")
		}
		return reply(obj{"content": text})
	}
	return refuse(-32601, "method not supported by this client: "+m.Method)
}

// confine resolves p (symlinks included) and rejects anything outside the session cwd.
func (c *client) confine(p string) (string, error) {
	if !filepath.IsAbs(p) {
		return "", errors.New("path must be absolute")
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil { // may not exist yet (a write): resolve the parent instead
		dir, derr := filepath.EvalSymlinks(filepath.Dir(p))
		if derr != nil {
			return "", derr
		}
		real = filepath.Join(dir, filepath.Base(p))
	}
	if rel, err := filepath.Rel(c.root, real); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("path is outside the session cwd")
	}
	return real, nil
}
