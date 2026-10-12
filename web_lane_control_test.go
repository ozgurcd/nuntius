package nuntius

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type webGuardBody struct{ reads int }

func TestLaneHTTPHeldProjection(t *testing.T) {
	for _, session := range []string{"held-session", ""} {
		t.Run(map[bool]string{true: "session", false: "no-session"}[session != ""], func(t *testing.T) {
			m, h := heldReviewFixture(t, session)
			w, _ := NewWeb(m, "127.0.0.1:8123")
			snapshot := func() map[string][]byte {
				files := map[string][]byte{}
				if err := filepath.WalkDir(m.store, func(p string, d os.DirEntry, err error) error {
					if err == nil && !d.IsDir() {
						files[p], err = os.ReadFile(p)
					}
					return err
				}); err != nil {
					t.Fatal("fixture census failed")
				}
				return files
			}
			before := snapshot()
			read := func() map[string]any {
				req := httptest.NewRequest("GET", "http://localhost:8123/data", nil)
				req.Header.Set("X-Nuntius-Token", w.token)
				out := httptest.NewRecorder()
				w.ServeHTTP(out, req)
				var data map[string]any
				if out.Code != 200 || json.Unmarshal(out.Body.Bytes(), &data) != nil {
					t.Fatal("operational read failed")
				}
				v, ok := data["held_messages"].(map[string]any)
				if !ok || v["schema_version"] != float64(1) || v["status"] != "ok" {
					t.Fatal("held review projection missing")
				}
				return v
			}
			v := read()
			rows, ok := v["messages"].([]any)
			if !ok || len(rows) != 1 {
				t.Fatal("held identity missing")
			}
			row := rows[0].(map[string]any)
			if row["lane"] != h.Lane || row["run_id"] != h.RunID || row["goal_id"] != h.GoalID || row["content_revision"] != float64(h.Revision) || row["content_sha256"] != h.ContentSHA256 || row["state"] != "held_for_review" || row["text"] != Redact(reviewedText) || row["session_available"] != (session != "") || row["delivery_confirmed"] != false {
				t.Fatal("held review identity/content/session projection differs")
			}
			for _, key := range []string{"session_id", "run_sha256", "path"} {
				if _, exists := row[key]; exists {
					t.Fatal("private binding field exposed")
				}
			}
			after := snapshot()
			if len(before) != len(after) {
				t.Fatal("GET created held/control/audit artifacts")
			}
			for p, b := range before {
				if !bytes.Equal(b, after[p]) {
					t.Fatal("GET changed held/control/audit artifacts")
				}
			}
			laneReply(t, laneHTTP(t, w, "/held-message", heldBody(h, "discard-held")), 200, "")
			row = read()["messages"].([]any)[0].(map[string]any)
			if row["state"] != "discarded" || row["text"] != "" || row["delivery_confirmed"] != false {
				t.Fatal("terminal discard receipt not projected")
			}
		})
	}
}

func TestLaneHTTPHeldProjectionBound(t *testing.T) {
	m, h := heldReviewFixture(t, "held-session")
	original, err := loadRun(filepath.Join(m.store, "runs", h.RunID), h.RunID)
	if err != nil || original == nil {
		t.Fatal("fixture run unavailable")
	}
	for i := 1; i <= 65; i++ {
		r := *original
		r.ID = fmt.Sprintf("%032x", i)
		r.Started = original.Started.Add(-time.Duration(i) * time.Minute)
		if err := privateDir(filepath.Join(m.store, "runs", r.ID)); err != nil {
			t.Fatal(err)
		}
		if err := m.save(&r); err != nil {
			t.Fatal(err)
		}
		binding := h
		binding.RunID = r.ID
		b, _ := json.Marshal(messageReceipt{Version: 1, Binding: binding, State: "discarded", Discarded: true})
		if err := atomicPrivate(filepath.Join(m.store, "runs", r.ID, "message-receipt.json"), b); err != nil {
			t.Fatal(err)
		}
	}
	w, _ := NewWeb(m, "127.0.0.1:8123")
	req := httptest.NewRequest("GET", "http://localhost:8123/data", nil)
	req.Header.Set("X-Nuntius-Token", w.token)
	out := httptest.NewRecorder()
	w.ServeHTTP(out, req)
	var data struct {
		HeldMessages struct {
			Status               string            `json:"status"`
			Messages             []json.RawMessage `json:"messages"`
			OlderReceiptsOmitted bool              `json:"older_receipts_omitted"`
		} `json:"held_messages"`
	}
	if out.Code != 200 || json.Unmarshal(out.Body.Bytes(), &data) != nil || data.HeldMessages.Status != "ok" || len(data.HeldMessages.Messages) != 65 || !data.HeldMessages.OlderReceiptsOmitted {
		t.Fatal("bounded terminal history hid the active held message")
	}
	var first map[string]any
	if json.Unmarshal(data.HeldMessages.Messages[0], &first) != nil || first["run_id"] != h.RunID || first["state"] != "held_for_review" {
		t.Fatal("active review must precede terminal receipt history")
	}
}

