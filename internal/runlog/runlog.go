// Package runlog writes a run's events as JSONL, redacting what harnesses
// send.
//
// Harnesses send the account email, organisation and plan in auth
// notifications, and process listings hold local paths. Only session updates
// of known kinds are logged verbatim; everything else is logged as its name,
// its size and a hash. Large repeated updates, such as
// available_commands_update, are also logged as a size and a hash.
package runlog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"sync"
	"time"

	"refraktal.dev/refraktal/internal/proc"
)

// verbatim lists the session update kinds logged as sent. They carry the
// task's own content: messages, thoughts, tool calls, plans, modes, usage.
var verbatim = map[string]bool{
	"agent_message_chunk":  true,
	"agent_thought_chunk":  true,
	"user_message_chunk":   true,
	"tool_call":            true,
	"tool_call_update":     true,
	"plan":                 true,
	"current_mode_update":  true,
	"config_option_update": true,
	"usage_update":         true,
	"session_info_update":  true,
}

// MaxVerbatim is the size above which even an allowlisted update is logged as
// a size and a hash.
const MaxVerbatim = 64 << 10

// Log is one run's event log.
type Log struct {
	mu  sync.Mutex
	f   *os.File
	now func() time.Time
}

// Create creates the log file at path. It fails if the file exists.
func Create(path string) (*Log, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	return &Log{f: f, now: time.Now}, nil
}

// Close closes the file.
func (l *Log) Close() error { return l.f.Close() }

// Event writes one event. fields must hold only values that are safe to log:
// Refraktal's own data, never raw harness output.
func (l *Log) Event(event string, fields map[string]any) error {
	rec := make(map[string]any, len(fields)+2)
	for k, v := range fields {
		rec[k] = v
	}
	rec["event"] = event
	l.mu.Lock()
	defer l.mu.Unlock()
	rec["t"] = l.now().UTC().Format(time.RFC3339Nano)
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = l.f.Write(append(b, '\n'))
	return err
}

// Digest is how redacted data is logged: its size and a hash, so repeats and
// changes stay visible.
type Digest struct {
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// Of returns the digest of b.
func Of(b []byte) Digest {
	h := sha256.Sum256(b)
	return Digest{Bytes: len(b), SHA256: hex.EncodeToString(h[:])}
}

// Update logs one session update: verbatim if its kind is allowlisted and it
// is not too large, otherwise as a digest.
func (l *Log) Update(harness, sessionID, kind string, raw json.RawMessage) error {
	f := map[string]any{"harness": harness, "session": sessionID, "kind": kind}
	if verbatim[kind] && len(raw) <= MaxVerbatim && json.Valid(raw) {
		f["update"] = raw
	} else {
		f["digest"] = Of(raw)
	}
	return l.Event("update", f)
}

// Notification logs a notification other than session/update: its method and
// a digest only, because auth notifications carry account details.
func (l *Log) Notification(harness, method string, params json.RawMessage) error {
	return l.Event("notification", map[string]any{"harness": harness, "method": method, "digest": Of(params)})
}

// loggedProc is a process with its command line reduced to a digest.
type loggedProc struct {
	PID   int    `json:"pid"`
	PPID  int    `json:"ppid"`
	PGID  int    `json:"pgid"`
	Start string `json:"start"`
	Cmd   Digest `json:"cmd"`
}

// Procs logs a process listing with each command line as a digest, because
// command lines hold local paths and the names of the user's tools.
func (l *Log) Procs(event string, procs []proc.Proc) error {
	out := make([]loggedProc, 0, len(procs))
	for _, p := range procs {
		out = append(out, loggedProc{PID: p.PID, PPID: p.PPID, PGID: p.PGID, Start: p.Start, Cmd: Of([]byte(p.Cmd))})
	}
	return l.Event(event, map[string]any{"procs": out})
}
