package nuntius

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSettingsBriefContention(t *testing.T) {
	for _, kind := range []string{"settings", "manager", "control"} {
		t.Run(kind, func(t *testing.T) {
			m, w, _ := settingsQuietFixture(t)
			var unlock func()
			switch kind {
			case "settings":
				w.settingsState.mu.Lock()
				unlock = w.settingsState.mu.Unlock
			case "manager":
				m.mu.Lock()
				unlock = m.mu.Unlock
			case "control":
				guard, err := m.controlLock()
				if err != nil {
					t.Fatal(err)
				}
				unlock = func() { _ = guard.Close() }
			}
			done := make(chan struct{})
			go func() { time.Sleep(40 * time.Millisecond); unlock(); close(done) }()
			out := settingsHTTP(t, w, "GET", "")
			p0TestJoin(t, done)
			if out.Code != 200 {
				t.Fatalf("brief %s contention: HTTP %d", kind, out.Code)
			}
		})
	}
}

func TestSettingsReadCancellation(t *testing.T) {
	for _, kind := range []string{"settings", "manager", "control"} {
		t.Run(kind, func(t *testing.T) {
			m, w, _ := settingsQuietFixture(t)
			revision := settingsRevision(t, w)
			var unlock func()
			switch kind {
			case "settings":
				w.settingsState.mu.Lock()
				unlock = w.settingsState.mu.Unlock
			case "manager":
				m.mu.Lock()
				unlock = m.mu.Unlock
			case "control":
				guard, err := m.controlLock()
				if err != nil {
					t.Fatal(err)
				}
				unlock = func() { _ = guard.Close() }
			}
			defer func() {
				if unlock != nil {
					unlock()
				}
			}()
			start := time.Now()
			post := settingsHTTP(t, w, "POST", settingsBody(revision, `{"pools":{"codex":1}}`))
			if post.Code != 503 || post.Header().Get("Retry-After") != "1" || time.Since(start) > 200*time.Millisecond {
				t.Fatal("busy POST did not refuse immediately")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
			defer cancel()
			r := httptest.NewRequest("GET", "http://localhost:8123/settings", nil).WithContext(ctx)
			r.Header.Set("X-Nuntius-Token", w.token)
			out := httptest.NewRecorder()
			w.ServeHTTP(out, r)
			if ctx.Err() == nil || out.Code != 503 || !strings.Contains(out.Body.String(), `"code":"locked"`) {
				t.Fatal("read did not wait until its bounded request cancellation")
			}
			unlock()
			unlock = nil
			if settingsHTTP(t, w, "GET", "").Code != 200 {
				t.Fatal("cancelled read stranded a lock")
			}
		})
	}
}

func TestSettingsReadFaultCodes(t *testing.T) {
	m, w, path := settingsQuietFixture(t)
	for _, code := range []string{"configuration_unavailable", "invalid_configuration"} {
		if code == "configuration_unavailable" {
			m.config.SourcePath = ""
		} else {
			m.config.SourcePath = path
			if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		out := settingsHTTP(t, w, "GET", "")
		if out.Code != 503 || !strings.Contains(out.Body.String(), `"code":"`+code+`"`) {
			t.Fatalf("wrong specific fault for %s: HTTP %d", code, out.Code)
		}
	}
}

func TestIdleWorkerDoesNotParseEndedEvents(t *testing.T) {
	m, _, _ := manager(t)
	id := fmt.Sprintf("%032x", 1)
	dir := filepath.Join(m.store, "runs", id)
	if err := privateDir(dir); err != nil {
		t.Fatal(err)
	}
	ended := time.Now().Add(-time.Minute)
	r := Run{ID: id, Lane: "codex", Kind: "codex", GoalID: "IDLE-1", Started: ended.Add(-time.Minute), Ended: &ended, State: "ended", StopReason: "completed"}
	m.mu.Lock()
	err := m.save(&r)
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte("{\"type\":\"session_meta\",\"reasoning_effort\":\"high\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1300 * time.Millisecond)
	run, err := loadRun(dir, id)
	if err != nil || run == nil || run.Effort != "" {
		t.Fatal("idle worker parsed ended event details")
	}
	rows, err := m.Status()
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Lane == "codex" && row.Effort != "high" {
			t.Fatal("explicit status lost event details")
		}
	}
}

// Opt-in profiling uses only fake ended records, local handler calls and scratch
// storage. Normal verification does not spend seconds collecting CPU samples.
func TestSettingsIdleProfile(t *testing.T) {
	if os.Getenv("NUNTIUS_IDLE_PROFILE") != "1" {
		t.Skip("opt-in CPU measurement")
	}
	m, w, p := settingsQuietFixture(t)
	for i := 0; i < 29; i++ {
		id := fmt.Sprintf("%032x", i+1)
		dir := filepath.Join(m.store, "runs", id)
		if err := privateDir(dir); err != nil {
			t.Fatal(err)
		}
		ended := time.Now().Add(-time.Duration(i+1) * time.Minute)
		lane, kind := "claude-1", "claude"
		if i%2 == 0 {
			lane, kind = "codex-1", "codex"
		}
		r := Run{ID: id, Lane: lane, Kind: kind, GoalID: "PROFILE-1", Started: ended.Add(-time.Minute), Ended: &ended, State: "ended", StopReason: "completed"}
		if err := m.save(&r); err != nil {
			t.Fatal(err)
		}
		// Complete harmless JSON lines fill a bounded event window.
		line := `{"type":"assistant","message":{"content":[{"type":"text","text":"fixture"}]}}` + "\n"
		if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(strings.Repeat(line, (1<<20)/len(line))), 0600); err != nil {
			t.Fatal(err)
		}
	}
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	webManager, err := NewManager(c, m.store)
	if err != nil {
		t.Fatal(err)
	}
	defer webManager.Close()
	w.manager = webManager
	for i := 0; i < 2; i++ {
		poller, err := NewManager(c, m.store)
		if err != nil {
			t.Fatal(err)
		}
		defer poller.Close()
		// Serve enables recovery once at stdio startup. Include that periodic
		// path while keeping all records ended and all agents fake.
		poller.mu.Lock()
		poller.recovery = true
		poller.mu.Unlock()
	}
	// Model the operational dashboard's two-second poll while independent
	// managers run the identical worker used by idle stdio MCP processes.
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-t.Context().Done():
				return
			case <-done:
				return
			case <-tick.C:
				_, _ = webManager.Status()
			}
		}
	}()
	defer func() {
		close(done)
		p0TestJoin(t, stopped)
	}()
	counts := map[string]int{}
	for i := 0; i < 8; i++ {
		out := settingsHTTP(t, w, "GET", "")
		key := fmt.Sprint(out.Code)
		if out.Code == 503 {
			var fault struct {
				Code string `json:"code"`
			}
			if json.Unmarshal(out.Body.Bytes(), &fault) != nil {
				t.Fatal("invalid error response")
			}
			key += "/" + fault.Code
		}
		counts[key]++
		time.Sleep(time.Second)
	}
	t.Logf("eight Settings reads, one/second, 29 fake ended records, two MCP-equivalent workers and a dashboard worker: %v", counts)
}