func TestLaneHTTPHeldProjectionSend(t *testing.T) {
	m, h := heldReviewFixture(t, "held-session")
	w, _ := NewWeb(m, "127.0.0.1:8123")
	started := time.Now().UTC()
	t.Cleanup(func() {
		_ = m.Cancel(h.Lane)
		waitState(t, m, h.Lane, "ended")
		runs, err := m.history()
		if err != nil {
			t.Error("fixture history failed")
		}
		for _, r := range runs {
			if sameProcess(r) {
				t.Error("offline fake child survived")
			}
		}
		t.Logf("offline projected-send fixture start=%s end=%s teardown=verified", started.Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano))
	})
	laneReply(t, laneHTTP(t, w, "/held-message", heldBody(h, "send-held")), 200, "")
	read := func() webHeldSnapshot {
		req := httptest.NewRequest("GET", "http://localhost:8123/data", nil)
		req.Header.Set("X-Nuntius-Token", w.token)
		out := httptest.NewRecorder()
		w.ServeHTTP(out, req)
		var data struct {
			HeldMessages webHeldSnapshot `json:"held_messages"`
		}
		if out.Code != 200 || json.Unmarshal(out.Body.Bytes(), &data) != nil || data.HeldMessages.Status != "ok" || len(data.HeldMessages.Messages) != 1 {
			t.Fatal("receipt projection failed")
		}
		return data.HeldMessages
	}
	row := read().Messages[0]
	if row.State != "consumed" || !row.DeliveryConfirmed || row.RunID != h.RunID || row.Text != "" {
		t.Fatal("identified consumed receipt missing")
	}
	heldReviewChildren(t, m, h, 1)
	original, err := loadRun(filepath.Join(m.store, "runs", h.RunID), h.RunID)
	if err != nil || original == nil {
		t.Fatal("original fixture run unavailable")
	}
	original.ContinueRunID = strings.Repeat("d", 32)
	if err := m.save(original); err != nil {
		t.Fatal(err)
	}
	if read().Messages[0].DeliveryConfirmed {
		t.Fatal("wrong follow-up identity claimed delivery")
	}
}

func (b *webGuardBody) Read([]byte) (int, error) { b.reads++; return 0, errors.New("RAW-PRIVATE") }
func (b *webGuardBody) Close() error             { return nil }

