package verify

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Inspect the actual production dependency graphs, including indirect imports.
// Do not use -test: the root's finding reproductions are intentionally tests.
func TestProductionDoesNotImportVerify(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "-json", ".", "./cmd/refraktal")
	cmd.Dir = ".."
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("inspect production dependencies: %v", err)
	}
	dec := json.NewDecoder(strings.NewReader(string(out)))
	for dec.More() {
		var pkg struct {
			ImportPath string
		}
		if err := dec.Decode(&pkg); err != nil {
			t.Fatal(err)
		}
		if pkg.ImportPath == "refraktal.dev/refraktal/verify" {
			t.Fatal("production binary imports the test-only model checker")
		}
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".go") && !strings.HasSuffix(entry.Name(), "_test.go") {
			t.Errorf("verify contains non-test Go source: %s", entry.Name())
		}
	}
}
