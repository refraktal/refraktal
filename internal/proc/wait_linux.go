package proc

import (
	"syscall"
	"unsafe"
)

// waitExited blocks until pid has exited, without reaping it (waitid with
// WNOWAIT), so the exited child keeps its PID, and with it the session ID,
// reserved until the caller reaps it.
func waitExited(pid int) error {
	const pPID, wEXITED, wNOWAIT = 1, 0x4, 0x01000000
	var info [128]byte
	for {
		_, _, e := syscall.Syscall6(syscall.SYS_WAITID, pPID, uintptr(pid), uintptr(unsafe.Pointer(&info[0])), wEXITED|wNOWAIT, 0, 0)
		if e == syscall.EINTR {
			continue
		}
		if e != 0 {
			return e
		}
		return nil
	}
}
