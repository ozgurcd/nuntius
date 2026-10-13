package nuntius

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRound3WholeScopeSinks(t *testing.T) {
	full, body := reviewKey(t)
	half := len(body) / 2
	for _, tc := range []struct{ id, text string }{
		{"plain-prefix-orphan", "before\n" + body + "\n-----END PRIVATE KEY-----\nafter"},
		{"plain-closed-orphan-pair", "before\n" + body + "\n" + full + "between\n-----END PRIVATE KEY-----\n" + full + "after"},
		{"plain-open-pair", "before\n" + body + "\n-----BEGIN PRIVATE KEY-----\n" + full + "after"},
		{"plain-mismatch-pair", "before\n" + body + "\n-----BEGIN EC PRIVATE KEY-----\n-----END RSA PRIVATE KEY-----\n" + full + "after"},
		{"plain-closed-partial", "before\n" + body + "\n" + full + "between\n-----END PRI\nafter"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			if Redact(tc.text) != privateHidden {
				t.Error("whole segment=false")
			}
			for sink, exposed := range reviewSinks(t, tc.text, body) {
				if exposed {
					t.Errorf("%s no-key=false", sink)
				}
			}
		})
	}
	for _, tc := range []struct{ id, wire string }{
		{"event-split-fields", fmt.Sprintf(`{"type":"text","part":{"type":"text","text":%q},"other":%q,"orphan":"-----END PRIVATE KEY-----","after":"public-after"}`, body[:half], body[half:])},
		{"event-closed-orphan-pair", fmt.Sprintf(`{"type":"text","part":{"type":"text","text":%q},"metadata":%q,"orphan":"-----END PRIVATE KEY-----","later":%q,"after":"public-after"}`, body, full, full)},
		{"event-escaped-key", fmt.Sprintf(`{"type":"text","part":{"type":"text","text":%q},"\u002d\u002d\u002d\u002d\u002dEND PRIVATE KEY\u002d\u002d\u002d\u002d\u002d":"after"}`, body)},
		{"event-slash-key", fmt.Sprintf(`{"type":"text","part\/text":%q,"part":{"type":"text","text":%q},"orphan":"-----END PRIVATE KEY-----"}`, full, body)},
		{"event-bracket-key", fmt.Sprintf(`{"type":"assistant","message/content/[]/text":%q,"message":{"content":[{"type":"text","text":%q}]},"orphan":"-----END PRIVATE KEY-----"}`, full, body)},
		{"event-open-after-closed", fmt.Sprintf(`{"type":"text","part":{"type":"text","text":%q},"metadata":%q,"other":"-----BEGIN PRIVATE KEY-----","after":"public-after"}`, body, full)},
		{"event-end-before-begin", fmt.Sprintf(`{"type":"text","part":{"type":"text","text":%q},"end":"-----END PRIVATE KEY-----","begin":"-----BEGIN PRIVATE KEY-----","after":"public-after"}`, body)},
	} {
		t.Run(tc.id, func(t *testing.T) {
			wire := []byte(tc.wire + "\n")
			p := privateEventPrivacy{}
			safe, _ := p.project(wire)
			if string(safe) != privateHidden {
				t.Error("whole event=false")
			}
			probes := []string{body}
			if tc.id == "event-split-fields" {
				probes = []string{body[:half], body[half:]}
			}
			for _, probe := range probes {
				for sink, exposed := range reviewSinks(t, "", probe, wire) {
					if exposed {
						t.Errorf("%s no-key=false", sink)
					}
				}
			}
		})
	}
}

func TestRound3EventBoundaries(t *testing.T) {
	_, body := reviewKey(t)
	half := len(body) / 2
	earlier := privateMessage("public λ\t\r\n" + body[:half])
	current := privateMessage(body[half:] + "\n-----END PRIVATE KEY-----\npublic-after")
	later := privateMessage("-----BEGIN PRIVATE KEY-----\nfixture\n-----END PRIVATE KEY-----\nlater")
	result := privateEventProjection(append(append(bytes.Clone(earlier), current...), later...), privateEventPrivacy{})
	if !bytes.Equal(result[0], bytes.TrimSuffix(earlier, []byte("\n"))) || string(result[1]) != privateHidden || string(result[2]) != privateHidden {
		t.Error("event scope or irreversible uncertainty=false")
	}
	for sink, exposed := range reviewSinks(t, "", body[half:], append(bytes.Clone(earlier), current...)) {
		if exposed {
			t.Errorf("%s final piece hidden=false", sink)
		}
	}
	prior := privateMessage("earlier complete public λ\t\r\n")
	pending := privateMessage("same-event before\n--")
	data := append(append(append(bytes.Clone(prior), pending...), privateMessage(body)...), privateMessage("END PRIVATE KEY")...)
	result = privateEventProjection(data, privateEventPrivacy{})
	if !bytes.Equal(result[0], bytes.TrimSuffix(prior, []byte("\n"))) {
		t.Error("rewind crosses complete prior event=false")
	}
	for _, event := range result[1 : len(result)-1] {
		if string(event) != privateHidden {
			t.Error("pending scope hidden whole=false")
		}
	}
	for sink, exposed := range reviewSinks(t, "", body, data) {
		if exposed {
			t.Errorf("%s rewind hidden=false", sink)
		}
	}
	// Bare trailing dashes are pending under the established stream grammar.
	// Record whether this conservative boundary expands on an orphan; no claim
	// that a pending delimiter event is an earlier complete public event.
	trailing := privateMessage("ordinary progress -")
	result = privateEventProjection(append(bytes.Clone(trailing), privateMessage("-----END PRIVATE KEY-----")...), privateEventPrivacy{})
	t.Logf("pending-trailing-dash prior-hidden=%t", string(result[0]) == privateHidden)
}

