package worktree

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestChangedRejectsCommittedJudgeChanges(t *testing.T) {
	for _, empty := range []bool{false, true} {
		name := "edited_commit"
		if empty {
			name = "empty_commit"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			w, err := Create(ctx, repo(t), filepath.Join(t.TempDir(), "judge"), "refraktal/judge")
			if err != nil {
				t.Fatal(err)
			}
			if !empty {
				if err := os.WriteFile(filepath.Join(w.Dir, "a.txt"), []byte("judge wrote and committed this\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				fixtureGit(t, w.Dir, "add", "a.txt")
			}
			fixtureGit(t, w.Dir, "commit", "--allow-empty", "-qm", "judge commit")
			paths, err := w.Changed(ctx)
			if err == nil {
				t.Fatalf("judge moved HEAD away from pinned base but passed integrity check: paths=%v", paths)
			}
			if !strings.Contains(err.Error(), "base") {
				t.Fatalf("integrity failure does not identify pinned base: %v", err)
			}
			if !empty && !slices.Contains(paths, "a.txt") {
				t.Fatalf("committed edit was not reported: %v", paths)
			}
		})
	}
}

func TestChangedReportsDirtyJudgeWorktree(t *testing.T) {
	for _, kind := range []string{"clean", "modified", "staged", "deleted", "untracked", "ignored"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			w, err := Create(ctx, repo(t), filepath.Join(t.TempDir(), "judge"), "refraktal/judge")
			if err != nil {
				t.Fatal(err)
			}
			want := "a.txt"
			switch kind {
			case "modified", "staged":
				putFile(t, w.Dir, "a.txt", "judge edit\n")
				if kind == "staged" {
					fixtureGit(t, w.Dir, "add", "a.txt")
				}
			case "deleted":
				if err := os.Remove(filepath.Join(w.Dir, "a.txt")); err != nil {
					t.Fatal(err)
				}
			case "untracked":
				want = "new.txt"
				putFile(t, w.Dir, want, "judge added this\n")
			case "ignored":
				want = "build/"
				if err := os.Mkdir(filepath.Join(w.Dir, "build"), 0o755); err != nil {
					t.Fatal(err)
				}
				putFile(t, w.Dir, "build/output", "judge generated this\n")
			}
			paths, err := w.Changed(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "clean" {
				if len(paths) != 0 {
					t.Fatalf("unchanged judge worktree reported paths: %v", paths)
				}
			} else if !slices.Equal(paths, []string{want}) {
				t.Fatalf("changed paths = %v, want [%s]", paths, want)
			}
		})
	}
}