func TestLaneHTTPBoundaryBeforeParse(t *testing.T) {
	m, _, _ := manager(t)
	w, _ := NewWeb(m, "127.0.0.1:8123", "fixture.ts.net")
	for _, path := range []string{"/lane-control", "/held-message"} {
		for _, bad := range []string{"host", "port", "grant", "origin"} {
			body := &webGuardBody{}
			r := httptest.NewRequest(http.MethodPost, "http://localhost:8123"+path, body)
			r.Header.Set("X-Nuntius-Token", w.token)
			r.Header.Set("Origin", "http://localhost:8123")
			switch bad {
			case "host":
				r.Host = "other:8123"
			case "port":
				r.Host = "localhost:8124"
			case "grant":
				r.Header.Del("X-Nuntius-Token")
			case "origin":
				r.Header.Set("Origin", "null")
			}
			out := httptest.NewRecorder()
			w.ServeHTTP(out, r)
			laneReply(t, out, 403, "forbidden")
			if body.reads != 0 {
				t.Fatal("boundary parsed body")
			}
		}
	}
	for _, host := range []string{"fixture.ts.net", "fixture.ts.net:443"} {
		r := httptest.NewRequest("POST", "https://"+host+"/lane-control", strings.NewReader(laneBody(0, "resume")))
		r.Header.Set("X-Nuntius-Token", w.token)
		r.Header.Set("Origin", "https://fixture.ts.net")
		r.Header.Set("Content-Type", "application/json")
		out := httptest.NewRecorder()
		w.ServeHTTP(out, r)
		laneReply(t, out, 200, "")
	}
}

// Emulate the server's read deadline on the real mux without opening a network
// listener or sleeping five seconds. An expired slow read returns a timeout.
type webDeadlineBody struct {
	deadline time.Time
	reads    int
}

func (b *webDeadlineBody) Read([]byte) (int, error) {
	b.reads++
	if b.deadline.IsZero() {
		return 0, io.EOF
	}
	return 0, os.ErrDeadlineExceeded
}
func (b *webDeadlineBody) Close() error { return nil }

type webDeadlineWriter struct {
	*httptest.ResponseRecorder
	body      *webDeadlineBody
	deadlines []time.Time
}

func (w *webDeadlineWriter) SetReadDeadline(at time.Time) error {
	w.deadlines = append(w.deadlines, at)
	w.body.deadline = at
	return nil
}

func TestLaneHTTPBodyDeadlineAndCeiling(t *testing.T) {
	m, _, _ := manager(t)
	w, _ := NewWeb(m, "127.0.0.1:8123")
	b := &webDeadlineBody{}
	r := httptest.NewRequest("POST", "http://localhost:8123/lane-control", b)
	r.Header.Set("X-Nuntius-Token", w.token)
	r.Header.Set("Origin", "http://localhost:8123")
	r.Header.Set("Content-Type", "application/json")
	out := &webDeadlineWriter{ResponseRecorder: httptest.NewRecorder(), body: b}
	start := time.Now()
	w.ServeHTTP(out, r)
	laneReply(t, out.ResponseRecorder, 400, "invalid_request")
	if b.reads != 1 || len(out.deadlines) != 2 || out.deadlines[0].Sub(start) < 4*time.Second || out.deadlines[0].Sub(start) > 6*time.Second || !out.deadlines[1].IsZero() {
		t.Fatal("five-second deadline not installed/reset")
	}
	w.settingsState.post = settingsBucket{}
	// Exactly 4096 bytes passes framing; one more byte cannot mutate.
	body := laneBody(0, "resume")
	body += strings.Repeat(" ", 4096-len(body))
	laneReply(t, laneHTTP(t, w, "/lane-control", body), 200, "")
	laneReply(t, laneHTTP(t, w, "/lane-control", body+" "), 400, "invalid_request")
	ceiling := httptest.NewRecorder()
	webControlReply(ceiling, 200, "", map[string]any{"value": strings.Repeat("x", 32769)})
	laneReply(t, ceiling, 503, "unavailable")
}

func heldBody(h heldMessage, operation string) string {
	b, _ := json.Marshal(map[string]any{"schema_version": 1, "lane": h.Lane, "operation": operation, "run_id": h.RunID, "content_revision": h.Revision, "content_sha256": h.ContentSHA256})
	return string(b)
}

