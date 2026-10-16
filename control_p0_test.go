package nuntius

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestP0ObservationHistoryNoRescan(t *testing.T) {
	c, store := fixture(t)
	m, err := newReadManager(c, filepath.Join(store, "absent"))
	if err != nil {
		t.Fatal("observer fixture")
	}
	defer m.cancelLife()
	m.observation = &displaySnapshot{runs: []Run{{ID: strings.Repeat("a", 32), Lane: "codex", State: "ended"}}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	runs, err := m.historyContext(ctx)
	if err != nil || len(runs) != 1 || runs[0].State != "ended" {
		t.Fatal("read-only observation rescanned the store")
	}
	runs[0].State = "running"
	if m.observation.runs[0].State != "ended" {
		t.Fatal("history aliased the cached slice")
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if _, err := m.historyContext(canceled); !errors.Is(err, context.Canceled) {
		t.Fatal("cached read ignored cancellation")
	}
	m.readOnly = false
	if _, err := m.historyContext(ctx); err == nil {
		t.Fatal("authoritative read used an advisory snapshot")
	}
	m.readOnly, m.observation = true, nil
	if _, err := m.historyContext(ctx); err == nil {
		t.Fatal("uncached observer did not read the store")
	}
}

// Display is advisory: an unrelated writer does not establish ineligibility.
func TestP0DisplayWriterHold(t *testing.T) {
	m, _, store := manager(t)
	w, err := NewWeb(m, "127.0.0.1:8123")
	if err != nil {
		t.Fatal(err)
	}
	release := oldControlHolder(t, store)
	defer release()
	var readers uint64
	if controlMetricsEnabled {
		for i := range controlHistograms[1][0] {
			readers += controlHistograms[1][0][i].Load()
		}
	}
	for _, path := range []string{"mcp", "/status", "/data"} {
		start := time.Now()
		var value any
		if path == "mcp" {
			value, err = callContext(context.Background(), m, "nuntius_status", json.RawMessage(`{}`))
		} else {
			r := httptest.NewRequest("GET", "http://localhost:8123"+path, nil)
			r.Header.Set("X-Nuntius-Token", w.token)
			out := httptest.NewRecorder()
			if path == "/status" {
				w.menubarHandler().ServeHTTP(out, r)
			} else {
				w.ServeHTTP(out, r)
			}
			if out.Code != 200 {
				t.Fatalf("%s did not answer", path)
			}
			if json.Unmarshal(out.Body.Bytes(), &value) != nil {
				t.Fatalf("%s invalid response", path)
			}
			if path == "/status" {
				continue
			}
		}
		if err != nil || time.Since(start) >= time.Second {
			t.Fatalf("%s display blocked", path)
		}
		b, _ := json.Marshal(value)
		var v struct {
			Lanes []struct {
				LaneControl laneControlView `json:"lane_control"`
			} `json:"lanes"`
		}
		if json.Unmarshal(b, &v) != nil || len(v.Lanes) == 0 {
			t.Fatalf("%s missing lanes", path)
		}
		for _, row := range v.Lanes {
			if !row.LaneControl.DispatchEligible {
				t.Fatalf("%s asserted idle lane blocked: %v", path, row.LaneControl.Blockers)
			}
		}
	}
	if controls := mcpLaneControlSnapshot(m); controls == nil {
		t.Fatal("MCP control response unavailable during a writer hold")
	}
	if controlMetricsEnabled {
		var after uint64
		for i := range controlHistograms[1][0] {
			after += controlHistograms[1][0][i].Load()
		}
		if after != readers {
			t.Fatal("advisory display attempted a shared control lock")
		}
	}
}

func TestP0CancellableWaits(t *testing.T) {
	m, _, store := manager(t)
	release := oldControlHolder(t, store)
	defer release()
	root, err := openLaneControlRoot(store)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	lock, err := openLaneControlLockFile(root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.File.Close()
	for _, operation := range []string{"flock", "mutex", "cache", "dispatch"} {
		t.Run(operation, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var mu sync.Mutex
			if !mu.TryLock() {
				t.Fatal("fixture mutex")
			}
			defer mu.Unlock()
			if operation == "cache" {
				if !m.display.mu.TryLock() {
					t.Fatal("fixture cache")
				}
				defer m.display.mu.Unlock()
			}
			done := make(chan error, 1)
			started := make(chan struct{})
			go func() {
				close(started)
				var err error
				switch operation {
				case "flock":
					err = boundedFlock(int(lock.Fd()), unix.LOCK_EX, time.Second, ctx)
				case "mutex":
					err = lockContext(ctx, &mu)
				case "cache":
					_, err = m.displaySnapshot(ctx)
				case "dispatch":
					_, err = m.DispatchContext(ctx, "codex", "P0-CANCEL-WAIT", "ack-goal")
				}
				done <- err
			}()
			select {
			case <-started:
			case <-time.After(time.Second):
				cancel()
				t.Fatal("waiter did not start")
			}
			barrier := time.NewTimer(20 * time.Millisecond)
			select {
			case err := <-done:
				barrier.Stop()
				cancel()
				t.Fatalf("wait did not contend: %v", err)
			case <-barrier.C:
			}
			start := time.Now()
			cancel()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("canceled waiter succeeded")
				}
			case <-time.After(100 * time.Millisecond):
				t.Error("canceled waiter did not return")
			}
			if time.Since(start) > 10*time.Millisecond {
				t.Errorf("%s cancellation exceeded 10ms", operation)
			}
		})
	}
	if runs, err := m.history(); err != nil || len(runs) != 0 {
		t.Fatal("canceled admission produced effects")
	}
}

