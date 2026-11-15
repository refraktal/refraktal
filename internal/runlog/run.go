package runlog

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Run is one run's folder: <root>/.refraktal/runs/<id>/.
type Run struct {
	ID   string
	Dir  string // absolute
	Rel  string // relative to root, for printing
	root *os.Root
}

// AppendRaw appends an error's raw text to the capture file name, relative
// to the run folder, through the run's pinned handle.
func (r Run) AppendRaw(name, what, raw string) { AppendRaw(r.root, name, what, raw) }

// Close releases the run's directory handle.
func (r Run) Close() error { return r.root.Close() }

// OpenRoot pins a subdirectory for Refraktal's later writes.
func (r Run) OpenRoot(name string) (*os.Root, error) { return r.root.OpenRoot(name) }

// Create creates a log within the run folder.
func (r Run) Create(name string) (*Log, error) {
	f, err := r.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	return &Log{f: f, now: time.Now}, nil
}

// WriteFile creates an artifact within the run folder. It refuses existing
// entries, including hard links, so they cannot redirect a write elsewhere.
func (r Run) WriteFile(name string, data []byte, perm os.FileMode) error {
	f, err := r.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	return errors.Join(err, f.Close())
}

// Mkdir creates a directory within the run folder.
func (r Run) Mkdir(name string, perm os.FileMode) error {
	return r.root.Mkdir(name, perm)
}

// NewRun creates a run folder under root/.refraktal/runs, and writes
// root/.refraktal/.gitignore containing "*" so nothing there is committed.
// The caller must close the returned Run when its writes are complete.
//
// The repository may not be trusted: it could commit .refraktal, runs or
// .gitignore as symlinks pointing elsewhere. NewRun refuses any of them that
// is a symlink or of the wrong type. All accesses use pinned directory
// handles, so a directory swap cannot redirect writes outside root.
func NewRun(root string) (Run, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return Run{}, err
	}
	repo, err := os.OpenRoot(root)
	if err != nil {
		return Run{}, err
	}
	defer repo.Close()
	base, err := ensureDir(repo, ".refraktal")
	if err != nil {
		return Run{}, err
	}
	defer base.Close()
	runs, err := ensureDir(base, "runs")
	if err != nil {
		return Run{}, err
	}
	defer runs.Close()
	if err := ensureIgnore(base); err != nil {
		return Run{}, err
	}
	for range 5 {
		var r [3]byte
		rand.Read(r[:])
		id := time.Now().Format("20060102-150405") + "-" + hex.EncodeToString(r[:])
		err := runs.Mkdir(id, 0o755)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return Run{}, err
		}
		pinned, err := ensureDir(runs, id)
		if err != nil {
			return Run{}, err
		}
		rel := filepath.Join(".refraktal", "runs", id)
		return Run{ID: id, Dir: filepath.Join(root, rel), Rel: rel, root: pinned}, nil
	}
	return Run{}, errors.New("runlog: could not pick a unique run ID")
}

// ensureDir creates dir, or checks that it is a real directory and not a
// symlink.
func ensureDir(parent *os.Root, dir string) (*os.Root, error) {
	if err := parent.Mkdir(dir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	fi, err := parent.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() { // Lstat does not follow links: a symlink is never a directory here
		return nil, fmt.Errorf("%s is not a plain directory (a symlink or a file): refusing to use it", dir)
	}
	d, err := parent.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	opened, err := d.Stat(".")
	if err != nil || !os.SameFile(fi, opened) {
		d.Close()
		return nil, fmt.Errorf("%s changed while opening it: refusing to use it", dir)
	}
	return d, nil
}

// ensureIgnore makes path a regular file containing "*\n".
func ensureIgnore(base *os.Root) error {
	const path = ".gitignore"
	fi, err := base.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return err
	case !fi.Mode().IsRegular():
		return fmt.Errorf("%s is not a regular file (a symlink or other): refusing to replace it", path)
	default:
		f, err := base.Open(path)
		if err != nil {
			return err
		}
		opened, err := f.Stat()
		if err != nil || !os.SameFile(fi, opened) {
			f.Close()
			return fmt.Errorf("%s changed while opening it: refusing to use it", path)
		}
		var b [3]byte // Two expected bytes and one byte to detect trailing data.
		n, readErr := io.ReadFull(f, b[:])
		if err := f.Close(); err != nil {
			return err
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
			return readErr
		}
		if n == 2 && string(b[:n]) == "*\n" {
			return nil
		}
	}
	// Remove the directory entry, then create exclusively. This also leaves
	// hard-linked files untouched and refuses a symlink planted in the gap.
	if err := base.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := base.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString("*\n"); err != nil {
		f.Close()
		return err
	}
	return f.Close()
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

// resolve follows symlinks in the nearest existing parent of p and appends
// the rest, so a folder not created yet resolves as it will once created
// (on macOS $TMPDIR lives under /var, a link to /private/var).
func resolve(p string) string {
	dir, rest := filepath.Clean(p), ""
	for {
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return filepath.Clean(p)
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = parent
	}
}