func TestLaneHTTPHeldIdentityAndPause(t *testing.T) {
	m, h := heldReviewFixture(t, "held-session")
	w, _ := NewWeb(m, "127.0.0.1:8123")
	stale := h
	stale.Revision++
	laneReply(t, laneHTTP(t, w, "/held-message", heldBody(stale, "discard-held")), 409, "revision_conflict")
	successorPause(t, m)
	laneReply(t, laneHTTP(t, w, "/held-message", heldBody(h, "send-held")), 409, "lane_paused")
	heldReviewRetained(t, m, h)
	heldReviewChildren(t, m, h, 0)
	w.settingsState.post = settingsBucket{}
	laneReply(t, laneHTTP(t, w, "/held-message", heldBody(h, "discard-held")), 200, "")
	laneReply(t, laneHTTP(t, w, "/held-message", heldBody(h, "discard-held")), 200, "")
	heldReviewChildren(t, m, h, 0)
}

func TestLaneHTTPHeldSendAndGuards(t *testing.T) {
	m, h := heldReviewFixture(t, "held-session")
	w, _ := NewWeb(m, "127.0.0.1:8123")
	for _, body := range []string{
		strings.Replace(heldBody(h, "send-held"), `"content_revision":1`, `"content_revision":null`, 1),
		strings.Replace(heldBody(h, "send-held"), `"run_id":`, `"path":"RAW-PRIVATE","run_id":`, 1),
		strings.Replace(heldBody(h, "send-held"), `"operation":"send-held"`, `"operation":"send"`, 1),
	} {
		w.settingsState.post = settingsBucket{}
		laneReply(t, laneHTTP(t, w, "/held-message", body), 400, "invalid_request")
	}
	w.settingsState.post = settingsBucket{}
	m.mu.Lock()
	out := laneHTTP(t, w, "/held-message", heldBody(h, "send-held"))
	m.mu.Unlock()
	laneReply(t, out, 503, "locked")
	guard, err := m.controlLock()
	if err != nil {
		t.Fatal(err)
	}
	out = laneHTTP(t, w, "/held-message", heldBody(h, "send-held"))
	guard.Close()
	laneReply(t, out, 503, "locked")
	w.settingsState.post = settingsBucket{}
	started := time.Now().UTC()
	t.Cleanup(func() {
		// A finite fake can already have exited; end/identity checks below are
		// authoritative for teardown rather than Cancel's already-ended refusal.
		_ = m.Cancel(h.Lane)
		waitState(t, m, h.Lane, "ended")
		runs, err := m.history()
		if err != nil {
			t.Error("fixture history failed")
		}
		for _, r := range runs {
			if sameProcess(r) {
				t.Error("fixture child survived")
			}
		}
		t.Logf("offline held-send fixture start=%s end=%s teardown=verified", started.Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano))
	})
	laneReply(t, laneHTTP(t, w, "/held-message", heldBody(h, "send-held")), 200, "")
	heldReviewChildren(t, m, h, 1)
}

func TestLaneHTTPUnavailableAndUncertain(t *testing.T) {
	for _, stage := range []string{"state.rename", "audit.outcome.append"} {
		t.Run(stage, func(t *testing.T) {
			m, _, _ := manager(t)
			w, _ := NewWeb(m, "127.0.0.1:8123")
			w.controlIO = &laneControlIO{checkpoint: func(at string) error {
				if at == stage {
					return errors.New("RAW-PRIVATE path")
				}
				return nil
			}}
			laneReply(t, laneHTTP(t, w, "/lane-control", laneBody(0, "pause")), 503, "uncertain")
		})
	}
	m, _, _ := manager(t)
	w, _ := NewWeb(m, "127.0.0.1:8123")
	if err := os.WriteFile(filepath.Join(m.store, "lane-control.json"), []byte("RAW-PRIVATE"), 0600); err != nil {
		t.Fatal(err)
	}
	laneReply(t, laneHTTP(t, w, "/lane-control", laneBody(0, "pause")), 503, "unavailable")
}

func laneHTTP(t *testing.T, w *Web, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "http://localhost:8123"+path, strings.NewReader(body))
	r.Header.Set("X-Nuntius-Token", w.token)
	r.Header.Set("Origin", "http://localhost:8123")
	r.Header.Set("Content-Type", "application/json")
	out := httptest.NewRecorder()
	w.ServeHTTP(out, r)
	return out
}

