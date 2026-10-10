package main

import (
	"fmt"
	"strings"
)

type Lane struct {
	Lane     string `json:"lane"`
	Goal     string `json:"goal_id"`
	State    string `json:"state"`
	Elapsed  int64  `json:"elapsed_seconds"`
	Question bool   `json:"question"`
	Failed   bool   `json:"failed"`
}
type Tray interface {
	Update(string, string, []string, bool)
}
type Dashboard interface {
	Status() ([]Lane, error)
	Action(string) error
}
type Controller struct {
	tray   Tray
	client Dashboard
	run    func(string) error
	quit   func()
}

func (c *Controller) Refresh() {
	lanes, err := c.client.Status()
	if err != nil {
		c.tray.Update("", "Nuntius · dashboard stopped", nil, false)
		return
	}
	running, attention := 0, 0
	rows := make([]string, 0, len(lanes))
	for _, s := range lanes {
		state := strings.ToLower(s.State)
		if strings.HasPrefix(state, "running") || state == "stuck?" {
			running++
		}
		if s.Failed || s.Question || state == "stuck?" || state == "ended_no_report" || strings.HasPrefix(state, "failed") || strings.HasPrefix(state, "interrupted") || strings.HasPrefix(state, "cancelled") || strings.HasPrefix(state, "timed_out") || strings.HasPrefix(state, "timed out") {
			attention++
		}
		goal := s.Goal
		if goal == "" {
			goal = "—"
		}
		rows = append(rows, fmt.Sprintf("%s · %s · %s · %s", s.Lane, goal, s.State, elapsed(s.Elapsed)))
	}
	title := ""
	if running > 0 {
		title = fmt.Sprint(running)
	}
	if attention > 0 {
		title = "●"
	}
	c.tray.Update(title, fmt.Sprintf("Nuntius · %d lanes · %d running · %d attention", len(lanes), running, attention), rows, true)
}
func elapsed(s int64) string {
	if s < 0 {
		s = 0
	}
	if s >= 3600 {
		return fmt.Sprintf("%dh %dm", s/3600, s%3600/60)
	}
	return fmt.Sprintf("%dm %ds", s/60, s%60)
}
func (c *Controller) Action(a string) {
	switch a {
	case "open", "phone", "qr":
		if c.client.Action(a) != nil {
			c.tray.Update("●", "Nuntius · action unavailable; check dashboard configuration", nil, true)
		}
	case "start", "stop":
		if c.run(a) != nil {
			c.tray.Update("●", "Nuntius · dashboard command failed", nil, false)
		}
	case "quit":
		c.quit()
	}
}
