package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func archive(t *testing.T, bin []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := pack(&buf, []packed{{"refraktal", 0o755, bin}, {"LICENSE", 0o644, []byte("l")}, {"README.md", 0o644, []byte("r")}}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// The same inputs give the same bytes: fixed timestamps, owners and order.
func TestArchivesAreDeterministic(t *testing.T) {
	a, b := archive(t, []byte("binary")), archive(t, []byte("binary"))
	if !bytes.Equal(a, b) {
		t.Fatal("two packs of the same files differ")
	}
	gz, err := gzip.NewReader(bytes.NewReader(a))
	if err != nil {
		t.Fatal(err)
	}
	if !gz.ModTime.IsZero() || gz.Name != "" {
		t.Fatalf("gzip header carries a time or name: %v %q", gz.ModTime, gz.Name)
	}
	tr := tar.NewReader(gz)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if !h.ModTime.Equal(epoch) || h.Uid != 0 || h.Gid != 0 {
			t.Fatalf("%s: header %+v", h.Name, h)
		}
		if h.Name == "refraktal" && h.Mode != 0o755 {
			t.Fatalf("binary mode %o", h.Mode)
		}
		names = append(names, h.Name)
	}
	if strings.Join(names, ",") != "refraktal,LICENSE,README.md" {
		t.Fatalf("entries %v", names)
	}
}

func TestSumsAndNames(t *testing.T) {
	s := Sums(map[string][]byte{"b.tar.gz": []byte("b"), "a.tar.gz": []byte("a")})
	want := "ca978112ca1bbdcafac231b39a23dc4da786eff8147c4e72b9807785afee48bb  a.tar.gz\n" +
		"3e23e8160039594a33894f6564e1b1348bbd7a0088d42c4acb73eeaed59c009d  b.tar.gz\n"
	if s != want {
		t.Fatalf("sums:\n%s", s)
	}
	if got := Archive("v0.1.0", Target{"darwin", "arm64"}); got != "refraktal_0.1.0_darwin_arm64.tar.gz" {
		t.Fatal(got)
	}
	for v, ok := range map[string]bool{"v0.1.0": true, "v1.2.3-rc.1": true, "0.1.0": false, "v1.2": false, "v01.2.3": false, "v1.2.3;rm": false} {
		if semver.MatchString(v) != ok {
			t.Errorf("%q: semver %v", v, !ok)
		}
	}
}

// The formula names every target's archive with its checksum.
func TestFormula(t *testing.T) {
	archives := map[Target][]byte{}
	for _, tg := range Targets {
		archives[tg] = archive(t, []byte(tg.OS+tg.Arch))
	}
	f, err := Formula("v0.1.0", archives)
	if err != nil {
		t.Fatal(err)
	}
	sums := Sums(map[string][]byte{"x": archives[Target{"linux", "amd64"}]})
	for _, want := range []string{`version "0.1.0"`, "releases/download/v0.1.0/refraktal_0.1.0_darwin_arm64.tar.gz",
		"refraktal_0.1.0_linux_amd64.tar.gz", `sha256 "` + sums[:64] + `"`, `bin.install "refraktal"`} {
		if !strings.Contains(f, want) {
			t.Errorf("formula lacks %q", want)
		}
	}
	delete(archives, Target{"linux", "arm64"})
	if _, err := Formula("v0.1.0", archives); err == nil {
		t.Fatal("a missing target was accepted")
	}
}

// install.sh installs the archive for this machine only when SHA256SUMS
// matches; a tampered checksum installs nothing.
func TestInstallScript(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("install.sh supports macOS and Linux")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("needs curl")
	}
	base := t.TempDir()
	rel := filepath.Join(base, "v0.1.0")
	os.MkdirAll(rel, 0o755)
	name := Archive("v0.1.0", Target{runtime.GOOS, runtime.GOARCH})
	a := archive(t, []byte("#!/bin/sh\necho refraktal-fake\n"))
	os.WriteFile(filepath.Join(rel, name), a, 0o644)
	install := func(sums string) (string, error) {
		os.WriteFile(filepath.Join(rel, "SHA256SUMS"), []byte(sums), 0o644)
		dir := filepath.Join(t.TempDir(), "bin")
		cmd := exec.Command("sh", "../install.sh")
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "TMPDIR=" + t.TempDir(),
			"REFRAKTAL_VERSION=v0.1.0", "REFRAKTAL_INSTALL_DIR=" + dir, "REFRAKTAL_DOWNLOAD_BASE=file://" + base}
		out, err := cmd.CombinedOutput()
		return dir, func() error {
			if err != nil {
				return &exec.ExitError{ProcessState: cmd.ProcessState, Stderr: out}
			}
			return nil
		}()
	}
	dir, err := install(Sums(map[string][]byte{name: a}))
	if err != nil {
		t.Fatalf("install failed: %v %s", err, err.(*exec.ExitError).Stderr)
	}
	if out, err := exec.Command(filepath.Join(dir, "refraktal")).Output(); err != nil || strings.TrimSpace(string(out)) != "refraktal-fake" {
		t.Fatalf("installed binary: %q %v", out, err)
	}
	dir, err = install(Sums(map[string][]byte{name: []byte("something else")}))
	if err == nil || !strings.Contains(string(err.(*exec.ExitError).Stderr), "checksum mismatch") {
		t.Fatalf("a tampered checksum installed: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, "refraktal")); serr == nil {
		t.Fatal("a binary was installed despite the checksum mismatch")
	}
	if _, err := install("0000  other.tar.gz\n"); err == nil || !strings.Contains(string(err.(*exec.ExitError).Stderr), "not listed") {
		t.Fatalf("an unlisted archive installed: %v", err)
	}
}
