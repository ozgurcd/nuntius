package nuntius

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func opencodeFixture(t *testing.T) (Config, string) {
	t.Helper()
	c, store := fixture(t)
	c.Lanes = c.Lanes[:1]
	c.Lanes[0].Name, c.Lanes[0].Kind = "opencode-1", "opencode"
	b, _ := json.Marshal(c.Lanes[0])
	var fields map[string]any
	_ = json.Unmarshal(b, &fields)
	fields["model"] = "opencode/fixture-free"
	b, _ = json.Marshal(fields)
	_ = json.Unmarshal(b, &c.Lanes[0])
	return c, store
}

func TestOpenCodeConfigAndArgv(t *testing.T) {
	c, _ := opencodeFixture(t)
	if _, err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	l := c.Lanes[0]
	for _, model := range []string{"", "bare", "/model", "provider/", "-flag/model", "provider/model\x1b", "provider/model\n", strings.Repeat("x", 129) + "/model"} {
		c.Lanes[0].Model = model
		if _, err := c.Validate(); err == nil {
			t.Fatal("unsafe model accepted")
		}
		c.Pools = map[string]AgentPool{"opencode": {Size: 0, Template: Lane{Binary: l.Binary, Workdir: l.Workdir, Model: model}}}
		if _, err := c.Validate(); err == nil {
			t.Fatal("disabled unsafe model accepted")
		}
		c.Pools = nil
	}
	c.Lanes[0] = l
	l.ExtraArgs = []string{"--agent", "build", "--variant=low"}
	got := strings.Join(agentArgs(l, "ses_fixture"), "|")
	if got != "run|--format|json|--model|opencode/fixture-free|--agent|build|--variant=low|--session|ses_fixture" {
		t.Fatalf("unexpected argv structure: %q", got)
	}
	for _, args := range [][]string{{"--auto"}, {"--pure"}, {"--session", "other"}, {"--model", "paid/model"}, {"--attach", "http://localhost"}, {"--interactive"}, {"--permission-mode", "auto"}, {"--agent"}, {"--variant", "-flag"}} {
		c.Lanes[0].ExtraArgs = args
		if _, err := c.Validate(); err == nil {
			t.Fatalf("unsafe flags accepted: %v", args)
		}
	}
	c.Lanes[0] = l
	c.Lanes[0].StartMode = "goal_then_continue"
	if _, err := c.Validate(); err == nil {
		t.Fatal("automatic continuation accepted")
	}
}

func TestOpenCodePoolsAndViews(t *testing.T) {
	c, store := opencodeFixture(t)
	p := filepath.Join(c.ReportsDir, "lanes.json")
	b, _ := json.Marshal(c)
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	for _, cmd := range [][]string{{"config", "check"}, {"config", "convert"}, {"set", "opencode-agent-pool", "2"}} {
		if CLI(context.Background(), append([]string{"--config", p, "--store", store}, cmd...), strings.NewReader(""), &out, &errOut) != 0 {
			t.Fatal(errOut.String())
		}
	}
	c, err := LoadConfig(p)
	if err != nil || len(c.Lanes) != 2 || c.Lanes[1].Name != "opencode-2" {
		t.Fatal("pool expansion failed", err)
	}
	m, err := NewManager(c, store)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	agents, _ := m.Agents()
	s, _ := m.Status()
	for _, v := range []any{agents, s} {
		b, _ := json.Marshal(v)
		if !strings.Contains(string(b), `"model":"opencode/fixture-free"`) || !strings.Contains(string(b), `"kind":"opencode"`) {
			t.Fatal("missing model/kind")
		}
	}
	for _, cmd := range []string{"status", "agents"} {
		out.Reset()
		if CLI(context.Background(), []string{"--config", p, "--store", store, cmd}, strings.NewReader(""), &out, &errOut) != 0 || !strings.Contains(out.String(), "opencode/fixture-free") {
			t.Fatal("human view missing model", errOut.String())
		}
	}
}

