package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunJSONReportsWriteFailure(t *testing.T) {
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(filepath.Join(repoRoot, "cmd", "refraktal", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	// Compile the unchanged CLI implementation with an init hook that invokes
	// the real run function using an ENOSPC writer. Dry + pinned avoids all
	// harness preparation, API routing and price fetching.
	buildDir, err := os.MkdirTemp(repoRoot, ".run-json-build-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(buildDir)
	mainPath := filepath.Join(buildDir, "main.go")
	if err := os.WriteFile(mainPath, src, 0600); err != nil {
		t.Fatal(err)
	}
	const probe = `package main
import("bytes";"context";"fmt";"os";"strings";"syscall")
type fullOutput struct{ writes int }
func(w *fullOutput)Write([]byte)(int,error){w.writes++;return 0,syscall.ENOSPC}
func init(){
 if os.Getenv("REVIEW_CLI_OUTPUT_PROBE")!="1" {return}
 w:=&fullOutput{}; var errb bytes.Buffer
 code:=run(context.Background(),[]string{"run","--dry","--harness","refraktal","--json","task"},w,&errb)
 fmt.Printf("writes=%d exit=%d stderr=%q\n",w.writes,code,errb.String())
 if code!=3 || !strings.Contains(errb.String(), "refraktal run:") || !strings.Contains(errb.String(), syscall.ENOSPC.Error()) {os.Exit(1)}
 os.Exit(0)
}
`
	probePath := filepath.Join(buildDir, "probe.go")
	if err := os.WriteFile(probePath, []byte(probe), 0600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(buildDir, "cli-probe")
	build := exec.Command("go", "build", "-o", bin, mainPath, probePath)
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("compile: %v %s", err, out)
	}
	// Default roots are deliberately prohibited under /tmp. Keep the fixture
	// beside the checkout and remove it after the test.
	fixture, err := os.MkdirTemp(repoRoot, ".run-json-fixture-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(fixture)
	repo := filepath.Join(fixture, "repo")
	home := filepath.Join(fixture, "home")
	for _, dir := range []string{repo, home} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "GOCACHE=" + os.Getenv("GOCACHE"), "REVIEW_CLI_OUTPUT_PROBE=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null"}
	initRepo := exec.Command("git", "init", "--quiet", repo)
	initRepo.Env = env
	if out, err := initRepo.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	cmd := exec.Command(bin)
	cmd.Dir = repo
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	t.Log(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("JSON output failure did not report ENOSPC on stderr and exit 3: %v", err)
	}
	if !strings.Contains(string(out), "writes=1") {
		t.Fatalf("probe never reached output: %s", out)
	}
}
