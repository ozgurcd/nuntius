package nuntius

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestDetachedServerHelper(t *testing.T) {
	if len(os.Args) < 4 || os.Args[len(os.Args)-3] != "--fake-server" {
		return
	}
	c, err := LoadConfig(os.Args[len(os.Args)-2])
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(c, os.Args[len(os.Args)-1])
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Dispatch("codex", "DETACHED-1", "wait"); err != nil {
		t.Fatal(err)
	}
	// Do not let the observer's startup reconciliation contend with admission.
	// The survival assertion begins only after the helper confirms dispatch.
	if err = os.WriteFile(filepath.Join(m.store, "fixture-dispatched"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {}
}

func TestDetachedRunSurvivesKilledServer(t *testing.T) {
	c, store := fixture(t)
	p := filepath.Join(c.ReportsDir, "lanes.json")
	b, _ := json.Marshal(c)
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	server := exec.Command(os.Args[0], "-test.run=^TestDetachedServerHelper$", "--", "--fake-server", p, store)
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Process.Kill(); _ = server.Wait() })
	var next *Manager
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(store, "fixture-dispatched")); err == nil {
			next, _ = NewManager(c, store)
			if next != nil {
				s, _ := next.Status()
				if len(s) > 1 && s[1].State == "running" {
					break
				}
				_ = next.Close()
				next = nil
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if next == nil {
		t.Fatal("fake server never started run")
	}
	_ = next.Close()
	if err := server.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = server.Wait()
	next, err := NewManager(c, store)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	waitReady(t, next, "codex")
	s, err := next.Status()
	if err != nil || s[1].State != "running" {
		t.Fatal("detached run lost")
	}
	if err = next.Cancel("codex"); err != nil {
		t.Fatal(err)
	}
	waitState(t, next, "codex", "ended")
}

func TestProcessStartTimeMismatchNeverSignalled(t *testing.T) {
	m, _, store := manager(t)
	id, err := m.Dispatch("codex", "IDENTITY-1", "wait")
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, m, "codex")
	m.mu.Lock()
	r, _ := m.latest("codex")
	original := *r
	r.ProcessStart = "wrong-start"
	err = m.save(r)
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if m.Cancel("codex") == nil {
		t.Fatal("mismatched identity accepted")
	}
	if !sameProcess(original) {
		t.Fatal("mismatched process was signalled")
	}
	m.mu.Lock()
	_ = m.save(&original)
	m.mu.Unlock()
	_ = m.Cancel("codex")
	waitState(t, m, "codex", "ended")
	_ = id
	_ = store
}

func TestFreezeAcrossManagersAndCLI(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m, c, store := manager(t)
	_, err := m.Dispatch("codex", "FREEZE-1", "wait")
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, m, "codex")
	if err = m.Freeze("owner pause"); err != nil {
		t.Fatal(err)
	}
	waitState(t, m, "codex", "ended")
	next, err := NewManager(c, store)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	for _, resume := range []bool{false, true} {
		_, err = next.start("codex", "FREEZE-2", "hello", resume)
		if err == nil || !strings.Contains(err.Error(), "frozen by owner since") {
			t.Fatal("freeze not enforced")
		}
	}
	s, _ := next.Status()
	if s[1].Freeze == nil || s[1].Freeze.Reason != "owner pause" {
		t.Fatal("freeze missing from status")
	}
	p := filepath.Join(c.ReportsDir, "config.json")
	b, _ := json.Marshal(c)
	_ = os.WriteFile(p, b, 0600)
	var out bytes.Buffer
	if CLI(context.Background(), []string{"--config", p, "--store", store, "unfreeze"}, strings.NewReader(""), &out, &out) != 0 {
		t.Fatal("CLI unfreeze failed")
	}
	if _, err = next.Dispatch("codex", "FREEZE-3", "hello"); err != nil {
		t.Fatal(err)
	}
	waitState(t, next, "codex", "ended")
}

