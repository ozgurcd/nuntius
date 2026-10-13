package nuntius

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Disposable keys stay in memory except for task-owned private input fixtures.
// Failures contain only surface IDs and booleans, never fixture bytes.
func hotfixReviewKey(t *testing.T) (string, string) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal("K01 generation=false")
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal("K01 encoding=false")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		t.Fatal("K01 parsing=false")
	}
	msg := []byte("offline-fixture-validity")
	if !ed25519.Verify(pub, msg, ed25519.Sign(parsed.(ed25519.PrivateKey), msg)) {
		t.Fatal("K01 signature=false")
	}
	full := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	return full, strings.Split(full, "\n")[1]
}

func hotfixReviewSinks(t *testing.T, events []byte, reportText, body string) {
	t.Helper()
	m, c, store := manager(t)
	seedHistory(t, m, 1)
	r, err := m.latest("claude")
	if err != nil || r == nil {
		t.Fatal("S00 fixture=false")
	}
	path := filepath.Join(store, "runs", r.ID, "events.jsonl")
	if os.WriteFile(path, events, 0600) != nil {
		t.Fatal("S00 events=false")
	}
	outputs := map[string]string{}
	if reportText != "" {
		if os.WriteFile(filepath.Join(c.ReportsDir, r.GoalID+".txt"), []byte(reportText), 0600) != nil {
			t.Fatal("S00 report=false")
		}
		report, err := m.Report(r.GoalID)
		if err != nil {
			t.Fatal("S01 entrance=false")
		}
		outputs["report"] = report
	}
	tail, err := m.Tail("claude", 200)
	if err != nil {
		t.Fatal("S02 entrance=false")
	}
	outputs["tail"] = strings.Join(tail, "\n")
	outputs["current-action"] = actionText(m.currentAction(r.ID, events, time.Time{}, time.Now()))
	w, err := NewWeb(m, "127.0.0.1:0")
	if err != nil {
		t.Fatal("S03 entrance=false")
	}
	req := httptest.NewRequest("GET", "http://127.0.0.1:0/data", nil)
	req.Header.Set("X-Nuntius-Token", w.token)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatal("S03 authenticated=false")
	}
	outputs["data"] = rec.Body.String()
	input := "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{}}\n{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}\n"
	for i, name := range []string{"nuntius_tail", "nuntius_report"} {
		args := `{"lane":"claude","lines":200}`
		if name == "nuntius_report" {
			if reportText == "" {
				continue
			}
			args = fmt.Sprintf(`{"goal_id":%q}`, r.GoalID)
		}
		input += fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s}}`+"\n", i+2, name, args)
	}
	var out bytes.Buffer
	if Serve(context.Background(), m, strings.NewReader(input), &out) != nil {
		t.Fatal("S04 entrance=false")
	}
	outputs["mcp-wire-and-content"] = out.String()
	configPath := filepath.Join(c.ReportsDir, "fixture-config.json")
	configBytes, err := json.Marshal(c)
	if err != nil || os.WriteFile(configPath, configBytes, 0600) != nil {
		t.Fatal("S05 CLI-config=false")
	}
	var watch, watchErr bytes.Buffer
	if CLI(context.Background(), []string{"--config", configPath, "--store", store, "watch", "claude"}, strings.NewReader(""), &watch, &watchErr) != 0 {
		t.Fatal("S05 CLI-watch-entrance=false")
	}
	outputs["cli-watch"] = watch.String()
	warm, err := m.Tail("claude", 200)
	if err != nil {
		t.Fatal("S06 warm-cache-entrance=false")
	}
	outputs["warm-cache-tail"] = strings.Join(warm, "\n")
	for _, window := range m.eventWindows {
		outputs["cache"] += window.final + window.latest.Summary + strings.Join(window.tail, "\n")
		for _, line := range window.lines {
			outputs["cache-lines"] += line.final
			for _, action := range line.actions {
				outputs["cache-lines"] += action.event.Summary + action.event.Text
			}
		}
	}
	for id, output := range outputs {
		exposed := strings.Contains(output, body)
		t.Logf("%s exposed=%t", id, exposed)
		if exposed {
			t.Errorf("%s no-key=false", id)
		}
	}
}

func TestHotfixReviewF1OrphanPrefix(t *testing.T) {
	full, body := hotfixReviewKey(t)
	t.Run("PM01", func(t *testing.T) {
		text := "public-before\n" + body + "\n" + full + "public-between\n-----END PRIVATE KEY-----\n" + full + "public-after"
		if Redact(text) != privateHidden {
			t.Error("PM01 whole-segment=false")
		}
		hotfixReviewSinks(t, privateMessage(text), text, body)
	})
	for _, tc := range []struct{ id, wire string }{
		{"AF06", fmt.Sprintf(`{"type":"text","part":{"type":"text","text":%q},"metadata":%q,"orphan":"-----END PRIVATE KEY-----","after":"public-after"}`, body, full)},
		{"AF03", fmt.Sprintf(`{"type":"text","part":{"type":"text","text":%q},"metadata":%q,"ending":"-----END PRIVATE KEY-----","after":"public-after"}`, body, full)},
		{"FS01", fmt.Sprintf(`{"type":"text","part":{"type":"text","text":%q},"other":%q,"ending":"-----END PRIVATE KEY-----","after":"public-after"}`, body[:len(body)/2], body[len(body)/2:])},
	} {
		t.Run(tc.id, func(t *testing.T) {
			var privacy privateEventPrivacy
			safe, _ := privacy.project([]byte(tc.wire))
			if string(safe) != privateHidden {
				t.Error("whole-event=false")
			}
			earlier := privateMessage("public-progress\r\n\tUnicode: λ")
			events := append(append([]byte{}, earlier...), []byte(tc.wire+"\n")...)
			projected := privateEventProjection(events, privateEventPrivacy{})
			if !bytes.Equal(projected[0], bytes.TrimSuffix(earlier, []byte("\n"))) {
				t.Error("earlier-event-byte-preservation=false")
			}
			probe := body
			if tc.id == "FS01" {
				probe = body[:len(body)/2]
			}
			hotfixReviewSinks(t, events, "", probe)
			c, store := opencodeFixture(t)
			m, err := NewManager(c, store)
			if err != nil {
				t.Fatal("terminal fixture=false")
			}
			defer m.Close()
			r := seedTerminalRun(t, m, terminalStart+tc.wire+"\n"+terminalStop, "ended")
			ended, final := m.terminalTurn(r)
			if !ended || strings.Contains(final, probe) {
				t.Error("terminal fallback lifecycle or no-key=false")
			}
		})
	}
	for _, text := range []string{
		"public-before\n" + body + "\n-----END PRIVATE KEY-----\nprivate-after",
		body + "\n-----END PRIVATE KEY-----\n" + full + "later-public",
		"public-before\n" + full + "public-between\n" + body + "\n-----END PRIVATE KEY-----",
	} {
		if strings.Contains(Redact(text), body) {
			t.Error("F1 plain-no-key=false")
		}
		hotfixReviewSinks(t, privateMessage(text), text, body)
	}
	// The same event may place the orphan after a different decoded value/key.
	for _, wire := range []string{
		fmt.Sprintf(`{"type":"text","part":{"text":%q},"metadata":"-----END PRIVATE KEY-----"}`, body),
		fmt.Sprintf(`{"type":"text","part":{"text":%q},"-----END PRIVATE KEY-----":"metadata"}`, body),
		fmt.Sprintf(`{"type":"text","part":{"text":%q},"metadata":"-----END PRIVATE KEY-----\n-----BEGIN PRIVATE KEY-----\n-----END PRIVATE KEY-----"}`, body),
	} {
		hotfixReviewSinks(t, []byte(wire+"\n"), "", body)
	}
	// An earlier complete event is outside the new hide-back boundary.
	events := append(privateMessage("public-progress"), privateMessage(body+"\n-----END PRIVATE KEY-----")...)
	w := (&Manager{}).eventWindow("F1-boundary", events, false)
	if !strings.Contains(strings.Join(w.tail, "\n"), "public-progress") {
		t.Error("F1 event-boundary-preservation=false")
	}
}

func TestHotfixReviewF2PathCollision(t *testing.T) {
	_, body := hotfixReviewKey(t)
	for _, tc := range []struct{ id, wire string }{
		{"J01", `{"type":"text","part/text":"-----BEGIN PRIVATE KEY-----","part":{"text":"-----END PRIVATE KEY-----"}}`},
		{"J02", `{"type":"item.completed","item/text":"-----BEGIN PRIVATE KEY-----","item":{"type":"agent_message","text":"-----END PRIVATE KEY-----"}}`},
		{"J03", `{"type":"assistant","message/content/[]/text":"-----BEGIN PRIVATE KEY-----","message":{"content":[{"type":"text","text":"-----END PRIVATE KEY-----"}]}}`},
		{"J04", `{"type":"assistant","message":{"content":{"[]":{"text":"-----BEGIN PRIVATE KEY-----"}}},"other":"-----END PRIVATE KEY-----"}`},
		{"J05", `{"type":"assistant","message":{"content":[{"type":"tool_use","input/cmd":"-----BEGIN PRIVATE KEY-----","input":{"cmd":"-----END PRIVATE KEY-----"}}]}}`},
	} {
		t.Run(tc.id, func(t *testing.T) {
			p := privateEventPrivacy{}
			safe, _ := p.project([]byte(tc.wire))
			if !p.state.ambiguous || strings.Contains(string(safe), body) {
				t.Error("F2 untrusted-pair=false")
			}
			events := append([]byte(tc.wire+"\n"), privateMessage(body)...)
			hotfixReviewSinks(t, events, "", body)
		})
	}
	// Structural paths distinguish literal member names and array syntax.
	tokens, valid := sourceScalars([]byte(`{"message":{"content":{"[]":{"text":"public"}}}}`))
	if !valid {
		t.Fatal("F2 tokenizer-control=false")
	}
	for _, token := range tokens {
		if token.text == "public" && scalarRole(token, tokens) != "" {
			t.Error("F2 object-array-collision=false")
		}
	}
}

func TestHotfixReviewF3TerminalRewind(t *testing.T) {
	_, body := hotfixReviewKey(t)
	c, store := opencodeFixture(t)
	m, err := NewManager(c, store)
	if err != nil {
		t.Fatal("F3 fixture=false")
	}
	defer m.Close()
	events := terminalStart + terminalText("--") + terminalText(body) + `{"type":"tool_use","part":{"type":"tool","metadata":"BEGIN PRIVATE KEY"}}` + "\n" + terminalStop
	r := seedTerminalRun(t, m, events, "ended")
	ended, final := m.terminalTurn(r)
	if !ended {
		t.Fatal("F3 lifecycle-positive=false")
	}
	if strings.Contains(final, body) {
		t.Error("F3 fallback-no-key=false")
	}
	rows, err := m.Tail(r.Lane, 200)
	if err != nil {
		t.Fatal("F3 tail-entrance=false")
	}
	if strings.Contains(strings.Join(rows, "\n"), body) {
		t.Error("F3 tail-no-key=false")
	}
	// The stopped lifecycle must still reject an invalid session.
	invalid := strings.Replace(events, terminalStop, `{"type":"step_finish","sessionID":"invalid session","part":{"type":"step-finish","reason":"stop"}}`+"\n", 1)
	if os.WriteFile(filepath.Join(store, "runs", r.ID, "events.jsonl"), []byte(invalid), 0600) != nil {
		t.Fatal("F3 invalid-session-fixture=false")
	}
	if ended, _ := m.terminalTurn(r); ended {
		t.Error("F3 session-guard=false")
	}
	public := terminalStart + terminalText("public-final") + `{"type":"tool_use","part":{"type":"tool","metadata":"--"}}` + "\n" + `{"type":"tool_use","part":{"type":"tool","metadata":"BEGIN PRIVATE KEY"}}` + "\n" + terminalStop
	if os.WriteFile(filepath.Join(store, "runs", r.ID, "events.jsonl"), []byte(public), 0600) != nil {
		t.Fatal("F3 public-fixture=false")
	}
	if ended, final := m.terminalTurn(r); !ended || final != "public-final" {
		t.Error("F3 before-pending-preservation=false")
	}
}

// Keep encoding/json in the fixture control: escaped member names decode to
// the same untrusted member, and must not acquire structural identity.
func TestHotfixReviewEscapedCollision(t *testing.T) {
	_, body := hotfixReviewKey(t)
	wire := strings.ReplaceAll(`{"type":"text","part/text":"-----BEGIN PRIVATE KEY-----","part":{"text":"-----END PRIVATE KEY-----"}}`, "part/text", `part\u002ftext`)
	if !json.Valid([]byte(wire)) {
		t.Fatal("F2 escaped-fixture=false")
	}
	hotfixReviewSinks(t, append([]byte(wire+"\n"), privateMessage(body)...), "", body)
}
