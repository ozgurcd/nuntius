package nuntius

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestDashboardAssetsBootstrap(t *testing.T) {
	m, _, _ := manager(t)
	w, err := NewWeb(m, "127.0.0.1:8123")
	if err != nil {
		t.Fatal(err)
	}
	for _, asset := range []struct{ path, contentType string }{
		{"/", "text/html; charset=utf-8"},
		{"/app.css", "text/css; charset=utf-8"},
		{"/app.js", "text/javascript; charset=utf-8"},
	} {
		for _, auth := range []bool{false, true} {
			path := asset.path
			if auth {
				path += "?token=" + w.token
			}
			req := httptest.NewRequest("GET", "http://localhost:8123"+path, nil)
			out := httptest.NewRecorder()
			w.ServeHTTP(out, req)
			if !auth {
				if out.Code != 403 {
					t.Fatal("unguarded asset")
				}
				continue
			}
			if out.Code != 200 || out.Header().Get("Content-Type") != asset.contentType || out.Body.Len() == 0 {
				t.Fatal("asset unavailable")
			}
			for key, want := range map[string]string{"Cache-Control": "no-store", "Referrer-Policy": "no-referrer", "X-Content-Type-Options": "nosniff"} {
				if out.Header().Get(key) != want {
					t.Fatal("asset header missing")
				}
			}
			if asset.path == "/" {
				if !strings.Contains(out.Body.String(), "/app.js?token="+w.token) || !strings.Contains(out.Body.String(), "/app.css?token="+w.token) || strings.Contains(out.Body.String(), "<style") || strings.Contains(out.Body.String(), "<script>") {
					t.Fatal("external asset bootstrap missing")
				}
			} else if strings.Contains(out.Body.String(), w.token) {
				t.Fatal("credential entered static source")
			}
		}
	}
	page, err := dashboardAssets.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(page)
	if strings.Count(html, `role="tablist"`) != 1 || strings.Count(html, `role="tab"`) != 5 || strings.Count(html, `role="tabpanel"`) != 5 || strings.Count(html, `aria-selected="true"`) != 1 {
		t.Fatal("dashboard needs five accessible tabs and one initial selection")
	}
	for _, id := range []string{"agents", "messages", "digest", "runs", "auditSection"} {
		if !strings.Contains(html, `id="tab-`+id+`"`) || !strings.Contains(html, `aria-controls="`+id+`"`) || !strings.Contains(html, `aria-labelledby="tab-`+id+`"`) {
			t.Fatal("tab and panel association missing")
		}
	}
	if !strings.Contains(html, `id="tab-agents" role="tab" aria-selected="true" tabindex="0"`) || strings.Count(html, `role="tabpanel" hidden`) != 4 {
		t.Fatal("Agents must be the only initially visible panel")
	}
	script, err := dashboardAssets.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	// Source contracts are visible to Go; runtime focus/layout remain judge proofs.
	for _, contract := range []string{"selectTab(location.hash.slice(1))", "'/' + location.hash", "'hashchange'", "'ArrowLeft'", "'ArrowRight'", "'Home'", "'End'", "panel.hidden = !selected", "tab.setAttribute('aria-selected', String(selected))", "tab.tabIndex = selected ? 0 : -1", "if (lastData) renderPanels(lastData)", "if (!el('agents').hidden)", "if (!el('messages').hidden", "if (!el('digest').hidden", "if (!el('runs').hidden", "if (!el('auditSection').hidden"} {
		if !strings.Contains(string(script), contract) {
			t.Fatalf("tab source contract missing: %s", contract)
		}
	}
}

// Strict decoding binds the screenshot fixture to actual card/status types and
// the existing history, notes and freeze JSON shape, including optional fields.
func TestDashboardFixtureMatchesData(t *testing.T) {
	type historyView struct {
		RunID    string            `json:"run_id"`
		Lane     string            `json:"lane"`
		GoalID   string            `json:"goal_id"`
		Started  time.Time         `json:"started"`
		Ended    *time.Time        `json:"ended"`
		State    string            `json:"state"`
		Exit     *int              `json:"exit"`
		Goal     string            `json:"goal"`
		Report   string            `json:"report"`
		Sections map[string]string `json:"sections"`
	}
	type dashboard struct {
		Freeze    *FreezeState        `json:"freeze"`
		Lanes     []webLane           `json:"lanes"`
		Tails     map[string][]string `json:"tails"`
		History   []historyView       `json:"history"`
		Notes     []OwnerNote         `json:"notes"`
		Audit     string              `json:"audit"`
		Oversight string              `json:"oversight"`
	}
	decode := func(b []byte) dashboard {
		t.Helper()
		var d dashboard
		decoder := json.NewDecoder(bytes.NewReader(b))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&d) != nil {
			t.Fatal("dashboard fixture/data shape differs")
		}
		for _, lane := range d.Lanes {
			if lane.Kind == "" || lane.ETA == "" || lane.Activity == "" {
				t.Fatal("missing presentation field")
			}
		}
		return d
	}
	sample, err := os.ReadFile("testdata/dashboard-data.json")
	if err != nil {
		t.Fatal(err)
	}
	fixture := decode(sample)
	if len(fixture.Lanes) != 4 || len(fixture.History) == 0 || len(fixture.Notes) == 0 || fixture.Oversight == "" {
		t.Fatal("unrealistic fixture")
	}
	states := map[string]bool{}
	for _, lane := range fixture.Lanes {
		states[lane.State] = true
	}
	if len(states) != 4 {
		t.Fatal("fixture lacks varied states")
	}
	for _, h := range fixture.History {
		expected := reportSections(h.Report)
		if expected["QUESTIONS"] != h.Sections["QUESTIONS"] || expected["DECISIONS"] != h.Sections["DECISIONS"] {
			t.Fatal("fixture sections differ from real extraction")
		}
	}
	if Redact(string(sample)) != string(sample) {
		t.Fatal("fixture is not redacted")
	}
	m, _, _ := manager(t)
	if _, err = m.Dispatch("codex", "SHAPE-1", "hello"); err != nil {
		t.Fatal(err)
	}
	waitState(t, m, "codex", "ended")
	w, err := NewWeb(m, "127.0.0.1:8123")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "http://localhost:8123/data", nil)
	req.Header.Set("X-Nuntius-Token", w.token)
	out := httptest.NewRecorder()
	w.ServeHTTP(out, req)
	if out.Code != 200 {
		t.Fatal("data unavailable")
	}
	real := decode(out.Body.Bytes())
	if len(real.History) == 0 || len(real.Lanes) == 0 {
		t.Fatal("real shape not exercised")
	}
	var actualKeys, sampleKeys map[string]json.RawMessage
	if json.Unmarshal(out.Body.Bytes(), &actualKeys) != nil || json.Unmarshal(sample, &sampleKeys) != nil {
		t.Fatal("shape decode")
	}
	if len(actualKeys) != len(sampleKeys) {
		t.Fatal("fixture root keys differ")
	}
	for key := range actualKeys {
		if _, ok := sampleKeys[key]; !ok {
			t.Fatal("fixture root field missing")
		}
	}
}