func laneBody(revision uint64, operation string) string {
	return fmt.Sprintf(`{"schema_version":1,"lane":"claude","operation":%q,"expected_revision":%d}`, operation, revision)
}

func laneReply(t *testing.T, out *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	var v map[string]any
	if out.Code != status || json.Unmarshal(out.Body.Bytes(), &v) != nil || (code != "" && v["code"] != code) {
		t.Fatalf("response status %d; expected %d/%s", out.Code, status, code)
	}
	if code != "" && (v["reason"] == nil || v["reason"] == "") {
		t.Fatal("error has no plain reason")
	}
	if out.Body.Len() > 32768 || strings.Contains(out.Body.String(), "RAW-PRIVATE") {
		t.Fatal("unsafe or oversized response")
	}
}

func TestLaneHTTPControlCAS(t *testing.T) {
	m, _, _ := manager(t)
	w, err := NewWeb(m, "127.0.0.1:8123")
	if err != nil {
		t.Fatal(err)
	}
	laneReply(t, laneHTTP(t, w, "/lane-control", laneBody(0, "pause")), 200, "")
	laneReply(t, laneHTTP(t, w, "/lane-control", laneBody(0, "resume")), 409, "revision_conflict")
	laneReply(t, laneHTTP(t, w, "/lane-control", laneBody(1, "resume")), 200, "")
	runs, _ := m.history()
	if len(runs) != 0 {
		t.Fatal("control started work")
	}
}

func TestLaneHTTPGuards(t *testing.T) {
	m, _, _ := manager(t)
	w, _ := NewWeb(m, "127.0.0.1:8123", "fixture.ts.net")
	for _, tc := range []struct {
		name, method, host, origin, path, content string
		auth                                      bool
		want                                      int
	}{
		{"host", "POST", "other:8123", "http://other:8123", "/lane-control", "application/json", true, 403},
		{"port", "POST", "localhost:8124", "http://localhost:8124", "/lane-control", "application/json", true, 403},
		{"tailport", "POST", "fixture.ts.net:444", "https://fixture.ts.net", "/lane-control", "application/json", true, 403},
		{"auth", "POST", "localhost:8123", "http://localhost:8123", "/lane-control", "application/json", false, 403},
		{"origin", "POST", "localhost:8123", "null", "/lane-control", "application/json", true, 403},
		{"method", "GET", "localhost:8123", "", "/lane-control", "application/json", true, 405},
		{"query", "POST", "localhost:8123", "http://localhost:8123", "/lane-control?RAW-PRIVATE=1", "application/json", true, 400},
		{"emptyquery", "POST", "localhost:8123", "http://localhost:8123", "/lane-control?", "application/json", true, 400},
		{"content", "POST", "localhost:8123", "http://localhost:8123", "/lane-control", "text/plain", true, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "http://"+tc.host+tc.path, strings.NewReader("RAW-PRIVATE"))
			if tc.auth {
				r.Header.Set("X-Nuntius-Token", w.token)
			}
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("Content-Type", tc.content)
			out := httptest.NewRecorder()
			w.ServeHTTP(out, r)
			laneReply(t, out, tc.want, "")
		})
	}
}

func TestLaneHTTPStrictSchema(t *testing.T) {
	m, _, _ := manager(t)
	w, _ := NewWeb(m, "127.0.0.1:8123")
	good := laneBody(0, "pause")
	for _, body := range []string{
		`{}`, `null`, `[]`, good + `{}`, strings.Repeat(" ", 4097),
		strings.Replace(good, `"schema_version":1`, `"schema_version":2`, 1),
		strings.Replace(good, `"lane"`, `"Lane"`, 1),
		strings.Replace(good, `"lane":"claude"`, `"lane":"RAW-PRIVATE/../path"`, 1),
		strings.Replace(good, `"lane":"claude"`, `"lane":"claude","lane":"codex"`, 1),
		strings.Replace(good, `"expected_revision":0`, `"expected_revision":null`, 1),
		strings.Replace(good, `"expected_revision":0`, `"expected_revision":"0"`, 1),
		strings.Replace(good, `"expected_revision":0`, `"expected_revision":0.5`, 1),
		strings.Replace(good, `"expected_revision":0`, `"expected_revision":-1`, 1),
		strings.Replace(good, `"expected_revision":0`, `"expected_revision":9007199254740992`, 1),
		strings.Replace(good, `}`, `,"after_current":"true"}`, 1),
		strings.Replace(good, `}`, `,"after_current":null}`, 1),
		strings.Replace(good, `}`, `,"path":"RAW-PRIVATE"}`, 1),
		strings.Replace(laneBody(0, "resume"), `}`, `,"after_current":false}`, 1),
	} {
		w.settingsState.post = settingsBucket{}
		laneReply(t, laneHTTP(t, w, "/lane-control", body), 400, "invalid_request")
	}
}

