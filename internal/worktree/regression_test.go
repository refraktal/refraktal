package worktree

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Fixture commands use normal conversions, independently of the safe helper.
func fixtureGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "core.hooksPath=/dev/null"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fixture git %v: %v %s", args, err, out)
	}
	return string(out)
}

func putFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestConditionalIncludeDriversRunNothing(t *testing.T) {
	for _, condition := range []string{"onbranch:refraktal/**", "gitdir"} {
		t.Run(condition, func(t *testing.T) {
			ctx, r := context.Background(), repo(t)
			putFile(t, r, ".gitattributes", "* filter=x\n")
			fixtureGit(t, r, "add", ".gitattributes")
			fixtureGit(t, r, "commit", "-qm", "conditional filter fixture")
			base := strings.TrimSpace(fixtureGit(t, r, "rev-parse", "HEAD"))
			marker := filepath.Join(t.TempDir(), "FILTER-RAN")
			include := filepath.Join(r, ".git", "conditional")
			for _, kind := range []string{"clean", "smudge"} {
				fixtureGit(t, r, "config", "--file", include, "filter.x."+kind, "touch '"+marker+"'; cat")
			}
			pattern := condition
			if condition == "gitdir" {
				common, err := filepath.EvalSymlinks(filepath.Join(r, ".git"))
				if err != nil {
					t.Fatal(err)
				}
				pattern = "gitdir:" + filepath.ToSlash(common) + "/worktrees/**"
			}
			fixtureGit(t, r, "config", "includeIf."+pattern+".path", include)
			// Discovery on main sees no filter. Only the new branch/path matches.
			out, err := exec.Command("git", "-C", r, "config", "--get-regexp", `^filter\.`).CombinedOutput()
			if err == nil || len(out) != 0 {
				t.Fatalf("filter unexpectedly visible on main: %q, %v", out, err)
			}
			putFile(t, r, "uncommitted.txt", "user only\n")
			checkMarker := func(operation string) {
				t.Helper()
				if _, err := os.Stat(marker); !os.IsNotExist(err) {
					t.Fatalf("conditional filter ran during %s: %v", operation, err)
				}
			}
			w, err := Create(ctx, r, filepath.Join(t.TempDir(), "wt"), "refraktal/conditional")
			if err != nil {
				t.Fatal(err)
			}
			checkMarker("create")
			if w.Base != base || strings.TrimSpace(fixtureGit(t, w.Dir, "symbolic-ref", "--short", "HEAD")) != w.Branch {
				t.Fatalf("wrong base or branch: %+v", w)
			}
			if b, err := os.ReadFile(filepath.Join(w.Dir, "a.txt")); err != nil || string(b) != "one\n" {
				t.Fatalf("checkout content = %q, %v", b, err)
			}
			if _, err := os.Stat(filepath.Join(w.Dir, "uncommitted.txt")); !os.IsNotExist(err) {
				t.Fatalf("user's uncommitted file was checked out: %v", err)
			}
			putFile(t, w.Dir, "a.txt", "conditional change\n")
			res, err := w.Collect(ctx, "conditional include")
			if err != nil {
				t.Fatal(err)
			}
			checkMarker("collect")
			if _, err := w.Apply(ctx, res, snapStore(t)); err != nil {
				t.Fatal(err)
			}
			checkMarker("apply")
			if b, err := os.ReadFile(filepath.Join(r, "a.txt")); err != nil || string(b) != "conditional change\n" {
				t.Fatalf("applied content = %q, %v", b, err)
			}
		})
	}
}

func TestCollectRejectsRewrittenGitfile(t *testing.T) {
	for _, when := range []string{"before collect", "during collect"} {
		t.Run(when, func(t *testing.T) {
			ctx, r := context.Background(), repo(t)
			w, err := Create(ctx, r, filepath.Join(t.TempDir(), "wt"), "refraktal/gitfile")
			if err != nil {
				t.Fatal(err)
			}
			other := repo(t)
			putFile(t, other, ".gitattributes", "* filter=poison\n")
			fixtureGit(t, other, "add", "-A")
			fixtureGit(t, other, "commit", "-qm", "other repository")
			head := fixtureGit(t, other, "rev-parse", "HEAD")
			marker := filepath.Join(t.TempDir(), "OTHER-FILTER-RAN")
			fixtureGit(t, other, "config", "filter.poison.clean", "touch '"+marker+"'; cat")
			// If Git follows the swapped gitfile, its attributes now come from
			// the child's checkout and its index/commits belong to the other repo.
			putFile(t, w.Dir, ".gitattributes", "* filter=poison\n")
			putFile(t, w.Dir, "a.txt", "child change\n")
			if when == "before collect" {
				putFile(t, w.Dir, ".git", "gitdir: "+filepath.Join(other, ".git")+"\n")
			} else {
				realGit, err := exec.LookPath("git")
				if err != nil {
					t.Fatal(err)
				}
				bin := t.TempDir()
				body := fmt.Sprintf("#!/bin/sh\ncase \" $* \" in *' add -A '*) printf 'gitdir: %%s\\n' '%s' > '%s';; esac\nexec '%s' \"$@\"\n", filepath.Join(other, ".git"), filepath.Join(w.Dir, ".git"), realGit)
				putFile(t, bin, "git", body)
				if err := os.Chmod(filepath.Join(bin, "git"), 0o755); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			}
			_, err = w.Collect(ctx, "must not commit to other repository")
			if err == nil || !strings.Contains(err.Error(), "gitfile") {
				t.Errorf("rewritten gitfile did not fail collect: %v", err)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Errorf("other repository's filter ran: %v", err)
			}
			if got := fixtureGit(t, other, "rev-parse", "HEAD"); got != head {
				t.Errorf("other repository was committed: got %q, want %q", got, head)
			}
		})
	}
}

