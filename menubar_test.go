package nuntius

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMenubarAPI(t *testing.T) {
	m, _, _ := manager(t)
	w, err := NewWeb(m, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	oldOpen, oldSend := menubarOpenFile, menubarSend
	defer func() { menubarOpenFile, menubarSend = oldOpen, oldSend }()
	opened := ""
	menubarOpenFile = func(_ context.Context, p string) error { opened = p; return nil }
	sent := false
	menubarSend = func(_ context.Context, _, _ string) error { sent = true; return nil }
	w.allowHost, w.notify = "test.example.ts.net", "+12345678901"
	for _, action := range []string{"status", "open", "qr", "phone", "dispatch", "cancel"} {
		r := httptest.NewRequest(http.MethodPost, "http://unix/"+action, nil)
		if action == "status" {
			r.Method = http.MethodGet
		}
		out := httptest.NewRecorder()
		w.menubarHandler().ServeHTTP(out, r)
		if action == "dispatch" || action == "cancel" {
			if out.Code != 404 {
				t.Fatal("unsafe action exposed")
			}
			continue
		}
		if out.Code != 200 {
			t.Fatalf("%s: %d", action, out.Code)
		}
		if strings.Contains(out.Body.String(), w.token) {
			t.Fatal("credential entered helper response")
		}
		if action == "status" {
			var v map[string]any
			if json.Unmarshal(out.Body.Bytes(), &v) != nil || v["lanes"] == nil {
				t.Fatal("status unavailable")
			}
		}
	}
	if !sent || opened == "" {
		t.Fatal("explicit actions not routed")
	}
	info, err := os.Stat(opened)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("presentation is not private")
	}
	b, _ := os.ReadFile(opened)
	if !strings.Contains(string(b), "data:image/png;base64,") {
		t.Fatal("QR missing")
	}
	out := httptest.NewRecorder()
	w.menubarHandler().ServeHTTP(out, httptest.NewRequest("GET", "http://unix/open", nil))
	if out.Code != 405 {
		t.Fatal("action accepted a GET")
	}
	// A resumed run keeps its readable display label even when no report exists.
	// The attention flag must still come from the underlying terminal state.
	ended, exit := time.Now().UTC(), 0
	run := Run{ID: strings.Repeat("a", 32), Lane: "codex", GoalID: "ATTENTION-1", Started: ended.Add(-time.Minute), Ended: &ended, Exit: &exit, State: "ended", RecoveryRun: true, ResumeCount: 1}
	if err := os.MkdirAll(filepath.Join(m.store, "runs", run.ID), 0700); err != nil {
		t.Fatal(err)
	}
	if err := m.save(&run); err != nil {
		t.Fatal(err)
	}
	out = httptest.NewRecorder()
	w.menubarHandler().ServeHTTP(out, httptest.NewRequest("GET", "http://unix/status", nil))
	var resumed struct {
		Lanes []struct {
			Lane, State string
			Failed      bool
		}
	}
	if out.Code != 200 || json.Unmarshal(out.Body.Bytes(), &resumed) != nil {
		t.Fatal("resumed status unavailable")
	}
	found := false
	for _, row := range resumed.Lanes {
		if row.Lane == "codex" {
			found = true
			if !strings.Contains(row.State, "resumed") || !row.Failed {
				t.Fatal("resumed end without report lost attention")
			}
		}
	}
	if !found {
		t.Fatal("resumed fixture not loaded")
	}
}

func TestMenubarReportQuestions(t *testing.T) {
	for _, tc := range []struct {
		report  string
		waiting bool
	}{
		{"QUESTIONS:\nnone\nDECISIONS:\nsafe choice", false},
		{"QUESTIONS:\nNone.\nDECISIONS:\nsafe choice", false},
		{"QUESTIONS:\n\nDECISIONS:\nsafe choice", false},
		{"QUESTIONS:\n(a) resume (b) park; recommend resume\nDECISIONS:\nsafe choice", true},
	} {
		if menubarReportQuestion(tc.report) != tc.waiting {
			t.Fatal("waiting question mapping")
		}
	}
}

func TestMenubarLoginFileOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "LaunchAgents", "io.nuntius.menubar.plist")
	if err := writeMenubarLogin(path, "/Applications/Nuntius Menu Bar.app/Contents/MacOS/nuntius-menubar", "/tmp/store", "/tmp/config", "/usr/local/bin/nuntius", true); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	for _, want := range []string{"RunAtLoad", "nuntius-menubar", "--store", "--config", "--nuntius"} {
		if !strings.Contains(string(b), want) {
			t.Fatal("missing login field")
		}
	}
	if strings.Contains(string(b), "KeepAlive") || strings.Contains(string(b), "web start") {
		t.Fatal("login starts dashboard")
	}
	if err := writeMenubarLogin(path, "", "", "", "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("login file retained")
	}
}

func TestMenubarOptionalStart(t *testing.T) {
	m, _, _ := manager(t)
	find, gui, launch := menubarFind, menubarGUI, menubarLaunch
	defer func() { menubarFind, menubarGUI, menubarLaunch = find, gui, launch }()
	starts := 0
	menubarFind = func() (string, error) { return "/fake/nuntius-menubar", nil }
	menubarGUI = func() bool { return false }
	menubarLaunch = func(_ string, args []string) error {
		starts++
		if strings.Contains(strings.Join(args, " "), "token") {
			t.Fatal("credential in argv")
		}
		return nil
	}
	m.startMenubar()
	if starts != 0 {
		t.Fatal("helper started headless")
	}
	menubarGUI = func() bool { return true }
	m.startMenubar()
	if starts != 1 {
		t.Fatal("installed helper not started")
	}
	menubarFind = func() (string, error) { return "", errors.New("absent") }
	m.startMenubar()
	if starts != 1 {
		t.Fatal("missing helper not ignored")
	}
}
