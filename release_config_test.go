package nuntius

import (
	"errors"
	"strings"
)

// Fail closed on unsupported YAML forms rather than interpreting new release
// targets loosely. Every goos block must be an explicit Darwin/Linux list.
func checkReleaseGOOS(text string) error {
	lines := strings.Split(text, "\n")
	found := false
	for i, line := range lines {
		clean, _, _ := strings.Cut(line, "#")
		trim := strings.TrimSpace(clean)
		if !strings.HasPrefix(trim, "goos:") {
			continue
		}
		found = true
		if trim != "goos:" {
			return errors.New("goos must be an explicit block list of darwin and linux")
		}
		indent := len(clean) - len(strings.TrimLeft(clean, " "))
		count := 0
		for _, next := range lines[i+1:] {
			next, _, _ = strings.Cut(next, "#")
			value := strings.TrimSpace(next)
			if value == "" {
				continue
			}
			if len(next)-len(strings.TrimLeft(next, " ")) <= indent {
				break
			}
			if !strings.HasPrefix(value, "- ") {
				return errors.New("invalid goos list")
			}
			value = strings.Trim(strings.TrimSpace(strings.TrimPrefix(value, "- ")), "\"'")
			if value != "darwin" && value != "linux" {
				return errors.New("release goos must be darwin or linux only")
			}
			count++
		}
		if count == 0 {
			return errors.New("empty goos list")
		}
	}
	if !found {
		return errors.New("release config must declare goos")
	}
	return nil
}
