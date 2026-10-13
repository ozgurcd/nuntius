package nuntius

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func privateMessage(text string) []byte {
	b, _ := json.Marshal(map[string]any{"type": "item.completed", "item": map[string]any{"type": "agent_message", "text": text}})
	return append(b, '\n')
}

func assertPrivateOutputs(t *testing.T, m *Manager, path, body string) {
	t.Helper()
	for range 2 { // Cold/rebuilt and digest-reused windows.
		rows, err := m.Tail("claude", 50)
		if err != nil || strings.Contains(strings.Join(rows, "\n"), body) {
			t.Error("private body reached Tail or tail read failed")
		}
		b, err := readWindow(path, true)
		if err != nil {
			t.Fatal("bounded read failed")
		}
		for _, a := range streamActions(b) {
			if strings.Contains(a.Summary, body) {
				t.Error("private body reached uncached actions")
				break
			}
		}
		w, err := NewWeb(m, "127.0.0.1:0")
		if err != nil {
			t.Fatal("web fixture failed")
		}
		request := httptest.NewRequest("GET", "http://127.0.0.1:0/data", nil)
		request.Header.Set("X-Nuntius-Token", w.token)
		out := httptest.NewRecorder()
		w.ServeHTTP(out, request)
		if out.Code != 200 || strings.Contains(out.Body.String(), body) {
			t.Error("private body reached authenticated data or request failed")
		}
		for _, window := range m.eventWindows {
			if strings.Contains(window.final+strings.Join(window.tail, "\n")+window.latest.Summary, body) {
				t.Error("private body retained in window")
			}
			for _, line := range window.lines {
				if strings.Contains(line.final, body) {
					t.Error("private final retained")
					break
				}
				for _, a := range line.actions {
					if a.event.Text != "" || strings.Contains(a.event.Summary, body) {
						t.Error("private action retained")
						break
					}
				}
			}
		}
	}
}

// Port of the final review's B1 observation, with a privacy oracle rather
// than an assertion that the leak still exists. All bodies are synthetic.
func TestPrivateBoundaryDisplaced(t *testing.T) {
	for _, closed := range []bool{false, true} {
		for _, warm := range []bool{false, true} {
			t.Run(strings.Join([]string{map[bool]string{false: "open", true: "closed"}[closed], map[bool]string{false: "cold", true: "warm"}[warm]}, "/"), func(t *testing.T) {
				m, c, store := manager(t)
				seedHistory(t, m, 1)
				r, err := m.latest("claude")
				if err != nil || r == nil {
					t.Fatal("run fixture failed")
				}
				if os.WriteFile(filepath.Join(c.ReportsDir, r.GoalID+".txt"), []byte("done"), 0600) != nil {
					t.Fatal("report fixture failed")
				}
				path := filepath.Join(store, "runs", r.ID, "events.jsonl")
				body := "synthetic-private-boundary-body"
				data := append(privateMessage("-----BEGIN PRIVATE KEY-----"), privateMessage(body)...)
				if warm {
					if os.WriteFile(path, data, 0600) != nil {
						t.Fatal("event fixture failed")
					}
					assertPrivateOutputs(t, m, path, body)
				}
				for len(data) < 2<<20 {
					data = append(data, privateMessage(body)...)
				}
				if closed {
					data = append(data, privateMessage("-----END PRIVATE KEY-----")...)
				}
				if os.WriteFile(path, data, 0600) != nil {
					t.Fatal("large event fixture failed")
				}
				assertPrivateOutputs(t, m, path, body)
			})
		}
	}
}

func TestPrivateMarkerVariants(t *testing.T) {
	body := "synthetic-private-variant-body"
	commandMarker := []byte(`{"type":"item.completed","item":{"type":"command_execution","command":"prefix -----BEGIN PRIVATE KEY-----"}}` + "\n")
	for _, tc := range []struct {
		name  string
		steps [][]byte
	}{
		{"not-line-start", [][]byte{commandMarker, privateMessage(body), privateMessage("-----END PRIVATE KEY-----")}},
		{"split-appends", [][]byte{privateMessage("-----BE"), privateMessage("GIN PRIVATE KEY-----"), privateMessage(body), privateMessage("-----END PRI"), privateMessage("VATE KEY-----")}},
		{"whitespace", [][]byte{privateMessage("----- BEGIN\tRSA  PRIVATE\nKEY -----"), privateMessage(body), privateMessage("----- END RSA PRIVATE KEY -----")}},
		{"json-string", [][]byte{[]byte(`{"type":"item.completed","item":{"type":"command_execution","command":"\u002d\u002d\u002d\u002d\u002dBEGIN PRIVATE KEY-----"}}` + "\n"), privateMessage(body), privateMessage("-----END PRIVATE KEY-----")}},
		{"end-without-begin", [][]byte{privateMessage("---"), privateMessage(body), privateMessage("-----END PRIVATE KEY-----")}},
		{"begin-without-end", [][]byte{privateMessage("-----BEGIN PRIVATE KEY"), privateMessage(body)}},
		{"partial-end", [][]byte{privateMessage("-----BEGIN PRIVATE KEY-----"), privateMessage("-----END PRIVATE KEY"), privateMessage(body)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, c, store := manager(t)
			seedHistory(t, m, 1)
			r, err := m.latest("claude")
			if err != nil || r == nil {
				t.Fatal("run fixture failed")
			}
			if os.WriteFile(filepath.Join(c.ReportsDir, r.GoalID+".txt"), []byte("done"), 0600) != nil {
				t.Fatal("report fixture failed")
			}
			path := filepath.Join(store, "runs", r.ID, "events.jsonl")
			m.mu.Lock()
			m.eventWindow(r.ID, privateMessage(body), false)
			m.mu.Unlock() // A formerly public cached line must obey its new context.
			var data []byte
			for i, step := range tc.steps {
				data = append(data, step...)
				if os.WriteFile(path, data, 0600) != nil {
					t.Fatal("event fixture failed")
				}
				// An orphan END can establish privacy only once it arrives.
				if tc.name != "end-without-begin" || i == len(tc.steps)-1 {
					assertPrivateOutputs(t, m, path, body)
				}
			}
		})
	}
}