func TestRound3LegacyP03OwnerRule(t *testing.T) {
	full, body := reviewKey(t)
	text := "public-before\n" + full + "public-between\n-----END PRIVATE KEY-----\n" + body
	if Redact(text) != privateHidden {
		t.Error("whole segment=false")
	}
	p := privateEventPrivacy{}
	safe, _ := p.project(privateMessage(text))
	if string(safe) != privateHidden {
		t.Error("whole event=false")
	}
}

func TestRound3BudgetEarlierPiece(t *testing.T) {
	_, body := reviewKey(t)
	for _, wire := range []string{
		fmt.Sprintf(`{"type":"text","part":{"type":"text","text":%q},%q:"-----END PRIVATE KEY-----"}`, body, strings.Repeat("m", 128)),
		fmt.Sprintf(`{"type":"text","part":{"type":"text","text":%q},"other":%q,"orphan":"-----END PRIVATE KEY-----"}`, body, strings.Repeat("x", (1<<20)+1)),
	} {
		p := privateEventPrivacy{}
		safe, _ := p.project([]byte(wire))
		if string(safe) != privateHidden {
			t.Error("budget whole scope=false")
		}
		for sink, exposed := range reviewSinks(t, "", body, []byte(wire+"\n")) {
			if exposed {
				t.Errorf("%s budget hidden=false", sink)
			}
		}
	}
}

func TestRound3OriginalCollisionAllSinks(t *testing.T) {
	_, body := reviewKey(t)
	for _, tc := range []struct{ id, wire string }{
		{"J01", fmt.Sprintf(`{"type":"text","part/text":"-----BEGIN PRIVATE KEY-----","part":{"text":"-----END PRIVATE KEY-----\n%s"}}`, body)},
		{"J02", fmt.Sprintf(`{"type":"item.completed","item/text":"-----BEGIN PRIVATE KEY-----","item":{"type":"agent_message","text":"-----END PRIVATE KEY-----\n%s"}}`, body)},
		{"J03", fmt.Sprintf(`{"type":"assistant","message/content/[]/text":"-----BEGIN PRIVATE KEY-----","message":{"content":[{"type":"text","text":"-----END PRIVATE KEY-----\n%s"}]}}`, body)},
		{"J04", `{"type":"text","part/text":"-----BEGIN PRIVATE KEY-----","part":{"text":"-----END PRIVATE KEY-----"}}`},
	} {
		t.Run(tc.id, func(t *testing.T) {
			data := []byte(tc.wire + "\n")
			if tc.id == "J04" {
				data = append(data, privateMessage(body)...)
			}
			for sink, exposed := range reviewSinks(t, tc.wire, body, data) {
				t.Logf("%s/%s exposed=%t", tc.id, sink, exposed)
				if exposed {
					t.Errorf("%s no-key=false", sink)
				}
			}
		})
	}
}