func TestLaneHTTPGlobalRateAndLocks(t *testing.T) {
	m, w, _ := settingsFixture(t)
	body := strings.Replace(laneBody(0, "resume"), `"claude"`, `"claude-1"`, 1)
	// All authenticated writes spend from one bucket, including existing actions.
	for i := 0; i < 3; i++ {
		laneReply(t, laneHTTP(t, w, "/lane-control", body), 200, "")
	}
	laneReply(t, laneHTTP(t, w, "/settings", `{}`), 429, "rate_limited")
	laneReply(t, laneHTTP(t, w, "/action", `{"action":"note","text":"safe"}`), 429, "rate_limited")
	w.settingsState.post = settingsBucket{}
	m.mu.Lock()
	start := time.Now()
	out := laneHTTP(t, w, "/lane-control", laneBody(0, "pause"))
	m.mu.Unlock()
	laneReply(t, out, 503, "locked")
	if time.Since(start) > time.Second {
		t.Fatal("manager lock blocked")
	}
	w.settingsState.post = settingsBucket{}
	guard, err := m.controlLock()
	if err != nil {
		t.Fatal(err)
	}
	out = laneHTTP(t, w, "/lane-control", laneBody(0, "pause"))
	guard.Close()
	laneReply(t, out, 503, "locked")
}

func TestLaneHTTPDataPureAndTruthful(t *testing.T) {
	m, _, _ := manager(t)
	w, _ := NewWeb(m, "127.0.0.1:8123")
	r := Run{ID: strings.Repeat("a", 32), Lane: "claude", Kind: "claude", GoalID: "WEB.PURE", Started: time.Now().Add(-time.Hour), State: "running", PID: 2147483647, ProcessStart: "absent"}
	if err := privateDir(filepath.Join(m.store, "runs", r.ID)); err != nil {
		t.Fatal(err)
	}
	if err := m.save(&r); err != nil {
		t.Fatal(err)
	}
	// Missing signal directory and unresolved control intent must stay missing/unrepaired.
	if err := os.RemoveAll(filepath.Join(m.store, "signals")); err != nil {
		t.Fatal(err)
	}
	intent := filepath.Join(m.store, "lane-control-intent.json")
	if err := os.WriteFile(intent, []byte("RAW-PRIVATE invalid intent"), 0600); err != nil {
		t.Fatal(err)
	}
	snapshot := func() map[string][]byte {
		files := map[string][]byte{}
		filepath.WalkDir(m.store, func(p string, d os.DirEntry, e error) error {
			if e == nil && !d.IsDir() {
				files[p], _ = os.ReadFile(p)
			}
			return e
		})
		return files
	}
	before := snapshot()
	req := httptest.NewRequest("GET", "http://localhost:8123/data", nil)
	req.Header.Set("X-Nuntius-Token", w.token)
	out := httptest.NewRecorder()
	w.ServeHTTP(out, req)
	if out.Code != 200 {
		t.Fatalf("data status %d", out.Code)
	}
	var data struct {
		Lanes   []Status
		Version int `json:"lane_control_version"`
	}
	if json.Unmarshal(out.Body.Bytes(), &data) != nil {
		t.Fatal("invalid data")
	}
	if data.Version != 1 || len(data.Lanes) != 2 || data.Lanes[0].State == "running" || data.Lanes[0].LaneControl.Phase == "draining" {
		t.Fatal("untruthful observation")
	}
	after := snapshot()
	if len(before) != len(after) {
		t.Fatal("GET created files")
	}
	for p, b := range before {
		if !bytes.Equal(b, after[p]) {
			t.Fatal("GET changed files")
		}
	}
	if _, err := os.Lstat(filepath.Join(m.store, "signals")); !os.IsNotExist(err) {
		t.Fatal("GET created signal directory")
	}
}

