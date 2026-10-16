package nuntius

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// This helper runs in a separate test process, using only compiled fake agents.
func TestControlMonitorProcess(t *testing.T) {
	role := os.Getenv("NUNTIUS_CONTROL_MONITOR_ROLE")
	if role == "" {
		return
	}
	// Roles select fixed fixture filenames; never use an arbitrary name as a path.
	profileNames := map[string][3]string{
		"mcp":          {"cpu-mcp.pprof", "block-mcp.pprof", "mutex-mcp.pprof"},
		"mcp-observer": {"cpu-observer.pprof", "block-observer.pprof", "mutex-observer.pprof"},
		"web":          {"cpu-web.pprof", "block-web.pprof", "mutex-web.pprof"},
	}[role]
	if profileNames[0] == "" {
		t.Fatal("invalid fixture role")
	}
	life, stopLife := context.WithTimeout(context.Background(), 8*time.Minute)
	defer stopLife()
	c, err := LoadConfig(os.Getenv("NUNTIUS_CONTROL_MONITOR_CONFIG"))
	if err != nil {
		t.Fatal("fixture config")
	}
	m, err := NewManager(c, os.Getenv("NUNTIUS_CONTROL_MONITOR_STORE"))
	if err != nil {
		t.Fatal("fixture manager")
	}
	defer m.Close()
	if os.Getenv("NUNTIUS_CONTROL_PROFILE") == "1" {
		f, err := privateFile(filepath.Join(filepath.Dir(m.store), profileNames[0]), os.O_WRONLY|os.O_CREATE|os.O_EXCL)
		if err != nil || life.Err() != nil {
			t.Fatal("fixture profile open")
		}
		if pprof.StartCPUProfile(f) != nil {
			f.Close()
			t.Fatal("fixture profile start")
		}
		runtime.SetBlockProfileRate(1)
		previousMutex := runtime.SetMutexProfileFraction(1)
		defer func() {
			pprof.StopCPUProfile()
			if f.Close() != nil {
				t.Error("fixture CPU profile close")
			}
			runtime.SetBlockProfileRate(0)
			runtime.SetMutexProfileFraction(previousMutex)
			for i, kind := range []string{"block", "mutex"} {
				if life.Err() != nil {
					t.Error("fixture profile deadline")
					return
				}
				output, err := privateFile(filepath.Join(filepath.Dir(m.store), profileNames[i+1]), os.O_WRONLY|os.O_CREATE|os.O_EXCL)
				if err != nil {
					t.Error("fixture profile open")
					continue
				}
				err = pprof.Lookup(kind).WriteTo(output, 0)
				closeErr := output.Close()
				if err != nil || closeErr != nil {
					t.Error("fixture profile write")
				}
			}
		}()
	}
	defer func() {
		if !controlMetricsEnabled {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		b, err := json.Marshal(controlMetricsSnapshot())
		if err == nil && ctx.Err() == nil {
			_ = os.WriteFile(filepath.Join(filepath.Dir(m.store), "metrics-"+role+".json"), b, 0600)
		}
	}()
	output, err := controlMonitorOutput(life)
	if err != nil {
		t.Fatal("fixture output deadline unavailable")
	}
	defer output.Close()
	out := json.NewEncoder(output)
	if role == "mcp" || role == "mcp-observer" {
		if lockContext(life, &m.mu) != nil {
			t.Fatal("fixture monitor mutex")
		}
		monitors := len(m.active)
		m.mu.Unlock()
		if role == "mcp" && monitors != 3 {
			t.Fatal("MCP does not own both run monitors")
		}
		_ = out.Encode(map[string]int{"monitors": monitors})
		if Serve(life, m, os.Stdin, output) != nil {
			t.Error("fixture MCP transport")
		}
		return
	}
	w, err := NewWeb(m, "127.0.0.1:8123")
	if err != nil {
		t.Fatal("fixture web")
	}
	pollCtx, stopPoll := context.WithCancel(life)
	done := make(chan struct{})
	var polls atomic.Int64
	go func() {
		defer close(done)
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-pollCtx.Done():
				return
			case <-tick.C:
				for _, path := range []string{"/data", "/data", "/status"} {
					ctx, cancel := context.WithTimeout(pollCtx, time.Second)
					req := httptest.NewRequest("GET", "http://localhost:8123"+path, nil).WithContext(ctx)
					req.Header.Set("X-Nuntius-Token", w.token)
					response := httptest.NewRecorder()
					if path == "/status" {
						w.menubarHandler().ServeHTTP(response, req)
					} else {
						w.ServeHTTP(response, req)
					}
					cancel()
					if response.Code == 200 {
						polls.Add(1)
					}
				}
			}
		}
	}()
	defer func() {
		stopPoll()
		drain, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		select {
		case <-done:
		case <-drain.Done():
			t.Error("fixture poller did not drain; helper process exit fences it")
		}
	}()
	_ = out.Encode(map[string]bool{"web": true})
	input := bufio.NewScanner(os.Stdin)
	for input.Scan() {
		switch input.Text() {
		case "hold":
			// A confirmed web transaction longer than the old 250 ms admission
			// budget makes the contention proof independent of scheduler luck.
			ctx, cancel := context.WithTimeout(life, 3*time.Second)
			began := time.Now()
			if err := lockContext(ctx, &m.mu); err != nil {
				_ = out.Encode(map[string]any{"held": false, "fault": "mutex", "mutex_ms": float64(time.Since(began)) / float64(time.Millisecond)})
				cancel()
				t.Fatal("fixture web mutex")
			}
			mutexWait := time.Since(began)
			began = time.Now()
			guard, err := m.admissionControlLockWithin(dispatchControlLockWait, ctx)
			if err != nil {
				m.mu.Unlock()
				_ = out.Encode(map[string]any{"held": false, "fault": "flock", "mutex_ms": float64(mutexWait) / float64(time.Millisecond), "flock_ms": float64(time.Since(began)) / float64(time.Millisecond)})
				cancel()
				t.Fatal("fixture web transaction")
			}
			_ = out.Encode(map[string]any{"held": true, "mutex_ms": float64(mutexWait) / float64(time.Millisecond), "flock_ms": float64(time.Since(began)) / float64(time.Millisecond)})
			timer := time.NewTimer(350 * time.Millisecond)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
			}
			guard.Close()
			m.mu.Unlock()
			cancel()
			_ = out.Encode(map[string]bool{"released": true})
		case "stats":
			_ = out.Encode(map[string]int64{"polls": polls.Load()})
		}
	}
}

