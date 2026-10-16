package nuntius

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

type crashMCP struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out *bufio.Reader
	id  int
}

func startCrashMCP(t *testing.T, binary, config, store string) *crashMCP {
	t.Helper()
	p := &crashMCP{cmd: exec.Command(binary, "--config", config, "--store", store, "mcp")}
	var err error
	p.in, err = p.cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := p.cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	p.out = bufio.NewReader(out)
	if err = p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.in.Close(); _ = p.cmd.Process.Kill(); _ = p.cmd.Wait() })
	p.request(t, "initialize", map[string]any{})
	_, _ = fmt.Fprintln(p.in, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	return p
}

func (p *crashMCP) request(t *testing.T, method string, params any) json.RawMessage {
	t.Helper()
	p.id++
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": p.id, "method": method, "params": params})
	if _, err := fmt.Fprintln(p.in, string(b)); err != nil {
		t.Fatal(err)
	}
	line, err := p.out.ReadBytes('\n')
	if err != nil {
		t.Fatal("MCP response unavailable", err)
	}
	var response struct {
		Result json.RawMessage `json:"result"`
		Error  any             `json:"error"`
	}
	if json.Unmarshal(line, &response) != nil || response.Error != nil {
		t.Fatal("MCP request failed")
	}
	return response.Result
}

