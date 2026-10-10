package nuntius

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestActionViewsRealFixtures(t *testing.T) {
	m, c, store := manager(t)
	seedHistory(t, m, 1)
	r, err := m.latest("claude")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("testdata/action-events.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "runs", r.ID, "events.jsonl"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.ReportsDir, r.GoalID+".txt"), []byte("done"), 0600); err != nil {
		t.Fatal(err)
	}
	tail, err := m.Tail("claude", 200)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(tail, "\n")
	for _, want := range []string{"go test ./...", "edit /workspace/example.go", "write /workspace/example.go", "read /workspace/example.go", "ToolSearch select:gograph", "gograph-idp-oss.gograph_source Status", "node_repl.js inspect();", "Agent inspect the change", "add /workspace/example.go", "web_search select:gograph"} {
		if !strings.Contains(text, want) {
			t.Fatalf("action fixture missing summary for %s", want)
		}
	}
	if strings.Contains(text, "tool:") || strings.Contains(text, "Second line.") || strings.Contains(text, "go vet") {
		t.Fatal("bare tools or multiline action")
	}
	v, err := call(m, "nuntius_tail", []byte(`{"lane":"claude","lines":200}`))
	if err != nil || toolText(v) != toolText(tail) {
		t.Fatal("MCP tail summary parity")
	}
	cliFixture(t, c, store)
	var out, diagnostic bytes.Buffer
	if CLI(context.Background(), []string{"watch", "claude"}, strings.NewReader(""), &out, &diagnostic) != 0 || !strings.Contains(out.String(), "edit /workspace/example.go") {
		t.Fatal("watch action summary")
	}
	w := &Web{manager: m}
	web, err := w.data()
	if err != nil || !strings.Contains(toolText(web), "edit /workspace/example.go") {
		t.Fatal("web tail action summary")
	}
}

func TestActionSummaryInputsAndSafety(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{`{"type":"tool_use","name":"Bash","input":{"command":"go test ./...\ngo vet ./..."}}`, "go test ./..."},
		{`{"type":"tool_use","name":"NotebookEdit","input":{"notebook_path":"notes.ipynb","new_source":"hidden"}}`, "edit notebook notes.ipynb"},
		{`{"type":"tool_use","name":"MultiEdit","input":{"file_path":"a.go","edits":[{"new_string":"hidden"}]}}`, "edit a.go"},
		{`{"type":"tool_use","name":"Grep","input":{"pattern":"TODO","path":"src"}}`, "Grep TODO src"},
		{`{"type":"tool_use","name":"Glob","input":{"pattern":"*.go","path":"src"}}`, "Glob *.go src"},
		{`{"type":"tool_use","name":"WebFetch","input":{"url":"https://example.org/docs","prompt":"hidden"}}`, "WebFetch https://example.org/docs"},
		{`{"type":"tool_use","name":"WebSearch","input":{"query":"release docs"}}`, "WebSearch release docs"},
		{`{"type":"tool_use","name":"Task","input":{"description":"check docs","prompt":"hidden"}}`, "Task check docs"},
		{`{"type":"tool_use","name":"mcp__graph__source","input":{"intention":"hidden","symbol":"Status"}}`, "graph.source Status"},
		{`{"type":"mcp_tool_call","server":"graph","tool":"source","arguments":{"symbol":"Status"},"result":"hidden"}`, "graph.source Status"},
		{`{"type":"file_change","changes":[{"path":"a.go","kind":"add"},{"path":"b.go","kind":"update"},{"path":"c.go","kind":"delete"}],"output":"hidden"}`, "add a.go; update b.go; delete c.go"},
		{`{"type":"reasoning","text":"Checking this.\nNext line."}`, "Checking this."},
		{`{"type":"agent_message","text":"Checking this.\nNext line."}`, "Checking this."},
		{`{"type":"future_action","id":"internal","label":"inspect docs","output":"hidden","result":"hidden"}`, "future_action inspect docs"},
		{`{"type":"tool_use","name":"FutureTool","input":{"z":"last","a":"first"},"output":"hidden"}`, "FutureTool first"},
		{`{"type":"command_execution","command":"password=synthetic-sensitive\ngo test ./...","aggregated_output":"hidden"}`, "[REDACTED]"},
		{`{"type":"command_execution","command":"echo sk-synthetic-placeholder"}`, "echo [REDACTED]"},
		{`{"type":"tool_use","name":"Bash","input":{"description":"inspect docs"}}`, "Bash inspect docs"},
		{`{"type":"mcp_tool_call","server":"graph","tool":"source","arguments":"{\"symbol\":\"Status\"}"}`, "graph.source Status"},
	} {
		var v map[string]any
		if json.Unmarshal([]byte(tc.input), &v) != nil {
			t.Fatal("fixture invalid")
		}
		if got := actionSummary(v); got != tc.want {
			t.Fatalf("summary predicate failed: expected %s", tc.want)
		}
	}
	for _, value := range []string{strings.Repeat("世", 150), "go test\r\nsecond", "go\t test\x1b[31m", "-----BEGIN PRIVATE KEY-----\nsynthetic material\n-----END PRIVATE KEY-----"} {
		got := actionSummary(map[string]any{"type": "command_execution", "command": value})
		if len([]rune(got)) > 120 || strings.ContainsAny(got, "\n\r\t\x1b") || strings.Contains(got, "synthetic material") {
			t.Fatal("summary safety/bound predicate")
		}
	}
}