func TestOpenCodeEventSchema(t *testing.T) {
	b, err := os.ReadFile("testdata/opencode-events.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	actions := streamActions(b)
	var summaries []string
	for _, a := range actions {
		summaries = append(summaries, a.Summary)
	}
	got := strings.Join(summaries, "\n")
	for _, want := range []string{"go test ./...", "edit src/main.go", "write src/new.go", "patch src/patch.go", "read README.md", "grep TODO src", "glob *.go", "webfetch https://example.org", "docs.lookup nuntius", "future_event useful", "error fixture failure", "step_finish stop", "Final Report"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(got, "must-not-return") {
		t.Fatal("tool result leaked")
	}
	for _, a := range actions {
		if a.At.IsZero() {
			t.Fatal("numeric timestamp lost")
		}
	}
	final, _ := questionSummary(b)
	if !strings.Contains(final, "Final Report") {
		t.Fatal("no-report final text lost")
	}
}

func TestOpenCodeDispatchResumeAndLimits(t *testing.T) {
	c, store := opencodeFixture(t)
	m, err := NewManager(c, store)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	prefix := "\"$() `literal`\n"
	brief := prefix + strings.Repeat("x", 12000-len(prefix))
	id, err := m.Dispatch("opencode-1", "OPENCODE-TEST", brief)
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, m, "opencode-1", "ended_no_report")
	if m.session(id) != "ses_fixture" {
		t.Fatal("missing opencode session")
	}
	input, _ := os.ReadFile(filepath.Join(store, "runs", id, "goal.txt"))
	if string(input) != brief {
		t.Fatal("brief changed")
	}
	id2, err := m.Reply("opencode-1", "reply")
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, m, "opencode-1", "ended_no_report")
	b, _ := os.ReadFile(filepath.Join(store, "runs", id2, "events.jsonl"))
	if !strings.Contains(string(b), `--session`) {
		t.Fatal("resume flag missing")
	}
	rows, err := m.History(HistoryFilter{Limit: 20})
	if err != nil || len(rows) != 2 || rows[0].RunID != id2 {
		t.Fatal("reply history missing", err)
	}
	historyJSON, _ := json.Marshal(rows)
	if !strings.Contains(string(historyJSON), `"model":"opencode/fixture-free"`) {
		t.Fatal("history model missing")
	}
	if _, err = m.Dispatch("opencode-1", "OPENCODE-LIMIT", strings.Repeat("x", 12001)); err == nil {
		t.Fatal("oversized brief accepted")
	}
}

func TestOpenCodeRecoveryAndLaunchd(t *testing.T) {
	for _, mode := range []string{"wait", "wait no-session"} {
		t.Run(mode, func(t *testing.T) {
			c, store := opencodeFixture(t)
			m, err := NewManager(c, store)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			f := &fakeLaunchctl{jobs: map[string]string{}}
			m.launcher = &jobLauncher{domain: "gui/fixture", call: f.call}
			id, err := m.Dispatch("opencode-1", "OPENCODE-RECOVER", mode)
			if err != nil {
				t.Fatal(err)
			}
			waitReady(t, m, "opencode-1")
			r, _ := m.latest("opencode-1")
			if r.LaunchdLabel == "" || r.ProcessStart == "" {
				t.Fatal("launchd identity missing")
			}
			m.Close()
			if err = signalRun(*r, syscall.SIGKILL); err != nil {
				t.Fatal(err)
			}
			for deadline := time.Now().Add(3 * time.Second); sameProcess(*r) && time.Now().Before(deadline); {
				time.Sleep(10 * time.Millisecond)
			}
			next, err := NewManager(c, store)
			if err != nil {
				t.Fatal(err)
			}
			defer next.Close()
			next.mu.Lock()
			next.launcher = m.launcher
			next.mu.Unlock()
			if err = next.Recover(); err != nil {
				t.Fatal(err)
			}
			waitReady(t, next, "opencode-1")
			current, _ := next.latest("opencode-1")
			if current.ID == id || current.ParentRunID != id || current.ResumeCount != 1 || current.LaunchdLabel == r.LaunchdLabel {
				t.Fatal("recovery link/job missing")
			}
			input, _ := os.ReadFile(filepath.Join(store, "runs", current.ID, "goal.txt"))
			if !strings.HasPrefix(string(input), "Previous run ended before report delivery; exit cause unknown at ") || !strings.HasSuffix(string(input), mode) {
				t.Fatal("recovery brief lost")
			}
			b, _ := os.ReadFile(filepath.Join(store, "runs", current.ID, "events.jsonl"))
			if strings.Contains(string(b), "--session") == strings.Contains(mode, "no-session") {
				t.Fatal("wrong session/fresh recovery")
			}
			status, _ := next.Status()
			if status[0].CurrentAction == nil || status[0].CurrentAction.AgeSource != "event" {
				t.Fatal("current action absent")
			}
			for _, tool := range []string{"nuntius_status"} {
				v, err := call(next, tool, json.RawMessage(`{}`))
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(toolText(v), `opencode/fixture-free`) {
					t.Fatal("MCP model missing")
				}
			}
			v, err := (&Web{manager: next}).data()
			if err != nil || !strings.Contains(toolText(v), `opencode/fixture-free`) {
				t.Fatal("web model missing", err)
			}
			if err = next.Cancel("opencode-1"); err != nil {
				t.Fatal(err)
			}
			waitState(t, next, "opencode-1", "ended_no_report")
			rows, err := next.History(HistoryFilter{Limit: 20})
			if err != nil || len(rows) != 2 || rows[0].ParentRunID != id || rows[0].ChainID != id {
				t.Fatal("recovery history chain missing", err)
			}
			f.mu.Lock()
			jobs := len(f.jobs)
			f.mu.Unlock()
			if jobs != 0 {
				t.Fatal("launchd job leaked")
			}
		})
	}
}

