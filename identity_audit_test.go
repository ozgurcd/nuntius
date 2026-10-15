package nuntius

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEffectiveLaunchIdentity(t *testing.T) {
	for _, tc := range []struct {
		kind          string
		args          []string
		model, effort string
	}{
		{"claude", []string{"--model", "opus", "--effort", "high"}, "opus", "high"},
		{"claude", []string{"--model=opus", "--effort=max"}, "opus", "max"},
		{"claude", []string{"-m", "sonnet"}, "sonnet", "default"},
		{"claude", []string{"-m=sonnet"}, "sonnet", "default"},
		{"codex", []string{"--model", "gpt-test"}, "gpt-test", "default"},
		{"codex", []string{"--model=gpt-test"}, "gpt-test", "default"},
		{"codex", []string{"-m=gpt-test"}, "gpt-test", "default"},
		{"codex", []string{"-m", "gpt-test"}, "gpt-test", "default"},
		{"codex", nil, "default", "default"},
		{"claude", nil, "default", "default"},
	} {
		model, effort := effectiveIdentity(Lane{Kind: tc.kind, ExtraArgs: tc.args})
		if model != tc.model || effort != tc.effort {
			t.Fatal("effective launch flag identity differs")
		}
	}
	model, effort := effectiveIdentity(Lane{Kind: "opencode", Model: "provider/model", ExtraArgs: []string{"--model=ignored"}})
	if model != "provider/model" || effort != "default" {
		t.Fatal("opencode template identity changed")
	}
	for _, line := range []string{`{"type":"session_config","model_reasoning_effort":"high"}`, `{"type":"session_meta","payload":{"reasoning_effort":"high"}}`, `{"type":"thread.started","reasoning_effort":"high"}`, `{"type":"event_msg","payload":{"type":"session_configured","reasoning_effort":"high"}}`} {
		if codexEffort([]byte(line+"\n")) != "high" {
			t.Fatal("structured startup effort missing")
		}
	}
	for _, line := range []string{`{"type":"item.completed","reasoning_effort":"high"}`, `{"type":"thread.started"}`, `{"type":"session_meta","reasoning_effort":"bad value"}`, `invalid`} {
		if codexEffort([]byte(line+"\n")) != "" {
			t.Fatal("non-startup effort accepted")
		}
	}
}

