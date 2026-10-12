package fixturelife

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

func processStart(pid int) (string, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", err
	}
	end := strings.LastIndexByte(string(b), ')')
	if end < 0 {
		return "", errors.New("invalid process status")
	}
	f := strings.Fields(string(b[end+1:]))
	if len(f) < 20 || f[0] == "Z" || f[0] == "X" {
		return "", errors.New("process unavailable")
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(boot)) + ":" + f[19], nil
}
