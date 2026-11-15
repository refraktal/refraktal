package proc

import "syscall"

// getsid returns the session ID of pid. Go's syscall package has no Getsid
// on Linux, so this makes the system call directly.
func getsid(pid int) (int, error) {
	sid, _, errno := syscall.RawSyscall(syscall.SYS_GETSID, uintptr(pid), 0, 0)
	if errno != 0 {
		return 0, errno
	}
	return int(sid), nil
}