type controlMonitorPeer struct {
	in   *controlMonitorPipe
	pipe *os.File
	out  *json.Decoder
	ctx  context.Context
}

func controlMonitorOutput(ctx context.Context) (*controlMonitorPipe, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Exec inherits a blocking fd1; os.Stdout was constructed before it could
	// join Go's poller. Own a nonblocking duplicate so deadlines really work.
	fd, err := unix.Dup(int(os.Stdout.Fd()))
	if err != nil {
		return nil, err
	}
	unix.CloseOnExec(fd)
	if err = unix.SetNonblock(fd, true); err != nil {
		unix.Close(fd)
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "fixture-output")
	writer := &controlMonitorPipe{file, ctx}
	deadline, _ := ctx.Deadline()
	if err := writer.SetWriteDeadline(minTime(deadline, time.Now().Add(5*time.Second))); err != nil {
		file.Close()
		return nil, err
	}
	return writer, nil
}

func TestP0FixtureInheritedOutput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if os.Getenv("NUNTIUS_CONTROL_OUTPUT_PROBE") == "1" {
		writer, err := controlMonitorOutput(ctx)
		if errors.Is(err, os.ErrNoDeadline) {
			t.Fatal("inherited_pipe_deadline_unsupported")
		}
		if err != nil {
			t.Fatal("fixture output construction")
		}
		defer writer.Close()
		if _, err = writer.Write([]byte("probe\n")); err != nil {
			t.Fatal("fixture output write")
		}
		return
	}
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal("fixture output pipe")
	}
	defer read.Close()
	defer write.Close()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestP0FixtureInheritedOutput$")
	cmd.WaitDelay = 100 * time.Millisecond
	cmd.Env = append(os.Environ(), "NUNTIUS_CONTROL_OUTPUT_PROBE=1")
	cmd.Stdout, cmd.Stderr = write, os.Stderr
	err = cmd.Run()
	write.Close()
	if read.SetReadDeadline(time.Now().Add(time.Second)) != nil {
		t.Fatal("fixture output read deadline")
	}
	b, readErr := io.ReadAll(io.LimitReader(read, 4096))
	if err != nil || readErr != nil || !strings.Contains(string(b), "probe\n") {
		t.Fatalf("fixture inherited output failed unsupported=%t", strings.Contains(string(b), "inherited_pipe_deadline_unsupported"))
	}
}

