package worktree

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Re-exec the test binary as git-lfs. Implement Git's pkt-line filter protocol
// so required=true proves that the allowed process actually passed data through.
func TestFakeLFS(t *testing.T) {
	marker := os.Getenv("REFRAKTAL_TEST_LFS_MARKER")
	log := os.Getenv("REFRAKTAL_TEST_LFS_LOG")
	if marker == "" && log == "" {
		return
	}
	if marker != "" {
		if err := os.WriteFile(marker, []byte("ran\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	record := func(command string) {
		if log == "" {
			return
		}
		f, err := os.OpenFile(log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		skip, set := os.LookupEnv("GIT_LFS_SKIP_SMUDGE")
		err = json.NewEncoder(f).Encode(lfsInvocation{os.Args[2:], command, skip, set})
		closeErr := f.Close()
		if err != nil || closeErr != nil {
			t.Fatalf("record LFS invocation: %v, %v", err, closeErr)
		}
	}
	record("start")
	read := func() []string {
		var packets []string
		for {
			var header [4]byte
			if _, err := io.ReadFull(os.Stdin, header[:]); err == io.EOF {
				os.Exit(0)
			} else if err != nil {
				t.Fatal(err)
			}
			n, err := strconv.ParseUint(string(header[:]), 16, 16)
			if err != nil || n > 0 && n < 4 {
				t.Fatalf("invalid packet length %q", header)
			}
			if n == 0 {
				return packets
			}
			b := make([]byte, int(n)-4)
			if _, err := io.ReadFull(os.Stdin, b); err != nil {
				t.Fatal(err)
			}
			packets = append(packets, string(b))
		}
	}
	write := func(packets ...string) {
		for _, p := range packets {
			fmt.Fprintf(os.Stdout, "%04x%s", len(p)+4, p)
		}
		fmt.Fprint(os.Stdout, "0000")
	}
	read()
	write("git-filter-server\n", "version=2\n")
	read()
	write("capability=clean\n", "capability=smudge\n")
	for {
		for _, header := range read() {
			if command, ok := strings.CutPrefix(header, "command="); ok {
				record(strings.TrimSpace(command))
			}
		}
		content := read()
		write("status=success\n")
		write(content...)
		write()
	}
}

type lfsInvocation struct {
	Argv    []string
	Command string
	Skip    string
	SkipSet bool
}

func TestApplyLFSNotice(t *testing.T) {
	// Exercise both outcomes in one test: an LFS notice must name only the
	// affected LFS files, and a later ordinary patch must produce no notice.
	ctx, r := context.Background(), repo(t)
	paths := []string{"user only.dat", "line\nbreak.dat"}
	for _, name := range append(slices.Clone(paths), "plain.lfs") {
		putFile(t, r, name, "original\n")
	}
	fixtureGit(t, r, "add", "-A")
	fixtureGit(t, r, "commit", "-qm", "notice fixture")
	w, err := Create(ctx, r, filepath.Join(t.TempDir(), "wt"), "refraktal/lfs-notice")
	if err != nil {
		t.Fatal(err)
	}
	// User-only attributes prove Apply consults the user's repository, not
	// the child's attributes or a filename extension heuristic.
	putFile(t, r, ".gitattributes", "*.dat filter=lfs\n")
	for _, name := range append(slices.Clone(paths), "a.txt") {
		putFile(t, w.Dir, name, "changed\n")
	}
	res, err := w.Collect(ctx, "LFS notice")
	if err != nil {
		t.Fatal(err)
	}
	notice, err := w.Apply(ctx, res, snapStore(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range paths {
		if !strings.Contains(notice, strconv.Quote(name)) {
			t.Errorf("notice does not name %q: %q", name, notice)
		}
	}
	if !strings.Contains(notice, "left as LFS pointers") || !strings.Contains(notice, "git lfs smudge -- FILE < FILE") || strings.Contains(notice, "a.txt") {
		t.Errorf("incorrect LFS notice: %q", notice)
	}
	// A separate branch isolates a patch that touches only a non-LFS path.
	w, err = Create(ctx, r, filepath.Join(t.TempDir(), "plain"), "refraktal/plain-notice")
	if err != nil {
		t.Fatal(err)
	}
	putFile(t, w.Dir, "plain.lfs", "plain change\n")
	res, err = w.Collect(ctx, "no LFS notice")
	if err != nil {
		t.Fatal(err)
	}
	notice, err = w.Apply(ctx, res, snapStore(t))
	if err != nil || notice != "" {
		t.Errorf("plain patch notice = %q, error = %v", notice, err)
	}
	// Removing an LFS attribute in the same patch must not hide the notice
	// for a path that was LFS-tracked in the user's repository.
	r = repo(t)
	putFile(t, r, ".gitattributes", "*.dat filter=lfs\n")
	putFile(t, r, "data.dat", "original\n")
	fixtureGit(t, r, "add", "-A")
	fixtureGit(t, r, "commit", "-qm", "LFS attribute fixture")
	w, err = Create(ctx, r, filepath.Join(t.TempDir(), "attrs"), "refraktal/attrs-notice")
	if err != nil {
		t.Fatal(err)
	}
	putFile(t, w.Dir, ".gitattributes", "*.dat -filter\n")
	putFile(t, w.Dir, "data.dat", "changed\n")
	res, err = w.Collect(ctx, "remove LFS attribute")
	if err != nil {
		t.Fatal(err)
	}
	notice, err = w.Apply(ctx, res, snapStore(t))
	if err != nil || !strings.Contains(notice, "data.dat") {
		t.Errorf("removed LFS attribute hid notice: %q, %v", notice, err)
	}
}

func TestLFSSmudgeIsolation(t *testing.T) {
	ctx, r := context.Background(), repo(t)
	putFile(t, r, ".gitattributes", "*.lfs filter=lfs\n")
	putFile(t, r, ".lfsconfig", "[lfs]\nurl = https://user.invalid/lfs\n")
	putFile(t, r, "data.lfs", "original\n")
	fixtureGit(t, r, "add", "-A")
	fixtureGit(t, r, "commit", "-qm", "LFS fixture")
	for key, value := range map[string]string{
		"clean": "git-lfs clean -- %f", "smudge": "git-lfs smudge -- %f",
		"process": "git-lfs filter-process", "required": "true",
	} {
		fixtureGit(t, r, "config", "filter.lfs."+key, value)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	gitLog, lfsLog := filepath.Join(bin, "git.log"), filepath.Join(bin, "lfs.jsonl")
	putFile(t, bin, "git-lfs", "#!/bin/sh\nexec '"+os.Args[0]+"' -test.run=^TestFakeLFS$ \"$@\"\n")
	putFile(t, bin, "git", fmt.Sprintf("#!/bin/sh\nprintf '%%s\\t%%s\\n' \"${GIT_LFS_SKIP_SMUDGE-UNSET}\" \"$*\" >> '%s'\nexec '%s' \"$@\"\n", gitLog, realGit))
	for _, name := range []string{"git", "git-lfs"} {
		if err := os.Chmod(filepath.Join(bin, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	requireFakeLFS(t, bin)
	t.Setenv("REFRAKTAL_TEST_LFS_LOG", lfsLog)
	// Even an inherited value must be removed for Create.
	t.Setenv("GIT_LFS_SKIP_SMUDGE", "inherited")
	check := func(phase, wantSkip, wantCommand string) {
		t.Helper()
		b, err := os.ReadFile(gitLog)
		if err != nil || len(b) == 0 {
			t.Fatalf("%s: missing git log: %v", phase, err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			flag, _, _ := strings.Cut(line, "\t")
			if flag != wantSkip {
				t.Errorf("%s git: GIT_LFS_SKIP_SMUDGE = %q, want %q", phase, flag, wantSkip)
			}
		}
		b, err = os.ReadFile(lfsLog)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			var call lfsInvocation
			if err := json.Unmarshal([]byte(line), &call); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(call.Argv, []string{"filter-process"}) {
				t.Errorf("%s LFS argv = %q", phase, call.Argv)
			}
			if wantSkip == "UNSET" && call.SkipSet || wantSkip == "1" && (!call.SkipSet || call.Skip != "1") {
				t.Errorf("%s LFS: skip = %q, set = %v", phase, call.Skip, call.SkipSet)
			}
			found = found || call.Command == wantCommand
		}
		if !found {
			t.Errorf("%s: no LFS %s request", phase, wantCommand)
		}
		putFile(t, bin, "git.log", "")
		putFile(t, bin, "lfs.jsonl", "")
	}
	w, err := Create(ctx, r, filepath.Join(t.TempDir(), "wt"), "refraktal/lfs-env")
	if err != nil {
		t.Fatal(err)
	}
	check("Create", "UNSET", "smudge")
	putFile(t, w.Dir, "data.lfs", "changed\n")
	putFile(t, w.Dir, ".lfsconfig", "[lfs]\nurl = https://child.invalid/lfs\n")
	res, err := w.Collect(ctx, "child rewrote .lfsconfig")
	if err != nil {
		t.Fatal(err)
	}
	check("Collect", "1", "clean")
	if _, err := w.Apply(ctx, res, snapStore(t)); err != nil {
		t.Fatal(err)
	}
	check("Apply", "1", "smudge")
}

func TestLFSAllowList(t *testing.T) {
	for _, variant := range []string{
		"standard", "clean shell suffix", "smudge path", "process suffix",
		"missing clean", "missing smudge", "missing process", "extra clean", "extra smudge", "extra process",
		"extension", "custom transfer", "standalone transfer",
	} {
		t.Run(variant, func(t *testing.T) {
			ctx, r := context.Background(), repo(t)
			putFile(t, r, ".gitattributes", "*.lfs filter=lfs\n")
			putFile(t, r, "data.lfs", "original\n")
			fixtureGit(t, r, "add", "-A")
			fixtureGit(t, r, "commit", "-qm", "LFS fixture")
			for key, value := range map[string]string{
				"clean": "git-lfs clean -- %f", "smudge": "git-lfs smudge -- %f",
				"process": "git-lfs filter-process", "required": "true",
			} {
				fixtureGit(t, r, "config", "filter.lfs."+key, value)
			}
			bin := t.TempDir()
			marker, shellMarker := filepath.Join(bin, "LFS-RAN"), filepath.Join(bin, "SHELL-RAN")
			putFile(t, bin, "git-lfs", "#!/bin/sh\nexec '"+os.Args[0]+"' -test.run=^TestFakeLFS$\n")
			if err := os.Chmod(filepath.Join(bin, "git-lfs"), 0o755); err != nil {
				t.Fatal(err)
			}
			switch variant {
			case "clean shell suffix":
				fixtureGit(t, r, "config", "filter.lfs.clean", "git-lfs clean -- %f; touch '"+shellMarker+"'")
			case "smudge path":
				fixtureGit(t, r, "config", "filter.lfs.smudge", filepath.Join(bin, "git-lfs")+" smudge -- %f")
			case "process suffix":
				fixtureGit(t, r, "config", "filter.lfs.process", "git-lfs filter-process --extra")
			case "extension":
				fixtureGit(t, r, "config", "lfs.extension.poison.clean", "")
			case "custom transfer":
				include := filepath.Join(r, ".git", "lfs-include")
				fixtureGit(t, r, "config", "--file", include, "lfs.customtransfer.poison.path", "/bin/false")
				fixtureGit(t, r, "config", "include.path", include)
			case "standalone transfer":
				t.Setenv("HOME", t.TempDir())
				fixtureGit(t, r, "config", "--global", "lfs.standalonetransferagent", "poison")
			default:
				if key, ok := strings.CutPrefix(variant, "missing "); ok {
					fixtureGit(t, r, "config", "--unset", "filter.lfs."+key)
				}
				if key, ok := strings.CutPrefix(variant, "extra "); ok {
					fixtureGit(t, r, "config", "--add", "filter.lfs."+key, "git-lfs unexpected")
				}
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			requireFakeLFS(t, bin)
			t.Setenv("REFRAKTAL_TEST_LFS_MARKER", marker)
			checkMarker := func() {
				t.Helper()
				_, err := os.Stat(marker)
				if variant == "standard" {
					if err != nil {
						t.Errorf("standard LFS did not run: %v", err)
					} else if err := os.Remove(marker); err != nil {
						t.Fatal(err)
					}
				} else if !os.IsNotExist(err) {
					t.Errorf("nonstandard LFS ran: %v", err)
				}
				if _, err := os.Stat(shellMarker); !os.IsNotExist(err) {
					t.Errorf("shell suffix ran: %v", err)
				}
			}
			w, err := Create(ctx, r, filepath.Join(t.TempDir(), "wt"), "refraktal/lfs")
			if err != nil {
				t.Fatal(err)
			}
			checkMarker()
			if b, err := os.ReadFile(filepath.Join(w.Dir, "data.lfs")); err != nil || string(b) != "original\n" {
				t.Fatalf("checkout content = %q, %v", b, err)
			}
			putFile(t, w.Dir, "data.lfs", "changed\n")
			res, err := w.Collect(ctx, "LFS change")
			if err != nil {
				t.Fatal(err)
			}
			checkMarker()
			if _, err := w.Apply(ctx, res, snapStore(t)); err != nil {
				t.Fatal(err)
			}
			if variant != "standard" {
				checkMarker()
			}
			if b, err := os.ReadFile(filepath.Join(r, "data.lfs")); err != nil || string(b) != "changed\n" {
				t.Fatalf("applied content = %q, %v", b, err)
			}
		})
	}
}

func TestLFSCommandsPinnedAcrossConfigChange(t *testing.T) {
	ctx, r := context.Background(), repo(t)
	putFile(t, r, ".gitattributes", "*.lfs filter=lfs\n")
	putFile(t, r, "data.lfs", "original\n")
	fixtureGit(t, r, "add", "-A")
	fixtureGit(t, r, "commit", "-qm", "LFS fixture")
	w, err := Create(ctx, r, filepath.Join(t.TempDir(), "wt"), "refraktal/pinned-lfs")
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{
		"clean": "git-lfs clean -- %f", "smudge": "git-lfs smudge -- %f",
		"process": "git-lfs filter-process", "required": "true",
	} {
		fixtureGit(t, r, "config", "filter.lfs."+key, value)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	marker, swapped := filepath.Join(bin, "LFS-RAN"), filepath.Join(bin, "SWAPPED-RAN")
	putFile(t, bin, "git-lfs", "#!/bin/sh\nexec '"+os.Args[0]+"' -test.run=^TestFakeLFS$\n")
	// Change the commands after discovery, just before the staging process
	// reads configuration. The old snapshot must supply the actual commands.
	body := fmt.Sprintf(`#!/bin/sh
case " $* " in
*' add -A '*)
  for key in clean smudge process; do
    '%s' -C '%s' config "filter.lfs.$key" "touch '%s'; cat" || exit 99
  done
  ;;
esac
exec '%s' "$@"
`, realGit, r, swapped, realGit)
	putFile(t, bin, "git", body)
	for _, name := range []string{"git", "git-lfs"} {
		if err := os.Chmod(filepath.Join(bin, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	requireFakeLFS(t, bin)
	t.Setenv("REFRAKTAL_TEST_LFS_MARKER", marker)
	putFile(t, w.Dir, "data.lfs", "changed\n")
	_, err = w.Collect(ctx, "pinned LFS commands")
	if err == nil || !strings.Contains(err.Error(), "driver configuration changed") {
		t.Errorf("config change did not fail collect: %v", err)
	}
	if _, err := os.Stat(swapped); !os.IsNotExist(err) {
		t.Errorf("replacement LFS command ran: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("pinned standard LFS command did not run: %v", err)
	}
}

func TestLFSConfigChangeDuringCommandFails(t *testing.T) {
	for _, key := range []string{"lfs.extension.poison.clean", "lfs.customtransfer.poison.path", "lfs.standalonetransferagent"} {
		t.Run(key, func(t *testing.T) {
			ctx, r := context.Background(), repo(t)
			realGit, err := exec.LookPath("git")
			if err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			body := fmt.Sprintf("#!/bin/sh\n'%s' \"$@\"\nstatus=$?\ncase \" $* \" in *' rev-parse --verify '*) '%s' -C '%s' config '%s' '' || exit 99;; esac\nexit $status\n", realGit, realGit, r, key)
			putFile(t, bin, "git", body)
			if err := os.Chmod(filepath.Join(bin, "git"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			_, err = Create(ctx, r, filepath.Join(t.TempDir(), "wt"), "refraktal/lfs-race")
			if err == nil || !strings.Contains(err.Error(), "driver configuration changed") {
				t.Fatalf("LFS config change did not fail create: %v", err)
			}
		})
	}
}
