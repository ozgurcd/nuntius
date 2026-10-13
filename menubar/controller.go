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
	tray           Tray
	client         Dashboard
	run            func(string) error
	quit           func()
	settings       func() (Settings, error)
	apply          func([]string) error
	phoneFailures  map[string]ActionState
	commandFailure string
}

func (c *Controller) Refresh() {
	c.refreshSettings()
	lanes, err := c.client.Status()
	if err != nil {
		message := "Nuntius · dashboard stopped"
		var actions map[string]ActionState
		if failure, ok := err.(actionFailure); ok {
			message = failure.Error()
			current := safeAction(ActionState{Code: failure.code})
			actions = map[string]ActionState{"phone": current, "qr": current}
		}
		if c.commandFailure != "" {
			message = c.commandFailure
		}
		c.tray.Update("", message, nil, false)
		c.updatePhoneActions(actions)
		return
	}
	running, attention := 0, 0
	c.commandFailure = ""
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
	var actions map[string]ActionState
	if client, ok := c.client.(interface{ PhoneActions() map[string]ActionState }); ok {
		actions = client.PhoneActions()
	}
	c.updatePhoneActions(actions)
}
func (c *Controller) updatePhoneActions(actions map[string]ActionState) {
	state := map[string]ActionState{}
	for _, key := range []string{"phone", "qr"} {
		a := safeAction(actions[key])
		if a.Enabled || a.Code != "" {
			delete(c.phoneFailures, key)
		} else if failure, ok := c.phoneFailures[key]; ok {
			a = failure
		}
		state[key] = a
	}
	if tray, ok := c.tray.(interface{ PhoneActions(map[string]ActionState) }); ok {
		tray.PhoneActions(state)
	}
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
	if c.settingsAction(a) {
		return
	}
	switch a {
	case "open", "phone", "qr", "settings":
		if a == "phone" || a == "qr" {
			delete(c.phoneFailures, a)
		}
		if err := c.client.Action(a); err != nil {
			message := "Nuntius · action unavailable; check dashboard configuration"
			if failure, ok := err.(actionFailure); ok {
				message = failure.Error()
				if c.phoneFailures == nil {
					c.phoneFailures = map[string]ActionState{}
				}
				c.phoneFailures[a] = safeAction(ActionState{Code: failure.code})
				c.updatePhoneActions(nil)
			}
			if a == "settings" {
				message = "Dashboard stopped. Start it locally, then open Settings."
			}
			c.tray.Update("●", message, nil, true)
		}
	case "start", "stop":
		if err := c.run(a); err != nil {
			message := actionReason("")
			if failure, ok := err.(actionFailure); ok {
				message = failure.Error()
			}
			c.tray.Update("●", message, nil, false)
			c.commandFailure = message
		} else {
			c.commandFailure = ""
		}
	case "quit":
		c.quit()
	}
}
