package nuntius

import (
	"encoding/binary"
	"errors"
	"fmt"
	"syscall"
	"unsafe"
)

// XNU kern.proc.pid exports kinfo_proc beginning with extern_proc.
// Its p_starttime is timeval at offset 0, p_stat at 36, p_pid at 40
// on the supported 64-bit Darwin targets. Use microseconds, not ps seconds.
// https://github.com/apple-oss-distributions/xnu/blob/main/bsd/sys/proc.h
func processStart(pid int) (string, error) {
	// CTL_KERN, KERN_PROC, KERN_PROC_PID: the PID is a numeric MIB suffix,
	// not a named sysctl node resolved by Sysctl's name-to-OID lookup.
	mib := []int32{1, 14, 1, int32(pid)}
	b := make([]byte, 4096)
	size := uintptr(len(b))
	_, _, errno := syscall.Syscall6(syscall.SYS___SYSCTL, uintptr(unsafe.Pointer(&mib[0])), uintptr(len(mib)), uintptr(unsafe.Pointer(&b[0])), uintptr(unsafe.Pointer(&size)), 0, 0)
	if errno != 0 {
		return "", errno
	}
	b = b[:size]
	if len(b) < 44 || b[36] == 5 || int(binary.LittleEndian.Uint32(b[40:44])) != pid {
		return "", errors.New("process unavailable")
	}
	seconds := binary.LittleEndian.Uint64(b[:8])
	micros := binary.LittleEndian.Uint32(b[8:12])
	if seconds == 0 || micros >= 1000000 {
		return "", errors.New("invalid process start time")
	}
	return fmt.Sprintf("%d.%06d", seconds, micros), nil
}
