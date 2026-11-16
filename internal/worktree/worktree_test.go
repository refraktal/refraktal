package worktree

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Create adds a worktree at dir on a new branch, from the repository's HEAD.
// Uncommitted changes in the user's tree are not part of it.
func Create(ctx context.Context, repo, dir, branch string) (Worktree, error) {
	return CreateAt(ctx, repo, dir, branch, "HEAD")
}

// repo makes a repository with one commit and git hooks that would leave a
// marker if any of them ran.
func repo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	os.MkdirAll(dir, 0o755)
	run("init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644)
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("build/\n"), 0o644)
	run("add", "-A")
	run("commit", "-q", "-m", "init")
	// post-checkout runs on worktree add and post-commit on commit; neither
	// is skipped by --no-verify.
	for _, name := range []string{"pre-commit", "post-checkout", "post-commit"} {
		hook := filepath.Join(dir, ".git", "hooks", name)
		os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+filepath.Join(dir, "HOOK-RAN")+"\n"), 0o755)
	}
	return dir
}

func TestCreateCollectApplyRemove(t *testing.T) {
	ctx := context.Background()
	r := repo(t)
	if root, err := Root(ctx, filepath.Join(r, ".git", "..")); err != nil || filepath.Base(root) != "repo" {
		t.Fatalf("root %q %v", root, err)
	}
	w, err := Create(ctx, r, filepath.Join(t.TempDir(), "wt"), "refraktal/test")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(w.Dir, "a.txt"), []byte("one\ntwo\n"), 0o644)
	os.WriteFile(filepath.Join(w.Dir, "new.bin"), []byte{0, 1, 2, 255}, 0o644)
	os.MkdirAll(filepath.Join(w.Dir, "build"), 0o755)
	os.WriteFile(filepath.Join(w.Dir, "build", "out"), []byte("x"), 0o644) // ignored

	res, err := w.Collect(ctx, "refraktal test")
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(res.Files)
	if strings.Join(res.Files, ",") != "a.txt,new.bin" || res.Commit == "" || len(res.Diff) == 0 {
		t.Fatalf("result %+v", res)
	}
	if _, err := os.Stat(filepath.Join(r, "HOOK-RAN")); err == nil {
		t.Fatal("the repository's git hook ran")
	}

	if _, err := w.Apply(ctx, res); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(r, "a.txt")); string(b) != "one\ntwo\n" {
		t.Fatalf("a.txt = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(r, "new.bin")); string(b) != "\x00\x01\x02\xff" {
		t.Fatalf("new.bin = %q", b)
	}
	if err := w.Remove(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(w.Dir); !os.IsNotExist(err) {
		t.Fatal("worktree folder still there")
	}
	if out, _ := exec.Command("git", "-C", r, "branch", "--list", "refraktal/test").Output(); len(out) != 0 {
		t.Fatal("branch not deleted")
	}
}

func TestCollectIncludesTheChildsOwnCommits(t *testing.T) {
	ctx := context.Background()
	w, err := Create(ctx, repo(t), filepath.Join(t.TempDir(), "wt"), "refraktal/own")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(w.Dir, "b.txt"), []byte("b\n"), 0o644)
	fixtureGit(t, w.Dir, "add", "b.txt")
	fixtureGit(t, w.Dir, "commit", "-q", "-m", "child commit")
	res, err := w.Collect(ctx, "refraktal")
	if err != nil || strings.Join(res.Files, ",") != "b.txt" {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestNoChanges(t *testing.T) {
	ctx := context.Background()
	w, err := Create(ctx, repo(t), filepath.Join(t.TempDir(), "wt"), "refraktal/none")
	if err != nil {
		t.Fatal(err)
	}
	res, err := w.Collect(ctx, "refraktal")
	_, applyErr := w.Apply(ctx, res)
	if err != nil || len(res.Files) != 0 || res.Commit != "" || applyErr != nil {
		t.Fatalf("%+v %v", res, err)
	}
}

// A diff that conflicts with the user's own edits must leave their tree as
// it was.
func TestApplyConflictChangesNothing(t *testing.T) {
	ctx := context.Background()
	r := repo(t)
	w, err := Create(ctx, r, filepath.Join(t.TempDir(), "wt"), "refraktal/conflict")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(w.Dir, "a.txt"), []byte("child\n"), 0o644)
	os.WriteFile(filepath.Join(w.Dir, "c.txt"), []byte("c\n"), 0o644)
	res, err := w.Collect(ctx, "refraktal")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(r, "a.txt"), []byte("mine\n"), 0o644)
	if _, err := w.Apply(ctx, res); err == nil {
		t.Fatal("want a conflict")
	}
	if b, _ := os.ReadFile(filepath.Join(r, "a.txt")); string(b) != "mine\n" {
		t.Fatalf("user's edit lost: %q", b)
	}
	if _, err := os.Stat(filepath.Join(r, "c.txt")); err == nil {
		t.Fatal("part of a failed diff was applied")
	}
}

