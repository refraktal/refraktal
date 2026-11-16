package refraktal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"refraktal.dev/refraktal/internal/harness"
)

// Port of the run-6 review reproduction, without invoking Git or a harness
// process. A .git directory is enough to mark the unsafe repository ancestor.
func TestReviewDoctorNeutralMustBeOutsideRepository(t *testing.T) {
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Use the real default Neutral path: repository-local XDG_CACHE_HOME on
	// Linux, or a repository-root HOME on macOS.
	t.Setenv("HOME", repo)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(repo, ".cache"))
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	insideRepo := func(path string) bool {
		rel, err := filepath.Rel(repo, path)
		return path != "" && err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	var gotCWD string
	h := doctorProbeHarness{fake: fake{name: "probe"}, prepare: func(_ context.Context, o harness.Options) (harness.Launch, error) {
		gotCWD = o.Cwd
		return harness.Launch{}, errors.New("synthetic stop before process start")
	}}
	rep, err := Doctor(context.Background(), DoctorOptions{
		Root: repo, LiveDir: filepath.Join(repo, "live"), ConfigPath: filepath.Join(repo, "absent.json"),
		Harnesses: []harness.Harness{h}, Getenv: func(string) string { return "" },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("default_cache_inside_repo=%v prepare_called=%v session_is_in_git_repo=%v",
		insideRepo(cache), gotCWD != "", insideRepo(gotCWD))
	if insideRepo(gotCWD) {
		t.Fatal("doctor passed a session folder inside a Git repository to the harness")
	}
	if gotCWD != "" {
		t.Fatal("doctor prepared a harness despite the repository-local neutral folder")
	}
	assertDoctorFolderUnusable(t, rep, filepath.Join(cache, "refraktal", "doctor"), 1)
}

func TestDoctorNeutralRepositoryMarkers(t *testing.T) {
	for _, kind := range []string{"directory", "gitfile", "dangling_git_symlink", "symlinked_cache"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			repo := filepath.Join(dir, "repo")
			if err := os.Mkdir(repo, 0o700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(repo, ".git")
			var err error
			switch kind {
			case "gitfile":
				// Detection must not read the gitfile or follow its target.
				err = os.WriteFile(marker, []byte("gitdir: /unreadable/not-used\n"), 0o600)
			case "dangling_git_symlink":
				err = os.Symlink(filepath.Join(dir, "missing"), marker)
			default:
				err = os.Mkdir(marker, 0o700)
			}
			if err != nil {
				t.Fatal(err)
			}
			cache := filepath.Join(repo, "cache")
			if kind == "symlinked_cache" {
				// The configured cache spelling is outside the repository;
				// its real target and the actual session are inside it.
				if err := os.Mkdir(cache, 0o700); err != nil {
					t.Fatal(err)
				}
				alias := filepath.Join(dir, "cache-alias")
				if err := os.Symlink(cache, alias); err != nil {
					t.Fatal(err)
				}
				cache = alias
			}
			neutral := filepath.Join(cache, "refraktal", "doctor")
			rep, calls := doctorWithFolderProbe(t, neutral)
			if calls != 0 {
				t.Fatalf("prepared %d harnesses inside a repository", calls)
			}
			assertDoctorFolderUnusable(t, rep, neutral, 2)
			for _, c := range rep.Harnesses {
				if !strings.Contains(c.Reason, "inside repository") || !strings.Contains(c.Reason, ".git marker") {
					t.Errorf("missing repository reason: %+v", c)
				}
			}
		})
	}
}

func TestDoctorNeutralFilesystemErrors(t *testing.T) {
	for _, kind := range []string{"file", "dangling_symlink", "symlink_loop", "unreadable"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			neutral := filepath.Join(dir, "neutral")
			var err error
			switch kind {
			case "file":
				err = os.WriteFile(neutral, []byte("keep\n"), 0o600)
			case "dangling_symlink":
				err = os.Symlink(filepath.Join(dir, "missing"), neutral)
			case "symlink_loop":
				err = os.Symlink(neutral, neutral)
			case "unreadable":
				if os.Geteuid() == 0 {
					t.Skip("root bypasses directory read permissions")
				}
				err = os.Mkdir(neutral, 0o700)
				if err == nil {
					err = os.Chmod(neutral, 0)
					t.Cleanup(func() {
						if err := os.Chmod(neutral, 0o700); err != nil {
							t.Error(err)
						}
					})
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			rep, calls := doctorWithFolderProbe(t, neutral)
			if calls != 0 {
				t.Fatalf("prepared %d harnesses after a filesystem error", calls)
			}
			assertDoctorFolderUnusable(t, rep, neutral, 2)
		})
	}
}

// Both harnesses stop at the existing Prepare seam, before any process or
// network operation. A nonempty Decider reason and a forced cleanup read error
// show that the non-harness checks still ran when the folder was refused.
func doctorWithFolderProbe(t *testing.T, neutral string) (DoctorReport, int32) {
	t.Helper()
	dir := t.TempDir()
	live := filepath.Join(dir, "live-is-a-file")
	if err := os.WriteFile(live, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	var hs []harness.Harness
	for _, name := range []string{"claude", "codex"} {
		hs = append(hs, doctorProbeHarness{fake: fake{name: name}, prepare: func(_ context.Context, o harness.Options) (harness.Launch, error) {
			calls.Add(1)
			return harness.Launch{}, errors.New("synthetic stop before process start")
		}})
	}
	rep, err := Doctor(context.Background(), DoctorOptions{
		Root: dir, LiveDir: live, Neutral: neutral, ConfigPath: filepath.Join(dir, "absent.json"),
		Harnesses: hs, Getenv: func(string) string { return "" },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Cleanup.Errors) == 0 || rep.Decider.Kind != "fallback" || !strings.Contains(rep.Decider.Reason, "not set") {
		t.Fatalf("non-harness checks did not run: %+v", rep)
	}
	return rep, calls.Load()
}

func assertDoctorFolderUnusable(t *testing.T, rep DoctorReport, folder string, count int) {
	t.Helper()
	if rep.ExitCode() != ExitNothingUsable || len(rep.Usable) != 0 || len(rep.Harnesses) != count {
		t.Fatalf("expected all %d harnesses unusable with exit 3: %+v", count, rep)
	}
	var text, jsonOut bytes.Buffer
	WriteDoctorText(&text, rep)
	if err := WriteDoctorJSON(&jsonOut, rep); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(jsonOut.String()), "\n")
	for i, c := range rep.Harnesses {
		if c.Status != StatusUnavailable || !strings.Contains(c.Reason, folder) ||
			!strings.Contains(c.Reason, "point XDG_CACHE_HOME outside any repository") {
			t.Errorf("missing refusal and remedy: %+v", c)
		}
		var line struct {
			Type string `json:"type"`
			HarnessCheck
		}
		if err := json.Unmarshal([]byte(lines[i]), &line); err != nil {
			t.Fatal(err)
		}
		if line.Type != "harness" || line.Name != c.Name || line.Status != c.Status || line.Reason != c.Reason ||
			!strings.Contains(text.String(), c.Reason) {
			t.Errorf("report writers lost harness refusal: JSON %s; text %s", lines[i], text.String())
		}
	}
	var summary struct {
		Type string `json:"type"`
		Exit int    `json:"exit"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Type != "summary" || summary.Exit != ExitNothingUsable || !strings.Contains(text.String(), "nothing usable") {
		t.Errorf("report writers lost unusable result: JSON %s; text %s", jsonOut.String(), text.String())
	}
}

func TestDoctorNeutralOutsideRepository(t *testing.T) {
	neutral := filepath.Join(t.TempDir(), "neutral")
	rep, calls := doctorWithFolderProbe(t, neutral)
	if calls != 2 {
		t.Fatalf("prepared %d harnesses outside any repository, want 2", calls)
	}
	for _, c := range rep.Harnesses {
		if c.Reason != "synthetic stop before process start" {
			t.Errorf("normal harness check replaced by folder refusal: %+v", c)
		}
	}
	if entries, err := os.ReadDir(neutral); err != nil || len(entries) != 0 {
		t.Errorf("session folders not cleaned: %v, %v", entries, err)
	}
}

func TestDoctorNeutralUsesResolvedFolder(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cache, alias := filepath.Join(dir, "cache"), filepath.Join(dir, "cache-alias")
	if err := os.Mkdir(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(cache, alias); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	var gotCWD string
	h := doctorProbeHarness{fake: fake{name: "probe"}, prepare: func(_ context.Context, o harness.Options) (harness.Launch, error) {
		gotCWD = o.Cwd
		if !o.ProbeVersions {
			t.Error("normal doctor version checks were disabled")
		}
		// Retarget the configured alias at the first harness boundary. The
		// checked cwd must still refer to the original safe real folder.
		if err := os.Remove(alias); err != nil {
			return harness.Launch{}, err
		}
		if err := os.Symlink(repo, alias); err != nil {
			return harness.Launch{}, err
		}
		real, err := filepath.EvalSymlinks(o.Cwd)
		if err != nil || real != o.Cwd || filepath.Dir(real) != filepath.Join(cache, "refraktal", "doctor") {
			t.Errorf("harness cwd differs from checked real folder: %q -> %q, %v", o.Cwd, real, err)
		}
		return harness.Launch{}, errors.New("synthetic stop before process start")
	}}
	rep, err := Doctor(context.Background(), DoctorOptions{
		Root: dir, LiveDir: filepath.Join(dir, "no-live"), Neutral: filepath.Join(alias, "refraktal", "doctor"),
		ConfigPath: filepath.Join(dir, "absent.json"), Harnesses: []harness.Harness{h}, Getenv: func(string) string { return "" },
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotCWD == "" || len(rep.Harnesses) != 1 || rep.Harnesses[0].Reason != "synthetic stop before process start" {
		t.Fatalf("safe resolved folder did not reach harness preparation: %+v", rep)
	}
	if _, err := os.Lstat(gotCWD); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("session folder not cleaned: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".git")); err != nil {
		t.Errorf("retargeted alias affected repository: %v", err)
	}
}