type controlMonitorPipe struct {
	*os.File
	ctx context.Context
}

func (p *controlMonitorPipe) Write(b []byte) (int, error) {
	if err := p.ctx.Err(); err != nil {
		return 0, err
	}
	d, _ := p.ctx.Deadline()
	if err := p.SetWriteDeadline(minTime(d, time.Now().Add(5*time.Second))); err != nil {
		return 0, err
	}
	return p.File.Write(b)
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
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestControlMonitorProcess$")
	cmd.WaitDelay = 100 * time.Millisecond
	cmd.Env = append(os.Environ(), "NUNTIUS_CONTROL_MONITOR_ROLE="+role,
		"NUNTIUS_CONTROL_MONITOR_CONFIG="+config, "NUNTIUS_CONTROL_MONITOR_STORE="+store)
	cmd.Dir = filepath.Dir(store)
	input, in, err := os.Pipe()
	if err != nil {
		cancel()
		t.Fatal("fixture input")
	}
	out, output, err := os.Pipe()
	if err != nil {
		input.Close()
		in.Close()
		cancel()
		t.Fatal("fixture output")
	}
	cmd.Stdin, cmd.Stdout = input, output
	if cmd.Start() != nil {
		input.Close()
		in.Close()
		out.Close()
		output.Close()
		cancel()
		t.Fatal("fixture process start")
	}
	input.Close()
	output.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }() // command ctx bounds Wait; cleanup owns and joins it
	t.Cleanup(func() {
		in.Close()
		drain, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		select {
		case err := <-done:
			if err != nil {
				t.Error("fixture process exit")
			}
		case <-drain.Done():
			cancel()
			killed, stopKilled := context.WithTimeout(context.Background(), time.Second)
			defer stopKilled()
			select {
			case <-done:
			case <-killed.Done():
				t.Error("fixture waiter did not join after cancellation")
			}
			t.Error("fixture process did not close")
		}
		cancel()
		out.Close()
	})
	return controlMonitorPeer{in: &controlMonitorPipe{in, ctx}, pipe: out, out: json.NewDecoder(out), ctx: ctx}
}

