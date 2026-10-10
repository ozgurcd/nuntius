package main

import (
	"errors"
	"strings"
	"testing"
)

type settingsTray struct {
	fakeTray
	settings []Setting
}

func (s *settingsTray) Settings(rows []Setting) { s.settings = rows }
func TestSettingsConfirmation(t *testing.T) {
	tray := &settingsTray{}
	client := &fakeClient{}
	commands := [][]string{}
	c := Controller{tray: tray, client: client, settings: func() (Settings, error) {
		return Settings{Pools: map[string]int{"claude": 1, "codex": 2}, LoginKnown: true}, nil
	}, apply: func(a []string) error { commands = append(commands, a); return nil }}
	c.Refresh()
	if len(tray.settings) != 2 || !strings.Contains(tray.settings[0].Title, "Codex 2") {
		t.Fatal("minimal status missing")
	}
	for _, a := range []string{"setting:codex:3", "confirm", "dismiss", "settings-file", "setting:notify:off"} {
		c.Action(a)
	}
	if len(commands) != 0 {
		t.Fatal("removed settings actions executed")
	}
	c.Action("settings")
	if len(client.calls) != 1 || client.calls[0] != "settings" {
		t.Fatal("page opener not routed")
	}
	c.Action("setting:login:on")
	if len(commands) != 1 || strings.Join(commands[0], " ") != "menubar --at-login on" {
		t.Fatal("local login toggle changed authority")
	}
	client.err = errors.New("offline")
	c.Action("settings")
	if !strings.Contains(tray.tip, "Dashboard stopped") || len(commands) != 1 {
		t.Fatal("stopped dashboard implicitly started")
	}
}