func TestOwnerNotesPersistenceAckAndMCP(t *testing.T) {
	m, c, store := manager(t)
	if err := m.AddOwnerNote("review the decision"); err != nil {
		t.Fatal(err)
	}
	notes, err := m.OwnerNotes(time.Time{})
	if err != nil || len(notes) != 1 || notes[0].Read {
		t.Fatal("missing note")
	}
	if _, err = call(m, "nuntius_owner_notes_ack", json.RawMessage(`{"ids":["`+notes[0].ID+`"]}`)); err != nil {
		t.Fatal(err)
	}
	next, err := NewManager(c, store)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	notes, err = next.OwnerNotes(time.Time{})
	if err != nil || len(notes) != 1 || !notes[0].Read {
		t.Fatal("ack not durable")
	}
	if notes, err = next.OwnerNotes(time.Now().Add(time.Hour)); err != nil || len(notes) != 0 {
		t.Fatal("since ignored")
	}
	for _, name := range []string{"nuntius_freeze", "nuntius_unfreeze", "nuntius_owner_notes"} {
		arg := json.RawMessage(`{}`)
		if name == "nuntius_freeze" {
			arg = json.RawMessage(`{"reason":"pause"}`)
		}
		if _, err = call(next, name, arg); err != nil {
			t.Fatal(name, err)
		}
	}
}

func TestWebRequestGuards(t *testing.T) {
	m, _, _ := manager(t)
	w, err := NewWeb(m, "127.0.0.1:8123")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, method, host, origin, path string
		auth                             bool
		want                             int
	}{
		{"wrong Host", "GET", "evil.test", "", "/", true, 403},
		{"wrong Origin", "POST", "127.0.0.1:8123", "http://evil.test", "/action", true, 403},
		{"missing token", "POST", "127.0.0.1:8123", "http://127.0.0.1:8123", "/action", false, 403},
		{"GET action", "GET", "127.0.0.1:8123", "", "/action", true, 405},
		{"missing Origin", "POST", "127.0.0.1:8123", "", "/action", true, 403},
		{"wrong port", "GET", "127.0.0.1:9", "", "/", true, 403},
		{"allowed", "GET", "127.0.0.1:8123", "", "/", true, 200},
		{"public shell", "GET", "127.0.0.1:8123", "", "/", false, 200},
		{"CSS public", "GET", "127.0.0.1:8123", "", "/app.css", false, 200},
		{"JS public", "GET", "127.0.0.1:8123", "", "/app.js", false, 200},
		{"data requires header", "GET", "127.0.0.1:8123", "", "/data", false, 403},
		{"query cannot authenticate data", "GET", "127.0.0.1:8123", "", "/data?token=" + w.token, false, 403},
		{"query cannot authenticate action", "POST", "127.0.0.1:8123", "http://127.0.0.1:8123", "/action?token=" + w.token, false, 403},
		{"public asset Host guarded", "GET", "evil.test:8123", "", "/app.js", false, 403},
		{"public asset port guarded", "GET", "127.0.0.1:9", "", "/app.css", false, 403},
		{"public asset method guarded", "HEAD", "127.0.0.1:8123", "", "/app.js", false, 405},
		{"public asset Origin guarded", "POST", "127.0.0.1:8123", "http://evil.test", "/app.css", false, 403},
		{"CSS allowed", "GET", "127.0.0.1:8123", "", "/app.css", true, 200},
		{"JS allowed", "GET", "127.0.0.1:8123", "", "/app.js", true, 200},
		{"asset Host guarded", "GET", "evil.test:8123", "", "/app.js", true, 403},
		{"asset method guarded", "POST", "127.0.0.1:8123", "http://127.0.0.1:8123", "/app.css", true, 405},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "http://127.0.0.1:8123"+tc.path, strings.NewReader(`{"action":"unfreeze"}`))
			r.Host = tc.host
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if tc.auth {
				r.Header.Set("X-Nuntius-Token", w.token)
			}
			out := httptest.NewRecorder()
			w.ServeHTTP(out, r)
			if out.Code != tc.want {
				t.Fatalf("status=%d", out.Code)
			}
			if out.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatal("CORS enabled")
			}
			csp := out.Header().Get("Content-Security-Policy")
			if strings.Contains(csp, "unsafe-inline") || !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "style-src 'self'") {
				t.Fatal("static asset CSP missing")
			}
		})
	}
	for _, addr := range []string{"0.0.0.0:8123", "localhost:8123", "[::1]:8123", "192.168.1.2:8123"} {
		if _, err = NewWeb(m, addr); err == nil {
			t.Fatal("non-loopback bind accepted")
		}
	}
}

