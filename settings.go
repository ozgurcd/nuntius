package nuntius

import (
	"fmt"
	"io"
	"strconv"
	"strings"
)

// settingsSnapshot exposes display values only, never recipients or launch argv.
func settingsSnapshot(c Config) map[string]any {
	pools := map[string]int{"claude": 0, "codex": 0, "opencode": 0}
	lanes := []map[string]string{}
	for _, l := range c.Lanes {
		pools[l.Kind]++
		model, effort := effectiveIdentity(l)
		lanes = append(lanes, map[string]string{"name": cell(l.Name), "model": model, "effort": effort})
	}
	for kind, p := range c.Pools {
		pools[kind] = p.Size
	}
	return map[string]any{"pools": pools, "notify_imessage": c.imessageEnabled(), "tailnet": c.PhoneAccess, "reports_dir": cell(c.ReportsDir), "max_runtime": c.MaxRuntime, "auto_resume": c.AutoResume == nil || *c.AutoResume, "lanes": lanes}
}

func (m *Manager) SetPhoneAccess(path string, enabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	guard, err := m.controlLock()
	if err != nil {
		return err
	}
	defer guard.Close()
	c, err := LoadConfig(path)
	if err != nil {
		return err
	}
	return m.commitSettings(c, path, settingsChanges{PhoneAccess: &enabled}, false)
}

func (m *Manager) SetIMessage(path string, enabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	guard, err := m.controlLock()
	if err != nil {
		return err
	}
	defer guard.Close()
	c, err := LoadConfig(path)
	if err != nil {
		return err
	}
	return m.commitSettings(c, path, settingsChanges{Notify: &enabled}, false)
}

// Pool sizes and iMessage on/off are writable. Other keys remain owner-edited.
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
		fmt.Fprintf(out, "notify-imessage = %t (recipient configured: %t)\n", c.imessageEnabled(), c.NotifyIMessage != "")
		fmt.Fprintln(out, "read-only config: lanes, pools templates, max_runtime, reports_dir, notifications, auto_resume, oversight_path")
	}
	if key == "" || key == "phone-access" {
		fmt.Fprintf(out, "phone-access = %t (next local dashboard start)\n", c.PhoneAccess)
		found = true
	}
	if key == "notify-imessage" {
		fmt.Fprintf(out, "notify-imessage = %t\n", c.imessageEnabled())
		found = true
	}
	return found
}

func poolSetting(key string) bool {
	return key == "claude-agent-pool" || key == "codex-agent-pool" || key == "opencode-agent-pool"
}

func settingKind(key string) string { return strings.TrimSuffix(key, "-agent-pool") }
