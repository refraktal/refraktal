package worktree

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// ApplyError is a failed apply. git apply writes file by file, so a failure
// after the first file would leave the user's tree half-applied: Apply
// snapshots every path the patch touches first and restores them on any
// failure. Restored means the tree is exactly as it was; otherwise
// Unrestored names the paths that could not be put back and Snapshot is the
// folder holding their original contents (manifest.json lists them).
type ApplyError struct {
	Err        error
	Restored   bool
	Unrestored []string
	Snapshot   string
}

func (e *ApplyError) Error() string {
	if e.Restored {
		return "the apply failed and was rolled back, so the working tree is unchanged: " + e.Err.Error()
	}
	return fmt.Sprintf("the apply failed and the rollback could not restore %d paths (%s); their original contents are in %s: %v",
		len(e.Unrestored), strings.Join(e.Unrestored, ", "), e.Snapshot, e.Err)
}

func (e *ApplyError) Unwrap() error { return e.Err }

// snapEntry is one touched path as it was before the apply.
type snapEntry struct {
	Path string      `json:"path"` // slash-separated, relative to the repository
	Kind string      `json:"kind"` // absent, file, link or dir (a folder: left alone)
	Mode fs.FileMode `json:"mode,omitempty"`
	Link string      `json:"link,omitempty"`
	Blob string      `json:"blob,omitempty"` // the file's bytes, in the snapshot folder
}

// snapshot is the state of every path a patch touches, and of the folders
// above them, kept in dir until the apply has succeeded or been undone.
type snapshot struct {
	store   *os.Root               // the snapshot folder, pinned
	dir     string                 // its name, for messages
	Entries []snapEntry            `json:"entries"`
	Created []string               `json:"absent_folders"` // folders that did not exist: removed on restore
	Folders map[string]fs.FileMode `json:"folders"`        // folders that existed, with their permissions
}

// parents returns p's ancestor folders, shallowest first.
func parents(p string) []string {
	var out []string
	for d := path.Dir(p); d != "." && d != "/"; d = path.Dir(d) {
		out = append(out, d)
	}
	slices.Reverse(out)
	return out
}

// realParents reports whether every existing ancestor of p is a real folder
// (not a symlink or a file), so an operation on p cannot be redirected.
func realParents(root *os.Root, p string) bool {
	for _, d := range parents(p) {
		info, err := root.Lstat(filepath.FromSlash(d))
		if errors.Is(err, fs.ErrNotExist) {
			return true
		}
		if err != nil || !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
			return false
		}
	}
	return true
}

func depth(p string) int { return strings.Count(p, "/") }

// takeSnapshot records files (and their folders) in repo into store, an
// empty folder. It refuses, changing nothing, a path it cannot record exactly.
// A path below a file or link the patch itself replaces (a file that becomes
// a folder) is recorded as absent; below any other file or link it refuses,
// as git apply does ("beyond a symbolic link").
func takeSnapshot(repo string, files []string, store *os.Root) (*snapshot, error) {
	if store == nil {
		return nil, errors.New("apply snapshot: no folder for it")
	}
	root, err := os.OpenRoot(repo)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	s := &snapshot{store: store, dir: store.Name(), Folders: map[string]fs.FileMode{}}
	if _, err := store.Lstat("manifest.json"); err == nil {
		return nil, errors.New("apply snapshot: the folder already holds a snapshot")
	}
	fail := func(err error) (*snapshot, error) {
		s.clear()
		return nil, fmt.Errorf("apply snapshot: %w", err)
	}
	files = slices.Clone(files)
	slices.Sort(files)
	files = slices.Compact(files)
	touched := map[string]bool{}
	for _, f := range files {
		touched[f] = true
	}
	seen := map[string]bool{}
	for i, f := range files {
		e := snapEntry{Path: f}
		under := false // below a file or link the patch replaces
		for _, d := range parents(f) {
			info, err := root.Lstat(filepath.FromSlash(d))
			switch {
			case errors.Is(err, fs.ErrNotExist):
				if !seen[d] {
					s.Created = append(s.Created, d)
				}
			case err != nil:
				return fail(err)
			case info.IsDir() && info.Mode()&fs.ModeSymlink == 0:
				s.Folders[d] = info.Mode().Perm()
			case touched[d]:
				under = true
			default:
				return fail(fmt.Errorf("%s is beyond a symbolic link or a file", f))
			}
			seen[d] = true
			if under || errors.Is(err, fs.ErrNotExist) {
				break // nothing deeper exists
			}
		}
		info, err := root.Lstat(filepath.FromSlash(f))
		switch {
		case under || errors.Is(err, fs.ErrNotExist):
			e.Kind = "absent"
			// The folders below the replaced one are created by the apply.
			for _, d := range parents(f) {
				if !seen[d] {
					seen[d] = true
					s.Created = append(s.Created, d)
				}
			}
		case err != nil:
			return fail(err)
		case info.Mode()&fs.ModeSymlink != 0:
			e.Kind = "link"
			if e.Link, err = os.Readlink(filepath.Join(repo, filepath.FromSlash(f))); err != nil {
				return fail(err)
			}
		case info.IsDir():
			e.Kind, e.Mode = "dir", info.Mode().Perm() // a folder the patch replaces, or a submodule's
			s.Folders[f] = e.Mode
		case info.Mode().IsRegular():
			e.Kind, e.Mode, e.Blob = "file", info.Mode().Perm(), strconv.Itoa(i)
			if err := copyOut(root, f, store, e.Blob); err != nil {
				return fail(err)
			}
		default:
			return fail(fmt.Errorf("%s is not a file, a link or a folder", f))
		}
		s.Entries = append(s.Entries, e)
	}
	m, _ := json.MarshalIndent(s, "", "  ")
	if err := writeStore(store, "manifest.json", m); err != nil {
		return fail(err)
	}
	return s, nil
}

