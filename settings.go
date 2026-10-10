package nuntius

import (
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Only pool sizes are writable by set. Other config keys remain owner-edited.
func writeSettings(out io.Writer, c Config, key string) bool {
	found := false
	for _, kind := range []string{"claude", "codex", "opencode"} {
		name := kind + "-agent-pool"
		if key != "" && key != name {
			continue
		}
		size, source := 0, "default"
		if p, ok := c.Pools[kind]; ok {
			size, source = p.Size, c.SourcePath
		} else if c.Pools == nil {
			for _, l := range c.Lanes {
				if l.Kind == kind {
					size++
					source = c.SourcePath
				}
			}
		}
		fmt.Fprintf(out, "%s = %s (%s)\n", name, strconv.Itoa(size), cell(source))
		found = true
	}
	if key == "" {
		fmt.Fprintln(out, "read-only config: lanes, pools templates, max_runtime, reports_dir, notifications, auto_resume, oversight_path, notify_imessage (default: off); tailnet defaults: off (CLI only)")
	}
	return found
}

func poolSetting(key string) bool {
	return key == "claude-agent-pool" || key == "codex-agent-pool" || key == "opencode-agent-pool"
}

func settingKind(key string) string { return strings.TrimSuffix(key, "-agent-pool") }