func TestRound3OriginalTerminalAllSinks(t *testing.T) {
	_, body := reviewKey(t)
	c, store := opencodeFixture(t)
	m, err := NewManager(c, store)
	if err != nil {
		t.Fatal("fixture entrance=false")
	}
	defer m.Close()
	events := terminalStart + terminalText("--") + terminalText(body) + `{"type":"tool_use","part":{"type":"tool","metadata":"BEGIN PRIVATE KEY"}}` + "\n" + terminalStop
	r := seedTerminalRun(t, m, events, "ended")
	ended, final := m.terminalTurn(r)
	if !ended {
		t.Fatal("terminal positive control=false")
	}
	outputs := map[string]string{"terminal": final}
	rows, err := m.Tail(r.Lane, 200)
	if err != nil {
		t.Fatal("tail entrance=false")
	}
	outputs["tail"] = strings.Join(rows, "\n")
	status, err := m.StatusView()
	if err != nil {
		t.Fatal("status entrance=false")
	}
	outputs["status-json"] = toolText(status)
	var human bytes.Buffer
	if m.writeStatus(&human, status, true) != nil {
		t.Fatal("status entrance=false")
	}
	outputs["status-human"] = human.String()
	web, err := NewWeb(m, "127.0.0.1:0")
	if err != nil {
		t.Fatal("web entrance=false")
	}
	request := httptest.NewRequest("GET", "http://127.0.0.1:0/data", nil)
	request.Header.Set("X-Nuntius-Token", web.token)
	rec := httptest.NewRecorder()
	web.ServeHTTP(rec, request)
	if rec.Code != 200 {
		t.Fatal("data entrance=false")
	}
	outputs["data"] = rec.Body.String()
	var input strings.Builder
	input.WriteString("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{}}\n{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}\n")
	for i, name := range []string{"nuntius_status", "nuntius_tail", "nuntius_report", "nuntius_history"} {
		args := "{}"
		if name == "nuntius_tail" {
			args = fmt.Sprintf(`{"lane":%q,"lines":200}`, r.Lane)
		}
		if name == "nuntius_report" {
			args = fmt.Sprintf(`{"goal_id":%q}`, r.GoalID)
		}
		fmt.Fprintf(&input, `{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s}}`+"\n", i+2, name, args)
	}
	var output bytes.Buffer
	if Serve(context.Background(), m, strings.NewReader(input.String()), &output) != nil {
		t.Fatal("MCP entrance=false")
	}
	outputs["mcp-text-wire"] = output.String()
	config := filepath.Join(t.TempDir(), "config.json")
	encoded, _ := json.Marshal(c)
	if os.WriteFile(config, encoded, 0600) != nil {
		t.Fatal("CLI fixture=false")
	}
	for _, args := range [][]string{{"status", "--json"}, {"status", "--long"}, {"history", "--json"}, {"watch", r.Lane}} {
		var stdout, stderr bytes.Buffer
		if CLI(context.Background(), append([]string{"--config", config, "--store", store}, args...), strings.NewReader(""), &stdout, &stderr) != 0 {
			t.Fatal("CLI entrance=false")
		}
		outputs["cli-"+strings.Join(args, "-")] = stdout.String() + stderr.String()
	}
	rows, err = m.Tail(r.Lane, 200)
	if err != nil {
		t.Fatal("warm cache entrance=false")
	}
	outputs["warm-tail"] = strings.Join(rows, "\n")
	for _, window := range m.eventWindows {
		outputs["cache"] += window.final + window.latest.Summary + strings.Join(window.tail, "\n")
		for _, line := range window.lines {
			outputs["cache-lines"] += line.final
			for _, action := range line.actions {
				outputs["cache-lines"] += action.event.Text + action.event.Summary
			}
		}
	}
	for sink, value := range outputs {
		exposed := strings.Contains(value, body)
		t.Logf("T01/%s exposed=%t", sink, exposed)
		if exposed {
			t.Errorf("%s no-key=false", sink)
		}
	}
}

func FuzzRound3ScopePreservation(f *testing.F) {
	f.Add([]byte("public λ\t\r\n"), uint8(0))
	f.Add([]byte{0, 1, 255}, uint8(1))
	f.Fuzz(func(t *testing.T, data []byte, mode uint8) {
		if len(data) > 512 {
			return
		}
		prefix := fmt.Sprintf("public:%x λ\t\r\n", data)
		suffix := fmt.Sprintf("\npublic-after:%x\tλ", data)
		block := "-----BEGIN PRIVATE KEY-----\nfixture\n-----END PRIVATE KEY-----"
		complete := prefix + block + suffix
		if Redact(complete) != prefix+privateHidden+suffix {
			t.Error("complete outside scope bytes=false")
		}
		uncertain := []string{"-----END PRIVATE KEY-----", "-----BEGIN PRIVATE KEY-----", "-----BE", "-----BEGIN EC PRIVATE KEY-----\n-----END RSA PRIVATE KEY-----"}[mode%4]
		segment := prefix + block + suffix + "\n" + uncertain + "\n" + block + suffix
		if Redact(segment) != privateHidden {
			t.Error("whole uncertain segment=false")
		}
		// Format original wire in differing valid member order, indentation and
		// escapes; the earlier complete event must survive as exact wire bytes.
		earlier := []byte(fmt.Sprintf(` { "type" : "text", "part" : {"text":%q,"type":"text"},"public":"\u03bb" } `, prefix))
		current := []byte(fmt.Sprintf(`{"type":"text","part":{"type":"text","text":%q},"closed":%q,"orphan":"-----END PRIVATE KEY-----","after":%q}`, prefix, block, suffix))
		projected := privateEventProjection(append(append(bytes.Clone(earlier), '\n'), append(current, '\n')...), privateEventPrivacy{})
		if !bytes.Equal(projected[0], earlier) || string(projected[1]) != privateHidden {
			t.Error("earlier event bytes or current whole scope=false")
		}
	})
}

