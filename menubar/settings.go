package main

import (
	"fmt"
	"strconv"
	"strings"
)

type LaneIdentity struct {
	Name   string `json:"name"`
	Model  string `json:"model"`
	Effort string `json:"effort"`
}
type Settings struct {
	Pools      map[string]int `json:"pools"`
	Notify     bool           `json:"notify_imessage"`
	Tailnet    bool           `json:"tailnet"`
	ReportsDir string         `json:"reports_dir"`
	MaxRuntime string         `json:"max_runtime"`
	AutoResume bool           `json:"auto_resume"`
	Lanes      []LaneIdentity `json:"lanes"`
	AtLogin    bool
}
type Setting struct{ Title, Action string }

func (c *Controller) refreshSettings() {
	t, ok := c.tray.(interface{ Settings([]Setting) })
	if !ok || c.settings == nil {
		return
	}
	s, err := c.settings()
	if err != nil {
		t.Settings([]Setting{{Title: "Settings unavailable; run nuntius config check"}})
		return
	}
	rows := []Setting{}
	for _, kind := range []string{"claude", "codex", "opencode"} {
		rows = append(rows, Setting{Title: fmt.Sprintf("%s pool = %d", kind, s.Pools[kind])})
		for n := 0; n <= 8; n++ {
			rows = append(rows, Setting{Title: fmt.Sprintf("Set %s pool to %d…", kind, n), Action: fmt.Sprintf("setting:%s:%d", kind, n)})
		}
	}
	onOff := func(on bool) string {
		if on {
			return "on"
		}
		return "off"
	}
	rows = append(rows, Setting{Title: "notify_imessage = " + onOff(s.Notify)}, Setting{Title: "Turn iMessage on…", Action: "setting:notify:on"}, Setting{Title: "Turn iMessage off…", Action: "setting:notify:off"}, Setting{Title: "tailnet default = " + onOff(s.Tailnet)}, Setting{Title: "reports_dir = " + s.ReportsDir}, Setting{Title: "max_runtime = " + s.MaxRuntime}, Setting{Title: fmt.Sprintf("auto_resume = %t", s.AutoResume)})
	for _, l := range s.Lanes {
		rows = append(rows, Setting{Title: fmt.Sprintf("%s: model=%s effort=%s", l.Name, l.Model, l.Effort)})
	}
	nextLogin := "on"
	if s.AtLogin {
		nextLogin = "off"
	}
	rows = append(rows, Setting{Title: "Open settings file", Action: "settings-file"}, Setting{Title: "Start at login = " + onOff(s.AtLogin) + " (toggle…)", Action: "setting:login:" + nextLogin})
	confirm := "No pending change"
	if len(c.pending) > 0 {
		confirm = "Confirm: nuntius " + strings.Join(c.pending, " ")
	}
	rows = append(rows, Setting{Title: confirm, Action: "confirm"}, Setting{Title: "Discard pending change", Action: "dismiss"})
	t.Settings(rows)
}

func (c *Controller) settingsAction(action string) bool {
	if action == "dismiss" {
		c.pending = nil
		c.refreshSettings()
		return true
	}
	if action == "confirm" {
		args := c.pending
		c.pending = nil
		if len(args) > 0 && c.apply != nil && c.apply(args) != nil {
			c.tray.Update("●", "Nuntius · settings command failed; run nuntius config check", nil, false)
		}
		c.refreshSettings()
		return true
	}
	if action == "settings-file" {
		if c.reveal != nil {
			c.reveal()
		}
		return true
	}
	if !strings.HasPrefix(action, "setting:") {
		return false
	}
	parts := strings.Split(action, ":")
	if len(parts) != 3 {
		return true
	}
	switch parts[1] {
	case "claude", "codex", "opencode":
		n, err := strconv.Atoi(parts[2])
		if err != nil || n < 0 || n > 8 {
			return true
		}
		c.pending = []string{"set", parts[1] + "-agent-pool", strconv.Itoa(n)}
	case "notify", "login":
		if parts[2] != "on" && parts[2] != "off" {
			return true
		}
		if parts[1] == "notify" {
			c.pending = []string{"set", "notify-imessage", parts[2]}
		} else {
			c.pending = []string{"menubar", "--at-login", parts[2]}
		}
	default:
		return true
	}
	c.refreshSettings()
	return true
}