func TestP0DispatchDeadlineNoEffects(t *testing.T) {
	for _, mode := range []string{"manager-mutex", "cross-process-control"} {
		t.Run(mode, func(t *testing.T) {
			m, _, store := manager(t)
			if mode == "manager-mutex" {
				setup, cancel := context.WithTimeout(context.Background(), time.Second)
				if lockContext(setup, &m.mu) != nil {
					cancel()
					t.Fatal("fixture manager mutex")
				}
				cancel()
				// Bound the adversarial owner too: a broken implementation may
				// ignore the request deadline, but this fixture still releases it.
				releaseCtx, release := context.WithTimeout(context.Background(), 80*time.Millisecond)
				done := make(chan struct{})
				go func(ctx context.Context) { defer close(done); <-ctx.Done(); m.mu.Unlock() }(releaseCtx)
				defer func() {
					release()
					join, stop := context.WithTimeout(context.Background(), time.Second)
					defer stop()
					select {
					case <-done:
					case <-join.Done():
						t.Error("fixture owner did not join")
					}
				}()
			} else {
				defer oldControlHolder(t, store)()
			}
			before := admissionArtifacts(t, store)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			deadline, _ := ctx.Deadline()
			id, err := m.DispatchContext(ctx, "codex", "P0-DEADLINE", "ack-goal")
			returned := time.Now()
			if id != "" || laneControlCode(err) != "locked" || ctx.Err() != context.DeadlineExceeded || returned.After(deadline.Add(15*time.Millisecond)) {
				t.Fatal("dispatch deadline failed its 15ms allowance")
			}
			if fmt.Sprint(before) != fmt.Sprint(admissionArtifacts(t, store)) {
				t.Fatal("deadline refusal changed run artifacts")
			}
			t.Logf("wait=%s overshoot_ms=%.3f effects=0", mode, float64(returned.Sub(deadline))/float64(time.Millisecond))
		})
	}
}

func TestP0LocalOwnerDeadline(t *testing.T) {
	m, _, _ := manager(t)
	guard, err := m.controlLock()
	if err != nil {
		t.Fatal("fixture local owner")
	}
	defer guard.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	deadline, _ := ctx.Deadline()
	next, err := m.admissionControlLockWithin(time.Second, ctx)
	if next != nil {
		next.Close()
		t.Fatal("nested owner was replaced")
	}
	if laneControlCode(err) != "locked" || ctx.Err() != context.DeadlineExceeded || time.Now().After(deadline.Add(10*time.Millisecond)) {
		t.Fatal("local owner deadline exceeded 10ms")
	}
}

func TestP0StopRecordedAndIdentity(t *testing.T) {
	m, _, _ := manager(t)
	m.Grace = 50 * time.Millisecond
	id, err := m.Dispatch("codex", "P0-STOP-RECORDED", "ignore-int")
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, m, "codex")
	r, _ := loadRun(m.store+"/runs/"+id, id)
	guard, err := m.controlLock()
	if err != nil {
		t.Fatal(err)
	}
	err = m.requestStopContext(context.Background(), r, "cancelled", func(Run, syscall.Signal) error { return errors.New("offline signal fault") })
	guard.Close()
	var outcome *operationOutcome
	if !errors.As(err, &outcome) || outcome.Code != "stop_recorded" {
		t.Fatal("lost durable stop outcome")
	}
	wire := mcpToolResult("nuntius_cancel", nil, err)
	if wire["structuredContent"].(map[string]any)["code"] != "stop_recorded" {
		t.Fatal("wire lost stop acceptance")
	}
	wrong := *r
	wrong.ProcessStart += "-reused"
	if m.escalate(context.Background(), wrong, 0) || !sameProcess(*r) {
		t.Fatal("reused identity signalled")
	}
	waitState(t, m, "codex", "ended")
}