func TestCollectLeavesUntouchedConvertedFile(t *testing.T) {
	for _, source := range []string{"repository", "user"} {
		t.Run(source, func(t *testing.T) {
			ctx, r := context.Background(), repo(t)
			attrs := ".gitattributes"
			if source == "user" {
				attrs = filepath.Join(t.TempDir(), "attributes")
				fixtureGit(t, r, "config", "core.attributesFile", attrs)
			} else {
				attrs = filepath.Join(r, attrs)
			}
			if err := os.WriteFile(attrs, []byte("*.bat text eol=crlf\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			putFile(t, r, "script.bat", "echo original\r\n")
			fixtureGit(t, r, "add", "-A")
			fixtureGit(t, r, "commit", "-qm", "converted file")
			w, err := Create(ctx, r, filepath.Join(t.TempDir(), "wt"), "refraktal/conversion")
			if err != nil {
				t.Fatal(err)
			}
			if b, err := os.ReadFile(filepath.Join(w.Dir, "script.bat")); err != nil || string(b) != "echo original\r\n" {
				t.Fatalf("checkout conversion = %q, %v", b, err)
			}
			putFile(t, w.Dir, "a.txt", "changed\n")
			res, err := w.Collect(ctx, "change another file")
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(res.Files, []string{"a.txt"}) || strings.Contains(string(res.Diff), "script.bat") {
				t.Errorf("untouched file included: files %q, patch %q", res.Files, res.Diff)
			}
			if _, err := w.Apply(ctx, res, snapStore(t)); err != nil {
				t.Fatal(err)
			}
			if b, err := os.ReadFile(filepath.Join(r, "script.bat")); err != nil || string(b) != "echo original\r\n" {
				t.Fatalf("untouched file = %q, %v", b, err)
			}
		})
	}
}

func TestApplyWithUserAutocrlf(t *testing.T) {
	ctx, r := context.Background(), repo(t)
	fixtureGit(t, r, "config", "core.autocrlf", "true")
	// Re-check out the committed text using the user's conversion setting.
	if err := os.Remove(filepath.Join(r, "a.txt")); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, r, "checkout", "--", "a.txt")
	w, err := Create(ctx, r, filepath.Join(t.TempDir(), "wt"), "refraktal/autocrlf")
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(w.Dir, "a.txt")); err != nil || string(b) != "one\r\n" {
		t.Errorf("checkout conversion = %q, %v", b, err)
	}
	putFile(t, w.Dir, "a.txt", "one\r\ntwo\r\n")
	res, err := w.Collect(ctx, "text change")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Apply(ctx, res, snapStore(t)); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(r, "a.txt")); err != nil || string(b) != "one\r\ntwo\r\n" {
		t.Fatalf("applied content = %q, %v", b, err)
	}
}

func TestCollectApplySparseCheckout(t *testing.T) {
	ctx, r := context.Background(), repo(t)
	for _, dir := range []string{"inside", "outside"} {
		if err := os.Mkdir(filepath.Join(r, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		putFile(t, r, dir+"/file.txt", "original\n")
	}
	fixtureGit(t, r, "add", "-A")
	fixtureGit(t, r, "commit", "-qm", "directories")
	fixtureGit(t, r, "sparse-checkout", "set", "--cone", "inside")
	w, err := Create(ctx, r, filepath.Join(t.TempDir(), "wt"), "refraktal/sparse")
	if err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, w.Dir, "sparse-checkout", "set", "--cone", "inside")
	for _, dir := range []string{r, w.Dir} {
		if _, err := os.Stat(filepath.Join(dir, "outside", "file.txt")); !os.IsNotExist(err) {
			t.Fatalf("excluded file is on disk: %v", err)
		}
		if out := fixtureGit(t, dir, "ls-files", "-t", "outside/file.txt"); out != "S outside/file.txt\n" {
			t.Fatalf("missing skip-worktree bit: %q", out)
		}
	}
	putFile(t, w.Dir, "inside/file.txt", "changed\n")
	res, err := w.Collect(ctx, "sparse change")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Files, []string{"inside/file.txt"}) {
		t.Fatalf("files = %q", res.Files)
	}
	if _, err := w.Apply(ctx, res, snapStore(t)); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(r, "inside", "file.txt")); err != nil || string(b) != "changed\n" {
		t.Fatalf("applied content = %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(r, "outside", "file.txt")); !os.IsNotExist(err) {
		t.Fatalf("apply materialized excluded file: %v", err)
	}
}