func writeStore(store *os.Root, name string, b []byte) error {
	f, err := store.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	return errors.Join(err, f.Close())
}

// clear empties the snapshot folder (the caller removes the folder).
func (s *snapshot) clear() {
	for _, e := range s.Entries {
		if e.Blob != "" {
			s.store.Remove(e.Blob)
		}
	}
	s.store.Remove("manifest.json")
}

func copyOut(root *os.Root, f string, store *os.Root, blob string) error {
	in, err := root.Open(filepath.FromSlash(f))
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := store.OpenFile(blob, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	return errors.Join(err, out.Close())
}

// restore puts every recorded path back byte for byte (content, permissions,
// link target, folder or not, existence) and removes the folders the apply
// created. It returns the paths it could not restore. Every operation goes
// through os.Root and checks first that no ancestor is a symlink or a file,
// so nothing the failed apply left behind can redirect it.
func (s *snapshot) restore(repo string) []string {
	root, err := os.OpenRoot(repo)
	if err != nil {
		var all []string
		for _, e := range s.Entries {
			all = append(all, e.Path)
		}
		return all
	}
	defer root.Close()
	bad, same := map[string]bool{}, map[string]bool{}
	for _, e := range s.Entries { // untouched (the apply failed before it): left alone
		same[e.Path] = (e.Kind == "absent" || realParents(root, e.Path)) && s.matches(root, repo, e)
	}
	shallow := slices.Clone(s.Entries)
	slices.SortStableFunc(shallow, func(a, b snapEntry) int { return depth(a.Path) - depth(b.Path) })
	deep := slices.Clone(shallow)
	slices.Reverse(deep)
	current := func(p string) (fs.FileInfo, bool) {
		info, err := root.Lstat(filepath.FromSlash(p))
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return nil, true
		}
		if err != nil || !realParents(root, p) {
			bad[p] = true
			return nil, false
		}
		return info, true
	}
	isFolder := func(info fs.FileInfo) bool { return info.IsDir() && info.Mode()&fs.ModeSymlink == 0 }
	// 1. Remove files and links the apply left at changed paths, shallowest
	// first, so a link put where a folder was goes before anything below it.
	for _, e := range shallow {
		if same[e.Path] {
			continue
		}
		if info, ok := current(e.Path); ok && info != nil && !isFolder(info) && root.Remove(filepath.FromSlash(e.Path)) != nil {
			bad[e.Path] = true
		}
	}
	// 2. Remove folders the apply put where something else was, deepest
	// first (emptied by step 1).
	for _, e := range deep {
		if same[e.Path] || e.Kind == "dir" || bad[e.Path] {
			continue
		}
		if info, ok := current(e.Path); ok && info != nil && isFolder(info) && root.Remove(filepath.FromSlash(e.Path)) != nil {
			bad[e.Path] = true
		}
	}
	// 3. Recreate folders, files and links, shallowest first.
	for _, e := range shallow {
		if same[e.Path] || bad[e.Path] || e.Kind == "absent" {
			continue
		}
		within := e.Path
		if e.Kind == "dir" {
			within = path.Join(e.Path, "_") // the folder itself too
		}
		if !s.ensureFolders(root, repo, within) {
			bad[e.Path] = true
			continue
		}
		switch e.Kind {
		case "file":
			if s.writeBack(root, e) != nil {
				bad[e.Path] = true
			}
		case "link":
			if !realParents(root, e.Path) || os.Symlink(e.Link, filepath.Join(repo, filepath.FromSlash(e.Path))) != nil {
				bad[e.Path] = true
			}
		}
	}
	// 4. Remove the folders the apply created, deepest first.
	created := slices.Clone(s.Created)
	slices.SortStableFunc(created, func(a, b string) int { return depth(b) - depth(a) })
	for _, d := range created {
		if info, ok := current(d); ok && info != nil && (!isFolder(info) || root.Remove(filepath.FromSlash(d)) != nil) {
			bad[d] = true
		}
	}
	// 5. Check every path against the snapshot.
	for _, e := range s.Entries {
		if !bad[e.Path] && !((e.Kind == "absent" || realParents(root, e.Path)) && s.matches(root, repo, e)) {
			bad[e.Path] = true
		}
	}
	for d, mode := range s.Folders {
		if info, err := root.Lstat(filepath.FromSlash(d)); err != nil || !isFolder(info) || info.Mode().Perm() != mode {
			bad[d] = true
		}
	}
	out := make([]string, 0, len(bad))
	for p := range bad {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

// ensureFolders recreates the folders above p that existed before (the
// apply may have removed them as it emptied them), with their permissions.
func (s *snapshot) ensureFolders(root *os.Root, repo, p string) bool {
	for _, d := range parents(p) {
		info, err := root.Lstat(filepath.FromSlash(d))
		switch {
		case err == nil && info.IsDir() && info.Mode()&fs.ModeSymlink == 0:
			continue
		case err == nil, !errors.Is(err, fs.ErrNotExist):
			return false
		}
		mode, ok := s.Folders[d]
		if !ok {
			return false // never existed: nothing recorded belongs below it
		}
		if root.Mkdir(filepath.FromSlash(d), 0o700) != nil {
			return false
		}
		if os.Chmod(filepath.Join(repo, filepath.FromSlash(d)), mode) != nil { // exact, whatever the umask
			return false
		}
	}
	return true
}

func (s *snapshot) writeBack(root *os.Root, e snapEntry) error {
	in, err := s.store.Open(e.Blob)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := root.OpenFile(filepath.FromSlash(e.Path), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	return errors.Join(err, out.Chmod(e.Mode), out.Close())
}

func (s *snapshot) matches(root *os.Root, repo string, e snapEntry) bool {
	info, err := root.Lstat(filepath.FromSlash(e.Path))
	switch e.Kind {
	case "absent": // below a file (ENOTDIR) is absent too
		return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
	case "dir":
		return err == nil && info.IsDir() && info.Mode()&fs.ModeSymlink == 0 && info.Mode().Perm() == e.Mode
	case "link":
		l, lerr := os.Readlink(filepath.Join(repo, filepath.FromSlash(e.Path)))
		return err == nil && info.Mode()&fs.ModeSymlink != 0 && lerr == nil && l == e.Link
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != e.Mode {
		return false
	}
	want, err1 := readStore(s.store, e.Blob)
	f, err2 := root.Open(filepath.FromSlash(e.Path))
	if err1 != nil || err2 != nil {
		return false
	}
	defer f.Close()
	got, err := io.ReadAll(f)
	return err == nil && bytes.Equal(got, want)
}

// rollback undoes a failed apply and returns the error to report. The
// snapshot is removed once the tree is restored, and kept otherwise.
func (s *snapshot) rollback(repo string, cause error) error {
	e := &ApplyError{Err: cause}
	if e.Unrestored = s.restore(repo); len(e.Unrestored) == 0 {
		e.Restored = true
		s.clear()
	} else {
		e.Snapshot = s.dir
	}
	return e
}

func readStore(store *os.Root, name string) ([]byte, error) {
	f, err := store.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}
