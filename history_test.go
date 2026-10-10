package nuntius

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func seedHistory(t *testing.T, m *Manager, n int) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < n; i++ {
		end := now.Add(-time.Duration(i) * time.Hour)
		exit := i % 8
		r := Run{ID: fmt.Sprintf("%032x", i+1), Lane: "codex", GoalID: fmt.Sprintf("HISTORY-%d", i), Started: end.Add(-time.Minute), Ended: &end, Exit: &exit, State: "ended"}
		if i == 0 {
			// Completion order must win over start order.
			r.Started = end.Add(-48 * time.Hour)
			r.Lane = "claude"
			r.StopReason = "cancelled"
		}
		if i == 1 {
			r.State, r.StopReason, r.Exit = "timed_out", "timed_out", nil
		}
		if err := privateDir(filepath.Join(m.store, "runs", r.ID)); err != nil {
			t.Fatal(err)
		}
		if err := m.save(&r); err != nil {
			t.Fatal(err)
		}
	}
}

func cliFixture(t *testing.T, c Config, store string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "lanes.json")
	b, _ := json.Marshal(c)
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NUNTIUS_CONFIG", path)
	t.Setenv("NUNTIUS_DATA_DIR", store)
}

func historyRows(t *testing.T, v any) []map[string]any {
	t.Helper()
	var rows []map[string]any
	if json.Unmarshal([]byte(toolText(v)), &rows) != nil {
		t.Fatal("invalid history JSON")
	}
	return rows
}

func TestHistoryCLIAndMCP(t *testing.T) {
	m, c, store := manager(t)
	seedHistory(t, m, 25)
	if err := os.WriteFile(filepath.Join(c.ReportsDir, "HISTORY-0.txt"), []byte("finished"), 0600); err != nil {
		t.Fatal(err)
	}
	cliFixture(t, c, store)
	var out, errOut bytes.Buffer
	if CLI(context.Background(), []string{"history"}, strings.NewReader(""), &out, &errOut) != 0 {
		t.Fatal("history CLI unavailable")
	}
	for _, heading := range []string{"ENDED", "AGENT", "GOAL", "OUTCOME", "DURATION", "REPORT", "EXIT"} {
		if !strings.Contains(out.String(), heading) {
			t.Fatal("history column absent")
		}
	}
	if strings.Count(out.String(), "HISTORY-") != 20 || strings.Index(out.String(), "HISTORY-0 ") > strings.Index(out.String(), "HISTORY-1 ") {
		t.Fatal("default completion order or limit")
	}
	if !strings.Contains(out.String(), "cancelled") || !strings.Contains(out.String(), "timed_out") || !strings.Contains(out.String(), "yes") {
		t.Fatal("terminal outcomes or report omitted")
	}
	for _, tc := range []struct {
		flags []string
		args  string
		count int
	}{
		{nil, `{}`, 20},
		{[]string{"--limit", "2"}, `{"limit":2}`, 2},
		{[]string{"--lane", "claude"}, `{"lane":"claude"}`, 1},
		{[]string{"--goal", "HISTORY-1"}, `{"goal_id":"HISTORY-1"}`, 1},
		{[]string{"--since", "24h"}, `{"since":"24h"}`, 24},
		{[]string{"--lane", "claude", "--goal", "HISTORY-1"}, `{"lane":"claude","goal_id":"HISTORY-1"}`, 0},
	} {
		// Use an explicit large limit when checking the since filter.
		if tc.count == 24 {
			tc.flags = append(tc.flags, "--limit", "200")
			tc.args = `{"since":"24h","limit":200}`
		}
		out.Reset()
		errOut.Reset()
		args := append([]string{"history", "--json"}, tc.flags...)
		if CLI(context.Background(), args, strings.NewReader(""), &out, &errOut) != 0 {
			t.Fatal("history flags failed")
		}
		var cliRows []map[string]any
		if json.Unmarshal(out.Bytes(), &cliRows) != nil || len(cliRows) != tc.count {
			t.Fatal("history CLI filter")
		}
		v, err := call(m, "nuntius_history", []byte(tc.args))
		if err != nil {
			t.Fatal("history MCP unavailable")
		}
		rows := historyRows(t, v)
		var fullRows []HistoryEntry
		if json.Unmarshal(out.Bytes(), &fullRows) != nil {
			t.Fatal("CLI JSON history")
		}
		if len(rows) != tc.count || toolText(v) != toolText(publicHistory(fullRows)) {
			t.Fatal("CLI MCP parity")
		}
		if len(rows) > 0 {
			if _, ok := rows[0]["duration_seconds"].(float64); !ok {
				t.Fatal("duration machine type")
			}
			if _, ok := rows[0]["report_exists"].(bool); !ok {
				t.Fatal("report machine type")
			}
			if _, ok := rows[0]["ended"].(string); !ok {
				t.Fatal("ended machine type")
			}
			for _, key := range []string{"session_id", "pid", "sha256", "process_start", "goal", "report"} {
				if _, exists := rows[0][key]; exists {
					t.Fatal("private history data exposed")
				}
			}
		}
	}
	for _, args := range []string{`{"limit":0}`, `{"limit":201}`, `{"limit":1.5}`, `{"limit":null}`, `{"lane":"../x"}`, `{"goal_id":"bad"}`, `{"since":"-1h"}`, `{"since":"0s"}`, `{"since":"invalid"}`, `{"extra":true}`} {
		if _, err := call(m, "nuntius_history", []byte(args)); err == nil {
			t.Fatal("invalid history MCP input accepted")
		}
	}
	for _, flags := range [][]string{{"--limit", "0"}, {"--limit", "201"}, {"--since", "-1h"}, {"--lane", "../x"}, {"--unknown"}, {"extra"}} {
		out.Reset()
		errOut.Reset()
		if CLI(context.Background(), append([]string{"history"}, flags...), strings.NewReader(""), &out, &errOut) == 0 || out.Len() != 0 {
			t.Fatal("invalid CLI history input accepted")
		}
	}
	found := false
	for _, tool := range tools() {
		if tool.Name == "nuntius_history" {
			found = tool.Annotations["readOnlyHint"] == true
		}
	}
	if !found {
		t.Fatal("read-only history tool missing")
	}
}