func TestWebViewsNotesQueuedReplyAndQuestions(t *testing.T) {
	m, c, _ := manager(t)
	c.OversightPath = filepath.Join(c.ReportsDir, "OVERSIGHT.md")
	m.mu.Lock()
	m.config = c
	m.mu.Unlock()
	_ = os.WriteFile(c.OversightPath, []byte("# Owner digest\nwaiting for owner"), 0600)
	_, err := m.Dispatch("codex", "WEB-1", "wait")
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, m, "codex")
	if err = m.SendMessage("codex", "answer", false); err != nil {
		t.Fatal(err)
	}
	if err = m.Cancel("codex"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, _ := m.latest("codex")
		if r != nil && r.Resumed && r.State == "ended" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	r, _ := m.latest("codex")
	if r == nil || !r.Resumed || r.State != "ended" {
		t.Fatal("queued message not delivered")
	}
	_ = os.WriteFile(filepath.Join(c.ReportsDir, "WEB-1.txt"), []byte("QUESTIONS:\n(a) choose\nDECISIONS:\nkept normal approvals\nIDS:\nnone"), 0600)
	w, err := NewWeb(m, "127.0.0.1:8123")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "http://127.0.0.1:8123/data", nil)
	req.Header.Set("X-Nuntius-Token", w.token)
	out := httptest.NewRecorder()
	w.ServeHTTP(out, req)
	if out.Code != 200 || !strings.Contains(out.Body.String(), "waiting for owner") || !strings.Contains(out.Body.String(), "kept normal approvals") || !strings.Contains(out.Body.String(), "goal") {
		t.Fatal("dashboard views missing")
	}
	var data struct {
		Lanes []struct{ Kind, ETA, Activity string }
	}
	if json.Unmarshal(out.Body.Bytes(), &data) != nil || len(data.Lanes) == 0 {
		t.Fatal("card data missing")
	}
	for _, lane := range data.Lanes {
		if lane.Kind == "" || lane.ETA == "" || lane.Activity == "" {
			t.Fatal("card status helpers missing")
		}
	}
}

func TestDetachedCloseAndEventAge(t *testing.T) {
	m, c, store := manager(t)
	_, err := m.Dispatch("codex", "CLOSE-1", "wait")
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, m, "codex")
	s, _ := m.Status()
	if s[1].LastEventAgeSeconds == nil {
		t.Fatal("missing last event age")
	}
	_ = m.Close()
	next, err := NewManager(c, store)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if err = next.Cancel("codex"); err != nil {
		t.Fatal(err)
	}
	waitState(t, next, "codex", "ended")
}

// Ensure the fake has its own session rather than only a process group.
func TestRunOwnSession(t *testing.T) {
	m, _, _ := manager(t)
	_, err := m.Dispatch("codex", "SESSION-1", "wait")
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, m, "codex")
	r, _ := m.latest("codex")
	pg, err := syscall.Getpgid(r.PID)
	if err != nil || pg != r.PID || r.ProcessStart == "" {
		_, identityErr := processStart(r.PID)
		t.Fatalf("run is not detached: group error %v, identity error %v", err, identityErr)
	}
	_ = m.Cancel("codex")
	waitState(t, m, "codex", "ended")
}

var _ http.Handler = (*Web)(nil)
