package runlog

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Run is one run's folder: <root>/.refraktal/runs/<id>/.
type Run struct {
	ID  string
	Dir string // absolute
	Rel string // relative to root, for printing
}

// NewRun creates a run folder under root/.refraktal/runs, and writes
// root/.refraktal/.gitignore containing "*" so nothing there is committed.
func NewRun(root string) (Run, error) {
	base := filepath.Join(root, ".refraktal")
	if err := os.MkdirAll(filepath.Join(base, "runs"), 0o755); err != nil {
		return Run{}, err
	}
	ignore := filepath.Join(base, ".gitignore")
	if b, err := os.ReadFile(ignore); err != nil || string(b) != "*\n" {
		if err := os.WriteFile(ignore, []byte("*\n"), 0o644); err != nil {
			return Run{}, err
		}
	}
	for range 5 {
		var r [3]byte
		rand.Read(r[:])
		id := time.Now().Format("20060102-150405") + "-" + hex.EncodeToString(r[:])
		dir := filepath.Join(base, "runs", id)
		err := os.Mkdir(dir, 0o755)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return Run{}, err
		}
		return Run{ID: id, Dir: dir, Rel: filepath.Join(".refraktal", "runs", id)}, nil
	}
	return Run{}, errors.New("runlog: could not pick a unique run ID")
}

// CheckNotTemp refuses a path under $TMPDIR or /tmp. Worktrees and run folders
// must not live there: Codex treats those folders as writable.
func CheckNotTemp(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	abs = resolve(abs)
	for _, t := range []string{os.TempDir(), "/tmp", "/private/tmp"} {
		t = resolve(t)
		if abs == t || strings.HasPrefix(abs, t+string(filepath.Separator)) {
			return fmt.Errorf("%s is under %s: run folders must not live in a temporary folder", path, t)
		}
	}
	return nil
}

func resolve(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}