func TestOpenCodeActionTimingAndSafety(t *testing.T) {
	m := &Manager{}
	start := []byte(`{"type":"tool_use","timestamp":1000,"sessionID":"ses_fixture","part":{"tool":"bash","callID":"call_fixture","state":{"status":"running","input":{"command":"go test ./..."}}}}` + "\n")
	a := m.currentAction("fixture", start, time.Time{}, time.UnixMilli(5000))
	if a == nil || a.RunningSeconds == nil || *a.RunningSeconds != 4 || a.AgeSeconds != 4 {
		t.Fatal("start age missing")
	}
	end := []byte(`{"type":"tool_use","timestamp":5000,"sessionID":"ses_fixture","part":{"tool":"bash","callID":"call_fixture","state":{"status":"completed","input":{"command":"go test ./..."},"output":"must-not-return"}}}` + "\n")
	a = m.currentAction("fixture", append(start, end...), time.Time{}, time.UnixMilli(6000))
	if a.RunningSeconds != nil || strings.Contains(a.Summary, "must-not-return") {
		t.Fatal("completed command marked running/output leaked")
	}
	if activity(end) != "running tests" {
		t.Fatal("activity classification missing")
	}
	for _, input := range []string{
		`{"type":"text","sessionID":"ses_fixture","part":{"text":"token=fixture-hidden"}}`,
		`{"type":"tool_use","sessionID":"ses_fixture","part":{"tool":"write","state":{"input":{"filePath":"token=fixture-hidden"}}}}`,
		`{"type":"error","sessionID":"ses_fixture","error":{"data":{"message":"token=fixture-hidden"}}}`,
	} {
		for _, action := range eventActions([]byte(input)) {
			if strings.Contains(action.Summary, "fixture-hidden") {
				t.Fatal("redaction failure")
			}
		}
	}
}

func TestOpenCodeCancelTimeoutFreeze(t *testing.T) {
	for _, mode := range []string{"cancel", "timeout", "freeze"} {
		t.Run(mode, func(t *testing.T) {
			c, store := opencodeFixture(t)
			m, err := NewManager(c, store)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			m.Grace = 30 * time.Millisecond
			if mode == "timeout" {
				m.runtime = 200 * time.Millisecond
			}
			_, err = m.Dispatch("opencode-1", "OPENCODE-STOP", "ignore-int")
			if err != nil {
				t.Fatal(err)
			}
			waitReady(t, m, "opencode-1")
			if mode == "cancel" {
				err = m.Cancel("opencode-1")
			}
			if mode == "freeze" {
				err = m.Freeze("fixture")
			}
			if err != nil {
				t.Fatal(err)
			}
			state := "ended_no_report"
			if mode == "timeout" {
				state = "ended"
			}
			waitState(t, m, "opencode-1", state)
			if mode == "freeze" {
				if _, err = m.Dispatch("opencode-1", "FROZEN-TEST", "brief"); err == nil {
					t.Fatal("frozen dispatch accepted")
				}
			}
		})
	}
}
