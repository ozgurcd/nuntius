package nuntius

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Real mux, synthetic grants, scratch configuration; no network or owner files.
func TestSettingsGuardsAndBatch(t *testing.T) {
	m, c, _ := manager(t)
	c, err := c.poolConfig()
	if err != nil {
		t.Fatal(err)
	}
	c.NotifyIMessage = "fixture@example.invalid"
	p := filepath.Join(t.TempDir(), "config.json")
	b, _ := json.Marshal(c)
	if os.WriteFile(p, b, 0600) != nil {
		t.Fatal("fixture write")
	}
	m.mu.Lock()
	m.config.SourcePath = p
	m.mu.Unlock()
	w, err := NewWeb(m, "127.0.0.1:8123", "fixture.ts.net")
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, host, origin, body string, auth bool) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "http://"+host+"/settings", strings.NewReader(body))
		if auth {
			r.Header.Set("X-Nuntius-Token", w.token)
		}
		r.Header.Set("Origin", origin)
		r.Header.Set("Content-Type", "application/json")
		out := httptest.NewRecorder()
		w.ServeHTTP(out, r)
		return out
	}
	for _, host := range []string{"localhost:8123", "127.0.0.1:8123", "fixture.ts.net", "fixture.ts.net:443"} {
		if request("GET", host, "", "", true).Code != 200 {
			t.Fatal("valid settings GET refused")
		}
	}
	for _, host := range []string{"localhost:8124", "fixture.ts.net:444", "other.ts.net", "localhost"} {
		if request("GET", host, "", "", true).Code != 403 {
			t.Fatal("wrong settings host accepted")
		}
	}
	if request("GET", "localhost:8123", "", "", false).Code != 403 {
		t.Fatal("unsigned settings accepted")
	}
	for _, method := range []string{"PUT", "PATCH", "DELETE", "OPTIONS"} {
		if request(method, "localhost:8123", "", "{}", true).Code != 405 {
			t.Fatal("method accepted")
		}
	}
	for _, origin := range []string{"", "null", "https://localhost:8123", "http://other:8123"} {
		if request("POST", "localhost:8123", origin, "{}", true).Code != 403 {
			t.Fatal("origin accepted")
		}
	}
	get := request("GET", "localhost:8123", "", "", true)
	var current struct {
		Revision string `json:"revision"`
	}
	if json.Unmarshal(get.Body.Bytes(), &current) != nil || current.Revision == "" {
		t.Fatal("revision missing")
	}
	post := func(changes string) *httptest.ResponseRecorder {
		return request("POST", "localhost:8123", "http://localhost:8123", `{"schema_version":1,"expected_revision":"`+current.Revision+`","changes":`+changes+`}`, true)
	}
	before, _ := os.ReadFile(p)
	if post(`{"pools":{"claude":2,"codex":9}}`).Code != 422 {
		t.Fatal("invalid batch not refused")
	}
	after, _ := os.ReadFile(p)
	if !bytes.Equal(before, after) {
		t.Fatal("partial save")
	}
	if post(`{"pools":{"claude":2},"notify_imessage_enabled":false}`).Code != 200 {
		t.Fatal("valid batch refused")
	}
	actual, err := LoadConfig(p)
	if err != nil || actual.Pools["claude"].Size != 2 || actual.imessageEnabled() || actual.NotifyIMessage != c.NotifyIMessage {
		t.Fatal("batch preservation")
	}
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0600 {
		t.Fatal("private config mode")
	}
}

func TestSettingsTailnetAndHeaderOnly(t *testing.T) {
	_, w, _ := settingsFixture(t)
	for _, host := range []string{"fixture.ts.net", "fixture.ts.net:443"} {
		get := httptest.NewRequest("GET", "https://"+host+"/settings", nil)
		get.Header.Set("X-Nuntius-Token", w.token)
		out := httptest.NewRecorder()
		w.ServeHTTP(out, get)
		var value struct{ Revision string }
		if out.Code != 200 || json.Unmarshal(out.Body.Bytes(), &value) != nil {
			t.Fatal("valid tailnet read control")
		}
		for _, origin := range []string{"http://fixture.ts.net", "https://fixture.ts.net:443", "https://other.ts.net", "null", ""} {
			r := httptest.NewRequest("POST", "https://"+host+"/settings", strings.NewReader(settingsBody(value.Revision, `{"pools":{"claude":2}}`)))
			r.Header.Set("X-Nuntius-Token", w.token)
			r.Header.Set("Origin", origin)
			r.Header.Set("Content-Type", "application/json")
			out = httptest.NewRecorder()
			w.ServeHTTP(out, r)
			if out.Code != 403 {
				t.Fatal("tailnet origin guard")
			}
		}
		r := httptest.NewRequest("POST", "https://"+host+"/settings", strings.NewReader(settingsBody(value.Revision, `{"pools":{"claude":2}}`)))
		r.Header.Set("X-Nuntius-Token", w.token)
		r.Header.Set("Origin", "https://fixture.ts.net")
		r.Header.Set("Content-Type", "application/json")
		out = httptest.NewRecorder()
		w.ServeHTTP(out, r)
		if out.Code != 200 && out.Code != 422 {
			t.Fatal("valid tailnet POST control")
		}
	}
	for _, header := range []string{"", "wrong"} {
		r := httptest.NewRequest("GET", "http://localhost:8123/settings?token="+w.token, nil)
		r.Header.Set("X-Nuntius-Token", header)
		r.Header.Set("X-Forwarded-Host", "fixture.ts.net")
		out := httptest.NewRecorder()
		w.ServeHTTP(out, r)
		if out.Code != 403 {
			t.Fatal("query or forwarded auth accepted")
		}
	}
	get := httptest.NewRequest("GET", "http://localhost:8123/settings", strings.NewReader(`{"changes":{"pools":{"claude":8}}}`))
	get.Header.Set("X-Nuntius-Token", w.token)
	before := settingsRevision(t, w)
	out := httptest.NewRecorder()
	w.ServeHTTP(out, get)
	if out.Code != 200 || settingsRevision(t, w) != before {
		t.Fatal("GET mutated settings")
	}
	if out.Header().Get("Access-Control-Allow-Origin") != "" || !strings.Contains(out.Header().Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatal("CORS/CSP widened")
	}
	if strings.Contains(out.Body.String(), "fixture@example.invalid") || strings.Contains(out.Body.String(), "extra_args\": [") {
		t.Fatal("private fields returned")
	}
}

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
	// A newly started server rejects every remembered copy of the old grant.
	for _, current := range []bool{false, true} {
		req := httptest.NewRequest("GET", "http://localhost:8123/data", nil)
		credential, want := first.token, http.StatusForbidden
		if current {
			credential, want = second.token, http.StatusOK
		}
		req.Header.Set("X-Nuntius-Token", credential)
		out := httptest.NewRecorder()
		second.ServeHTTP(out, req)
		if out.Code != want {
			t.Fatal("new server grant revocation predicate failed")
		}
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
