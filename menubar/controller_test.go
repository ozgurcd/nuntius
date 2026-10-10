package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

type fakeTray struct {
	title, tip string
	rows       []string
	online     bool
}

func TestRelayStateIcon(t *testing.T) {
	base, err := assets.ReadFile("assets/relay-linux.png")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(base, stateIcon(base, "")) {
		t.Fatal("idle Relay changed")
	}
	if bytes.Equal(base, stateIcon(base, "1")) || bytes.Equal(stateIcon(base, "1"), stateIcon(base, "●")) {
		t.Fatal("icon states not distinct")
	}
}

func (t *fakeTray) Update(title, tip string, rows []string, online bool) {
	t.title, t.tip, t.rows, t.online = title, tip, rows, online
}

type fakeClient struct {
	calls []string
	lanes []Lane
	err   error
}

func (f *fakeClient) Status() ([]Lane, error) { return f.lanes, f.err }
func (f *fakeClient) Action(a string) error   { f.calls = append(f.calls, a); return f.err }
func TestControllerStateAndActions(t *testing.T) {
	tray := &fakeTray{}
	client := &fakeClient{}
	commands := []string{}
	quit := false
	c := Controller{tray: tray, client: client, run: func(a string) error { commands = append(commands, a); return nil }, quit: func() { quit = true }}
	c.Refresh()
	if tray.title != "" || !strings.Contains(tray.tip, "0 running") {
		t.Fatal("idle state")
	}
	client.lanes = []Lane{{Lane: "codex-1", Goal: "GOAL", State: "running", Elapsed: 65}, {Lane: "claude-1", State: "idle"}}
	c.Refresh()
	if tray.title != "1" || len(tray.rows) != 2 || !strings.Contains(tray.rows[0], "1m 5s") {
		t.Fatal("running count or row")
	}
	for _, lane := range []Lane{{State: "stuck?"}, {State: "ended_no_report"}, {State: "interrupted"}, {State: "failed"}, {State: "Timed out · resumed"}, {State: "running", Question: true}, {State: "ended", Failed: true}} {
		client.lanes = []Lane{lane}
		c.Refresh()
		if tray.title != "●" || !strings.Contains(tray.tip, "1 attention") {
			t.Fatal("attention state")
		}
	}
	for _, a := range []string{"open", "phone", "qr", "start", "stop", "quit"} {
		c.Action(a)
	}
	if strings.Join(client.calls, ",") != "open,phone,qr" || strings.Join(commands, ",") != "start,stop" || !quit {
		t.Fatal("menu routing")
	}
	c.Action("dispatch")
	c.Action("cancel")
	if len(client.calls) != 3 || len(commands) != 2 {
		t.Fatal("unsafe tray command")
	}
	client.err = errors.New("private detail")
	c.Refresh()
	if tray.online || strings.Contains(tray.tip, "private") {
		t.Fatal("offline error exposed")
	}
}
