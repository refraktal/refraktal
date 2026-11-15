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
//
// The repository may not be trusted: it could commit .refraktal, runs or
// .gitignore as symlinks pointing elsewhere. NewRun refuses any of them that
// is a symlink or of the wrong type, and replaces .gitignore by renaming a new
// file over it, which never writes through a link.
func NewRun(root string) (Run, error) {
	base := filepath.Join(root, ".refraktal")
	runs := filepath.Join(base, "runs")
	for _, d := range []string{base, runs} {
		if err := ensureDir(d); err != nil {
			return Run{}, err
		}
	}
	if err := ensureIgnore(filepath.Join(base, ".gitignore")); err != nil {
		return Run{}, err
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

// ensureDir creates dir, or checks that it is a real directory and not a
// symlink.
func ensureDir(dir string) error {
	fi, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		fi, err = os.Lstat(dir)
	}
	if err != nil {
		return err
	}
	if !fi.IsDir() { // Lstat does not follow links: a symlink is never a directory here
		return fmt.Errorf("%s is not a plain directory (a symlink or a file): refusing to use it", dir)
	}
	return nil
}

// ensureIgnore makes path a regular file containing "*\n".
func ensureIgnore(path string) error {
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return err
	case !fi.Mode().IsRegular():
		return fmt.Errorf("%s is not a regular file (a symlink or other): refusing to replace it", path)
	default:
		if b, err := os.ReadFile(path); err == nil && string(b) == "*\n" {
			return nil
		}
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".gitignore-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString("*\n"); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(0o644); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
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