func TestRound3PendingAfterClosedBlock(t *testing.T) {
	full, body := reviewKey(t)
	pendingText := full + "public-before-fragment\n--"
	data := append(append(privateMessage(pendingText), privateMessage("BEGIN PRIVATE KEY")...), privateMessage(body)...)
	data = append(data, privateMessage("-----END PRIVATE KEY-----")...)
	projected := privateEventProjection(data, privateEventPrivacy{})
	for i, event := range projected[:len(projected)-1] {
		exposed := bytes.Contains(event, []byte(body))
		t.Logf("CP01/event-%d exposed=%t", i, exposed)
		if exposed {
			t.Errorf("CP01/event-%d no-key=false", i)
		}
	}
	for sink, exposed := range reviewSinks(t, "", body, data) {
		t.Logf("CP01/%s exposed=%t", sink, exposed)
		if exposed {
			t.Errorf("CP01/%s no-key=false", sink)
		}
	}
	c, store := opencodeFixture(t)
	m, err := NewManager(c, store)
	if err != nil {
		t.Fatal("terminal fixture=false")
	}
	defer m.Close()
	events := terminalStart + terminalText(pendingText) + terminalText("BEGIN PRIVATE KEY") + terminalText(body) + terminalText("-----END PRIVATE KEY-----") + terminalStop
	r := seedTerminalRun(t, m, events, "ended")
	ended, final := m.terminalTurn(r)
	if !ended {
		t.Fatal("CP01/terminal-positive=false")
	}
	exposed := strings.Contains(final, body)
	t.Logf("CP01/terminal exposed=%t", exposed)
	if exposed {
		t.Error("CP01/terminal no-key=false")
	}
}

func TestRound3PendingClosedStopMetadata(t *testing.T) {
	full, body := reviewKey(t)
	c, store := opencodeFixture(t)
	m, err := NewManager(c, store)
	if err != nil {
		t.Fatal("fixture entrance=false")
	}
	defer m.Close()
	events := terminalStart + terminalText(full+"public-before-fragment\n--") + `{"type":"tool_use","part":{"type":"tool","metadata":"BEGIN PRIVATE KEY"}}` + "\n" + terminalText(body) + `{"type":"step_finish","part":{"type":"step-finish","reason":"stop","metadata":"-----END PRIVATE KEY-----"}}` + "\n"
	r := seedTerminalRun(t, m, events, "ended")
	ended, final := m.terminalTurn(r)
	if !ended {
		t.Fatal("CP02 terminal positive control=false")
	}
	outputs := map[string]string{"terminal": final}
	rows, err := m.Tail(r.Lane, 200)
	if err != nil {
		t.Fatal("tail entrance=false")
	}
	outputs["tail"] = strings.Join(rows, "\n")
	web, err := NewWeb(m, "127.0.0.1:0")
	if err != nil {
		t.Fatal("web entrance=false")
	}
	request := httptest.NewRequest("GET", "http://127.0.0.1:0/data", nil)
	request.Header.Set("X-Nuntius-Token", web.token)
	rec := httptest.NewRecorder()
	web.ServeHTTP(rec, request)
	if rec.Code != 200 {
		t.Fatal("data entrance=false")
	}
	outputs["data"] = rec.Body.String()
	input := "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{}}\n{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}\n" + fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"nuntius_tail","arguments":{"lane":%q,"lines":200}}}`+"\n", r.Lane)
	var output bytes.Buffer
	if Serve(context.Background(), m, strings.NewReader(input), &output) != nil {
		t.Fatal("MCP entrance=false")
	}
	outputs["mcp-tail-text-wire"] = output.String()
	config := filepath.Join(t.TempDir(), "config.json")
	encoded, _ := json.Marshal(c)
	if os.WriteFile(config, encoded, 0600) != nil {
		t.Fatal("CLI fixture=false")
	}
	var stdout, stderr bytes.Buffer
	if CLI(context.Background(), []string{"--config", config, "--store", store, "watch", r.Lane}, strings.NewReader(""), &stdout, &stderr) != 0 {
		t.Fatal("CLI entrance=false")
	}
	outputs["cli-watch"] = stdout.String() + stderr.String()
	for sink, value := range outputs {
		exposed := strings.Contains(value, body)
		t.Logf("CP02/%s exposed=%t", sink, exposed)
		if exposed {
			t.Errorf("CP02/%s no-key=false", sink)
		}
	}
}
