package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The curl fixture serves release files from disk. No listener or network is
// used, while extraction, checksum verification and installation stay real.
func offlineInstaller(t *testing.T) (string, string, func(string) (string, error)) {
	t.Helper()
	if (runtime.GOOS != "darwin" && runtime.GOOS != "linux") || (runtime.GOARCH != "arm64" && runtime.GOARCH != "amd64") {
		t.Skip("installer supports macOS and Linux on arm64 and amd64")
	}
	root := t.TempDir()
	release, bin := filepath.Join(root, "release"), filepath.Join(root, "tools")
	for _, dir := range []string{release, bin} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	name := Archive("v0.1.0", Target{runtime.GOOS, runtime.GOARCH})
	a := archive(t, []byte("#!/bin/sh\necho refraktal-offline\n"))
	for name, data := range map[string][]byte{name: a, "SHA256SUMS": []byte(Sums(map[string][]byte{name: a}))} {
		if err := os.WriteFile(filepath.Join(release, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	curl := `#!/bin/sh
set -eu
out=""
url=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) out=$2; shift 2 ;;
    --proto|--proto-redir) shift 2 ;;
    -fsSL) shift ;;
    https://offline.invalid/*) url=$1; shift ;;
    *) echo "unexpected fake curl argument" >&2; exit 2 ;;
  esac
done
[ -n "$out" ] && [ -n "$url" ]
cp "$FAKE_RELEASE/${url##*/}" "$out"
`
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(curl), 0o755); err != nil {
		t.Fatal(err)
	}
	install := func(dir string) (string, error) {
		cmd := exec.Command("sh", "../install.sh")
		cmd.Env = []string{
			"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
			"HOME=" + root, "TMPDIR=" + root, "FAKE_RELEASE=" + release,
			"REFRAKTAL_VERSION=v0.1.0", "REFRAKTAL_INSTALL_DIR=" + dir,
			"REFRAKTAL_DOWNLOAD_BASE=https://offline.invalid/download",
			"REFRAKTAL_RELEASES=https://offline.invalid/releases",
		}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	return root, bin, install
}

func TestInstallScriptRejectsDirectoryDestinationOffline(t *testing.T) {
	for _, link := range []bool{false, true} {
		name := "directory"
		if link {
			name = "symlink_to_directory"
		}
		t.Run(name, func(t *testing.T) {
			root, _, install := offlineInstaller(t)
			dir, target := filepath.Join(root, "bin"), filepath.Join(root, "target")
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			dest := filepath.Join(dir, "refraktal")
			if link {
				if err := os.Mkdir(target, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, dest); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(dest, 0o755); err != nil {
					t.Fatal(err)
				}
				target = dest
			}
			out, err := install(dir)
			if err == nil || !strings.Contains(out, "is a directory") || strings.Contains(out, "installed ") {
				t.Fatalf("directory destination reported success or unclear error: %v\n%s", err, out)
			}
			if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
				t.Fatalf("installer wrote into destination directory: entries=%v error=%v", entries, err)
			}
		})
	}
}

func TestInstallScriptRejectsUnwritableDestinationOffline(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can bypass file write permissions")
	}
	for _, directory := range []bool{false, true} {
		name := "file"
		if directory {
			name = "install_directory"
		}
		t.Run(name, func(t *testing.T) {
			root, _, install := offlineInstaller(t)
			dir := filepath.Join(root, "bin")
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			dest := filepath.Join(dir, "refraktal")
			if err := os.WriteFile(dest, []byte("keep existing binary\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			readonly := dest
			if directory {
				readonly = dir
			}
			if err := os.Chmod(readonly, 0o555); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(readonly, 0o755) })
			out, err := install(dir)
			if err == nil || !strings.Contains(out, "not writable") || strings.Contains(out, "installed ") {
				t.Fatalf("unwritable destination reported success or unclear error: %v\n%s", err, out)
			}
			if got, err := os.ReadFile(dest); err != nil || string(got) != "keep existing binary\n" {
				t.Fatalf("unwritable destination was modified: %q %v", got, err)
			}
		})
	}
}

func TestInstallScriptChecksFinalExecutableOffline(t *testing.T) {
	root, bin, install := offlineInstaller(t)
	realMV, err := exec.LookPath("mv")
	if err != nil {
		t.Fatal(err)
	}
	// Model a rename which succeeds but leaves a nonexecutable destination.
	// The success message must depend on the installed file's final state.
	move := fmt.Sprintf("#!/bin/sh\nset -eu\nfor dest do :; done\n'%s' \"$@\"\nchmod 0644 \"$dest\"\n", strings.ReplaceAll(realMV, "'", "'\\''"))
	if err := os.WriteFile(filepath.Join(bin, "mv"), []byte(move), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := install(filepath.Join(root, "bin"))
	if err == nil || !strings.Contains(out, "not an executable file") || strings.Contains(out, "installed ") {
		t.Fatalf("nonexecutable destination reported success or unclear error: %v\n%s", err, out)
	}
}

func TestInstallScriptInstallsAndReplacesOffline(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(fmt.Sprintf("replacement=%t", replacement), func(t *testing.T) {
			root, _, install := offlineInstaller(t)
			dir := filepath.Join(root, "bin")
			dest := filepath.Join(dir, "refraktal")
			if replacement {
				if err := os.Mkdir(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(dest, []byte("#!/bin/sh\necho old\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if out, err := install(dir); err != nil || !strings.Contains(out, "installed "+dest) {
				t.Fatalf("offline installation failed: %v\n%s", err, out)
			}
			if got, err := exec.Command(dest).Output(); err != nil || string(got) != "refraktal-offline\n" {
				t.Fatalf("installed executable: %q %v", got, err)
			}
			if staged, err := filepath.Glob(filepath.Join(dir, ".refraktal*")); err != nil || len(staged) != 0 {
				t.Fatalf("staging files remain: %v %v", staged, err)
			}
		})
	}
}
