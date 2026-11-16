package proc

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReviewUnreadableSessionsLooksLikeCleanShutdown(t *testing.T) {
	// Like the apply-failure seam, this fails the real inventory read even
	// when permissions cannot deny it (root and CI).
	injected := errors.New("injected: sessions read denied")
	report := assertSessionInventoryUnknown(t, func(t *testing.T, sessions string) {
		oldRead := testBeforeSessionRead
		testBeforeSessionRead = func(path string) error {
			if path != sessions {
				return nil
			}
			return injected
		}
		t.Cleanup(func() { testBeforeSessionRead = oldRead })
	})
	if !strings.Contains(report.Err, injected.Error()) {
		t.Fatalf("shutdown error = %q; want injected read failure", report.Err)
	}
}

func TestMalformedSessionsKeepsLiveRecord(t *testing.T) {
	for _, contents := range []string{
		"not json\n",
		`{"pid":64000,"start":"Sun Nov 15 15:45:52 2026"}` + "\nnot json\n",
		`{"pid":64000,"start":"invalid"}` + "\n",
		`{"pid":0,"start":"Sun Nov 15 15:45:52 2026"}` + "\n",
		strings.Repeat("x", 5000) + "\n",
	} {
		t.Run(contents[:min(len(contents), 24)], func(t *testing.T) {
			assertSessionInventoryUnknown(t, func(t *testing.T, sessions string) {
				if err := os.WriteFile(sessions, []byte(contents), 0o600); err != nil {
					t.Fatal(err)
				}
			})
		})
	}
}

func assertSessionInventoryUnknown(t *testing.T, corrupt func(*testing.T, string)) ShutdownReport {
	t.Helper()
	fakePS(t, "exit 0")
	sent := interceptSignals(t)
	oldMembers := membersOf
	membersOf = func(int) ([]Proc, bool, error) { return nil, false, nil }
	t.Cleanup(func() { membersOf = oldMembers })
	dir := t.TempDir()
	sessions := filepath.Join(dir, "sessions")
	if err := os.WriteFile(sessions, []byte(`{"pid":64000,"start":"Sun Nov 15 15:45:52 2026"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c, err := Start(Spec{Argv: []string{self}, Env: []string{helperEnv + "=exit", "GORACE=atexit_sleep_ms=0"},
		SessionsFile: sessions, LiveDir: filepath.Join(dir, "live"), PollEvery: time.Hour, SessionPollEvery: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown(time.Second)
	corrupt(t, sessions)
	report := c.Shutdown(time.Second)
	records, err := filepath.Glob(filepath.Join(dir, "live", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("shutdown error=%q unknown=%d remaining live records=%d", report.Err, len(report.Swept.Unknown), len(records))
	if report.Err == "" || len(records) != 1 {
		t.Fatal("unknown session inventory must report an error and retain the live record")
	}
	if len(*sent) != 0 {
		t.Fatalf("inventory failure signalled a process: %v", *sent)
	}
	rec, err := loadRecord(records[0])
	if err != nil || !rec.InventoryUnknown {
		t.Fatalf("live record must preserve unknown inventory: record=%+v err=%v", rec, err)
	}
	// The deterministic empty snapshot makes the owner appear dead. Inventory
	// uncertainty must survive the handoff from Shutdown to dead-run cleanup.
	cleanup, err := CleanLeftovers(filepath.Join(dir, "live"))
	if err != nil {
		t.Fatal(err)
	}
	records, err = filepath.Glob(filepath.Join(dir, "live", "*.json"))
	if err != nil || len(records) != 1 || cleanup.Dead != 1 ||
		!strings.Contains(strings.Join(cleanup.Errors, "\n"), "recorded sessions inventory is unknown") {
		t.Fatalf("dead-run cleanup lost unknown inventory: records=%d errors=%v err=%v", len(records), cleanup.Errors, err)
	}
	return report
}