func controlMonitorRead(t *testing.T, peer controlMonitorPeer, value any) {
	t.Helper()
	deadline, _ := peer.ctx.Deadline()
	if peer.ctx.Err() != nil || peer.pipe.SetReadDeadline(minTime(deadline, time.Now().Add(5*time.Second))) != nil {
		t.Fatal("fixture response deadline")
	}
	if err := peer.out.Decode(value); err != nil {
		kind := "decode"
		var syntax *json.SyntaxError
		switch {
		case errors.Is(err, os.ErrDeadlineExceeded):
			kind = "read_deadline"
		case errors.Is(err, io.EOF):
			kind = "eof"
		case errors.As(err, &syntax):
			kind = "syntax"
		}
		t.Fatalf("fixture response encoding or deadline code=%s", kind)
	}
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func TestControlMonitorMCPDispatchSeries(t *testing.T) {
	for _, floor := range []int{970, 2000} {
		t.Run(fmt.Sprint(floor), func(t *testing.T) { testP0MonitorSeries(t, floor) })
	}
}

func testP0MonitorSeries(t *testing.T, floor int) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	c, store := fixture(t)
	if os.Getenv("NUNTIUS_CONTROL_PROFILE") == "1" {
		// Both roots are fixture-owned. The only output names are constants.
		destination, err := os.Getwd()
		if err != nil {
			t.Fatal("fixture profile destination")
		}
		files := map[int][]string{
			970: {
				"Q10-970-mcp-cpu.pprof", "Q10-970-observer-cpu.pprof", "Q10-970-web-cpu.pprof",
				"Q10-970-mcp-block.pprof", "Q10-970-observer-block.pprof", "Q10-970-web-block.pprof",
				"Q10-970-mcp-mutex.pprof", "Q10-970-observer-mutex.pprof", "Q10-970-web-mutex.pprof",
			},
			2000: {
				"Q10-2000-mcp-cpu.pprof", "Q10-2000-observer-cpu.pprof", "Q10-2000-web-cpu.pprof",
				"Q10-2000-mcp-block.pprof", "Q10-2000-observer-block.pprof", "Q10-2000-web-block.pprof",
				"Q10-2000-mcp-mutex.pprof", "Q10-2000-observer-mutex.pprof", "Q10-2000-web-mutex.pprof",
			},
		}[floor]
		if len(files) != 9 {
			t.Fatal("invalid profile floor")
		}
		t.Cleanup(func() {
			preserve, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			for i, name := range []string{
				"cpu-mcp.pprof", "cpu-observer.pprof", "cpu-web.pprof",
				"block-mcp.pprof", "block-observer.pprof", "block-web.pprof",
				"mutex-mcp.pprof", "mutex-observer.pprof", "mutex-web.pprof",
			} {
				if preserve.Err() != nil {
					t.Error("fixture profile preservation deadline")
					return
				}
				input, err := privateFile(filepath.Join(filepath.Dir(store), name), os.O_RDONLY)
				if err != nil {
					t.Error("fixture profile read")
					continue
				}
				b, err := io.ReadAll(io.LimitReader(input, (8<<20)+1))
				closeErr := input.Close()
				if err != nil || closeErr != nil || len(b) > 8<<20 || preserve.Err() != nil {
					t.Error("fixture profile bounds")
					continue
				}
				output, err := privateFile(filepath.Join(destination, ".gograph", files[i]), os.O_WRONLY|os.O_CREATE|os.O_EXCL)
				if err != nil {
					t.Error("fixture profile preservation open")
					continue
				}
				_, err = output.Write(b)
				closeErr = output.Close()
				if err != nil || closeErr != nil {
					t.Error("fixture profile preservation")
				}
			}
		})
	}
	if controlMetricsEnabled {
		t.Cleanup(func() {
			for _, role := range []string{"mcp", "mcp-observer", "web"} {
				b, err := os.ReadFile(filepath.Join(filepath.Dir(store), "metrics-"+role+".json"))
				var metrics map[string]any
				if err != nil || len(b) > 16384 || json.Unmarshal(b, &metrics) != nil {
					t.Error("missing bounded process histograms")
					continue
				}
				t.Logf("history_floor=%d process=%s histograms=%s", floor, role, b)
			}
		})
	}
	off := false
	c.AutoResume, c.MaxRuntime = &off, "10m"
	claude := c.Lanes[0]
	claude.StartMode = "as_is"
	claude.Name = "claude-a"
	c.Lanes[0] = claude
	claude.Name = "claude-b"
	c.Lanes = append(c.Lanes, claude)
	claude.Name = "claude-c"
	c.Lanes = append(c.Lanes, claude)
	m, err := NewManager(c, store)
	if err != nil {
		t.Fatal("seed manager")
	}
	defer m.Close()
	for i := range floor {
		if ctx.Err() != nil {
			t.Fatal("history seed deadline")
		}
		r := seedCrashRun(t, m, fmt.Sprintf("%032x", i+1), fmt.Sprintf("P0-HISTORY-%d", i), "completed")
		r.State, r.PID = "ended", 0
		end := r.Started
		r.Ended = &end
		if err := m.save(&r); err != nil {
			t.Fatal("history seed")
		}
	}
	starts := t.TempDir()
	// The seed manager intentionally detaches; fixture() owns final teardown.
	// Register the marker destination without an owning-manager cleanup helper.
	for _, lane := range c.Lanes {
		if os.WriteFile(filepath.Join(lane.Workdir, testAgentStartPointer), []byte(starts), 0600) != nil {
			t.Fatal("fixture marker registration")
		}
	}
	for index, lane := range []string{"claude-a", "claude-b", "claude-c"} {
		if _, err = m.Dispatch(lane, fmt.Sprintf("MONITOR-SEED-%d", index), "ack-goal"); err != nil {
			t.Fatal("seed dispatch")
		}
		waitState(t, m, lane, "ended")
		text := "wait"
		if lane == "claude-c" {
			text = "p0-timeout"
		}
		if _, err = m.Reply(lane, text); err != nil {
			t.Fatal("seed resume")
		}
		waitReady(t, m, lane)
	}
	// Make one timeout due during the soak. The two other monitored runs stay
	// active throughout; only the private record's elapsed runtime is seeded.
	timeoutRun, err := m.latest("claude-c")
	if err != nil || timeoutRun == nil {
		t.Fatal("timeout fixture identity")
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
	if ready.Monitors != 3 {
		t.Fatal("wrong MCP monitor count")
	}
	observer := controlMonitorStart(t, "mcp-observer", config, store)
	var observerReady struct {
		Monitors int `json:"monitors"`
	}
	controlMonitorRead(t, observer, &observerReady)
	if observerReady.Monitors != 0 {
		t.Fatal("second MCP stole supervision")
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
		if response.Error != nil {
			text = `{"code":"jsonrpc_error"}`
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
	// Seed elapsed time only after adoption. Before adoption, an artificial
	// timestamp older than its parent would change latest-run selection.
	guard, err := m.admissionControlLockWithin(dispatchControlLockWait, ctx)
	if err != nil {
		t.Fatal("timeout seed control guard")
	}
	timeoutRun.Started = time.Now().Add(-10*time.Minute + 3*time.Second)
	timeoutDue := timeoutRun.Started.Add(10 * time.Minute)
	err = m.save(timeoutRun)
	guard.Close()
	if err != nil {
		t.Fatal("timeout fixture seed")
	}
	if !p0MonitorPause(ctx, 1100*time.Millisecond) {
		t.Fatal("fixture warm-up deadline")
	}
	latencies := make([]time.Duration, 0, 50)
	ids := map[string]bool{}
	failures := 0
	defer func() {
		values := make([]float64, len(latencies))
		for i, elapsed := range latencies {
			values[i] = float64(elapsed) / float64(time.Millisecond)
		}
		t.Logf("history_floor=%d recorded_attempts=%d succeeded=%d refused=%d latencies_ms=%v", floor, len(latencies), len(ids), failures, values)
		if len(latencies) == 0 || len(latencies) == 50 {
			return
		}
		slices.Sort(latencies)
		n := len(latencies)
		t.Logf("history_floor=%d partial_attempts=%d succeeded=%d refused=%d min_ms=%.3f p50_ms=%.3f p95_ms=%.3f max_ms=%.3f", floor, n, len(ids), failures, float64(latencies[0])/float64(time.Millisecond), float64(latencies[(n+1)/2-1])/float64(time.Millisecond), float64(latencies[(95*n+99)/100-1])/float64(time.Millisecond), float64(latencies[n-1])/float64(time.Millisecond))
	}()
	for trial := range 50 {
		if ctx.Err() != nil {
			t.Fatal("dispatch series deadline")
		}
		contended := trial%2 == 0
		if contended {
			_, _ = io.WriteString(web.in, "hold\n")
			var held struct {
				Held    bool    `json:"held"`
				Fault   string  `json:"fault"`
				MutexMS float64 `json:"mutex_ms"`
				FlockMS float64 `json:"flock_ms"`
			}
			controlMonitorRead(t, web, &held)
			if held.Fault == "mutex" || held.Fault == "flock" {
				t.Fatalf("fixture web acquisition trial=%d contended=true code=%s mutex_ms=%.3f flock_ms=%.3f", trial, held.Fault, held.MutexMS, held.FlockMS)
			}
			if !held.Held {
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
			code := "unclassified"
			var refused struct {
				Code string `json:"code"`
			}
			if json.Unmarshal([]byte(text), &refused) == nil {
				switch refused.Code {
				case "locked", "lane_control_unavailable", "invalid_request", "lane_paused", "launch_uncertain", "uncertain", "jsonrpc_error":
					code = refused.Code
				}
			} else {
				switch text {
				case "lane is busy or lock unavailable", "lane has a surviving process", "pending_message_requires_review", "waiting_to_resume", "server is closing", "cannot read detached identity":
					code = text
				}
			}
			t.Logf("history_floor=%d refused_trial=%d contended=%t code=%s", floor, trial, contended, code)
			continue
		}
		var result struct {
			RunID string `json:"run_id"`
		}
		if json.Unmarshal([]byte(text), &result) != nil {
			t.Fatal("dispatch receipt identity")
		}
		runs, err := m.historyContext(ctx)
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
			if !p0MonitorPause(ctx, 10*time.Millisecond) {
				t.Fatal("fixture child deadline")
			}
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
	b, err = os.ReadFile(filepath.Join(claude.Workdir, "P0-SIGINT-AT.json"))
	var signalAt int64
	if err != nil || json.Unmarshal(b, &signalAt) != nil {
		t.Fatal("timeout signal not observed")
	}
	stopLatency := time.Unix(0, signalAt).Sub(timeoutDue)
	t.Logf("history_floor=%d initial_timeout_sigint_ms=%.3f", floor, float64(stopLatency)/float64(time.Millisecond))
	if stopLatency < 0 || stopLatency > 5*time.Second {
		t.Error("initial timeout SIGINT exceeded the P-T3b regression threshold")
	}
	t.Logf("history_floor=%d mcp_processes=2 tabs=2 menu=1 attempts=50 succeeded=%d failed=%d controlled_contention=25 monitors_start=3 persistent_monitors=2 web_polls=%d min_ms=%.3f p50_ms=%.3f p95_ms=%.3f max_ms=%.3f agent_starts=%d", floor, len(ids), failures, stats.Polls, float64(latencies[0])/float64(time.Millisecond), float64(latencies[24])/float64(time.Millisecond), float64(latencies[47])/float64(time.Millisecond), float64(latencies[49])/float64(time.Millisecond), len(markers))
	if failures != 0 || len(ids) != 50 || len(markers) != 56 || latencies[49] > 5*time.Second {
		t.Fatal("bounded monitored MCP dispatch series failed")
	}
	// With 50 observations, nearest-rank p99 is ceil(.99*50)=50.
	// owner-approved 20:38Z: race gates correctness; installed build gates p99.
	t.Logf("race_build=%t p99_ms=%.3f latency_gating=%t", p0RaceBuild, float64(latencies[49])/float64(time.Millisecond), !p0RaceBuild)
	if !p0RaceBuild && latencies[49] >= 1500*time.Millisecond {
		t.Fatal("field p99 exceeded the P0 1.5s target")
	}
}

func p0MonitorPause(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
