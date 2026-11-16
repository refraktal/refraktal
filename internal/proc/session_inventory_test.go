package proc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReviewUnreadableSessionsLooksLikeCleanShutdown(t *testing.T) {
	assertSessionInventoryUnknown(t, func(t *testing.T, sessions string) {
		if err := os.Chmod(sessions, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(sessions, 0o600) })
		if _, err := RecordedSessions(sessions); err == nil {
			t.Fatal("fixture did not deny reads")
		}
	})
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

func assertSessionInventoryUnknown(t *testing.T, corrupt func(*testing.T, string)) {
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
	// The deterministic empty snapshot makes the owner appear dead. Inventory
	// uncertainty must survive the handoff from Shutdown to dead-run cleanup.
	cleanup, err := CleanLeftovers(filepath.Join(dir, "live"))
	if err != nil {
		t.Fatal(err)
	}
	records, err = filepath.Glob(filepath.Join(dir, "live", "*.json"))
	if err != nil || len(records) != 1 || len(cleanup.Errors) == 0 {
		t.Fatalf("dead-run cleanup lost unknown inventory: records=%d errors=%v err=%v", len(records), cleanup.Errors, err)
	}
}
