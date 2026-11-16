package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoctorJSONReportsWriteFailure(t *testing.T) {
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(filepath.Join(repoRoot, "cmd", "refraktal", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	// Substitute only the doctor dependency, preserving the CLI's parsing,
	// JSON writer and exit handling. No harness or network can be reached.
	const dependency = "refraktal.Doctor(ctx, refraktal.DoctorOptions{ShellPrefix: selfPath()})"
	if strings.Count(string(src), dependency) != 1 {
		t.Fatal("expected exactly one doctor dependency to substitute")
	}
	src = []byte(strings.Replace(string(src), dependency, "probeDoctor(ctx, refraktal.DoctorOptions{ShellPrefix: selfPath()})", 1))
	// As in TestRunJSONReportsWriteFailure, build beside the checkout so the
	// copied CLI can import the module's internal packages.
	buildDir, err := os.MkdirTemp(repoRoot, ".doctor-json-build-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(buildDir)
	mainPath := filepath.Join(buildDir, "main.go")
	if err := os.WriteFile(mainPath, src, 0600); err != nil {
		t.Fatal(err)
	}
	const probe = `package main
import("bytes";"context";"fmt";"os";"syscall";"refraktal.dev/refraktal")
func probeDoctor(context.Context,refraktal.DoctorOptions)(refraktal.DoctorReport,error){
 return refraktal.DoctorReport{Usable:[]string{"synthetic"}},nil
}
type fullOutput struct{ writes int }
func(w *fullOutput)Write([]byte)(int,error){w.writes++;return 0,syscall.ENOSPC}
func init(){
 if os.Getenv("REVIEW_CLI_OUTPUT_PROBE")!="1" {return}
 w:=&fullOutput{}; var errb bytes.Buffer
 code:=run(context.Background(),[]string{"doctor","--json"},w,&errb)
 fmt.Printf("writes=%d exit=%d stderr=%q\n",w.writes,code,errb.String())
 if w.writes!=1 || code!=3 || errb.String()!="refraktal doctor: "+syscall.ENOSPC.Error()+"\n" {os.Exit(1)}
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
	cmd := exec.Command(bin)
	cmd.Dir = buildDir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "GOCACHE=" + os.Getenv("GOCACHE"), "REVIEW_CLI_OUTPUT_PROBE=1"}
	out, err := cmd.CombinedOutput()
	t.Log(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("JSON output failure did not report ENOSPC on stderr and exit 3: %v", err)
	}
}
