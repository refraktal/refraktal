package refraktal

import (
	"bytes"
	"encoding/json"
	"runtime/debug"
	"strings"
	"testing"
)

// The version is the release stamp, else the module version go install
// recorded, else "dev" for a local build.
func TestResolveVersion(t *testing.T) {
	bi := func(v string) (*debug.BuildInfo, bool) { return &debug.BuildInfo{Main: debug.Module{Version: v}}, true }
	for _, tc := range []struct {
		stamp, module, want string
	}{
		{"v0.2.0", "v0.1.0", "v0.2.0"},
		{"", "v0.1.0", "v0.1.0"},
		{"", "(devel)", "dev"},
		{"", "", "dev"},
	} {
		if got := resolveVersion(tc.stamp, func() (*debug.BuildInfo, bool) { return bi(tc.module) }); got != tc.want {
			t.Errorf("stamp %q module %q: got %q, want %q", tc.stamp, tc.module, got, tc.want)
		}
	}
	if got := resolveVersion("", func() (*debug.BuildInfo, bool) { return nil, false }); got != "dev" {
		t.Errorf("no build info: %q", got)
	}
}

// doctor reports the version, in text and in --json: there is no version
// flag in the CLI surface.
func TestDoctorReportsTheVersion(t *testing.T) {
	r := newRig(t)
	rep := r.doctor(t)
	if rep.Version == "" || rep.Version != Version {
		t.Fatalf("version %q, want %q", rep.Version, Version)
	}
	var text, js bytes.Buffer
	WriteDoctorText(&text, rep)
	if !strings.HasPrefix(text.String(), "refraktal "+Version+"\n") {
		t.Fatalf("text starts %q", strings.SplitN(text.String(), "\n", 2)[0])
	}
	WriteDoctorJSON(&js, rep)
	lines := bytes.Split(bytes.TrimSpace(js.Bytes()), []byte("\n"))
	var last map[string]any
	json.Unmarshal(lines[len(lines)-1], &last)
	if last["type"] != "summary" || last["version"] != Version {
		t.Fatalf("json summary line %v", last)
	}
}
