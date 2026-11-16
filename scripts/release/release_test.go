package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
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
	for v, ok := range map[string]bool{"v0.1.0": true, "v1.2.3-rc.1": true, "v1.2.3-0": true, "v1.2.3-01a": true, "v1.2.3-x-y.7": true,
		"0.1.0": false, "v1.2": false, "v01.2.3": false, "v1.2.3;rm": false,
		// Review M8 (7): no empty or leading-zero numeric prerelease identifiers.
		"v1.2.3-rc.01": false, "v1.2.3-00": false, "v1.2.3-": false, "v1.2.3-rc..1": false, "v1.2.3-.rc": false, "v1.2.3-rc.": false,
		"v1.2.3+build": false} {
		if ValidVersion(v) != ok {
			t.Errorf("%q: valid %v", v, !ok)
		}
	}
	if Prerelease("v1.2.3") || !Prerelease("v1.2.3-rc.1") {
		t.Error("prerelease detection")
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
		"refraktal_0.1.0_linux_amd64.tar.gz", `sha256 "` + sums[:64] + `"`, `bin.install "refraktal"`, `depends_on "node"`} {
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
// matches, over HTTPS only; a tampered checksum installs nothing.
func TestInstallScript(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("install.sh supports macOS and Linux")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("needs curl")
	}
	name := Archive("v0.1.0", Target{runtime.GOOS, runtime.GOARCH})
	a := archive(t, []byte("#!/bin/sh\necho refraktal-fake\n"))
	var sums atomic.Value
	latest := "v0.1.0"
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest":
			http.Redirect(w, r, "/releases/tag/"+latest, http.StatusFound)
		case "/releases/tag/" + latest:
			w.Write([]byte("ok"))
		case "/download/v0.1.0/" + name:
			w.Write(a)
		case "/download/v0.1.0/SHA256SUMS":
			w.Write([]byte(sums.Load().(string)))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o644)
	install := func(sumsText string, env ...string) (string, string, error) {
		sums.Store(sumsText)
		dir := filepath.Join(t.TempDir(), "bin")
		cmd := exec.Command("sh", "../install.sh")
		cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "TMPDIR=" + t.TempDir(), "CURL_CA_BUNDLE=" + ca,
			"REFRAKTAL_INSTALL_DIR=" + dir, "REFRAKTAL_DOWNLOAD_BASE=" + srv.URL + "/download", "REFRAKTAL_RELEASES=" + srv.URL + "/releases"}, env...)
		out, err := cmd.CombinedOutput()
		return dir, string(out), err
	}
	good := Sums(map[string][]byte{name: a})
	dir, out, err := install(good) // the latest release
	if err != nil {
		t.Fatalf("install failed: %v %s", err, out)
	}
	if got, err := exec.Command(filepath.Join(dir, "refraktal")).Output(); err != nil || strings.TrimSpace(string(got)) != "refraktal-fake" {
		t.Fatalf("installed binary: %q %v", got, err)
	}
	if staged, _ := filepath.Glob(filepath.Join(dir, ".refraktal*")); len(staged) != 0 {
		t.Fatalf("staging files left: %v", staged)
	}
	dir, out, err = install(Sums(map[string][]byte{name: []byte("something else")}), "REFRAKTAL_VERSION=v0.1.0")
	if err == nil || !strings.Contains(out, "checksum mismatch") || exists(filepath.Join(dir, "refraktal")) {
		t.Fatalf("a tampered checksum installed: %v %s", err, out)
	}
	if _, out, err := install("0000  other.tar.gz\n", "REFRAKTAL_VERSION=v0.1.0"); err == nil || !strings.Contains(out, "not listed") {
		t.Fatalf("an unlisted archive installed: %v %s", err, out)
	}
	// Review M8 (2): HTTPS only, for the base and for every download.
	for _, base := range []string{"http://127.0.0.1:1/download", "file:///tmp/download"} {
		if _, out, err := install(good, "REFRAKTAL_VERSION=v0.1.0", "REFRAKTAL_DOWNLOAD_BASE="+base); err == nil || !strings.Contains(out, "https") {
			t.Errorf("base %s accepted: %v %s", base, err, out)
		}
	}
	// Review M8 (3): the staging file is made fresh (mktemp) in the target
	// folder: a planted symlink at the old fixed name is not written through.
	dir = filepath.Join(t.TempDir(), "bin")
	os.MkdirAll(dir, 0o755)
	victim := filepath.Join(t.TempDir(), "victim")
	os.WriteFile(victim, []byte("keep\n"), 0o644)
	os.Symlink(victim, filepath.Join(dir, ".refraktal.new"))
	sums.Store(good)
	cmd := exec.Command("sh", "../install.sh")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "TMPDIR=" + t.TempDir(), "CURL_CA_BUNDLE=" + ca,
		"REFRAKTAL_INSTALL_DIR=" + dir, "REFRAKTAL_DOWNLOAD_BASE=" + srv.URL + "/download", "REFRAKTAL_VERSION=v0.1.0"}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep\n" {
		t.Fatal("the installer wrote through a planted symlink")
	}
	// Review M8 (4): "latest" never installs a prerelease.
	latest = "v0.2.0-rc.1"
	if _, out, err := install(good); err == nil || !strings.Contains(out, "prerelease") {
		t.Fatalf("latest resolved to a prerelease and was installed: %v %s", err, out)
	}
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }
