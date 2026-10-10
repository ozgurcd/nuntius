package main

import "fmt"

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
	LoginKnown bool
}
type Setting struct{ Title, Action string }

func (c *Controller) refreshSettings() {
	t, ok := c.tray.(interface{ Settings([]Setting) })
	if !ok || c.settings == nil {
		return
	}
	s, err := c.settings()
	if err != nil {
		t.Settings([]Setting{{Title: "Status unavailable; run nuntius config check"}})
		return
	}
	rows := []Setting{{Title: fmt.Sprintf("Agents: Claude %d · Codex %d · OpenCode %d", s.Pools["claude"], s.Pools["codex"], s.Pools["opencode"])}}
	if !s.LoginKnown {
		rows = append(rows, Setting{Title: "Start helper at login: Unknown / unavailable"})
	} else {
		next, state := "on", "Off"
		if s.AtLogin {
			next, state = "off", "On"
		}
		rows = append(rows, Setting{Title: "Start helper at login: " + state + " (toggle)", Action: "setting:login:" + next})
	}
	t.Settings(rows)
}
func (c *Controller) settingsAction(action string) bool {
	if action != "setting:login:on" && action != "setting:login:off" {
		return false
	}
	on := "on"
	if action == "setting:login:off" {
		on = "off"
	}
	if c.apply != nil && c.apply([]string{"menubar", "--at-login", on}) != nil {
		c.tray.Update("●", "Nuntius · login setting failed; check locally", nil, false)
	}
	c.refreshSettings()
	return true
}
