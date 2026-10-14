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
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func reviewKey(t *testing.T) (string, string) {
	t.Helper()
	pub, key, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal("K01 generation")
	}
	der, e := x509.MarshalPKCS8PrivateKey(key)
	if e != nil {
		t.Fatal("K01 encode")
	}
	parsed, e := x509.ParsePKCS8PrivateKey(der)
	if e != nil {
		t.Fatal("K01 validity")
	}
	msg := []byte("offline-fixture-validity")
	if !ed25519.Verify(pub, msg, ed25519.Sign(parsed.(ed25519.PrivateKey), msg)) {
		t.Fatal("K01 signature")
	}
	full := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	body := strings.Split(full, "\n")[1]
	return full, body
}
func TestReviewPublicClosedThenOrphan(t *testing.T) {
	full, body := reviewKey(t)
	text := "public-before\n" + full + "public-between\n-----END PRIVATE KEY-----\n" + body
	plain := Redact(text)
	w := (&Manager{}).eventWindow("P03", privateMessage(text), false)
	for _, output := range []string{plain, w.final} {
		if output != privateHidden || strings.Contains(output, body) {
			t.Error("P03/public-orphan-split=false")
		}
	}
}
func TestReviewAncillaryPublicSinks(t *testing.T) {
	full, body := reviewKey(t)
	m, c, store := manager(t)
	seedHistory(t, m, 1)
	r, _ := m.latest("claude")
	r.Model = full
	r.Effort = full
	if m.save(r) != nil {
		t.Fatal("S15 metadata fixture")
	}
	if os.WriteFile(filepath.Join(store, "runs", r.ID, "goal.txt"), []byte(full), 0600) != nil {
		t.Fatal("S15 goal fixture")
	}
	c.OversightPath = filepath.Join(t.TempDir(), "OVERSIGHT.md")
	m.config.OversightPath = c.OversightPath
	if os.WriteFile(c.OversightPath, []byte(full), 0600) != nil {
		t.Fatal("S15 oversight fixture")
	}
	if m.AddOwnerNote(full, "claude") != nil {
		t.Fatal("S15 note fixture")
	}
	if m.Freeze(full) != nil {
		t.Fatal("S15 freeze fixture")
	}
	if m.audit("nuntius_status", "claude", r.GoalID, "", "pass", full) != nil {
		t.Fatal("S15 audit fixture")
	}
	notes, e := m.OwnerNotes(time.Time{})
	if e != nil {
		t.Fatal("S15 notes entrance")
	}
	agents, e := m.Agents()
	if e != nil {
		t.Fatal("S15 agents entrance")
	}
	audit, e := m.Audit("")
	if e != nil {
		t.Fatal("S15 audit entrance")
	}
	outputs := map[string]string{"notes": toolText(notes), "agents": toolText(agents), "audit": toolText(audit)}
	raw, e := os.ReadFile(filepath.Join(store, "audit.jsonl"))
	if e != nil {
		t.Fatal("S15 audit-file entrance")
	}
	outputs["audit-storage"] = string(raw)
	w, e := NewWeb(m, "127.0.0.1:0")
	if e != nil {
		t.Fatal("S15 web fixture")
	}
	req := httptest.NewRequest("GET", "http://127.0.0.1:0/data", nil)
	req.Header.Set("X-Nuntius-Token", w.token)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatal("S15 mux entrance")
	}
	outputs["data-goal-notes-oversight-freeze"] = rec.Body.String()
	for id, text := range outputs {
		if strings.Contains(text, body) {
			t.Errorf("S15/%s no-key=false", id)
		}
	}
}
func TestReviewOwnedChildReaping(t *testing.T) {
	cmd := exec.Command(fakeBinary)
	cmd.Dir = t.TempDir()
	cmd.Stdin = strings.NewReader("wait")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if cmd.Start() != nil {
		t.Fatal("CH03/start=false")
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	pid := cmd.Process.Pid
	start, e := processStart(pid)
	positive := e == nil && sameProcess(Run{PID: pid, ProcessStart: start})
	t.Logf("CH03/identity-positive=%t", positive)
	if !positive {
		t.Fatal("CH03/control=false")
	}
	if cmd.Process.Kill() != nil {
		t.Fatal("CH03/kill=false")
	}
	e = cmd.Wait()
	gone := e != nil && !sameProcess(Run{PID: pid, ProcessStart: start})
	t.Logf("CH04/owned-reaped=%t", gone)
	if !gone {
		t.Error("CH04/reaped=false")
	}
}
func TestReviewChildCensus(t *testing.T) {
	cmd := exec.Command(fakeBinary)
	cmd.Dir = t.TempDir()
	cmd.Stdin = strings.NewReader("wait")
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if cmd.Start() != nil {
		t.Fatal("CH01/start=false")
	}
	pid := cmd.Process.Pid
	// Always reap this exact owned child, including unavailable-census paths.
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	start, e := processStart(pid)
	if e != nil {
		t.Error("CH01/identity=false")
		return
	}
	if !sameProcess(Run{PID: pid, ProcessStart: start}) {
		t.Error("CH01/identity-positive=false")
		return
	}
	// args shows the executed path on both BSD and Linux ps; Linux comm is only
	// the 15-character task name.
	table, e := exec.Command("/bin/ps", "-axo", "pid=,ppid=,args=").Output()
	if e != nil {
		t.Log("CH01/census-available=false")
		return
	}
	found := false
	for _, line := range strings.Split(string(table), "\n") {
		if strings.Contains(line, fakeBinary) && strings.HasPrefix(strings.TrimSpace(line), fmt.Sprint(pid)+" ") {
			found = true
		}
	}
	t.Logf("CH01/census-positive=%t", found)
	if !found {
		t.Error("CH01/control=false")
		return
	}
	if cmd.Process.Kill() != nil || cmd.Wait() == nil {
		t.Error("CH01/reaped=false")
		return
	}
	table, e = exec.Command("/bin/ps", "-axo", "pid=,ppid=,args=").Output()
	if e != nil {
		t.Log("CH02/census-available=false")
		return
	}
	survivors := false
	for _, line := range strings.Split(string(table), "\n") {
		if strings.Contains(line, os.Getenv("TMPDIR")) && strings.Contains(line, "fake-agent") {
			survivors = true
		}
	}
	t.Logf("CH02/survivors=%t", survivors)
	if survivors {
		t.Error("CH02/no-survivors=false")
	}
}
func TestReviewTerminalRewind(t *testing.T) {
	_, body := reviewKey(t)
	c, store := opencodeFixture(t)
	m, e := NewManager(c, store)
	if e != nil {
		t.Fatal("T01 fixture")
	}
	defer m.Close()
	// A recognized fragment after a body joins a pending delimiter from before it.
	events := terminalStart + terminalText("--") + terminalText(body) + `{"type":"tool_use","part":{"type":"tool","metadata":"BEGIN PRIVATE KEY"}}` + "\n" + terminalStop
	r := seedTerminalRun(t, m, events, "ended")
	ended, final := m.terminalTurn(r)
	if !ended {
		t.Fatal("T01 lifecycle-positive=false")
	}
	exposed := strings.Contains(final, body)
	t.Logf("T01/terminal-fallback exposed=%t", exposed)
	if exposed {
		t.Error("T01/shared-rewind=false")
	}
	rows, e := m.Tail(r.Lane, 200)
	if e != nil {
		t.Fatal("T01 tail entrance")
	}
	t.Logf("T01/tail exposed=%t", strings.Contains(strings.Join(rows, "\n"), body))
}
func TestReviewJSONCollisionPublicSinks(t *testing.T) {
	_, body := reviewKey(t)
	m, _, store := manager(t)
	seedHistory(t, m, 1)
	r, _ := m.latest("claude")
	data := []byte(`{"type":"text","part/text":"-----BEGIN PRIVATE KEY-----","part":{"text":"-----END PRIVATE KEY-----"}}` + "\n")
	data = append(data, privateMessage(body)...)
	path := filepath.Join(store, "runs", r.ID, "events.jsonl")
	if os.WriteFile(path, data, 0600) != nil {
		t.Fatal("J04 fixture")
	}
	tail, e := m.Tail("claude", 200)
	if e != nil {
		t.Fatal("J04 entrance")
	}
	outputs := map[string]string{"tail": strings.Join(tail, "\n"), "current-action": actionText(m.currentAction(r.ID, data, time.Time{}, time.Now()))}
	w, e := NewWeb(m, "127.0.0.1:0")
	if e != nil {
		t.Fatal("J04 web")
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "http://127.0.0.1:0/data", nil)
	req.Header.Set("X-Nuntius-Token", w.token)
	w.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatal("J04 mux entrance")
	}
	outputs["real-mux-data"] = rec.Body.String()
	input := "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{}}\n{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}\n" + `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"nuntius_tail","arguments":{"lane":"claude","lines":200}}}` + "\n"
	var out bytes.Buffer
	if Serve(context.Background(), m, strings.NewReader(input), &out) != nil {
		t.Fatal("J04 MCP entrance")
	}
	outputs["mcp-tail"] = out.String()
	for _, window := range m.eventWindows {
		outputs["cache"] += window.final + window.latest.Summary + strings.Join(window.tail, "\n")
	}
	for id, text := range outputs {
		exposed := strings.Contains(text, body)
		t.Logf("J04/%s exposed=%t", id, exposed)
		if exposed {
			t.Errorf("J04/%s no-key=false", id)
		}
	}
}
func TestReviewWindowLifecycleAndTrust(t *testing.T) {
	full, body := reviewKey(t)
	path := filepath.Join(t.TempDir(), "events.jsonl")
	write := func(b []byte) {
		if os.WriteFile(path, b, 0600) != nil {
			t.Fatal("W00 fixture")
		}
	}
	check := func(id string) {
		b, e := readWindow(path, true)
		if e != nil {
			t.Fatal(id + "/entrance")
		}
		w := (&Manager{}).eventWindow(id, b, false)
		if strings.Contains(w.final+w.latest.Summary+strings.Join(w.tail, "\n"), body) {
			t.Error(id + "/no-key=false")
		}
	}
	data := append(privateMessage("-----BEGIN PRIVATE KEY-----"), privateMessage(strings.Repeat("padding ", 30000))...)
	for len(data) < 2<<20 {
		data = append(data, privateMessage(body)...)
	}
	write(data)
	check("W01")
	data = append(data, privateMessage("-----END PRIVATE KEY-----\npublic-progress")...)
	write(data)
	check("W02")
	write(privateMessage(full))
	check("W03")
	replacement := path + ".rotation"
	if os.WriteFile(replacement, privateMessage(full), 0600) != nil || os.Rename(replacement, path) != nil {
		t.Fatal("W04 fixture")
	}
	check("W04")
	// Same-size rewrite changes digest and must not inherit public parsed content.
	public := privateMessage(strings.Repeat("p", len(full)))
	write(public)
	check("W05")
	write(privateMessage(full))
	check("W06")
	// Over-budget prefix and oversized physical line both fail closed.
	data = append(privateMessage("-----BEGIN PRIVATE KEY-----"), privateMessage(strings.Repeat("p", 2<<20))...)
	data = append(data, privateMessage(body)...)
	write(data)
	check("W07")
	f, e := os.OpenFile(path, os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal("W08 fixture")
	}
	if f.Truncate((64<<20)+1) != nil {
		t.Fatal("W08 truncate")
	}
	_ = f.Close()
	check("W08")
	// Untrusted symlink is refused with a fixed reason.
	link := path + ".link"
	if os.Symlink(path, link) != nil {
		t.Fatal("W09 fixture")
	}
	b, e := readWindow(link, true)
	if e == nil || bytes.Contains(b, []byte(body)) || strings.Contains(e.Error(), body) {
		t.Error("W09/file-trust=false")
	}
	// Public complete events survive incomplete EOF; incomplete key data never enters parsing.
	write(append(privateMessage("public-progress"), []byte(fmt.Sprintf(`{"type":"text","part":{"text":"-----BEGIN PRIVATE KEY-----\n%s`, body))...))
	b, e = readSourceSnapshot(path)
	sealed := proofWire(approveEvents(b, false))
	if e != nil || !bytes.Contains(sealed, []byte("public-progress")) || bytes.Contains(sealed, []byte(body)) {
		t.Error("W10/incomplete=false")
	}
}
func TestReviewMCPKeysErrorsAndSelectors(t *testing.T) {
	full, body := reviewKey(t)
	v := map[string]any{full: full, "nested": []any{map[string]any{full: full}}}
	encoded, _ := json.Marshal(safeValue(v))
	if bytes.Contains(encoded, []byte(body)) {
		t.Error("S12/map-keys-values=false")
	}
	m, _, _ := manager(t)
	input := "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{}}\n{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}\n"
	for i, name := range []string{"nuntius_status", "nuntius_report", "nuntius_tail", "nuntius_history"} {
		input += fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":{"reveal":true,%q:%q}}}`+"\n", i+2, name, full, full)
	}
	var out bytes.Buffer
	if Serve(context.Background(), m, strings.NewReader(input), &out) != nil {
		t.Fatal("S13 entrance")
	}
	if strings.Contains(out.String(), body) || !strings.Contains(out.String(), `"isError":true`) {
		t.Error("S13/errors-selectors=false")
	}
	w, e := NewWeb(m, "127.0.0.1:0")
	if e != nil {
		t.Fatal("S14 entrance")
	}
	for _, route := range []string{"/reveal", "/raw", "/data?reveal=true", "/data?raw=true"} {
		req := httptest.NewRequest("GET", "http://127.0.0.1:0"+route, nil)
		req.Header.Set("X-Nuntius-Token", w.token)
		rec := httptest.NewRecorder()
		w.ServeHTTP(rec, req)
		if strings.Contains(rec.Body.String(), body) {
			t.Error("S14/selector-leak=true")
		}
		if route == "/reveal" || route == "/raw" {
			if rec.Code != 404 {
				t.Error("S14/raw-route=true")
			}
		}
	}
}
func TestReviewJSONPathCollision(t *testing.T) {
	_, body := reviewKey(t)
	for _, tc := range []struct{ id, data string }{
		{"J01", fmt.Sprintf(`{"type":"text","part/text":"-----BEGIN PRIVATE KEY-----","part":{"text":"-----END PRIVATE KEY-----\n%s"}}`, body)},
		{"J02", fmt.Sprintf(`{"type":"item.completed","item/text":"-----BEGIN PRIVATE KEY-----","item":{"type":"agent_message","text":"-----END PRIVATE KEY-----\n%s"}}`, body)},
		{"J03", fmt.Sprintf(`{"type":"assistant","message/content/[]/text":"-----BEGIN PRIVATE KEY-----","message":{"content":[{"type":"text","text":"-----END PRIVATE KEY-----\n%s"}]}}`, body)},
	} {
		t.Run(tc.id, func(t *testing.T) {
			p := privateEventPrivacy{}
			safe, _ := p.project([]byte(tc.data))
			exposed := strings.Contains(string(safe), body)
			t.Logf("%s/projection exposed=%t", tc.id, exposed)
			if exposed || !p.state.ambiguous {
				t.Error(tc.id + "/unrelated-fields-fail-closed=false")
			}
		})
	}
}
func TestReviewGrammarAndInputMutations(t *testing.T) {
	full, body := reviewKey(t)
	cases := []struct {
		id, text string
		known    bool
	}{
		{"U01", strings.ToLower(full), true}, // body case mutation changes identity; marker grammar is still recognized.
		{"U02", strings.ReplaceAll(full, " ", "\u2003"), true},
		{"U03", strings.ReplaceAll(full, " ", "\u00a0"), true},
		{"U04", strings.ReplaceAll(full, "BEGIN", "B\u200bEGIN"), false},
		{"U05", strings.ReplaceAll(strings.ReplaceAll(full, "BEGIN", "B\u202eEGIN"), "END", "E\u202eND"), false},
		{"U06", strings.ReplaceAll(full, "-", "\u2212"), false},
		{"U07", strings.ReplaceAll(full, "PRIVATE", "PRIVАTE"), true}, // Cyrillic A: malformed candidate recognized from BEGIN.
		{"U08", strings.ReplaceAll(full, "BEGIN", "\xffBEGIN"), true},
		{"U09", "-----BE\n" + body + "\n-----END PRIVATE KEY-----", true},
		{"U10", "-----BEGIN PRIVATE KEY-----\n" + body + "\n-----END EC PRIVATE KEY-----\n-----END PRIVATE KEY-----", true},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			output := Redact(tc.text)
			exposed := strings.Contains(output, body)
			if tc.id == "U01" {
				exposed = strings.Contains(output, strings.ToLower(body))
			}
			t.Logf("%s/plain recognized=%t exposed=%t", tc.id, tc.known, exposed)
			if tc.known && exposed {
				t.Error(tc.id + "/recognized-no-key=false")
			}
		})
	}
	// Independent JSON spelling, key/value order, cross-event and field mutations.
	for i, wire := range [][]byte{
		privateMessage(full),
		[]byte(strings.ReplaceAll(string(privateMessage(full)), "-", `\u002d`)),
		[]byte(strings.ReplaceAll(string(privateMessage(full)), "PRIVATE", `PRIV\u0041TE`)),
		[]byte(fmt.Sprintf(`{"type":"text","part":{"text":%q},"marker":%q}`+"\n", body, "-----BEGIN PRIVATE KEY-----")),
		append(append(privateMessage("-----BEGIN PRIVATE KEY-----"), privateMessage(body)...), privateMessage("-----END PRIVATE KEY-----")...),
		append(append(privateMessage("--"), privateMessage("BEGIN PRIVATE KEY")...), privateMessage(body)...),
		[]byte(fmt.Sprintf(`{"type":"text","part":{"text":"-----BEGIN PRIVATE KEY-----\n%s"`+"\n", body)),
	} {
		// Case 3 is explicitly public prefix before its first marker, tracked rather than a grammar failure.
		window := (&Manager{}).eventWindow(fmt.Sprint(i), wire, false)
		text := window.final + window.latest.Summary + strings.Join(window.tail, "\n")
		exposed := strings.Contains(text, body)
		t.Logf("M%02d/event exposed=%t", i, exposed)
		if i != 3 && exposed {
			t.Errorf("M%02d/recognized-no-key=false", i)
		}
	}
	// Byte/rune cuts through a recognized opening marker, within decoded fields and across events.
	for cut := 1; cut < len("-----BEGIN PRIVATE KEY-----"); cut++ {
		header := "-----BEGIN PRIVATE KEY-----"
		wire := append(append(privateMessage(header[:cut]), privateMessage(header[cut:])...), privateMessage(body)...)
		window := (&Manager{}).eventWindow(fmt.Sprint(cut), wire, false)
		if strings.Contains(window.final+window.latest.Summary+strings.Join(window.tail, "\n"), body) {
			t.Errorf("CUT%02d/no-key=false", cut)
		}
	}
}
func TestReviewCacheLifecycle(t *testing.T) {
	full, body := reviewKey(t)
	m := &Manager{}
	private := privateMessage(full)
	public := privateMessage("public-progress")
	inputs := [][]byte{private, private, append(bytes.Clone(private), public...), public, private, privateMessage(strings.Repeat("x", len(full)))}
	for i, data := range inputs {
		w := m.eventWindow("lane-a", data, false)
		if strings.Contains(w.final+w.latest.Summary+strings.Join(w.tail, "\n"), body) {
			t.Errorf("C%02d/no-key=false", i)
		}
	}
	for i := 0; i < 70; i++ {
		m.eventWindow(fmt.Sprintf("lane-%d", i), public, false)
	}
	w := m.eventWindow("lane-a", private, false)
	if strings.Contains(w.final+strings.Join(w.tail, "\n"), body) {
		t.Error("C70/eviction-no-key=false")
	}
	w = m.eventWindow("lane-b", public, false)
	if w.latest.Summary != "public-progress" {
		t.Error("C71/lane-isolation=false")
	}
}
func reviewSinks(t *testing.T, text, body string, eventData ...[]byte) map[string]bool {
	t.Helper()
	m, c, store := manager(t)
	seedHistory(t, m, 1)
	r, e := m.latest("claude")
	if e != nil || r == nil {
		t.Fatal("S00 entrance")
	}
	path := filepath.Join(store, "runs", r.ID, "events.jsonl")
	data := privateMessage(text)
	if len(eventData) > 0 {
		data = eventData[0]
	}
	if os.WriteFile(filepath.Join(c.ReportsDir, r.GoalID+".txt"), []byte(text), 0600) != nil || os.WriteFile(path, data, 0600) != nil {
		t.Fatal("S00 fixture")
	}
	outputs := map[string]string{}
	report, e := m.Report(r.GoalID)
	if e != nil {
		t.Fatal("S01 entrance")
	}
	outputs["report"] = report
	tail, e := m.Tail("claude", 200)
	if e != nil {
		t.Fatal("S02 entrance")
	}
	outputs["tail"] = strings.Join(tail, "\n")
	b, e := readWindow(path, true)
	if e != nil {
		t.Fatal("S03 entrance")
	}
	a := m.currentAction(r.ID, b, time.Time{}, time.Now())
	outputs["current-action"] = actionText(a)
	status, e := m.StatusView()
	if e != nil {
		t.Fatal("S04 entrance")
	}
	outputs["status-json"] = toolText(status)
	var human bytes.Buffer
	if m.writeStatus(&human, status, true) != nil {
		t.Fatal("S05 entrance")
	}
	outputs["status-human"] = human.String()
	h, e := m.History(HistoryFilter{Limit: 20})
	if e != nil {
		t.Fatal("S06 entrance")
	}
	outputs["history-json"] = toolText(h)
	human.Reset()
	if writeHistory(&human, h) != nil {
		t.Fatal("S07 entrance")
	}
	outputs["history-human"] = human.String()
	w, e := NewWeb(m, "127.0.0.1:0")
	if e != nil {
		t.Fatal("S08 entrance")
	}
	req := httptest.NewRequest("GET", "http://127.0.0.1:0/data", nil)
	req.Header.Set("X-Nuntius-Token", w.token)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatal("S08 entrance")
	}
	outputs["real-mux-data"] = rec.Body.String()
	rec = httptest.NewRecorder()
	w.menubarHandler().ServeHTTP(rec, httptest.NewRequest("GET", "http://127.0.0.1:0/status", nil))
	if rec.Code != 200 {
		t.Fatal("S09 entrance")
	}
	outputs["menu-status"] = rec.Body.String()
	input := "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{}}\n{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}\n"
	for i, name := range []string{"nuntius_status", "nuntius_tail", "nuntius_report", "nuntius_history"} {
		args := "{}"
		if name == "nuntius_tail" {
			args = `{"lane":"claude","lines":200}`
		}
		if name == "nuntius_report" {
			args = fmt.Sprintf(`{"goal_id":%q}`, r.GoalID)
		}
		input += fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s}}`+"\n", i+2, name, args)
	}
	var wire bytes.Buffer
	if Serve(context.Background(), m, strings.NewReader(input), &wire) != nil {
		t.Fatal("S10 entrance")
	}
	outputs["mcp-wire"] = wire.String()
	var messages []map[string]any
	for _, line := range bytes.Split(wire.Bytes(), []byte("\n")) {
		var v map[string]any
		if json.Unmarshal(line, &v) == nil {
			messages = append(messages, v)
		}
	}
	for i, v := range messages {
		if result, ok := v["result"].(map[string]any); ok {
			for _, field := range []string{"content", "structuredContent"} {
				if val, yes := result[field]; yes {
					encoded, _ := json.Marshal(val)
					outputs[fmt.Sprintf("mcp-%d-%s", i, field)] = string(encoded)
				}
			}
		}
	}
	// Explicit fixture paths keep CLI away from the owner's configuration and store.
	config := filepath.Join(t.TempDir(), "config.json")
	encoded, _ := json.Marshal(c)
	if os.WriteFile(config, encoded, 0600) != nil {
		t.Fatal("S11 fixture")
	}
	t.Setenv("HOME", t.TempDir())
	for _, args := range [][]string{{"status", "--long"}, {"status", "--json"}, {"history"}, {"history", "--json"}, {"watch", "claude"}} {
		var out, errOut bytes.Buffer
		code := CLI(context.Background(), append([]string{"--config", config, "--store", store}, args...), strings.NewReader(""), &out, &errOut)
		if code != 0 {
			t.Fatal("S11 entrance")
		}
		outputs["cli-"+strings.Join(args, "-")] = out.String() + errOut.String()
	}
	// Retained safe snapshots and caches are inspected in memory.
	for id, window := range m.eventWindows {
		outputs["cache-public-"+id.runID] = window.final + window.terminalFinal + window.latest.Summary + window.command.Summary + window.activity + window.effort + strings.Join(window.tail, "\n")
		for _, line := range window.lines {
			outputs["cache-lines-"+id.runID] += line.final
			for _, a := range line.actions {
				outputs["cache-lines-"+id.runID] += a.event.Summary + a.event.Text
			}
		}
	}
	leaks := map[string]bool{}
	for id, output := range outputs {
		leaks[id] = strings.Contains(output, body)
	}
	return leaks
}
func TestReviewOrphanPrefix(t *testing.T) {
	_, body := reviewKey(t)
	text := "public-before\n" + body + "\n-----END PRIVATE KEY-----\nprivate-after"
	for id, leak := range reviewSinks(t, text, body) {
		t.Logf("K01/%s exposed=%t", id, leak)
		if leak {
			t.Errorf("K01/%s no-key=false", id)
		}
	}
}
func TestReviewCanonicalSinks(t *testing.T) {
	full, body := reviewKey(t)
	for _, tc := range []struct{ id, text string }{
		{"K02", "public-before\n" + full + "public-after"},
		{"K03", "public-before\n-----END PRIVATE KEY-----\n" + body},
		{"K04", "public-before\n-----BEGIN PRIVATE KEY-----\n" + body},
		{"K05", "public-before\n-----BEGIN PRIVATE KEY-----\n" + body + "\n-----END RSA PRIVATE KEY-----\npublic-after"},
		{"K06", "public-before\n-----BEGIN PRIVATE KEY-----\n-----BEGIN EC PRIVATE KEY-----\n" + body + "\n-----END EC PRIVATE KEY-----\n-----END PRIVATE KEY-----"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			for id, leak := range reviewSinks(t, tc.text, body) {
				if leak {
					t.Errorf("%s/%s no-key=false", tc.id, id)
				}
			}
		})
	}
}
func TestReviewPreservation(t *testing.T) {
	text := "alpha\tβ\r\n-----BEGIN PRIVATE KEY-----\nfixture-body\n-----END PRIVATE KEY-----\nomega\tγ"
	got := Redact(text)
	if !strings.HasPrefix(got, "alpha\tβ\r\n") || !strings.HasSuffix(got, "\nomega\tγ") || strings.Contains(got, "fixture-body") {
		t.Error("P01 exact-outside=false")
	}
	// An unrelated public event before the first orphan stays public.
	data := append(privateMessage("public-progress"), privateMessage("-----END PRIVATE KEY-----\nfixture-body")...)
	window := (&Manager{}).eventWindow("offline", data, false)
	if !strings.Contains(strings.Join(window.tail, "\n"), "public-progress") || strings.Contains(window.final+strings.Join(window.tail, "\n"), "fixture-body") {
		t.Error("P02 orphan-prefix-preservation=false")
	}
}
