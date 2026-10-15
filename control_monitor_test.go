package nuntius

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

// This helper runs in a separate test process, using only compiled fake agents.
func TestControlMonitorProcess(t *testing.T) {
	role := os.Getenv("NUNTIUS_CONTROL_MONITOR_ROLE")
	if role == "" {
		return
	}
	c, err := LoadConfig(os.Getenv("NUNTIUS_CONTROL_MONITOR_CONFIG"))
	if err != nil {
		t.Fatal("fixture config")
	}
	m, err := NewManager(c, os.Getenv("NUNTIUS_CONTROL_MONITOR_STORE"))
	if err != nil {
		t.Fatal("fixture manager")
	}
	defer m.Close()
	out := json.NewEncoder(os.Stdout)
	if role == "mcp" {
		m.mu.Lock()
		monitors := len(m.active)
		m.mu.Unlock()
		if monitors != 2 {
			t.Fatal("MCP does not own both run monitors")
		}
		_ = out.Encode(map[string]int{"monitors": monitors})
		if Serve(context.Background(), m, os.Stdin, os.Stdout) != nil {
			t.Error("fixture MCP transport")
		}
		return
	}
	w, err := NewWeb(m, "127.0.0.1:8123")
	if err != nil {
		t.Fatal("fixture web")
	}
	stop, done := make(chan struct{}), make(chan struct{})
	var polls atomic.Int64
	go func() {
		defer close(done)
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				req := httptest.NewRequest("GET", "http://localhost:8123/data", nil)
				req.Header.Set("X-Nuntius-Token", w.token)
				response := httptest.NewRecorder()
				w.ServeHTTP(response, req)
				if response.Code == 200 {
					polls.Add(1)
				}
			}
		}
	}()
	defer func() { close(stop); <-done }()
	_ = out.Encode(map[string]bool{"web": true})
	input := bufio.NewScanner(os.Stdin)
	for input.Scan() {
		switch input.Text() {
		case "hold":
			// A confirmed web transaction longer than the old 250 ms admission
			// budget makes the contention proof independent of scheduler luck.
			m.mu.Lock()
			guard, err := m.controlLock()
			if err != nil {
				m.mu.Unlock()
				t.Fatal("fixture web transaction")
			}
			_ = out.Encode(map[string]bool{"held": true})
			time.Sleep(350 * time.Millisecond)
			guard.Close()
			m.mu.Unlock()
			_ = out.Encode(map[string]bool{"released": true})
		case "stats":
			_ = out.Encode(map[string]int64{"polls": polls.Load()})
		}
	}
}

type controlMonitorPeer struct {
	in  io.WriteCloser
	out *json.Decoder
}

func TestControlMonitorDispatchDeadline(t *testing.T) {
	m, store := laneControlCommitFixture(t)
	m.Close()
	release := oldControlHolder(t, store)
	defer release()
	before := admissionArtifacts(t, store)
	start := time.Now()
	id, err := m.Dispatch("codex", "MONITOR-DEADLINE", "ack-goal")
	elapsed := time.Since(start)
	if id != "" || laneControlCode(err) != "locked" || elapsed < 1800*time.Millisecond || elapsed > 2500*time.Millisecond {
		t.Fatal("public admission did not exhaust its finite wait without a child")
	}
	if after := admissionArtifacts(t, store); !maps.Equal(before, after) {
		t.Fatal("refused admission changed run artifacts")
	}
}

func controlMonitorStart(t *testing.T, role, config, store string) controlMonitorPeer {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestControlMonitorProcess$")
	cmd.Env = append(os.Environ(), "NUNTIUS_CONTROL_MONITOR_ROLE="+role,
		"NUNTIUS_CONTROL_MONITOR_CONFIG="+config, "NUNTIUS_CONTROL_MONITOR_STORE="+store)
	cmd.Dir = filepath.Dir(store)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal("fixture input")
	}
	out, err := cmd.StdoutPipe()
	if err != nil || cmd.Start() != nil {
		t.Fatal("fixture process start")
	}
	t.Cleanup(func() {
		in.Close()
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				t.Error("fixture process exit")
			}
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			t.Error("fixture process did not close")
		}
	})
	return controlMonitorPeer{in, json.NewDecoder(out)}
}

func controlMonitorRead(t *testing.T, peer controlMonitorPeer, value any) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- peer.out.Decode(value) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("fixture response encoding")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fixture request exceeded five seconds")
	}
}