func TestResultFilesPreservesUnusualNames(t *testing.T) {
	ctx, r := context.Background(), repo(t)
	w, err := Create(ctx, r, filepath.Join(t.TempDir(), "wt"), "refraktal/names")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{" leading and trailing ", "line\nbreak.txt", "quote\".txt"}
	for _, name := range want {
		putFile(t, w.Dir, name, "new\n")
	}
	res, err := w.Collect(ctx, "unusual names")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Files, want) {
		t.Errorf("files = %q; want %q", res.Files, want)
	}
	if _, err := w.Apply(ctx, res, snapStore(t)); err != nil {
		t.Fatal(err)
	}
	for _, name := range want {
		if b, err := os.ReadFile(filepath.Join(r, name)); err != nil || string(b) != "new\n" {
			t.Errorf("applied %q = %q, %v", name, b, err)
		}
	}
}

func TestDriverConfigChangeDuringCommandFails(t *testing.T) {
	for _, command := range []struct{ operation, pattern string }{
		{"create", " rev-parse --verify "}, {"create", " worktree add "},
		{"create", " reset --hard "},
		{"collect", " add -A "}, {"collect", " diff --cached "}, {"collect", " commit "},
		{"collect", " --no-textconv --name-only "}, {"collect", " --binary "}, {"collect", " rev-parse HEAD "},
		{"apply", " apply --check "}, {"apply", " apply --binary "},
	} {
		for _, change := range []string{"add", "value", "remove"} {
			t.Run(command.operation+command.pattern+change, func(t *testing.T) {
				ctx, r := context.Background(), repo(t)
				var w Worktree
				var res Result
				var err error
				if command.operation != "create" {
					w, err = Create(ctx, r, filepath.Join(t.TempDir(), "wt"), "refraktal/existing")
					if err != nil {
						t.Fatal(err)
					}
					putFile(t, w.Dir, "a.txt", "changed\n")
					if command.operation == "apply" {
						res, err = w.Collect(ctx, "prepare apply")
						if err != nil {
							t.Fatal(err)
						}
					}
				}
				if change != "add" {
					fixtureGit(t, r, "config", "filter.race.clean", "old value")
				}
				realGit, err := exec.LookPath("git")
				if err != nil {
					t.Fatal(err)
				}
				bin := t.TempDir()
				mutation := "config filter.race.clean 'new value'"
				if change == "remove" {
					mutation = "config --unset filter.race.clean"
				}
				// The wrapper changes config after Git acts but before it exits.
				body := fmt.Sprintf("#!/bin/sh\n'%s' \"$@\"\nstatus=$?\ncase \" $* \" in *'%s'*) '%s' -C '%s' %s || exit 99;; esac\nexit $status\n", realGit, command.pattern, realGit, r, mutation)
				putFile(t, bin, "git", body)
				if err := os.Chmod(filepath.Join(bin, "git"), 0o755); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
				switch command.operation {
				case "create":
					_, err = Create(ctx, r, filepath.Join(t.TempDir(), "wt"), "refraktal/race")
				case "collect":
					_, err = w.Collect(ctx, "racing collect")
				case "apply":
					_, err = w.Apply(ctx, res, snapStore(t))
				}
				if err == nil || !strings.Contains(err.Error(), "driver configuration changed") {
					t.Fatalf("config change did not fail operation: %v", err)
				}
			})
		}
	}
}

func TestDriverValuelessConfigChangeFails(t *testing.T) {
	ctx, r := context.Background(), repo(t)
	// A bare boolean is true; an explicit empty boolean is false. Both print
	// an empty value, distinguished by config's key/value newline separator.
	config := filepath.Join(r, ".git", "config")
	f, err := os.OpenFile(config, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString("\n[filter \"race\"]\n\trequired\n")
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("write fixture config: %v, %v", err, closeErr)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	body := fmt.Sprintf("#!/bin/sh\n'%s' \"$@\"\nstatus=$?\ncase \" $* \" in *' rev-parse --verify '*) '%s' -C '%s' config filter.race.required '' || exit 99;; esac\nexit $status\n", realGit, realGit, r)
	putFile(t, bin, "git", body)
	if err := os.Chmod(filepath.Join(bin, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, err = Create(ctx, r, filepath.Join(t.TempDir(), "wt"), "refraktal/boolean")
	if err == nil || !strings.Contains(err.Error(), "driver configuration changed") {
		t.Fatalf("valueless config change did not fail operation: %v", err)
	}
}