func TestPrivateTextVariants(t *testing.T) {
	body := "synthetic-private-text-body"
	for _, text := range []string{
		"----- BEGIN PRIVATE KEY -----\n" + body,
		"-----BEGIN PRIVATE KEY\n" + body,
		body + "\n-----END PRIVATE KEY-----",
		"-----BEGIN PRIVATE KEY-----\n-----END PRIVATE KEY\n" + body,
	} {
		if strings.Contains(Redact(text), body) {
			t.Error("private body reached redacted text")
		}
		if strings.Contains(terminalMessage(text), body) {
			t.Error("private body reached terminal text")
		}
	}
}

func TestPrivateTerminalFallback(t *testing.T) {
	for _, marker := range []string{"-----BEGIN PRIVATE KEY-----", "-----BE", "-----END PRIVATE KEY-----", "orphan-after"} {
		c, store := opencodeFixture(t)
		m, err := NewManager(c, store)
		if err != nil {
			t.Fatal("manager fixture failed")
		}
		t.Cleanup(func() { _ = m.Close() })
		body := "synthetic-private-terminal-body"
		text := terminalText(marker) + terminalText(body)
		if marker == "orphan-after" {
			text = terminalText(body) + terminalText("-----END PRIVATE KEY-----")
		}
		r := seedTerminalRun(t, m, terminalStart+text+terminalStop, "ended")
		if terminal, final := m.terminalTurn(r); !terminal || strings.Contains(final, body) {
			t.Error("terminal fallback privacy or classification failed")
		}
		rows, err := m.Tail(r.Lane, 50)
		if err != nil || strings.Contains(strings.Join(rows, "\n"), body) {
			t.Error("private terminal body reached Tail")
		}
	}
}

func TestPrivatePrefixControls(t *testing.T) {
	for _, prefix := range []string{"", strings.Repeat("safe", 20000), strings.Repeat("a", 32766) + "-----BE", strings.Repeat("a", 32765) + `\u002d`} {
		want := !strings.Contains(prefix, "---") && !strings.Contains(prefix, `\u002d`)
		if publicEventPrefix(strings.NewReader(prefix), int64(len(prefix))) != want {
			t.Error("omitted prefix or read-buffer boundary misclassified")
		}
	}
	if publicEventPrefix(strings.NewReader("safe"), 64<<20+1) || publicEventPrefix(strings.NewReader("short"), 10) {
		t.Error("unknown or over-budget context treated as public")
	}
	// Large marker-free histories still expose the newest public action.
	m, c, store := manager(t)
	seedHistory(t, m, 1)
	r, err := m.latest("claude")
	if err != nil || r == nil {
		t.Fatal("run fixture failed")
	}
	if os.WriteFile(filepath.Join(c.ReportsDir, r.GoalID+".txt"), []byte("done"), 0600) != nil {
		t.Fatal("report fixture failed")
	}
	data := strings.Repeat(string(privateMessage("ordinary action")), 24000) + string(privateMessage("newest public action"))
	if os.WriteFile(filepath.Join(store, "runs", r.ID, "events.jsonl"), []byte(data), 0600) != nil {
		t.Fatal("large public fixture failed")
	}
	rows, err := m.Tail("claude", 1)
	if err != nil || strings.Join(rows, "\n") != "newest public action" {
		t.Error("marker-free public tail lost")
	}
	// A JSON event split across writes stays invisible until its newline;
	// completing the BEGIN then appending a body must retain privacy.
	path := filepath.Join(store, "runs", r.ID, "events.jsonl")
	marker := privateMessage("-----BEGIN PRIVATE KEY-----")
	if os.WriteFile(path, marker[:len(marker)/2], 0600) != nil {
		t.Fatal("partial JSON fixture failed")
	}
	assertPrivateOutputs(t, m, path, "synthetic-split-json-body")
	if os.WriteFile(path, append(marker, privateMessage("synthetic-split-json-body")...), 0600) != nil {
		t.Fatal("completed JSON fixture failed")
	}
	assertPrivateOutputs(t, m, path, "synthetic-split-json-body")
}