func TestCurrentActionClock(t *testing.T) {
	m, _, _ := manager(t)
	now := time.Date(2026, 10, 10, 1, 4, 0, 0, time.UTC)
	start := []byte(`{"type":"item.started","timestamp":"2026-10-10T01:03:30Z","item":{"type":"command_execution","id":"cmd","command":"go test ./..."}}` + "\n")
	a := m.currentAction("clock", start, now, now)
	if a == nil || a.Summary != "go test ./..." || a.AgeSeconds != 30 || a.RunningSeconds == nil || *a.RunningSeconds != 30 || a.AgeSource != "event" {
		t.Fatal("timestamped running command")
	}
	completed := append(append([]byte{}, start...), []byte(`{"type":"item.completed","timestamp":"2026-10-10T01:03:50Z","item":{"type":"command_execution","id":"cmd","command":"go test ./...","aggregated_output":"hidden"}}`+"\n")...)
	a = m.currentAction("clock", completed, now, now)
	if a == nil || a.RunningSeconds != nil || a.AgeSeconds != 10 {
		t.Fatal("completion clears running duration")
	}
	untimed := []byte(`{"type":"item.started","item":{"type":"command_execution","id":"cmd","command":"go test ./..."}}` + "\n")
	a = m.currentAction("untimed", untimed, now.Add(-5*time.Second), now)
	if a.AgeSeconds != 5 || a.AgeSource != "observed" {
		t.Fatal("untimed initial estimate")
	}
	withResult := append(append([]byte{}, untimed...), []byte(`{"type":"result","result":"hidden"}`+"\n")...)
	a = m.currentAction("untimed", withResult, now.Add(20*time.Second), now.Add(20*time.Second))
	if a.AgeSeconds != 25 || *a.RunningSeconds != 25 {
		t.Fatal("unrelated results must not reset action age")
	}
	claude := []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"bash","name":"Bash","input":{"command":"make verify"}}]}}` + "\n")
	a = m.currentAction("claude", claude, now, now)
	if a == nil || a.RunningSeconds == nil {
		t.Fatal("Claude command start")
	}
	claude = append(claude, []byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"bash","content":"hidden"}]}}`+"\n")...)
	a = m.currentAction("claude", claude, now, now)
	if a == nil || a.RunningSeconds != nil {
		t.Fatal("Claude tool result closes command without exposing output")
	}
}

