//go:build !linux

package proc

import "syscall"

// getsid returns the session ID of pid.
func getsid(pid int) (int, error) { return syscall.Getsid(pid) }