func TestCrashMCPDeathAndBinaryReplacement(t *testing.T) {
	for _, report := range []bool{false, true} {
		t.Run(fmt.Sprint(report), func(t *testing.T) {
			c, store := fixture(t)
			off := false
			c.AutoResume = &off
			config := filepath.Join(c.ReportsDir, "lanes.json")
			b, _ := json.Marshal(c)
			if err := os.WriteFile(config, b, 0600); err != nil {
				t.Fatal(err)
			}
			binary := filepath.Join(c.ReportsDir, "nuntius")
			executable, err := os.ReadFile(cliBinary)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(binary, executable, 0700); err != nil {
				t.Fatal(err)
			}
			server := startCrashMCP(t, binary, config, store)
			server.request(t, "tools/call", map[string]any{"name": "nuntius_dispatch", "arguments": map[string]any{"lane": "codex", "goal_id": "MCP-23", "text": "orphan-detect"}})
			m, err := NewManager(c, store)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			waitReady(t, m, "codex")
			r := latestRun(t, m, "codex")
			defer signalRun(*r, syscall.SIGKILL)
			rows, _ := m.Tail("codex", 200)
			if !strings.Contains(strings.Join(rows, "\n"), "ppid=1") {
				t.Fatal("agent retained an MCP/app parent")
			}
			// Replace only this test's own executable inode, as a package upgrade does.
			if err = os.WriteFile(binary+".new", executable, 0700); err != nil {
				t.Fatal(err)
			}
			if err = os.Rename(binary+".new", binary); err != nil {
				t.Fatal(err)
			}
			server.request(t, "ping", nil)
			if !sameProcess(*r) {
				t.Fatal("binary replacement killed agent")
			}
			_ = m.Close()
			if err = server.cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			_ = server.cmd.Wait()
			time.Sleep(100 * time.Millisecond)
			if !sameProcess(*r) {
				t.Fatal("MCP death killed orphan-sensitive agent")
			}
			if report {
				if err = os.WriteFile(filepath.Join(c.ReportsDir, "MCP-23.txt"), []byte("done"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err = signalRun(*r, syscall.SIGKILL); err != nil {
				t.Fatal(err)
			}
			for deadline := time.Now().Add(3 * time.Second); sameProcess(*r) && time.Now().Before(deadline); {
				time.Sleep(time.Millisecond)
			}
			_ = startCrashMCP(t, binary, config, store)
			next, err := NewManager(c, store)
			if err != nil {
				t.Fatal(err)
			}
			defer next.Close()
			ended := latestRun(t, next, "codex")
			want := "interrupted"
			if report {
				want = "completed"
			}
			if ended.Exit != nil || ended.StopReason != want {
				t.Fatal("new MCP did not derive the report/unknown-exit outcome")
			}
		})
	}
}

func seedCrashRun(t *testing.T, m *Manager, id, goal, reason string) Run {
	t.Helper()
	r := Run{ID: id, Lane: "codex", GoalID: goal, State: "running", StopReason: reason, Started: time.Now().Add(-time.Minute).UTC(), Brief: "wait no-session", PID: 99999999}
	if err := privateDir(filepath.Join(m.store, "runs", id)); err != nil {
		t.Fatal(err)
	}
	if err := m.save(&r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCrashRecoveryExclusionsAndReadOnly(t *testing.T) {
	for _, mode := range []string{"frozen", "cancelled", "timed_out", "auto_resume_off"} {
		t.Run(mode, func(t *testing.T) {
			m, c, store := manager(t)
			reason := ""
			if mode == "cancelled" || mode == "timed_out" {
				reason = mode
			}
			r := seedCrashRun(t, m, strings.Repeat("b", 32), "EXCLUDE-23", reason)
			if mode == "frozen" {
				b, _ := json.Marshal(FreezeState{Since: time.Now().UTC(), Reason: "fixture"})
				if err := atomicPrivate(filepath.Join(store, "freeze.json"), b); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "auto_resume_off" {
				off := false
				m.mu.Lock()
				m.config.AutoResume = &off
				m.mu.Unlock()
			}
			if err := m.Recover(); err != nil {
				t.Fatal(err)
			}
			runs, err := m.history()
			if err != nil || len(runs) != 1 {
				t.Fatal("excluded run resumed")
			}
			got, _ := loadRun(filepath.Join(store, "runs", r.ID), r.ID)
			if reason == "" && got.State != "interrupted" {
				t.Fatal("interrupted run lost")
			}
			_ = m.Close()
			config := filepath.Join(c.ReportsDir, "lanes.json")
			b, _ := json.Marshal(c)
			_ = os.WriteFile(config, b, 0600)
			for _, command := range []string{"status", "agents", "history"} {
				var out, errs bytes.Buffer
				if code := CLI(context.Background(), []string{"--config", config, "--store", store, command}, strings.NewReader(""), &out, &errs); code != 0 {
					t.Fatal("read-only command failed")
				}
			}
			next, err := NewManager(c, store)
			if err != nil {
				t.Fatal(err)
			}
			defer next.Close()
			web, err := NewWeb(next, "127.0.0.1:8123")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = web.data(); err != nil {
				t.Fatal(err)
			}
			runs, _ = next.history()
			if len(runs) != 1 {
				t.Fatal("display command launched a recovery")
			}
		})
	}
}

func TestCrashRecoveryBusyLaneAndLimit(t *testing.T) {
	goalEndParallel(t)
	m, _, store := manager(t)
	seedCrashRun(t, m, strings.Repeat("c", 32), "QUEUED-23", "")
	if _, err := m.Dispatch("codex", "BUSY-23", "wait"); err != nil {
		t.Fatal(err)
	}
	waitReady(t, m, "codex")
	if err := m.Recover(); err != nil {
		t.Fatal(err)
	}
	old, _ := loadRun(filepath.Join(store, "runs", strings.Repeat("c", 32)), strings.Repeat("c", 32))
	if old.State != "interrupted" || old.ContinueRunID != "" {
		t.Fatal("busy lane dropped/resumed goal")
	}
	history, err := m.History(HistoryFilter{Limit: 20})
	if err != nil || len(history) != 1 || !strings.Contains(history[0].DisplayState, "waiting to resume") {
		t.Fatal("waiting goal invisible")
	}
	input, writer := io.Pipe()
	defer input.Close()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, m, input, io.Discard) }()
	defer func() { cancel(); _ = writer.Close(); <-done }()
	if err = m.Cancel("codex"); err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= 2; n++ {
		// Automatic recovery now runs every 10s; allow one tick and scheduling
		// margin while retaining the exact resume-chain and duplicate checks.
		wait, stopWait := context.WithTimeout(t.Context(), 12*time.Second)
		_, err = latestRunWhen(wait, m, "codex", func(r *Run) bool {
			return r.GoalID == "QUEUED-23" && r.ResumeCount == n && r.State == "running"
		})
		stopWait()
		if err != nil {
			t.Fatalf("waiting goal did not resume when lane freed: %v", err)
		}
		waitReady(t, m, "codex")
		current := latestRun(t, m, "codex")
		if !current.RecoveryRun || current.ParentRunID == "" {
			t.Fatal("resume chain lost")
		}
		if err = signalRun(*current, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
	}
	wait, stopWait := context.WithTimeout(t.Context(), 12*time.Second)
	_, err = latestRunWhen(wait, m, "codex", func(r *Run) bool { return r.State == "interrupted" })
	stopWait()
	if err != nil {
		t.Fatalf("bounded recovery did not stop at 2: %v", err)
	}
	current := latestRun(t, m, "codex")
	if current.ResumeCount != 2 || current.State != "interrupted" || current.ContinueRunID != "" {
		t.Fatal("bounded recovery did not stop at 2")
	}
	if err = m.Recover(); err != nil {
		t.Fatal(err)
	}
	runs, _ := m.history()
	if len(runs) != 4 {
		t.Fatal("recovery duplicated or dropped chain")
	}
}

func TestCrashIntegrityAndAtomicPrivate(t *testing.T) {
	m, _, store := manager(t)
	r := seedCrashRun(t, m, strings.Repeat("d", 32), "INTACT-23", "")
	path := filepath.Join(store, "runs", r.ID, "meta.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b = bytes.Replace(b, []byte("INTACT-23"), []byte("BROKEN-23"), 1)
	if err = os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := loadRun(filepath.Dir(path), r.ID); err != nil || got != nil {
		t.Fatal("valid JSON with torn content accepted")
	}
	if err = atomicPrivate(filepath.Join(store, "config.json"), []byte(`{"auto_resume":false}`)); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(store, "config.json"))
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("atomic write not private")
	}
	entries, _ := os.ReadDir(store)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".nuntius-") {
			t.Fatal("atomic temporary file leaked")
		}
	}
	if err := privateDir(filepath.Join(store, "signals")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"codex.TORN-23.interrupted", ".nuntius-torn"} {
		if err := os.WriteFile(filepath.Join(store, "signals", name), []byte("torn"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if names, err := m.Signals(); err != nil || len(names) != 0 {
		t.Fatal("torn signal delivered or blocked valid signals")
	}
}

// The fake dies if its parent changes. Killing or replacing a manager must
// therefore leave the already orphaned agent alive, not merely avoid SIGTERM.
func TestCrashDetachedOrphanSensitiveAgent(t *testing.T) {
	m, c, store := manager(t)
	id, err := m.Dispatch("codex", "DETACH-23", "orphan-detect")
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, m, "codex")
	rows, _ := m.Tail("codex", 200)
	if !strings.Contains(strings.Join(rows, "\n"), "ppid=1") {
		t.Fatal("agent has a manager or app parent")
	}
	r := latestRun(t, m, "codex")
	defer signalRun(*r, syscall.SIGKILL)
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if !sameProcess(*r) {
		t.Fatal("agent still depended on manager parent")
	}
	next, err := NewManager(c, store)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if next.session(id) == "" {
		t.Fatal("session events lost")
	}
}

func TestCrashExplicitRecoverySessionAndFresh(t *testing.T) {
	for _, mode := range []string{"wait", "wait no-session"} {
		t.Run(mode, func(t *testing.T) {
			m, c, store := manager(t)
			id, err := m.Dispatch("codex", "CRASH-23", mode)
			if err != nil {
				t.Fatal(err)
			}
			waitReady(t, m, "codex")
			r := latestRun(t, m, "codex")
			if err = m.Close(); err != nil {
				t.Fatal(err)
			}
			if err = signalRun(*r, syscall.SIGKILL); err != nil {
				t.Fatal(err)
			}
			for deadline := time.Now().Add(3 * time.Second); sameProcess(*r) && time.Now().Before(deadline); {
				time.Sleep(10 * time.Millisecond)
			}
			p := filepath.Join(c.ReportsDir, "lanes.json")
			b, _ := json.Marshal(c)
			if err = os.WriteFile(p, b, 0600); err != nil {
				t.Fatal(err)
			}
			var out, errs bytes.Buffer
			if code := CLI(context.Background(), []string{"--config", p, "--store", store, "recover"}, strings.NewReader(""), &out, &errs); code != 0 {
				t.Fatalf("recover refused: %s", errs.String())
			}
			next, err := NewManager(c, store)
			if err != nil {
				t.Fatal(err)
			}
			defer next.Close()
			current := latestRun(t, next, "codex")
			if current.ID == id {
				t.Fatal("interrupted goal was not resumed")
			}
			defer signalRun(*current, syscall.SIGKILL)
			waitReady(t, next, "codex")
			cliFixture(t, c, store)
			for _, command := range []string{"status", "agents"} {
				var display, diagnostic bytes.Buffer
				if CLI(context.Background(), []string{command}, strings.NewReader(""), &display, &diagnostic) != 0 || !strings.Contains(display.String(), "Running · resumed 1/2") {
					t.Fatal("resumed running wording missing from CLI")
				}
			}
			web, _ := NewWeb(next, "127.0.0.1:8123")
			data, err := web.data()
			if err != nil || !strings.Contains(toolText(data), "Running · resumed 1/2") {
				t.Fatal("resumed running wording missing from web data")
			}
			prompt, _ := os.ReadFile(filepath.Join(store, "runs", current.ID, "goal.txt"))
			if !strings.HasPrefix(string(prompt), "Previous run ended before report delivery; exit cause unknown at ") || !strings.Contains(string(prompt), "Re-measure the repositories") || !strings.HasSuffix(string(prompt), mode) {
				t.Fatal("recovery lost the brief or restart note")
			}
			rows, _ := next.Tail("codex", 200)
			resumed := strings.Contains(strings.Join(rows, "\n"), `"resume"`)
			if resumed == strings.Contains(mode, "no-session") {
				t.Fatal("wrong session/fresh argv")
			}
			signals, err := next.Signals()
			if err != nil || !strings.Contains(strings.Join(signals, ","), "codex.CRASH-23.interrupted") || !strings.Contains(strings.Join(signals, ","), "codex.CRASH-23.resumed") {
				t.Fatal("recovery signals missing")
			}
			if err = next.AckSignals(signals); err != nil {
				t.Fatal(err)
			}
			if err = next.Recover(); err != nil {
				t.Fatal(err)
			}
			signals, err = next.Signals()
			if err != nil || len(signals) != 0 {
				t.Fatal("acknowledged recovery signals repeated")
			}
			if err = next.Cancel("codex"); err != nil {
				t.Fatal(err)
			}
			waitState(t, next, "codex", "ended")
			if err = next.Recover(); err != nil {
				t.Fatal(err)
			}
			signals, err = next.Signals()
			if err != nil || strings.Contains(strings.Join(signals, ","), ".resumed") {
				t.Fatal("end overwrote durable signal delivery")
			}
			rowsHistory, err := next.History(HistoryFilter{Limit: 20})
			if err != nil || len(rowsHistory) != 2 || rowsHistory[0].ParentRunID != id || rowsHistory[1].ContinueRunID != current.ID || rowsHistory[0].ResumeCount != 1 {
				t.Fatal("history lost resume chain")
			}
		})
	}
}

func TestCrashTornRunIgnored(t *testing.T) {
	m, _, store := manager(t)
	dir := filepath.Join(store, "runs", strings.Repeat("a", 32))
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(`{"run_id":`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Status(); err != nil {
		t.Fatal("torn record blocked intact store")
	}
}
