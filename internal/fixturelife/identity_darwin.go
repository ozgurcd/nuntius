package fixturelife

import (
	"encoding/binary"
	"errors"
	"fmt"
	"syscall"
	"unsafe"
)

// Same kernel birth identity predicate as the server, kept in this offline
// helper so testagent does not depend on the server's launch implementation.
func processStart(pid int) (string, error) {
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
	seconds, micros := binary.LittleEndian.Uint64(b[:8]), binary.LittleEndian.Uint32(b[8:12])
	if seconds == 0 || micros >= 1000000 {
		return "", errors.New("invalid process start time")
	}
	return fmt.Sprintf("%d.%06d", seconds, micros), nil
}
