//go:build !linux

package proc

import "testing"

func requirePIDFDKillSafety(t *testing.T) {
	t.Helper()
	t.Skip("individual PID signals on macOS have no pidfd; the documented check-to-signal window remains")
}