func TestP0AdvisoryAdmissionAgreement(t *testing.T) {
	m, _, _ := manager(t)
	for _, paused := range []bool{true, false} {
		revision := uint64(0)
		if !paused {
			revision = 1
		}
		if _, err := m.setLaneControl(laneControlChange{Lane: "codex", Paused: paused, ExpectedRevision: &revision}, "mcp"); err != nil {
			t.Fatal(err)
		}
		view, err := m.StatusViewContext(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var eligible bool
		for _, row := range view.(map[string]any)["lane_control"].(laneControlSnapshot).Lanes {
			if row.Name == "codex" {
				eligible = row.DispatchEligible
			}
		}
		guard, err := m.controlLock()
		if err != nil {
			t.Fatal(err)
		}
		admit := m.admitLaneLocked("codex")
		guard.Close()
		if eligible != (admit == nil) || eligible == paused {
			t.Fatal("status and admission disagree")
		}
	}
}

func TestP0DisplayGoroutineCensus(t *testing.T) {
	m, _, _ := manager(t)
	before := runtime.NumGoroutine()
	for range 100 {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := m.displaySnapshot(ctx); err == nil {
			t.Fatal("canceled display succeeded")
		}
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Fatal("display cancellation added a goroutine")
	}
	if m.followCount.Load() != 0 {
		t.Fatal("display adopted a run")
	}
}

func TestP0MaxLaneGoroutineCensus(t *testing.T) {
	c, store := fixture(t)
	off := false
	c.Notifications, c.AutoResume, c.MaxRuntime = &off, &off, "10m"
	lane := c.Lanes[1]
	c.Lanes = nil
	for i := range 24 {
		copy := lane
		copy.Name = fmt.Sprintf("p0-%02d", i)
		copy.StartMode = "as_is"
		c.Lanes = append(c.Lanes, copy)
	}
	before := runtime.NumGoroutine()
	m, err := NewManager(c, store)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	for i, lane := range c.Lanes {
		if _, err := m.Dispatch(lane.Name, fmt.Sprintf("P0-CENSUS-%d", i), "wait"); err != nil {
			t.Fatal(err)
		}
		waitReady(t, m, lane.Name)
	}
	if m.followCount.Load() != 24 || runtime.NumGoroutine() > before+32+2*24 {
		t.Fatal("active-run census exceeded declared P0 growth")
	}
	for _, lane := range c.Lanes {
		if err := m.Cancel(lane.Name); err != nil {
			t.Fatal(err)
		}
	}
	for _, lane := range c.Lanes {
		waitState(t, m, lane.Name, "ended")
	}
	if err := m.Close(); err != nil || m.followCount.Load() != 0 {
		t.Fatal("supervision did not drain")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for runtime.NumGoroutine() > before {
		select {
		case <-ctx.Done():
			t.Fatal("goroutine growth after shutdown")
		case <-tick.C:
		}
	}
}

func TestP0AcceptedFaultRetainsSupervision(t *testing.T) {
	m, _, _ := manager(t)
	m.Grace = 50 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := lockContext(ctx, &m.mu); err != nil {
		t.Fatal(err)
	}
	guard, err := m.admissionControlLockWithin(dispatchControlLockWait, ctx)
	if err != nil {
		m.mu.Unlock()
		t.Fatal(err)
	}
	lane, _ := m.lane("codex")
	lock, err := m.lock(lane.Name)
	if err != nil {
		guard.Close()
		m.mu.Unlock()
		t.Fatal(err)
	}
	launch := &goalLaunchContext{id: strings.Repeat("e", 32), beforeStart: func() error { return nil }, fault: goalCommitIO{checkpoint: func(at string) error {
		if at == "after_os" {
			return errors.New("offline accepted fault")
		}
		return nil
	}}}
	id, err := m.launchMessage(lane, "P0-ACCEPTED-FAULT", "ignore-int", "", lock, nil, nil, nil, nil, launch)
	var outcome *operationOutcome
	supervised := m.active[lane.Name] != nil
	guard.Close()
	m.mu.Unlock()
	if id != launch.id || !errors.As(err, &outcome) || outcome.Code != "launch_uncertain" || !supervised {
		t.Fatal("accepted launch lost identity or supervision")
	}
	waitReady(t, m, lane.Name)
	if _, err = m.Dispatch(lane.Name, "P0-RETRY-FAULT", "ack-goal"); err == nil {
		t.Fatal("uncertain child duplicated")
	}
	if runs, err := m.history(); err != nil || len(runs) != 1 {
		t.Fatal("retry produced a second run")
	}
	if err := m.Cancel(lane.Name); err != nil {
		t.Fatal(err)
	}
	waitState(t, m, lane.Name, "ended")
}

func TestP0EscalationOutsideManagerAndControlLocks(t *testing.T) {
	m, _, store := manager(t)
	m.Grace = 100 * time.Millisecond
	id, err := m.Dispatch("codex", "P0-ESCALATE", "ignore-int")
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, m, "codex")
	if err = m.Cancel("codex"); err != nil {
		t.Fatal(err)
	}
	r, err := loadRun(m.store+"/runs/"+id, id)
	if err != nil || r == nil || r.StopAt == nil {
		t.Fatal("stop not durable")
	}
	release := oldControlHolder(t, store)
	defer release()
	m.mu.Lock()
	defer m.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for sameProcess(*r) {
		select {
		case <-ctx.Done():
			t.Fatal("durable stop escalation waited on a lock")
		case <-tick.C:
		}
	}
}
