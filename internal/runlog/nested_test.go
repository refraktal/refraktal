package runlog

import (
	"os"
	"path/filepath"
	"testing"
)

// Review M7 (5): the parent run folder a child Refraktal is given is opened
// once and its identity checked against what was inspected, so a swap for a
// symlink between the check and the open cannot redirect the child's writes.
func TestNewRunUnderRefusesASwap(t *testing.T) {
	base := t.TempDir()
	parent := filepath.Join(base, "scratch")
	elsewhere := filepath.Join(base, "elsewhere")
	os.MkdirAll(parent, 0o700)
	os.MkdirAll(elsewhere, 0o700)
	testAfterParentCheck = func() { // the swap, between the check and the open
		os.Rename(parent, parent+".old")
		os.Symlink(elsewhere, parent)
	}
	defer func() { testAfterParentCheck = nil }()
	r, err := NewRunUnder(parent)
	if err == nil {
		r.Close()
		t.Fatal("a parent run folder swapped for a symlink was used")
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Fatalf("wrote through the swapped-in symlink: %v", entries)
	}
}

// Any symlink in the parent's own place is refused outright.
func TestNewRunUnderRefusesASymlink(t *testing.T) {
	base := t.TempDir()
	os.MkdirAll(filepath.Join(base, "real"), 0o700)
	os.Symlink(filepath.Join(base, "real"), filepath.Join(base, "link"))
	if _, err := NewRunUnder(filepath.Join(base, "link")); err == nil {
		t.Fatal("a symlinked parent run folder was used")
	}
}
