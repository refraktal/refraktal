package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReviewRunningOracleFailsClosed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ps"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	got := running(os.Getpid())
	t.Logf("self_alive=true ps_failed=true oracle_running=%v", got)
	if !got {
		t.Fatal("cleanup oracle reports current live test process dead after process inspection fails")
	}
}

func TestRunningRequiresVerifiedExit(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		wantRunning  bool
	}{
		{"diagnostic", "echo unavailable >&2; exit 1", true},
		{"other status", "exit 2", true},
		{"empty output", "exit 0", true},
		{"running", "echo S", true},
		{"zombie", "echo Z", false},
		{"zombie with flags", "echo Z+", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "ps"), []byte("#!/bin/sh\n"+tc.script+"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			if got := running(os.Getpid()); got != tc.wantRunning {
				t.Fatalf("running=%v, want %v", got, tc.wantRunning)
			}
		})
	}
}
