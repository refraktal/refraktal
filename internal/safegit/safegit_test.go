package safegit_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"refraktal.dev/refraktal/internal/safegit"
)

func TestCommandStaticProtections(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, command := range []string{"rev-parse", "ls-files", "check-attr"} {
		t.Run(command, func(t *testing.T) {
			out, err := safegit.Command(context.Background(), bin, command).Output()
			if err != nil {
				t.Fatal(err)
			}
			args := strings.Split(strings.TrimSpace(string(out)), "\n")
			if !slices.Contains(args, "--no-replace-objects") {
				t.Errorf("missing --no-replace-objects in %q", args)
			}
			for _, setting := range []string{
				"core.hooksPath=/dev/null", "core.fsmonitor=false", "core.pager=cat",
				"core.sshCommand=false", "diff.external=", "commit.gpgSign=false", "tag.gpgSign=false",
			} {
				i := slices.Index(args, setting)
				if i < 1 || args[i-1] != "-c" {
					t.Errorf("missing -c %s in %q", setting, args)
				}
			}
		})
	}
}

func fixture(t *testing.T) (string, string) {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	fixtureGit(t, git, dir, "init", "-q")
	if err := os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("*.txt filter=poison\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data.txt"), []byte("content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, git
}

func fixtureGit(t *testing.T, git, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(git, append([]string{"-C", dir}, args...)...)
	cmd.Env = safegit.Env()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture git %v: %v %s", args, err, out)
	}
}

func TestCommandNeutralizesDrivers(t *testing.T) {
	dir, git := fixture(t)
	marker := filepath.Join(t.TempDir(), "FILTER-RAN")
	fixtureGit(t, git, dir, "config", "filter.poison.clean", "touch '"+marker+"'; cat")
	fixtureGit(t, git, dir, "config", "filter.poison.required", "true")
	if _, err := safegit.Command(context.Background(), dir, "add", "-A").Output(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("configured filter ran: %v", err)
	}
}

func TestCommandRejectsDriverChange(t *testing.T) {
	dir, git := fixture(t)
	bin := t.TempDir()
	body := fmt.Sprintf("#!/bin/sh\n'%s' \"$@\"\nstatus=$?\ncase \" $* \" in *' add -A '*) '%s' -C '%s' config filter.race.clean cat || exit 99;; esac\nexit $status\n", git, git, dir)
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, err := safegit.Command(context.Background(), dir, "add", "-A").Output()
	if err == nil || !strings.Contains(err.Error(), "driver configuration changed") {
		t.Fatalf("driver change did not fail command: %v", err)
	}
}

func TestCommandDisablesDiffDrivers(t *testing.T) {
	for _, kind := range []string{"command", "textconv"} {
		t.Run(kind, func(t *testing.T) {
			dir, git := fixture(t)
			if err := os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("*.txt diff=poison\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			fixtureGit(t, git, dir, "add", "-A")
			fixtureGit(t, git, dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgSign=false", "commit", "-qm", "diff fixture")
			marker := filepath.Join(t.TempDir(), "DIFF-RAN")
			fixtureGit(t, git, dir, "config", "diff.poison."+kind, "touch '"+marker+"'")
			if err := os.WriteFile(filepath.Join(dir, "data.txt"), []byte("changed\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := safegit.Command(context.Background(), dir, "diff").Output(); err != nil {
				t.Error(err)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Errorf("configured diff %s ran: %v", kind, err)
			}
		})
	}
}
