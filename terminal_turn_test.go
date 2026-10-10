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

const terminalStart = "{\"type\":\"step_start\",\"part\":{\"type\":\"step-start\"}}\n"
const terminalStop = "{\"type\":\"step_finish\",\"part\":{\"type\":\"step-finish\",\"reason\":\"stop\"}}\n"

func terminalText(text string) string {
	b, _ := json.Marshal(map[string]any{"type": "text", "part": map[string]string{"type": "text", "text": text}})
	return string(b) + "\n"
}

func seedTerminalRun(t *testing.T, m *Manager, stream, state string) Run {
	t.Helper()
	r := Run{ID: strings.Repeat("a", 32), Kind: "opencode", Lane: "opencode-1", GoalID: "TERMINAL-TEST", State: state, Started: time.Now().Add(-time.Minute).UTC(), Brief: "wait", PID: 99999999}
	if state == "ended" {
		now := time.Now().UTC()
		r.Ended, r.StopReason = &now, "ended_no_report"
	}
	dir := filepath.Join(m.store, "runs", r.ID)
	if err := privateDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := putPrivate(filepath.Join(dir, "events.jsonl"), []byte(stream)); err != nil {
		t.Fatal(err)
	}
	if err := m.save(&r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestTerminalMissingReportRecovery(t *testing.T) {
	for _, state := range []string{"running", "ended"} {
		t.Run(state, func(t *testing.T) {
			c, store := opencodeFixture(t)
			m, err := NewManager(c, store)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			final := "Final Report — TERMINAL-TEST\nQUESTIONS: (a) delivery is parked; (b) retry manually."
			// A native guard refuses a misspelled/outside-root write. This
			// synthetic tool error is data; nuntius must not bypass the guard.
			denied := "{\"type\":\"tool_use\",\"part\":{\"type\":\"tool\",\"tool\":\"write\",\"state\":{\"status\":\"error\",\"input\":{\"filePath\":\"/outside/identicum/report.txt\"},\"error\":\"outside workspace refused\"}}}\n"
			r := seedTerminalRun(t, m, terminalStart+denied+terminalText(final)+terminalStop, state)
			for range 3 {
				if err := m.Recover(); err != nil {
					t.Fatal(err)
				}
				s, err := m.Status()
				if err != nil || len(s) != 1 || s[0].RunID != r.ID || s[0].State != "ended_no_report" || s[0].ReportExists || s[0].Exit != nil || s[0].Badge != "amber" || !strings.Contains(s[0].DisplayState, "report missing") {
					t.Fatal("terminal report delivery failure was not finished with attention")
				}
			}
			tail, err := m.Tail(r.Lane, 50)
			if err != nil || strings.Join(tail, "\n") != final {
				t.Fatal("final text unavailable")
			}
			rows, err := m.History(HistoryFilter{Limit: 20})
			if err != nil || len(rows) != 1 || rows[0].Outcome == "completed" || rows[0].Badge != "amber" || rows[0].ContinueRunID != "" {
				t.Fatal("history accepted missing delivery or launched child")
			}
			v, err := (&Web{manager: m}).data()
			if err != nil || !strings.Contains(toolText(v), "report missing") || strings.Contains(toolText(v), `"state":"interrupted"`) {
				t.Fatal("dashboard lost attention outcome")
			}
			v, err = call(m, "nuntius_status", json.RawMessage(`{}`))
			if err != nil || !strings.Contains(toolText(v), `"state":"ended_no_report"`) || !strings.Contains(toolText(v), "report missing") {
				t.Fatal("MCP lost attention outcome")
			}
			cliFixture(t, c, store)
			var out, diagnostic bytes.Buffer
			if code := CLI(context.Background(), []string{"status"}, strings.NewReader(""), &out, &diagnostic); code != 0 || !strings.Contains(out.String(), "report missing") {
				t.Fatal("CLI lost attention outcome")
			}
			if err := m.Close(); err != nil {
				t.Fatal(err)
			}
			next, err := NewManager(c, store)
			if err != nil {
				t.Fatal(err)
			}
			defer next.Close()
			if err := next.Recover(); err != nil {
				t.Fatal(err)
			}
			runs, _ := next.history()
			if len(runs) != 1 || runs[0].State != "ended" || runs[0].ResumeCount != 0 || runs[0].WaitingToResume || runs[0].ContinueRunID != "" {
				t.Fatal("restart replayed terminal work")
			}
			signals, _ := next.Signals()
			for _, name := range signals {
				if strings.HasSuffix(name, ".interrupted") || strings.HasSuffix(name, ".resumed") {
					t.Fatal("terminal work emitted crash/recovery signal")
				}
			}
		})
	}
}

func TestTerminalStreamConservativeRecovery(t *testing.T) {
	stop := terminalStart + terminalText("done") + terminalStop
	cases := map[string]string{
		"no-stream": "", "mid-step": terminalStart,
		"later-start":       stop + terminalStart,
		"malformed-prefix":  "not json\n" + stop,
		"malformed-suffix":  stop + "{broken\n",
		"truncated-json":    stop + "{\"type\":",
		"unterminated-stop": strings.TrimSuffix(stop, "\n"),
		"oversized-line":    terminalText(strings.Repeat("x", 1<<20)) + stop,
		"unmatched-finish":  terminalStop,
		"overlapping-steps": terminalStart + stop,
		"tool-calls":        terminalStart + strings.ReplaceAll(terminalStop, "stop", "tool-calls"),
		"top-level-error":   stop + "{\"type\":\"error\"}\n",
		"unknown-event":     stop + "{\"type\":\"future\"}\n",
	}
	for name, stream := range cases {
		t.Run(name, func(t *testing.T) {
			c, store := opencodeFixture(t)
			m, err := NewManager(c, store)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			r := seedTerminalRun(t, m, stream, "running")
			if err := m.Recover(); err != nil {
				t.Fatal(err)
			}
			waitReady(t, m, r.Lane)
			current, _ := m.latest(r.Lane)
			if current.ID == r.ID || current.ParentRunID != r.ID || current.ResumeCount != 1 || !current.RecoveryRun {
				t.Fatal("partial stream suppressed genuine recovery")
			}
			input, err := os.ReadFile(filepath.Join(store, "runs", current.ID, "goal.txt"))
			if err != nil || !strings.HasPrefix(string(input), "Previous run ended before report delivery; exit cause unknown at ") || !strings.HasSuffix(string(input), "wait") {
				t.Fatal("recovery invented a crash cause or lost brief")
			}
		})
	}
}

func TestTerminalStreamWholeFileAndRedaction(t *testing.T) {
	c, store := opencodeFixture(t)
	m, err := NewManager(c, store)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	// A malformed prefix outside the tail must not be hidden by later valid rows.
	filler := strings.Repeat(terminalText(strings.Repeat("x", 2048)), 600)
	r := seedTerminalRun(t, m, "invalid\n"+filler+terminalStart+terminalText("final")+terminalStop, "running")
	if err := m.Recover(); err != nil {
		t.Fatal(err)
	}
	current, _ := m.latest(r.Lane)
	if current.ID == r.ID || !current.RecoveryRun {
		t.Fatal("tail mistaken for a complete stream")
	}
	_ = m.Cancel(r.Lane)
	waitState(t, m, r.Lane, "ended_no_report")
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	// A new store exercises a valid complete stream larger than the tail window.
	c, store = opencodeFixture(t)
	m, err = NewManager(c, store)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	final := "QUESTIONS: delivery parked.\ntoken=fixture-private-value\nhttps://example.test/?code=fixture-code&state=fixture-state\nsessionID=fixture-session"
	final += "\n" + strings.Repeat("explanation ", 70000)
	stopWithPadding := strings.TrimSuffix(terminalStop, "}\n") + ",\"padding\":\"" + strings.Repeat("x", 600000) + "\"}\n"
	r = seedTerminalRun(t, m, filler+terminalStart+terminalText(final)+stopWithPadding, "running")
	if err := m.Recover(); err != nil {
		t.Fatal(err)
	}
	tail, err := m.Tail(r.Lane, 50)
	joined := strings.Join(tail, "\n")
	if err != nil || len(joined) > 8192 || !strings.Contains(joined, "QUESTIONS: delivery parked") || strings.Contains(joined, "fixture-private-value") || strings.Contains(joined, "fixture-code") || strings.Contains(joined, "fixture-state") || strings.Contains(joined, "fixture-session") {
		t.Fatal("terminal text missing or unredacted")
	}
	runs, _ := m.history()
	if len(runs) != 1 || runs[0].State != "ended" || runs[0].StopReason == "completed" {
		t.Fatal("large complete stream replayed or passed delivery")
	}
}

func TestTerminalLiveSupervisionAndReport(t *testing.T) {
	for _, report := range []string{"none", "current", "stale", "symlink"} {
		t.Run(report, func(t *testing.T) {
			c, store := opencodeFixture(t)
			m, err := NewManager(c, store)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			id, err := m.Dispatch("opencode-1", "TERMINAL-LIVE", "terminal-hold")
			if err != nil {
				t.Fatal(err)
			}
			waitReady(t, m, "opencode-1")
			for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
				b, _ := os.ReadFile(filepath.Join(store, "runs", id, "events.jsonl"))
				if strings.Contains(string(b), `"reason":"stop"`) {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err := m.Recover(); err != nil {
				t.Fatal(err)
			}
			s, err := m.Status()
			r, _ := m.latest("opencode-1")
			if err != nil || s[0].State != "running" || r.ID != id || !sameProcess(*r) || r.StopAt != nil {
				t.Fatal("live terminal event released supervised lane")
			}
			if _, err := m.Dispatch(r.Lane, "DUPLICATE-TEST", "brief"); err == nil {
				t.Fatal("live terminal event allowed duplicate")
			}
			path := filepath.Join(c.ReportsDir, r.GoalID+".txt")
			if report != "none" {
				if err := os.WriteFile(path, []byte("report"), 0600); err != nil {
					t.Fatal(err)
				}
				if report == "stale" {
					old := r.Started.Add(-time.Minute)
					if err := os.Chtimes(path, old, old); err != nil {
						t.Fatal(err)
					}
				}
				if report == "symlink" {
					target := path + ".target"
					if err := os.Rename(path, target); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(target, path); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := os.WriteFile(filepath.Join(c.Lanes[0].Workdir, "terminal-release"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			state := "ended_no_report"
			if report == "current" {
				state = "ended"
			}
			s0 := waitState(t, m, r.Lane, state)
			if report == "current" {
				if s0.Outcome != "completed" || !s0.ReportExists {
					t.Fatal("current report did not complete")
				}
			} else if s0.Outcome != "report_delivery_failed" || s0.Badge != "amber" {
				t.Fatal("terminal end was not classified independently of report freshness")
			}
			if err := m.Recover(); err != nil {
				t.Fatal(err)
			}
			current, _ := m.latest(r.Lane)
			if current.ID != id || current.ContinueRunID != "" || current.Exit != nil {
				t.Fatal("ended terminal run replayed or inferred an exit")
			}
		})
	}
}

func TestTerminalCrashRecoveryLimit(t *testing.T) {
	c, store := opencodeFixture(t)
	m, err := NewManager(c, store)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	parent := seedTerminalRun(t, m, terminalStart, "running")
	for attempt := 1; attempt <= 2; attempt++ {
		if err := m.Recover(); err != nil {
			t.Fatal(err)
		}
		waitReady(t, m, parent.Lane)
		child, _ := m.latest(parent.Lane)
		if child.ParentRunID != parent.ID || child.ResumeCount != attempt || !child.RecoveryRun {
			t.Fatal("genuine recovery chain not linked exactly once")
		}
		if err := m.Recover(); err != nil {
			t.Fatal(err)
		}
		runs, _ := m.history()
		if len(runs) != attempt+1 {
			t.Fatal("recovery duplicated a live child")
		}
		input, _ := os.ReadFile(filepath.Join(store, "runs", child.ID, "goal.txt"))
		if !strings.HasPrefix(string(input), "Previous run ended before report delivery; exit cause unknown at ") {
			t.Fatal("recovery cause not neutral")
		}
		if err := signalRun(*child, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		waitState(t, m, child.Lane, "ended_no_report")
		parent = *child
	}
	for range 3 {
		if err := m.Recover(); err != nil {
			t.Fatal(err)
		}
	}
	runs, _ := m.history()
	current, _ := m.latest(parent.Lane)
	if len(runs) != 3 || current.ResumeCount != 2 || current.State != "interrupted" || current.WaitingToResume || current.ContinueRunID != "" {
		t.Fatal("recovery exceeded two attempts")
	}
}

func TestTerminalInactivityAndRuntimeSupervision(t *testing.T) {
	c, store := opencodeFixture(t)
	c.MaxRuntime = "1h"
	m, err := NewManager(c, store)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.Grace = 25 * time.Millisecond
	_, err = m.Dispatch("opencode-1", "TERMINAL-CLOCK", "terminal-hold ignore-int")
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, m, "opencode-1")
	m.mu.Lock()
	r, _ := m.latest("opencode-1")
	events := filepath.Join(store, "runs", r.ID, "events.jsonl")
	fi, err := os.Stat(events)
	if err != nil {
		m.mu.Unlock()
		t.Fatal(err)
	}
	// Advance the observer's clock, not the wall clock or the owner's state.
	stuck, err := m.checkStuck(r, fi.ModTime().Add(16*time.Minute))
	if err != nil || !stuck || r.StopAt != nil || !sameProcess(*r) {
		m.mu.Unlock()
		t.Fatal("inactivity warning killed terminal process")
	}
	stuck, err = m.checkStuck(r, fi.ModTime().Add(time.Minute))
	if err != nil || stuck || r.StopAt != nil || !sameProcess(*r) {
		m.mu.Unlock()
		t.Fatal("recent tool traffic was treated as idle")
	}
	// Move the temporary run's start beyond its configured runtime. The fake
	// ignores SIGINT, so the verified supervisor must escalate to SIGKILL.
	r.Started = time.Now().Add(-2 * time.Hour).UTC()
	err = m.save(r)
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	s := waitState(t, m, r.Lane, "ended")
	if s.Outcome != "timed_out" || s.Exit != nil {
		t.Fatal("terminal event bypassed configured runtime")
	}
	current, _ := m.latest(r.Lane)
	if current.StopAt == nil || sameProcess(*current) {
		t.Fatal("verified timeout did not stop fake process")
	}
	if err := m.Recover(); err != nil {
		t.Fatal(err)
	}
	runs, _ := m.history()
	if len(runs) != 1 || runs[0].RecoveryRun {
		t.Fatal("timeout was recovered despite explicit stop")
	}
}

func TestTerminalStreamBudgetAndPrivateFile(t *testing.T) {
	for _, mode := range []string{"oversized-stream", "linked-stream", "torn-metadata"} {
		t.Run(mode, func(t *testing.T) {
			c, store := opencodeFixture(t)
			m, err := NewManager(c, store)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			r := seedTerminalRun(t, m, terminalStart+terminalText("done")+terminalStop, "running")
			dir := filepath.Join(store, "runs", r.ID)
			path := filepath.Join(dir, "events.jsonl")
			switch mode {
			case "oversized-stream":
				if err := os.Truncate(path, 64<<20+1); err != nil {
					t.Fatal(err)
				}
			case "linked-stream":
				if err := os.Rename(path, path+".target"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".target", path); err != nil {
					t.Fatal(err)
				}
			case "torn-metadata":
				if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(`{"run_id":`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "linked-stream" {
				if terminal, _ := m.terminalTurn(r); terminal {
					t.Fatal("linked private stream classified as terminal")
				}
				return // Existing session access refuses linked run files.
			}
			if err := m.Recover(); err != nil {
				t.Fatal(err)
			}
			runs, _ := m.history()
			if mode == "torn-metadata" {
				if len(runs) != 0 {
					t.Fatal("torn metadata trusted")
				}
			} else {
				current, _ := m.latest(r.Lane)
				if current.ID == r.ID || !current.RecoveryRun {
					t.Fatal("over-budget stream accepted terminal prefix")
				}
			}
		})
	}
}
