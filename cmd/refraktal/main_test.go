package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCommands(t *testing.T) {
	for _, tc := range []struct {
		args []string
		code int
		out  string // expected in stdout or stderr
	}{
		{nil, 3, "usage: refraktal"},
		{[]string{"--help"}, 0, "doctor          check harnesses"},
		{[]string{"run", "fix it"}, 3, "--harness is required"},
		{[]string{"run", "fix it", "--harness", "claude,codex"}, 3, "one harness at a time"},
		{[]string{"run", "--dry", "--harness", "claude", "x"}, 3, "not implemented yet"},
		{[]string{"run", "--mins", "0", "--harness", "claude", "x"}, 3, "--mins must be more than 0"},
		{[]string{"run", "--usd", "-1", "--harness", "claude", "x"}, 3, "must not be negative"},
		{[]string{"show"}, 3, "not implemented yet"},
		{[]string{"bogus"}, 3, `unknown command "bogus"`},
		{[]string{"doctor", "--bogus"}, 3, "flag provided but not defined"},
	} {
		var out, errb bytes.Buffer
		code := run(context.Background(), tc.args, &out, &errb)
		if code != tc.code || !strings.Contains(out.String()+errb.String(), tc.out) {
			t.Errorf("%v: code %d, output %q", tc.args, code, out.String()+errb.String())
		}
	}
}

func TestReadTask(t *testing.T) {
	// A pipe and a regular file are read and joined to the words.
	r, w, _ := os.Pipe()
	w.WriteString("details from stdin\n")
	w.Close()
	if got, err := readTask([]string{"fix", "it"}, r); err != nil || got != "fix it\n\ndetails from stdin" {
		t.Fatalf("pipe: %q %v", got, err)
	}
	f := filepath.Join(t.TempDir(), "task")
	os.WriteFile(f, []byte("only stdin"), 0o600)
	fh, _ := os.Open(f)
	if got, _ := readTask(nil, fh); got != "only stdin" {
		t.Fatalf("file: %q", got)
	}
	// A terminal-like device and a socket that never closes are not read.
	null, _ := os.Open(os.DevNull)
	if got, _ := readTask([]string{"x"}, null); got != "x" {
		t.Fatalf("/dev/null: %q", got)
	}
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	sock := os.NewFile(uintptr(fds[0]), "sock")
	defer sock.Close()
	defer syscall.Close(fds[1])
	done := make(chan string, 1)
	go func() { got, _ := readTask([]string{"y"}, sock); done <- got }()
	select {
	case got := <-done:
		if got != "y" {
			t.Fatalf("socket: %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("readTask blocked on a socket that never closes")
	}
}