func TestCreateNeedsACommit(t *testing.T) {
	dir := t.TempDir()
	exec.Command("git", "-C", dir, "init", "-q").Run()
	if _, err := Create(context.Background(), dir, filepath.Join(t.TempDir(), "wt"), "b"); err == nil || !strings.Contains(err.Error(), "at least one commit") {
		t.Fatalf("got %v", err)
	}
}

// The child shares .git: a config it poisoned must not run anything when
// Refraktal collects and applies the result.
func TestPoisonedConfigRunsNothing(t *testing.T) {
	ctx := context.Background()
	r := repo(t)
	w, err := Create(ctx, r, filepath.Join(t.TempDir(), "wt"), "refraktal/poison")
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "RAN")
	evil := filepath.Join(t.TempDir(), "evil.sh")
	os.WriteFile(evil, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755)
	for _, kv := range [][]string{{"core.fsmonitor", evil}, {"diff.external", evil}, {"core.pager", evil}} {
		if out, err := exec.Command("git", "-C", w.Dir, "config", kv[0], kv[1]).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	os.WriteFile(filepath.Join(w.Dir, "a.txt"), []byte("changed\n"), 0o644)
	res, err := w.Collect(ctx, "refraktal")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Apply(ctx, res); err != nil {
		t.Fatal(err)
	}
	w.Remove(ctx)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a command from the poisoned git config ran")
	}
}

// 1. Refraktal's own git commands never get an API key or GIT_* variables.
func TestGitGetsNoAPIKeyOrGitVars(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "must-not-leak")
	t.Setenv("GIT_DIR", "/elsewhere")
	bin := t.TempDir()
	log := filepath.Join(bin, "log")
	os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nenv >> "+log+"\necho /x\n"), 0o755)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	Root(context.Background(), t.TempDir())
	b, _ := os.ReadFile(log)
	if strings.Contains(string(b), "must-not-leak") || strings.Contains(string(b), "GIT_DIR=") {
		t.Fatalf("git env:\n%s", b)
	}
}