func TestControlMonitorMCPDispatchSeries(t *testing.T) {
	c, store := fixture(t)
	off := false
	c.AutoResume, c.MaxRuntime = &off, "10m"
	claude := c.Lanes[0]
	claude.StartMode = "as_is"
	claude.Name = "claude-a"
	c.Lanes[0] = claude
	claude.Name = "claude-b"
	c.Lanes = append(c.Lanes, claude)
	m, err := NewManager(c, store)
	if err != nil {
		t.Fatal("seed manager")
	}
	defer m.Close()
	starts := t.TempDir()
	// The seed manager intentionally detaches; fixture() owns final teardown.
	// Register the marker destination without an owning-manager cleanup helper.
	for _, lane := range c.Lanes {
		if os.WriteFile(filepath.Join(lane.Workdir, testAgentStartPointer), []byte(starts), 0600) != nil {
			t.Fatal("fixture marker registration")
		}
	}
	for index, lane := range []string{"claude-a", "claude-b"} {
		if _, err = m.Dispatch(lane, fmt.Sprintf("MONITOR-SEED-%d", index), "ack-goal"); err != nil {
			t.Fatal("seed dispatch")
		}
		waitState(t, m, lane, "ended")
		if _, err = m.Reply(lane, "wait"); err != nil {
			t.Fatal("seed resume")
		}
		waitReady(t, m, lane)
	}
	if m.Close() != nil {
		t.Fatal("detach seed manager")
	}
	config := filepath.Join(filepath.Dir(store), "lanes.json")
	b, _ := json.Marshal(c)
	if os.WriteFile(config, b, 0600) != nil {
		t.Fatal("fixture config write")
	}
	mcp := controlMonitorStart(t, "mcp", config, store)
	var ready struct {
		Monitors int `json:"monitors"`
	}
	controlMonitorRead(t, mcp, &ready)
	if ready.Monitors != 2 {
		t.Fatal("wrong MCP monitor count")
	}
	exchange := func(id int, name string, args any) (string, bool, time.Duration) {
		request := map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}}
		if name == "initialize" {
			request["method"] = "initialize"
			delete(request, "params")
		}
		start := time.Now()
		if json.NewEncoder(mcp.in).Encode(request) != nil {
			t.Fatal("MCP request write")
		}
		var response struct {
			ID     int `json:"id"`
			Error  any `json:"error"`
			Result struct {
				IsError bool `json:"isError"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		controlMonitorRead(t, mcp, &response)
		if response.ID != id {
			t.Fatal("MCP response identity")
		}
		text := ""
		if len(response.Result.Content) == 1 {
			text = response.Result.Content[0].Text
		}
		return text, response.Error == nil && !response.Result.IsError, time.Since(start)
	}
	if _, ok, _ := exchange(1, "initialize", nil); !ok {
		t.Fatal("MCP initialize")
	}
	_, _ = io.WriteString(mcp.in, "{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}\n")
	web := controlMonitorStart(t, "web", config, store)
	var webReady map[string]bool
	controlMonitorRead(t, web, &webReady)
	if !webReady["web"] {
		t.Fatal("web readiness")
	}
	time.Sleep(1100 * time.Millisecond) // Both ordinary background watchers and monitors tick.
	latencies := make([]time.Duration, 0, 50)
	ids := map[string]bool{}
	failures := 0
	for trial := range 50 {
		contended := trial%2 == 0
		if contended {
			_, _ = io.WriteString(web.in, "hold\n")
			var held map[string]bool
			controlMonitorRead(t, web, &held)
			if !held["held"] {
				t.Fatal("unconfirmed web contention")
			}
		}
		goal := fmt.Sprintf("MONITOR-DISPATCH-%02d", trial)
		text, ok, elapsed := exchange(trial+2, "nuntius_dispatch", map[string]string{"lane": "codex", "goal_id": goal, "text": "ack-goal"})
		latencies = append(latencies, elapsed)
		if contended {
			var released map[string]bool
			controlMonitorRead(t, web, &released)
			if !released["released"] {
				t.Fatal("web transaction did not release")
			}
		}
		if !ok {
			failures++
			continue
		}
		var result struct {
			RunID string `json:"run_id"`
		}
		if json.Unmarshal([]byte(text), &result) != nil {
			t.Fatal("dispatch receipt identity")
		}
		runs, err := m.history()
		if err != nil {
			t.Fatal("dispatch history")
		}
		matches, fullID := 0, ""
		for _, run := range runs {
			if run.GoalID == goal && shortRunID(run.ID) == result.RunID {
				matches++
				fullID = run.ID
			}
		}
		if matches != 1 || !runPattern.MatchString(fullID) || ids[fullID] {
			t.Fatal("dispatch receipt does not identify exactly one child")
		}
		result.RunID = fullID
		ids[result.RunID] = true
		for deadline := time.Now().Add(5 * time.Second); ; {
			r, err := loadRun(filepath.Join(store, "runs", result.RunID), result.RunID)
			if err != nil || r == nil || r.GoalID != goal || r.PID <= 1 {
				t.Fatal("dispatch child identity")
			}
			if r.State != "running" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("fixture dispatch did not finish")
			}
			time.Sleep(10 * time.Millisecond)
		}
		for _, lane := range []string{"claude-a", "claude-b"} {
			r, err := m.latest(lane)
			if err != nil || r == nil || !r.Resumed || !sameProcess(*r) {
				t.Fatal("resumed monitor child lost")
			}
		}
	}
	_, _ = io.WriteString(web.in, "stats\n")
	var stats struct {
		Polls int64 `json:"polls"`
	}
	controlMonitorRead(t, web, &stats)
	if stats.Polls == 0 {
		t.Error("no web observation occurred")
	}
	slices.Sort(latencies)
	markers, _ := filepath.Glob(filepath.Join(starts, "agent-start-*"))
	t.Logf("attempts=50 succeeded=%d failed=%d controlled_contention=25 monitors=2 web_polls=%d min_ms=%.3f p50_ms=%.3f p95_ms=%.3f max_ms=%.3f agent_starts=%d", len(ids), failures, stats.Polls, float64(latencies[0])/float64(time.Millisecond), float64(latencies[24])/float64(time.Millisecond), float64(latencies[47])/float64(time.Millisecond), float64(latencies[49])/float64(time.Millisecond), len(markers))
	if failures != 0 || len(ids) != 50 || len(markers) != 54 || latencies[49] > 5*time.Second {
		t.Fatal("bounded monitored MCP dispatch series failed")
	}
}