func TestHistoryExcludesRunningAndBoundsStore(t *testing.T) {
	m, _, _ := manager(t)
	v, err := call(m, "nuntius_history", nil)
	if err != nil || toolText(v) != `[]` {
		t.Fatal("empty history must be array")
	}
	if _, err := m.Dispatch("codex", "LIVE-1", "wait"); err != nil {
		t.Fatal(err)
	}
	waitReady(t, m, "codex")
	v, err = call(m, "nuntius_history", nil)
	if err != nil || toolText(v) != `[]` {
		t.Fatal("running goal exposed as finished")
	}
	if err := m.Cancel("codex"); err != nil {
		t.Fatal(err)
	}
	waitState(t, m, "codex", "ended")
	v, err = call(m, "nuntius_history", nil)
	if err != nil {
		t.Fatal(err)
	}
	rows := historyRows(t, v)
	if len(rows) != 1 || rows[0]["outcome"] != "cancelled" || rows[0]["report_exists"] != false {
		t.Fatal("cancelled no-report run missing")
	}
	if err := os.Mkdir(filepath.Join(m.store, "runs", "invalid"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := call(m, "nuntius_history", nil); err == nil {
		t.Fatal("invalid store was silently ignored")
	}
	// Cleanup cannot inspect the deliberately invalid store.
	if err := os.Remove(filepath.Join(m.store, "runs", "invalid")); err != nil {
		t.Fatal(err)
	}
}

func TestTerminalDisplayPreservesLegacyState(t *testing.T) {
	m, c, store := manager(t)
	seedHistory(t, m, 2)
	cliFixture(t, c, store)
	s, err := m.Status()
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range s {
		// Existing waitState/watch clients use this terminal vocabulary.
		if row.State != "ended_no_report" {
			t.Fatal("legacy state dependency changed")
		}
		var fields map[string]any
		_ = json.Unmarshal([]byte(toolText(row)), &fields)
		if fields["display_state"] != row.Outcome {
			t.Fatal("terminal presentation state missing")
		}
	}
	if err := os.WriteFile(filepath.Join(c.ReportsDir, "HISTORY-0.txt"), []byte("done"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err = m.Status()
	if err != nil || s[0].State != "ended" || s[0].DisplayState != "cancelled" {
		t.Fatal("reported cancelled run must preserve ended with distinct display")
	}
	for _, cmd := range []string{"status", "agents"} {
		var out, errOut bytes.Buffer
		if CLI(context.Background(), []string{cmd}, strings.NewReader(""), &out, &errOut) != 0 || !strings.Contains(out.String(), "timed_out") || !strings.Contains(out.String(), "cancelled") {
			t.Fatal("human terminal states missing")
		}
	}
	w, err := NewWeb(m, "127.0.0.1:8123")
	if err != nil {
		t.Fatal(err)
	}
	data, err := w.data()
	if err != nil || !strings.Contains(toolText(data), `"display_state":"cancelled"`) || !strings.Contains(toolText(data), `"display_state":"timed_out"`) {
		t.Fatal("web terminal states missing")
	}
}

func TestStuckSignalOnceAcrossAckAndRestart(t *testing.T) {
	c, store := fixture(t)
	c.MaxRuntime = "1h"
	m, err := NewManager(c, store)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Cancel("codex"); waitState(t, m, "codex", "ended"); _ = m.Close() }()
	id, err := m.Dispatch("codex", "STUCK-1", "wait")
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, m, "codex")
	cliFixture(t, c, store)
	report := filepath.Join(c.ReportsDir, "STUCK-1.txt")
	if err := os.WriteFile(report, []byte("finished"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-6 * time.Minute)
	if err := os.Chtimes(report, old, old); err != nil {
		t.Fatal(err)
	}
	s, err := m.Status()
	if err != nil || !strings.Contains(toolText(s), `"display_state":"stuck?"`) || !strings.Contains(toolText(s), `"state":"running"`) {
		t.Fatal("post-report running not marked stuck")
	}
	name := "codex.STUCK-1.stuck"
	names, err := m.Signals()
	if err != nil || toolText(names) != toolText([]string{name}) {
		t.Fatal("stuck signal missing")
	}
	fi, err := os.Stat(filepath.Join(store, "signals", name))
	if err != nil || fi.Size() != 0 || fi.Mode().Perm() != 0600 {
		t.Fatal("stuck signal not private and empty")
	}
	if err := m.AckSignals([]string{name}); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	m, err = NewManager(c, store)
	if err != nil {
		t.Fatal(err)
	}
	s, err = m.Status()
	if err != nil || !strings.Contains(toolText(s), `"display_state":"stuck?"`) {
		t.Fatal("restart lost stuck indication")
	}
	names, err = m.Signals()
	if err != nil || len(names) != 0 {
		t.Fatal("acknowledged stuck signal repeated")
	}
	if latest, err := m.latest("codex"); err != nil || latest.ID != id || latest.StopAt != nil {
		t.Fatal("stuck detection stopped run")
	}
	for _, cmd := range []string{"status", "agents"} {
		var out, errOut bytes.Buffer
		if CLI(context.Background(), []string{cmd}, strings.NewReader(""), &out, &errOut) != 0 || !strings.Contains(out.String(), "stuck?") {
			t.Fatal("human stuck view missing")
		}
	}
	w, err := NewWeb(m, "127.0.0.1:8123")
	if err != nil {
		t.Fatal(err)
	}
	data, err := w.data()
	if err != nil || !strings.Contains(toolText(data), `"display_state":"stuck?"`) {
		t.Fatal("web stuck state missing")
	}
	if err := os.Symlink(report, filepath.Join(store, "signals", "codex.LINK-1.stuck")); err != nil {
		t.Fatal(err)
	}
	if err := m.AckSignals([]string{"codex.LINK-1.stuck"}); err == nil {
		t.Fatal("linked stuck signal acknowledged")
	}
	if err := os.Remove(filepath.Join(store, "signals", "codex.LINK-1.stuck")); err != nil {
		t.Fatal(err)
	}
	if err := m.Cancel("codex"); err != nil {
		t.Fatal(err)
	}
	waitState(t, m, "codex", "ended")
	if err := m.AckSignals([]string{"codex.STUCK-1.ended"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Dispatch("codex", "STUCK-1", "wait"); err != nil {
		t.Fatal(err)
	}
	waitReady(t, m, "codex")
	names, err = m.Signals()
	if err != nil || toolText(names) != toolText([]string{name}) {
		t.Fatal("new run cannot deliver its stuck signal")
	}
}

func TestStuckSupervisorWithoutPolling(t *testing.T) {
	m, _, _ := manager(t)
	id, err := m.Dispatch("codex", "QUIET-1", "wait")
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, m, "codex")
	path := filepath.Join(m.store, "runs", id, "events.jsonl")
	old := time.Now().Add(-16 * time.Minute)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	// Observe the persisted signal directly; do not call Status or Signals.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if fi, err := os.Stat(filepath.Join(m.store, "signals", "codex.QUIET-1.stuck")); err == nil && fi.Mode().IsRegular() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("supervisor did not detect quiet run")
}

func TestStuckThresholds(t *testing.T) {
	now := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)
	for i, tc := range []struct {
		name                                string
		eventAge, reportAge                 time.Duration
		report, missingEvent, ended, linked bool
		want                                bool
	}{
		{name: "fresh", eventAge: time.Minute},
		{name: "event boundary", eventAge: 15 * time.Minute},
		{name: "quiet", eventAge: 15*time.Minute + time.Second, want: true},
		{name: "report boundary", eventAge: time.Minute, reportAge: 5 * time.Minute, report: true},
		{name: "post report", eventAge: time.Minute, reportAge: 5*time.Minute + time.Second, report: true, want: true},
		{name: "missing event", eventAge: 16 * time.Minute, missingEvent: true, want: true},
		{name: "ended", eventAge: 16 * time.Minute, ended: true},
		{name: "future", eventAge: -time.Hour, report: true, reportAge: -time.Hour},
		{name: "linked report", eventAge: time.Minute, report: true, reportAge: 6 * time.Minute, linked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, c, _ := manager(t)
			r := Run{ID: fmt.Sprintf("%032x", i+1), Lane: "codex", GoalID: "BOUNDARY-1", State: "running", Started: now.Add(-tc.eventAge)}
			if tc.ended {
				r.State = "ended"
			}
			dir := filepath.Join(m.store, "runs", r.ID)
			if err := privateDir(dir); err != nil {
				t.Fatal(err)
			}
			if err := m.save(&r); err != nil {
				t.Fatal(err)
			}
			if !tc.missingEvent {
				path := filepath.Join(dir, "events.jsonl")
				if err := os.WriteFile(path, nil, 0600); err != nil {
					t.Fatal(err)
				}
				stamp := now.Add(-tc.eventAge)
				if err := os.Chtimes(path, stamp, stamp); err != nil {
					t.Fatal(err)
				}
			}
			if tc.report {
				path := filepath.Join(c.ReportsDir, r.GoalID+".txt")
				target := path
				if tc.linked {
					target = filepath.Join(t.TempDir(), "outside")
				}
				if err := os.WriteFile(target, nil, 0600); err != nil {
					t.Fatal(err)
				}
				stamp := now.Add(-tc.reportAge)
				if err := os.Chtimes(target, stamp, stamp); err != nil {
					t.Fatal(err)
				}
				if tc.linked {
					if err := os.Symlink(target, path); err != nil {
						t.Fatal(err)
					}
				}
			}
			m.mu.Lock()
			guard, err := m.controlLock()
			if err != nil {
				m.mu.Unlock()
				t.Fatal(err)
			}
			got, err := m.checkStuck(&r, now)
			_ = guard.Close()
			m.mu.Unlock()
			if err != nil || got != tc.want || r.StuckSignalled != tc.want {
				t.Fatal("stuck threshold predicate")
			}
			_, err = os.Stat(filepath.Join(m.store, "signals", "codex.BOUNDARY-1.stuck"))
			if (err == nil) != tc.want {
				t.Fatal("stuck signal predicate")
			}
		})
	}
}

func TestHistoryTextSafetyAndTypes(t *testing.T) {
	m, _, _ := manager(t)
	seedHistory(t, m, 1)
	r, err := m.latest("claude")
	if err != nil {
		t.Fatal(err)
	}
	r.StopReason = "password=fixture-sensitive\n" + strings.Repeat("x", 1000)
	if err := m.save(r); err != nil {
		t.Fatal(err)
	}
	rows, err := m.History(HistoryFilter{Limit: 20})
	if err != nil || len(rows) != 1 || len(rows[0].Outcome) > 256 || strings.Contains(rows[0].Outcome, "fixture-sensitive") {
		t.Fatal("history text not bounded/redacted at source")
	}
	var out bytes.Buffer
	if err := writeHistory(&out, rows); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "\n") != 2 || !strings.Contains(out.String(), r.Ended.Local().Format("2006-01-02 15:04:05 MST")) {
		t.Fatal("history local time or multiline text safety")
	}
	decoded := historyRows(t, rows)
	if decoded[0]["exit"] != float64(0) {
		t.Fatal("numeric exit lost")
	}
	r.Exit = nil
	if err := m.save(r); err != nil {
		t.Fatal(err)
	}
	rows, err = m.History(HistoryFilter{Limit: 20})
	if err != nil || historyRows(t, rows)[0]["exit"] != nil {
		t.Fatal("unknown exit must be null")
	}
}