func TestReadableRunChain(t *testing.T) {
	m, c, store := manager(t)
	base := time.Date(2026, 10, 10, 1, 4, 0, 0, time.Local)
	ended := base.Add(time.Minute)
	ids := []string{"12345678" + strings.Repeat("a", 24), "23456789" + strings.Repeat("b", 24), "34567890" + strings.Repeat("c", 24)}
	runs := []Run{
		{ID: ids[0], Lane: "claude", GoalID: "CHAIN-1", State: "ended", Started: base, Ended: &ended, ContinueRunID: ids[1]},
		{ID: ids[1], Lane: "claude", GoalID: "CHAIN-1", State: "ended", Started: base.Add(2 * time.Minute), Ended: &ended, ParentRunID: ids[0], ContinueRunID: ids[2]},
		{ID: ids[2], Lane: "claude", GoalID: "CHAIN-1", State: "ended", Started: base.Add(3 * time.Minute), Ended: &ended, ParentRunID: ids[1], RecoveryRun: true, ResumeCount: 1},
	}
	for i := range runs {
		d := filepath.Join(store, "runs", runs[i].ID)
		if err := privateDir(d); err != nil {
			t.Fatal(err)
		}
		if err := m.save(&runs[i]); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := m.History(HistoryFilter{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := writeHistory(&out, rows); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"RUN", "RELATION", "first run", "continue step of the 01:04 run", "resumed 1/2 after interruption at 01:05", "resumed by the 01:07 run"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("readable relation missing: %s", want)
		}
	}
	if strings.Contains(out.String(), "PREVIOUS") || strings.Contains(out.String(), ids[0]) {
		t.Fatal("raw relationship IDs exposed")
	}
	for _, row := range rows {
		if row.ChainID != ids[0] || row.ChainDepth < 0 {
			t.Fatal("chain ancestry")
		}
	}
	cliFixture(t, c, store)
	var diagnostic bytes.Buffer
	for _, flags := range [][]string{{"status"}, {"status", "--long"}, {"history"}} {
		out.Reset()
		if CLI(context.Background(), flags, strings.NewReader(""), &out, &diagnostic) != 0 || strings.Contains(out.String(), ids[2]) {
			t.Fatal("human IDs or long flag")
		}
	}
	for _, name := range []string{"nuntius_status", "nuntius_history"} {
		v, err := call(m, name, []byte(`{}`))
		if err != nil || strings.Contains(toolText(v), ids[2]) {
			t.Fatal("MCP full ID exposed")
		}
	}
	out.Reset()
	if CLI(context.Background(), []string{"history", "--json"}, strings.NewReader(""), &out, &diagnostic) != 0 || !strings.Contains(out.String(), ids[2]) {
		t.Fatal("JSON full IDs lost")
	}
}

func TestCurrentActionSurfaces(t *testing.T) {
	m, c, store := manager(t)
	seedHistory(t, m, 1)
	r, err := m.latest("claude")
	if err != nil {
		t.Fatal(err)
	}
	r.State = "running"
	r.Ended = nil
	r.PID = os.Getpid()
	r.ProcessStart = ""
	// This test process represents an unverified live record. It cannot be
	// signalled or attached, and no real agent or owner process is involved.
	if err := m.save(r); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().UTC().Add(-30 * time.Second).Format(time.RFC3339Nano)
	v := map[string]any{"type": "item.started", "timestamp": stamp, "item": map[string]any{"type": "command_execution", "id": "cmd", "command": "go test ./..."}}
	b, _ := json.Marshal(v)
	path := filepath.Join(store, "runs", r.ID, "events.jsonl")
	if err := os.WriteFile(path, append(b, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	view, err := m.StatusView()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := m.writeStatus(&out, view, true); err != nil || !strings.Contains(out.String(), "go test ./...") || !strings.Contains(out.String(), "running for 30s") {
		t.Fatal("long status current action")
	}
	mcp, err := call(m, "nuntius_status", []byte(`{}`))
	if err != nil || !strings.Contains(toolText(mcp), `"current_action"`) || !strings.Contains(toolText(mcp), "go test ./...") || strings.Contains(toolText(mcp), r.ID) {
		t.Fatal("MCP current action and short ID")
	}
	w := &Web{manager: m}
	data, err := w.data()
	if err != nil || !strings.Contains(toolText(data), `"current_action"`) || !strings.Contains(toolText(data), "go test ./...") || strings.Contains(toolText(data), r.ID) {
		t.Fatal("web current action and short ID")
	}
	cliFixture(t, c, store)
	out.Reset()
	var diagnostic bytes.Buffer
	if CLI(context.Background(), []string{"status", "--long"}, strings.NewReader(""), &out, &diagnostic) != 0 || !strings.Contains(out.String(), "go test ./...") {
		t.Fatal("status long flag action")
	}
	for _, flags := range [][]string{{"status", "--invalid"}, {"status", "extra"}} {
		out.Reset()
		if CLI(context.Background(), flags, strings.NewReader(""), &out, &diagnostic) != 2 || out.Len() != 0 {
			t.Fatal("invalid status flags accepted")
		}
	}
	// Finish the fixture without invoking cancellation or supervision.
	r.State = "ended"
	ended := time.Now()
	r.Ended = &ended
	if err := m.save(r); err != nil {
		t.Fatal(err)
	}
	view, err = m.StatusView()
	if err != nil || strings.Contains(toolText(view), `"current_action"`) {
		t.Fatal("ended lane retains a current action")
	}
}