func TestLaunchIdentitySurfacesAndHistory(t *testing.T) {
	c, store := fixture(t)
	c.Lanes[0].ExtraArgs = []string{"--model", "launch-opus", "--effort=high", "--permission-mode=auto"}
	c.Lanes[0].StartMode = "as_is"
	m, err := NewManager(c, store)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	id, err := m.Dispatch("claude", "IDENTITY-1", "hello")
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, m, "claude", "ended")
	r, err := loadRun(filepath.Join(store, "runs", id), id)
	if err != nil || r == nil || r.Model != "launch-opus" || r.Effort != "high" {
		t.Fatal("launch snapshot missing")
	}
	m.mu.Lock()
	m.config.Lanes[0].ExtraArgs = []string{"--model=new-opus", "--effort=low", "--permission-mode=auto"}
	m.mu.Unlock()
	rows, err := m.History(HistoryFilter{Limit: 20})
	if err != nil || len(rows) != 1 || rows[0].Model != "launch-opus" || rows[0].Effort != "high" {
		t.Fatal("history followed config change")
	}
	v, err := call(m, "nuntius_history", []byte(`{}`))
	if err != nil || !strings.Contains(toolText(v), `"effort":"high"`) {
		t.Fatal("MCP history identity missing")
	}
	statuses, err := m.Status()
	if err != nil || statuses[0].Model != "launch-opus" || statuses[0].Effort != "high" {
		t.Fatal("status lost recorded identity")
	}
	statusView, err := call(m, "nuntius_status", []byte(`{}`))
	if err != nil || !strings.Contains(toolText(statusView), `"model":"launch-opus"`) || !strings.Contains(toolText(statusView), `"effort":"high"`) {
		t.Fatal("MCP status launch identity missing")
	}
	agents, err := m.Agents()
	if err != nil || agents[0].Model != "new-opus" || agents[0].Effort != "low" {
		t.Fatal("agents lost next-launch identity")
	}
	cliFixture(t, m.config, store)
	for _, args := range [][]string{{"agents"}, {"status", "--long"}, {"history"}} {
		var out, diagnostic bytes.Buffer
		if CLI(context.Background(), args, strings.NewReader(""), &out, &diagnostic) != 0 || !strings.Contains(out.String(), "model=") || !strings.Contains(out.String(), "effort=") {
			t.Fatal("CLI launch identity missing")
		}
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	m2, err := NewManager(m.config, store)
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	rows, err = m2.History(HistoryFilter{Limit: 20})
	if err != nil || rows[0].Model != "launch-opus" || rows[0].Effort != "high" {
		t.Fatal("restart changed launch snapshot")
	}
	// A completed run retains the effort its own structured stream reported.
	r.Kind, r.Effort = "codex", "default"
	if err := os.WriteFile(filepath.Join(store, "runs", id, "events.jsonl"), []byte("{\"type\":\"session_config\",\"reasoning_effort\":\"high\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m2.endRun(r); err != nil {
		t.Fatal(err)
	}
	reloaded, err := loadRun(filepath.Join(store, "runs", id), id)
	if err != nil || reloaded == nil || reloaded.Effort != "high" {
		t.Fatal("reported effort not persisted")
	}
}

func TestReadableAuditParsingAndCLI(t *testing.T) {
	m, c, store := manager(t)
	if rows, err := m.Audit(""); err != nil || len(rows) != 0 {
		t.Fatal("empty store audit unavailable")
	}
	now := time.Now().UTC()
	entries := []map[string]any{
		{"time": now.Add(-2 * time.Hour), "tool": "nuntius_dispatch", "lane": "codex", "goal_id": "AUDIT-1", "result": "accepted", "sha256": strings.Repeat("a", 64)},
		{"time": now, "tool": "nuntius_report", "goal_id": "AUDIT-1", "result": "pass"},
		{"time": now.Add(-time.Minute), "tool": "nuntius_dispatch", "lane": "codex", "goal_id": "AUDIT-2", "result": "refused", "reason": "lane busy password=hidden", "sha256": strings.Repeat("b", 64)},
	}
	var data bytes.Buffer
	for _, entry := range entries {
		b, _ := json.Marshal(entry)
		data.Write(b)
		data.WriteByte('\n')
	}
	data.WriteString("invalid\n{\"time\":\"unfinished")
	if err := os.WriteFile(filepath.Join(store, "audit.jsonl"), data.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	rows, err := m.Audit("")
	if err != nil || len(rows) != 3 || rows[0].Summary != "Read report AUDIT-1" || rows[2].Summary != "Dispatched AUDIT-1 to codex" || rows[2].Digest != strings.Repeat("a", 12) {
		t.Fatal("audit parse/order/summary differs")
	}
	if rows[1].Result != "refused" || strings.Contains(toolText(rows), "hidden") || !strings.Contains(rows[1].Reason, "[REDACTED]") {
		t.Fatal("audit redaction failed")
	}
	filtered, err := m.Audit("1h")
	if err != nil || len(filtered) != 2 {
		t.Fatal("audit since filter differs")
	}
	if _, err := m.Audit("-1h"); err == nil {
		t.Fatal("negative since accepted")
	}
	cliFixture(t, c, store)
	for _, args := range [][]string{{"audit"}, {"audit", "--since", "1h", "--json"}} {
		var out, diagnostic bytes.Buffer
		if CLI(context.Background(), args, strings.NewReader(""), &out, &diagnostic) != 0 || strings.Contains(out.String(), "hidden") {
			t.Fatal("CLI audit failed")
		}
		if args[len(args)-1] == "--json" {
			var decoded []AuditEntry
			if json.Unmarshal(out.Bytes(), &decoded) != nil || len(decoded) != 2 {
				t.Fatal("CLI audit JSON shape")
			}
		} else if !strings.Contains(out.String(), "Dispatched AUDIT-1 to codex") {
			t.Fatal("CLI audit not readable")
		}
	}
	w, err := NewWeb(m, "127.0.0.1:8123")
	if err != nil {
		t.Fatal(err)
	}
	d, err := w.data()
	if err != nil {
		t.Fatal(err)
	}
	if got := d.(map[string]any)["audit"]; toolText(got) != toolText(rows) {
		t.Fatal("dashboard structured audit parity")
	}
	// Classification is presentation metadata; reads remain in the complete log.
	reads := map[string]bool{
		"nuntius_status": true, "nuntius_report": true, "nuntius_signals": true,
		"nuntius_history": true, "nuntius_tail": true, "nuntius_owner_notes": true,
	}
	actions := []string{"freeze", "unfreeze", "run_end", "automatic_continue", "future_action", "nuntius_status_changed"}
	for _, tool := range tools() {
		actions = append(actions, tool.Name)
	}
	for _, action := range actions {
		b, _ := json.Marshal(map[string]any{"time": now, "tool": action, "result": "ok"})
		parsed := parseAudit(b, time.Time{})
		if len(parsed) != 1 || parsed[0].Action != action || parsed[0].Raw != string(b) {
			t.Fatal("classification changed the audit record")
		}
		var presented []map[string]any
		if json.Unmarshal([]byte(toolText(parsed)), &presented) != nil {
			t.Fatal("classification JSON unavailable")
		}
		if got, ok := presented[0]["read_only"].(bool); !ok || got != reads[action] {
			t.Errorf("audit read classification differs for %s", action)
		}
	}
	stored, err := os.ReadFile(filepath.Join(store, "audit.jsonl"))
	if err != nil || !bytes.Equal(stored, data.Bytes()) {
		t.Fatal("audit presentation rewrote the stored log")
	}
}

func TestLaneNotesAndAuditRefusals(t *testing.T) {
	m, _, _ := manager(t)
	w, err := NewWeb(m, "127.0.0.1:8123")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", "http://localhost:8123/action", strings.NewReader(`{"action":"note","lane":"codex","text":"lane-specific note"}`))
	request.Header.Set("Origin", "http://localhost:8123")
	request.Header.Set("X-Nuntius-Token", w.token)
	response := httptest.NewRecorder()
	w.ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatal("addressed note refused")
	}
	notes, err := m.OwnerNotes(time.Time{})
	if err != nil || len(notes) != 1 || notes[0].Lane != "codex" {
		t.Fatal("addressed note not persisted")
	}
	if err := m.AddOwnerNote("bad lane", "unknown"); err == nil {
		t.Fatal("unknown lane note accepted")
	}
	if err := m.Freeze("Owner pause"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Dispatch("codex", "REFUSAL-1", "hello"); err == nil {
		t.Fatal("freeze bypassed")
	}
	if err := m.Cancel("codex"); err == nil {
		t.Fatal("idle lane cancel accepted")
	}
	rows, err := m.Audit("")
	if err != nil {
		t.Fatal(err)
	}
	frozen, refused, cancelled := false, false, false
	for _, e := range rows {
		if e.Action == "nuntius_freeze" {
			frozen = e.Result == "ok" && e.Summary == "Froze dispatch"
		}
		if e.Action == "nuntius_dispatch" {
			refused = e.Result == "refused" && e.Reason != "" && e.GoalID == "REFUSAL-1"
		}
		if e.Action == "nuntius_cancel" {
			cancelled = e.Result == "refused" && e.Reason == "lane is not running"
		}
	}
	if !frozen || !refused || !cancelled {
		t.Fatal("audit result/reason recording differs")
	}
	var many bytes.Buffer
	for i := 0; i < 300; i++ {
		many.WriteString(`{"time":"2026-10-10T00:00:00Z","tool":"nuntius_status","result":"pass"}` + "\n")
	}
	boundedRows := parseAudit(many.Bytes(), time.Time{})
	if len(boundedRows) != 200 {
		t.Fatal("audit row limit differs")
	}
	encoded, err := json.Marshal(boundedRows)
	if err != nil || len(encoded) > 64202 {
		t.Fatal("audit presentation budget exceeded")
	}
}
