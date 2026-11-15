package proc

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// A live record is one file per running child in a shared folder (by default
// os.UserConfigDir()/refraktal/live). It names the Refraktal process that owns
// the child and every process tracked so far. The owner removes it after a
// clean shutdown; if the owner dies first, CleanLeftovers kills what the
// record lists, after the usual identity check.

// Record is the content of a live record.
type Record struct {
	Owner        Proc              `json:"owner"`                   // the Refraktal process that started the child
	Child        Proc              `json:"child"`                   // the harness process itself
	Session      bool              `json:"session"`                 // the child led a session of its own: sweep it
	SessionsFile string            `json:"sessions_file,omitempty"` // sessions its tool commands ran in (shell-prefix helper)
	Tracked      []Proc            `json:"tracked"`                 // its descendants seen so far
	Labels       map[string]string `json:"labels,omitempty"`
}

// DefaultLiveDir is os.UserConfigDir()/refraktal/live.
func DefaultLiveDir() (string, error) {
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "refraktal", "live"), nil
}

func writeRecord(path string, r Record) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path) // readers never see a half-written record
}

// CleanupReport says what CleanLeftovers found and did.
type CleanupReport struct {
	Records int        `json:"records"` // live records found
	Live    int        `json:"live"`    // owned by a Refraktal process that is still running: left alone
	Dead    int        `json:"dead"`    // owned by a process that is gone: cleaned up and removed
	Kill    KillReport `json:"kill"`    // what happened to the processes the dead records listed
	Errors  []string   `json:"errors"`
}

// CleanLeftovers kills the processes listed in records whose owner is no
// longer running, then removes those records. Each process is killed only if
// it still matches its record (PID, start time and command line).
func CleanLeftovers(dir string) (CleanupReport, error) {
	r := CleanupReport{Kill: KillReport{Killed: []Proc{}, Gone: []Proc{}, Changed: []Proc{}, Alive: []Proc{}}, Errors: []string{}}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	all, err := List()
	if err != nil {
		return r, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		r.Records++
		var rec Record
		b, err := os.ReadFile(path)
		if err == nil {
			err = json.Unmarshal(b, &rec)
		}
		if err != nil {
			r.Errors = append(r.Errors, fmt.Sprintf("%s: %v", e.Name(), err))
			continue
		}
		if now, ok := all[rec.Owner.PID]; ok && now.Same(rec.Owner) {
			r.Live++
			continue
		}
		r.Dead++
		kr, err := Kill(append(rec.Tracked, rec.Child))
		if err != nil {
			r.Errors = append(r.Errors, fmt.Sprintf("%s: %v", e.Name(), err))
			continue
		}
		r.Kill.Killed = append(r.Kill.Killed, kr.Killed...)
		r.Kill.Gone = append(r.Kill.Gone, kr.Gone...)
		r.Kill.Changed = append(r.Kill.Changed, kr.Changed...)
		r.Kill.Alive = append(r.Kill.Alive, kr.Alive...)
		if rec.Session {
			sr, err := SweepSession(rec.Child)
			if err != nil && !errors.Is(err, ErrSessionReused) {
				r.Errors = append(r.Errors, fmt.Sprintf("%s: %v", e.Name(), err))
			}
			r.Kill.Killed = append(r.Kill.Killed, sr.Killed...)
			r.Kill.Alive = append(r.Kill.Alive, sr.Alive...)
			kr.Alive = append(kr.Alive, sr.Alive...)
		}
		if rec.SessionsFile != "" {
			sr, err := sweepRecorded(rec.SessionsFile)
			if err != nil {
				r.Errors = append(r.Errors, fmt.Sprintf("%s: %v", e.Name(), err))
			}
			r.Kill.Killed = append(r.Kill.Killed, sr.Killed...)
			r.Kill.Alive = append(r.Kill.Alive, sr.Alive...)
			kr.Alive = append(kr.Alive, sr.Alive...)
		}
		if len(kr.Alive) == 0 {
			os.Remove(path)
		}
	}
	return r, nil
}
