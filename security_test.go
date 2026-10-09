package nuntius

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReportSectionForms(t *testing.T) {
	for _, r := range []string{"QUESTIONS:\nchoose\nDECISIONS:\nkept\nIDS:\nexcluded", "QUESTIONS: choose\nDECISIONS: kept\nIDS: excluded", "## QUESTIONS\nchoose\n## DECISIONS\nkept\n## IDS\nexcluded"} {
		s := reportSections(r)
		if !strings.Contains(s["QUESTIONS"], "choose") || !strings.Contains(s["DECISIONS"], "kept") || strings.Contains(s["DECISIONS"], "excluded") {
			t.Fatal("sections incorrectly parsed")
		}
	}
}

func TestWebRedactionAndTokenPerStart(t *testing.T) {
	m, c, _ := manager(t)
	if _, e := m.Dispatch("codex", "SAFE-1", "hello"); e != nil {
		t.Fatal(e)
	}
	waitState(t, m, "codex", "ended")
	text := "password=fixture-value\nplain text"
	if e := os.WriteFile(filepath.Join(c.ReportsDir, "SAFE-1.txt"), []byte(text), 0600); e != nil {
		t.Fatal(e)
	}
	if e := m.AddOwnerNote(text); e != nil {
		t.Fatal(e)
	}
	first, e := NewWeb(m, "127.0.0.1:8123")
	if e != nil {
		t.Fatal(e)
	}
	second, e := NewWeb(m, "127.0.0.1:8123")
	if e != nil || first.token == second.token {
		t.Fatal("web credential reused")
	}
	for _, path := range []string{"/", "/data"} {
		req := httptest.NewRequest("GET", "http://localhost:8123"+path, nil)
		req.Header.Set("X-Nuntius-Token", first.token)
		out := httptest.NewRecorder()
		first.ServeHTTP(out, req)
		if out.Code != 200 || strings.Contains(out.Body.String(), "fixture-value") || strings.Contains(out.Body.String(), first.token) {
			t.Fatal("web data leak")
		}
		if path == "/data" && !strings.Contains(out.Body.String(), "REDACTED") {
			t.Fatal("redaction missing")
		}
	}
}

func TestOwnerNoteBoundsAndUnknownAck(t *testing.T) {
	m, _, _ := manager(t)
	for i := 0; i < 51; i++ {
		if e := m.AddOwnerNote("owner instruction"); e != nil {
			t.Fatal(e)
		}
	}
	notes, e := m.OwnerNotes(time.Time{})
	if e != nil || len(notes) != 50 {
		t.Fatal("note output bound")
	}
	if e = m.AckOwnerNotes([]string{"unknown"}); e == nil {
		t.Fatal("unknown acknowledgement accepted")
	}
	if _, e = call(m, "nuntius_owner_notes", json.RawMessage(`{"since":"bad"}`)); e == nil {
		t.Fatal("invalid timestamp accepted")
	}
	if e = m.AddOwnerNote(strings.Repeat("x", 12001)); e == nil {
		t.Fatal("oversized note accepted")
	}
}

func TestRecoveredStartMismatchNeverSignalled(t *testing.T) {
	m, c, store := manager(t)
	_, e := m.Dispatch("codex", "REUSE-1", "wait")
	if e != nil {
		t.Fatal(e)
	}
	waitReady(t, m, "codex")
	_ = m.Close()
	r, e := m.latest("codex")
	if e != nil {
		t.Fatal(e)
	}
	original := *r
	r.ProcessStart = "wrong-start"
	if e = m.save(r); e != nil {
		t.Fatal(e)
	}
	next, e := NewManager(c, store)
	if e != nil {
		t.Fatal(e)
	}
	if next.Cancel("codex") == nil || !sameProcess(original) {
		t.Fatal("recovered identity mismatch signalled")
	}
	_ = next.Close()
	if e = m.save(&original); e != nil {
		t.Fatal(e)
	}
	cleanup, e := NewManager(c, store)
	if e != nil {
		t.Fatal(e)
	}
	defer cleanup.Close()
	if e = cleanup.Cancel("codex"); e != nil {
		t.Fatal(e)
	}
	waitState(t, cleanup, "codex", "ended")
}

func TestFreezeStatusWithDisabledPools(t *testing.T) {
	c, store := fixture(t)
	c.Pools = map[string]AgentPool{}
	c.Lanes = nil
	m, e := NewManager(c, store)
	if e != nil {
		t.Fatal(e)
	}
	defer m.Close()
	if e = m.Freeze("pause"); e != nil {
		t.Fatal(e)
	}
	v, e := call(m, "nuntius_status", nil)
	b, _ := json.Marshal(v)
	if e != nil || !strings.Contains(string(b), `"frozen":true`) {
		t.Fatal("global freeze status missing")
	}
}
