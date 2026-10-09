package nuntius

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

func processStart(pid int) (string, error) {
	b, e := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if e != nil {
		return "", e
	}
	end := strings.LastIndexByte(string(b), ')')
	if end < 0 {
		return "", errors.New("invalid process status")
	}
	f := strings.Fields(string(b[end+1:]))
	if len(f) < 20 || f[0] == "Z" || f[0] == "X" {
		return "", errors.New("process unavailable")
	}
	boot, e := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if e != nil {
		return "", e
	}
	return strings.TrimSpace(string(boot)) + ":" + f[19], nil
}
