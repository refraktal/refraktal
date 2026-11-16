package proc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Each child checks its own still-live process. A failed inspection must fail
// the assertion, rather than pass by claiming that this process has exited.
func TestReviewCleanupOracleFailsClosed(t *testing.T) {
	if mode := os.Getenv("REFRAKTAL_TEST_CLEANUP_ORACLE"); mode != "" {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("member-%d", os.Getpid())), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		fakePS(t, "echo 'ps unavailable' >&2; exit 1")
		switch mode {
		case "running":
			running(t, os.Getpid())
		case "survivors":
			survivors(t, dir)
		default:
			t.Fatal("unknown oracle")
		}
		return
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"running", "survivors"} {
		t.Run(mode, func(t *testing.T) {
			cmd := exec.Command(self, "-test.run=^TestReviewCleanupOracleFailsClosed$", "-test.count=1")
			cmd.Env = append(os.Environ(), "REFRAKTAL_TEST_CLEANUP_ORACLE="+mode, "GORACE=atexit_sleep_ms=0")
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || !strings.Contains(string(out), "inspect process exit: ps:") {
				t.Fatalf("%s oracle accepted failed process inspection: err=%v output=%s", mode, err, out)
			}
		})
	}
}