func TestGitIgnoresInheritedRepositoryAndConfig(t *testing.T) {
	for _, key := range []string{
		"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_COMMON_DIR", "GIT_CONFIG",
		"GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS",
	} {
		t.Run(key, func(t *testing.T) {
			ctx := context.Background()
			r := repo(t)
			poison := filepath.Join(t.TempDir(), "poison")
			if err := os.WriteFile(poison, []byte("[core]\n bare = true\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			value := poison
			switch key {
			case "GIT_CONFIG_COUNT":
				value = "1"
				t.Setenv("GIT_CONFIG_KEY_0", "core.bare")
				t.Setenv("GIT_CONFIG_VALUE_0", "true")
			case "GIT_CONFIG_PARAMETERS":
				value = "'core.bare=true'"
			}
			t.Setenv(key, value)
			want, err := filepath.EvalSymlinks(r) // macOS: /var is a link to /private/var
			if err != nil {
				t.Fatal(err)
			}
			root, err := Root(ctx, r)
			if err != nil || root != want {
				t.Fatalf("Root = %q, %v; want %q", root, err, want)
			}
			w, err := Create(ctx, r, filepath.Join(t.TempDir(), "wt"), "refraktal/env")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(w.Dir, "a.txt"), []byte("isolated\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			res, err := w.Collect(ctx, "environment isolation")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.Apply(ctx, res); err != nil {
				t.Fatal(err)
			}
			if b, err := os.ReadFile(filepath.Join(r, "a.txt")); err != nil || string(b) != "isolated\n" {
				t.Fatalf("applied content = %q, %v", b, err)
			}
			if err := w.Remove(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestConfiguredFiltersRunNothing(t *testing.T) {
	for _, filter := range []string{"clean", "smudge", "process", "all", "unconfigured"} {
		t.Run(filter, func(t *testing.T) {
			ctx := context.Background()
			r := repo(t)
			attrs := []byte("* filter=poison diff=poison merge=poison text\n")
			if err := os.WriteFile(filepath.Join(r, ".gitattributes"), attrs, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := git(ctx, r, "add", ".gitattributes"); err != nil {
				t.Fatal(err)
			}
			if _, err := git(ctx, r, "commit", "-qm", "attributes"); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(t.TempDir(), "FILTER-RAN")
			evil := filepath.Join(t.TempDir(), "filter.sh")
			if err := os.WriteFile(evil, []byte("#!/bin/sh\ntouch '"+marker+"'\ncat\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			settings := [][]string{
				{"merge.poison.driver", "'" + evil + "'"},
				{"diff.poison.command", evil}, {"diff.poison.textconv", evil},
				{"core.attributesFile", filepath.Join(r, ".gitattributes")},
			}
			if filter != "unconfigured" {
				kinds := []string{filter}
				if filter == "all" {
					kinds = []string{"clean", "smudge", "process"}
				}
				for _, kind := range kinds {
					settings = append(settings, []string{"filter.poison." + kind, "'" + evil + "'"})
				}
				settings = append(settings, []string{"filter.poison.required", "true"})
			}
			for _, kv := range settings {
				if out, err := exec.Command("git", "-C", r, "config", kv[0], kv[1]).CombinedOutput(); err != nil {
					t.Fatalf("git config: %v %s", err, out)
				}
			}
			// Existing info/attributes must stay untouched, even with text set.
			info := filepath.Join(r, ".git", "info", "attributes")
			if err := os.WriteFile(info, attrs, 0o640); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if _, err := os.Stat(marker); !os.IsNotExist(err) {
					t.Errorf("configured %s filter ran: %v", filter, err)
				}
				if b, err := os.ReadFile(info); err != nil || string(b) != string(attrs) {
					t.Errorf("original info/attributes changed: %q, %v", b, err)
				}
				if fi, err := os.Stat(info); err != nil || fi.Mode().Perm() != 0o640 {
					t.Errorf("original info/attributes mode changed: %v", err)
				}
			}()
			w, err := Create(ctx, r, filepath.Join(t.TempDir(), "wt"), "refraktal/filters")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("configured driver ran during create: %v", err)
			}
			if err := os.Mkdir(filepath.Join(w.Dir, "nested"), 0o755); err != nil {
				t.Fatal(err)
			}
			for name, content := range map[string][]byte{
				"a.txt": []byte("changed\n"), "nested/.gitattributes": attrs,
				"nested/new.txt": []byte("new\n"),
			} {
				if err := os.WriteFile(filepath.Join(w.Dir, name), content, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			res, err := w.Collect(ctx, "filters disabled")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("configured driver ran during collect: %v", err)
			}
			if _, err := w.Apply(ctx, res); err != nil {
				t.Fatal(err)
			}
			if b, err := os.ReadFile(filepath.Join(r, "nested", "new.txt")); err != nil || string(b) != "new\n" {
				t.Fatalf("new file content: %q, %v", b, err)
			}
			if b, err := os.ReadFile(filepath.Join(r, "a.txt")); err != nil || string(b) != "changed\n" {
				t.Fatalf("modified file content: %q, %v", b, err)
			}
			if err := w.Remove(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Exercise a real three-way merge: normal create/collect/apply operations do
// not need a merge driver, but the command helper must still disable it.
func TestMergeDriverRunsNoProgram(t *testing.T) {
	for _, configured := range []bool{true, false} {
		t.Run(fmt.Sprint(configured), func(t *testing.T) {
			ctx, r := context.Background(), repo(t)
			mustGit := func(args ...string) {
				t.Helper()
				if out, err := git(ctx, r, args...); err != nil {
					t.Fatalf("%v: %v %s", args, err, out)
				}
			}
			write := func(name, content string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(r, name), []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			write(".gitattributes", "*.txt merge=poison filter=unconfigured\n")
			write("a.txt", "one\ntwo\nthree\n")
			mustGit("add", "-A")
			mustGit("commit", "-qm", "base")
			mustGit("checkout", "-qb", "side")
			write("a.txt", "one\ntwo\nSIDE\n")
			mustGit("commit", "-qam", "side")
			mustGit("checkout", "-q", "main")
			write("a.txt", "MAIN\ntwo\nthree\n")
			mustGit("commit", "-qam", "main")
			marker := filepath.Join(t.TempDir(), "MERGE-RAN")
			if configured {
				if out, err := exec.Command("git", "-C", r, "config", "merge.poison.driver", "touch '"+marker+"'; exit 1").CombinedOutput(); err != nil {
					t.Fatalf("config: %v %s", err, out)
				}
			}
			_, err := git(ctx, r, "merge", "--no-commit", "--no-ff", "side")
			if configured && err == nil {
				t.Error("empty merge driver must fail closed with a conflict")
			}
			if !configured {
				if err != nil {
					t.Fatal(err)
				}
				if b, err := os.ReadFile(filepath.Join(r, "a.txt")); err != nil || string(b) != "MAIN\ntwo\nSIDE\n" {
					t.Errorf("unconfigured driver did not use built-in merge: %q, %v", b, err)
				}
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Errorf("merge driver ran: %v", err)
			}
		})
	}
}

func TestStagingPreservesNamesModesAndLinks(t *testing.T) {
	ctx, r := context.Background(), repo(t)
	w, err := Create(ctx, r, filepath.Join(t.TempDir(), "wt"), "refraktal/files")
	if err != nil {
		t.Fatal(err)
	}
	name, content := "space\tline\n\"\\é.txt", "text\n"
	if err := os.WriteFile(filepath.Join(w.Dir, name), []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(name, filepath.Join(w.Dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(w.Dir, "a.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Dir, ".gitattributes"), []byte("* text\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := w.Collect(ctx, "files")
	if err != nil {
		t.Fatal(err)
	}
	again, err := w.Collect(ctx, "no extra commit")
	if err != nil || again.Commit != res.Commit {
		t.Fatalf("second collect: commit %q, %v; want %q", again.Commit, err, res.Commit)
	}
	if _, err := w.Apply(ctx, res); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(r, name)); err != nil || string(b) != content {
		t.Errorf("content = %q, %v", b, err)
	}
	if fi, err := os.Stat(filepath.Join(r, name)); err != nil || fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("executable mode lost: %v", err)
	}
	if target, err := os.Readlink(filepath.Join(r, "link")); err != nil || target != name {
		t.Errorf("symlink = %q, %v", target, err)
	}
	if _, err := os.Stat(filepath.Join(r, "a.txt")); !os.IsNotExist(err) {
		t.Errorf("deleted file remains: %v", err)
	}
}

func TestGitRefusesUnrepresentableDriverKey(t *testing.T) {
	r := repo(t)
	if out, err := exec.Command("git", "-C", r, "config", "filter.name=with-equals.clean", "touch SHOULD-NOT-RUN").CombinedOutput(); err != nil {
		t.Fatalf("config: %v %s", err, out)
	}
	if _, err := Root(context.Background(), r); err == nil || !strings.Contains(err.Error(), "cannot safely override") {
		t.Fatalf("unsafe driver key accepted: %v", err)
	}
}

func TestGitDisablesDriversFromEveryConfigScope(t *testing.T) {
	r := repo(t)
	home, xdg := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	if err := os.Mkdir(filepath.Join(xdg, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []struct{ path, driver string }{
		{filepath.Join(xdg, "git", "config"), "xdg"},
		{filepath.Join(home, ".gitconfig"), "global.Mixed.name"},
		{filepath.Join(r, ".git", "included"), "included"},
		{filepath.Join(r, ".git", "config.worktree"), "worktree"},
	} {
		for _, key := range []string{"clean", "smudge", "process", "required"} {
			value := "touch SHOULD-NOT-RUN"
			if key == "required" {
				value = "true"
			}
			cmd := exec.Command("git", "config", "--file", scope.path, "filter."+scope.driver+"."+key, value)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("config: %v %s", err, out)
			}
		}
		if out, err := exec.Command("git", "config", "--file", scope.path, "merge."+scope.driver+".driver", "touch SHOULD-NOT-RUN").CombinedOutput(); err != nil {
			t.Fatalf("config: %v %s", err, out)
		}
	}
	for _, kv := range [][]string{
		{"include.path", filepath.Join(r, ".git", "included")},
		{"extensions.worktreeConfig", "true"},
		{"filter.local.clean", "touch SHOULD-NOT-RUN"},
		{"merge.local.driver", "touch SHOULD-NOT-RUN"},
	} {
		if out, err := exec.Command("git", "-C", r, "config", kv[0], kv[1]).CombinedOutput(); err != nil {
			t.Fatalf("config: %v %s", err, out)
		}
	}
	for _, name := range []string{"xdg", "global.Mixed.name", "included", "worktree", "local"} {
		for _, key := range []string{"clean", "smudge", "process", "required", "driver"} {
			prefix, want := "filter.", ""
			if key == "driver" {
				prefix = "merge."
			}
			if key == "required" {
				want = "false"
			}
			out, err := git(context.Background(), r, "config", "--get", prefix+name+"."+key)
			if err != nil || strings.TrimSuffix(string(out), "\n") != want {
				t.Errorf("%s%s.%s = %q, %v; want %q", prefix, name, key, out, err, want)
			}
		}
	}
}

func TestConfiguredSigningRunsNothing(t *testing.T) {
	ctx := context.Background()
	r := repo(t)
	w, err := Create(ctx, r, filepath.Join(t.TempDir(), "wt"), "refraktal/signing")
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "SIGNER-RAN")
	signer := filepath.Join(t.TempDir(), "signer.sh")
	if err := os.WriteFile(signer, []byte("#!/bin/sh\ntouch '"+marker+"'\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, kv := range [][]string{{"commit.gpgSign", "true"}, {"tag.gpgSign", "true"}, {"gpg.program", signer}} {
		if out, err := exec.Command("git", "-C", r, "config", kv[0], kv[1]).CombinedOutput(); err != nil {
			t.Fatalf("git config: %v %s", err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(w.Dir, "a.txt"), []byte("unsigned\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = w.Collect(ctx, "unsigned")
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Errorf("configured signer ran: %v", statErr)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestPatchIgnoresUserDiffFormatting(t *testing.T) {
	ctx := context.Background()
	r := repo(t)
	for _, kv := range [][]string{
		{"diff.noprefix", "true"}, {"diff.mnemonicPrefix", "true"},
		{"diff.srcPrefix", "source/"}, {"diff.dstPrefix", "destination/"}, {"color.ui", "always"},
	} {
		if out, err := exec.Command("git", "-C", r, "config", kv[0], kv[1]).CombinedOutput(); err != nil {
			t.Fatalf("git config: %v %s", err, out)
		}
	}
	w, err := Create(ctx, r, filepath.Join(t.TempDir(), "wt"), "refraktal/prefix")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Dir, "a.txt"), []byte("formatted safely\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := w.Collect(ctx, "stable patch")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(res.Diff), "diff --git a/a.txt b/a.txt\n") || strings.Contains(string(res.Diff), "\x1b") {
		t.Fatalf("patch has configured prefixes or color: %q", res.Diff)
	}
	if _, err := w.Apply(ctx, res); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(r, "a.txt")); err != nil || string(b) != "formatted safely\n" {
		t.Fatalf("applied content = %q, %v", b, err)
	}
}

// Keep text changes readable in the run log.
func TestTextChangesStayTextual(t *testing.T) {
	ctx := context.Background()
	w, err := Create(ctx, repo(t), filepath.Join(t.TempDir(), "wt"), "refraktal/text")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Dir, "a.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := w.Collect(ctx, "text")
	if err != nil {
		t.Fatal(err)
	}
	if d := string(res.Diff); strings.Contains(d, "GIT binary patch") || !strings.Contains(d, "\n+two\n") {
		t.Fatalf("text change is not a text patch: %q", d)
	}
}

// Kill the caller while Git is blocked, so no Go deferred restoration can run.
func TestKilledOperationLeavesAttributesUntouched(t *testing.T) {
	if r := os.Getenv("REFRAKTAL_TEST_KILLED_REPO"); r != "" {
		_, err := Create(context.Background(), r, filepath.Join(r, "../wt"), "refraktal/killed")
		t.Fatalf("operation should have been killed: %v", err)
	}
	r := repo(t)
	info := filepath.Join(r, ".git", "info")
	path := filepath.Join(info, "attributes")
	original := []byte("*.txt text filter=known merge=known\n")
	if err := os.WriteFile(path, original, 0o640); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	ready := filepath.Join(bin, "ready")
	script := fmt.Sprintf("#!/bin/sh\ncase \" $* \" in *' worktree add '*) echo $$ > '%s'; exec sleep 30;; esac\nexec '%s' \"$@\"\n", ready, realGit)
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestKilledOperationLeavesAttributesUntouched$")
	cmd.Env = append(os.Environ(), "REFRAKTAL_TEST_KILLED_REPO="+r, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	deadline := time.Now().Add(10 * time.Second)
	var pid int
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(ready)
		pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		if pid > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("Git wrapper never blocked")
	}
	blocked, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if blocked != nil {
			blocked.Kill()
		}
	}()
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	if err := blocked.Kill(); err != nil {
		t.Fatal(err)
	}
	blocked = nil
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) || after.Mode().Perm() != 0o640 {
		t.Errorf("attributes identity or mode changed: %v", err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != string(original) {
		t.Errorf("attributes changed after killed operation: %q, %v", b, err)
	}
	if err := filepath.WalkDir(filepath.Join(r, ".git"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.Contains(d.Name(), "refraktal-attributes") {
			t.Errorf("isolation state left in .git: %s", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