func TestLaneHTTPPauseAfterCurrentAndRateAuthentication(t *testing.T) {
	m, _, _ := manager(t)
	w, _ := NewWeb(m, "127.0.0.1:8123")
	for i := 0; i < 5; i++ {
		r := httptest.NewRequest("POST", "http://localhost:8123/lane-control", strings.NewReader(laneBody(0, "pause")))
		r.Header.Set("Origin", "http://localhost:8123")
		out := httptest.NewRecorder()
		w.ServeHTTP(out, r)
		laneReply(t, out, 403, "forbidden")
	}
	body := strings.TrimSuffix(laneBody(0, "pause"), "}") + `,"after_current":true}`
	laneReply(t, laneHTTP(t, w, "/lane-control", body), 200, "")
	snapshot, err := m.laneControls()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Revision != 1 || !snapshot.Lanes[0].Paused || snapshot.Lanes[0].Phase != "paused" || !snapshot.Lanes[0].RequestedAfterCurrent || snapshot.Lanes[0].Source != "web" {
		t.Fatal("pause flags/provenance lost")
	}
	laneReply(t, laneHTTP(t, w, "/lane-control", laneBody(1, "pause")), 200, "")
	laneReply(t, laneHTTP(t, w, "/held-message", `{}`), 400, "invalid_request")
	laneReply(t, laneHTTP(t, w, "/lane-control", laneBody(1, "resume")), 429, "rate_limited")
	// One token refills in six seconds, regardless of which write route uses it.
	w.settingsState.post.At = time.Now().Add(-6 * time.Second)
	laneReply(t, laneHTTP(t, w, "/lane-control", laneBody(1, "resume")), 200, "")
	laneReply(t, laneHTTP(t, w, "/settings", `{}`), 429, "rate_limited")
}

func TestLaneHTTPHeldDurabilityUncertain(t *testing.T) {
	m, h := heldReviewFixture(t, "held-session")
	w, _ := NewWeb(m, "127.0.0.1:8123")
	w.controlIO = heldReviewFault("receipt-discarded.renamed")
	laneReply(t, laneHTTP(t, w, "/held-message", heldBody(h, "discard-held")), 503, "uncertain")
}

func TestLaneHTTPObservedTerminalTailPure(t *testing.T) {
	c, store := opencodeFixture(t)
	m, err := NewManager(c, store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	final := "Final Report — TERMINAL-TEST\nQUESTIONS: (a) keep this complete answer; (b) review later.\n" + strings.Repeat("plain terminal result ", 20)
	run := seedTerminalRun(t, m, terminalStart+terminalText(final)+terminalStop, "running")
	path := filepath.Join(store, "runs", run.ID, "meta.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	w, err := NewWeb(m, "127.0.0.1:8123")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "http://localhost:8123/data", nil)
	req.Header.Set("X-Nuntius-Token", w.token)
	out := httptest.NewRecorder()
	w.ServeHTTP(out, req)
	var data struct {
		Tails map[string][]string
		Lanes []Status
	}
	if out.Code != 200 || json.Unmarshal(out.Body.Bytes(), &data) != nil || strings.Join(data.Tails[run.Lane], "\n") != final {
		t.Fatal("pure observation lost complete terminal fallback")
	}
	if len(data.Lanes) != 1 || data.Lanes[0].State != "ended_no_report" {
		t.Fatal("terminal attention lost")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("terminal GET saved metadata")
	}
}
