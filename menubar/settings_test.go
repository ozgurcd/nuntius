package main

import (
	"strings"
	"testing"
)

type settingsTray struct {
	fakeTray
	settings []Setting
}

func (t *settingsTray) Settings(rows []Setting) { t.settings = rows }

func TestSettingsConfirmation(t *testing.T) {
	tray := &settingsTray{}
	snapshot := Settings{Pools: map[string]int{"claude": 1, "codex": 2}, ReportsDir: "/workspace/reports", MaxRuntime: "8h", AutoResume: true, Lanes: []LaneIdentity{{Name: "codex-1", Model: "chosen", Effort: "default"}}}
	commands := [][]string{}
	c := Controller{tray: tray, client: &fakeClient{}, settings: func() (Settings, error) { return snapshot, nil }, apply: func(args []string) error { commands = append(commands, args); return nil }}
	c.Refresh()
	text := ""
	for _, r := range tray.settings {
		text += r.Title + "\n"
	}
	for _, want := range []string{"codex pool = 2", "notify_imessage = off", "tailnet default = off", "reports_dir = /workspace/reports", "max_runtime = 8h", "auto_resume = true", "codex-1: model=chosen effort=default"} {
		if !strings.Contains(text, want) {
			t.Fatal("missing read-only setting")
		}
	}
	c.Action("setting:codex:3")
	if len(commands) != 0 {
		t.Fatal("setting changed without confirmation")
	}
	c.Action("dismiss")
	c.Action("confirm")
	if len(commands) != 0 {
		t.Fatal("dismissed change executed")
	}
	c.Action("setting:codex:3")
	c.Action("confirm")
	if len(commands) != 1 || strings.Join(commands[0], " ") != "set codex-agent-pool 3" {
		t.Fatal("pool must use CLI")
	}
	c.Action("setting:notify:off")
	c.Action("confirm")
	c.Action("setting:login:on")
	c.Action("confirm")
	if strings.Join(commands[1], " ") != "set notify-imessage off" || strings.Join(commands[2], " ") != "menubar --at-login on" {
		t.Fatal("settings bypassed CLI")
	}
	for _, action := range []string{"setting:codex:9", "setting:codex:-1", "setting:model:other", "setting:notify:recipient", "setting:login:maybe"} {
		c.Action(action)
		c.Action("confirm")
	}
	if len(commands) != 3 {
		t.Fatal("unsafe settings accepted")
	}
}
