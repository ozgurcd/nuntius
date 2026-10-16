package nuntius

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestControlOwnerNativeTransactionBound(t *testing.T) {
	m, store := laneControlCommitFixture(t)
	m.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	m.life = ctx // watcher is joined; this proof owns an independent live lease.
	root, err := openLaneControlRoot(store)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	outer, err := laneControlReadLock(root)
	if err != nil {
		t.Fatal(err)
	}
	defer outer.Close()
	started := time.Now()
	guard, err := m.nativeDispatchTransactionLock()
	if guard != nil {
		guard.Close()
		t.Fatal("native transaction acquired over its held guard")
	}
	var busy *laneControlFault
	if !errors.As(err, &busy) || busy.Code != "locked" || !busy.local || time.Since(started) > 2500*time.Millisecond {
		t.Fatal("native local handoff did not retain its finite bound")
	}
}

func TestControlOwnerNativeExternalContention(t *testing.T) {
	m, req, ctx := dispatchFixture(t)
	release := oldControlHolder(t, m.store)
	defer release()
	started := time.Now()
	_, err := m.dispatchGoal(req, ctx, goalCommitIO{})
	var busy *laneControlFault
	if !errors.As(err, &busy) || busy.Code != "locked" || busy.local || time.Since(started) > 600*time.Millisecond {
		t.Fatal("native external contention lost its short refusal")
	}
	dispatchStarts(t, m, 0)
}

func TestControlOwnerNativeFaultHandoff(t *testing.T) {
	for _, stage := range []string{"after_os", "journal_sync", "journal_directory_sync", "publication"} {
		t.Run(stage, func(t *testing.T) {
			m, req, ctx := dispatchFixture(t)
			n := dispatchReopen(t, m)
			workCtx, cancelWork := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancelWork()
			ready, release := make(chan struct{}), make(chan struct{})
			released := false
			defer func() {
				if !released {
					close(release)
				}
			}()
			publishing := false
			fault := goalCommitIO{checkpoint: func(s string) error {
				if s == "running_publication" {
					publishing = true
				}
				if s == stage && (stage == "after_os" || publishing) {
					close(ready)
					select {
					case <-release:
					case <-workCtx.Done():
						return workCtx.Err()
					}
					return errors.New("injected publication boundary failure")
				}
				return nil
			}}
			type outcome struct {
				result goalDispatchResult
				err    error
			}
			first, retry := make(chan outcome, 1), make(chan outcome, 1)
			firstDone, retryStarted, retryDone := false, false, false
			defer func() {
				cancelWork()
				if !firstDone {
					m.cancelLife()
				}
				if retryStarted && !retryDone {
					n.cancelLife()
				}
				if !released {
					close(release)
					released = true
				}
				if !firstDone {
					p0TestJoin(t, first)
				}
				if retryStarted && !retryDone {
					p0TestJoin(t, retry)
				}
			}()
			go func() { r, err := m.dispatchGoal(req, ctx, fault); first <- outcome{r, err} }()
			select {
			case <-ready:
			case <-time.After(10 * time.Second):
				t.Fatal("publication boundary was not reached")
			}
			retryStarted = true
			go func() { r, err := n.dispatchGoal(req, ctx, goalCommitIO{}); retry <- outcome{r, err} }()
			// The other manager is finishing the exact child publication. A
			// single short acquisition must not abandon its durable receipt.
			select {
			case <-retry:
				retryDone = true
				t.Fatal("native retry abandoned the in-process publication holder")
			case <-time.After(450 * time.Millisecond):
			}
			close(release)
			released = true
			initial := p0TestJoin(t, first)
			firstDone = true
			if !errors.Is(initial.err, errGoalLaunchHeld) || initial.result.RunID == "" {
				t.Fatal("publication failure did not retain its child")
			}
			select {
			case again := <-retry:
				retryDone = true
				if again.err != nil || again.result.RunID != initial.result.RunID || again.result.Phase != "running" {
					t.Fatal("same native child was not reconciled", again.err)
				}
			case <-time.After(8 * time.Second):
				t.Fatal("native retry did not finish within its bound")
			}
			dispatchStarts(t, m, 1)
			_, data := dispatchRecord(t, m, req.GoalID)
			if len(data.Dispatches) != 1 || data.Dispatches[0].RunID != initial.result.RunID || data.Dispatches[0].PID <= 1 {
				t.Fatal("exact launch identity was not retained")
			}
		})
	}
}

func TestControlOwnerMCPArgumentAdmission(t *testing.T) {
	m, _ := laneControlCommitFixture(t)
	for _, name := range []string{"nuntius_dispatch", "nuntius_reply"} {
		calls := 0
		gate := func(context.Context) error { calls++; return &laneControlFault{Code: "locked"} }
		if _, err := callContext(context.Background(), m, name, json.RawMessage(`{"unexpected":true}`), gate); err == nil || calls != 0 {
			t.Fatal("invalid arguments entered startup recovery")
		}
		_, err := callContext(context.Background(), m, name, json.RawMessage(`{"lane":"codex","text":"wait"}`), gate)
		var busy *laneControlFault
		if !errors.As(err, &busy) || busy.Code != "locked" || calls != 1 {
			t.Fatal("decoded admission bypassed recovery refusal")
		}
	}
}

func TestControlOwnerNestedAcquire(t *testing.T) {
	for _, kind := range []string{"legacy", "dispatch", "admission", "reader", "native"} {
		t.Run(kind, func(t *testing.T) {
			m, store := laneControlCommitFixture(t)
			m.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			m.life = ctx // joined watcher; keep the local-contention premise live.
			root, err := openLaneControlRoot(store)
			if err != nil {
				t.Fatal("root")
			}
			defer root.Close()
			outer, err := laneControlReadLock(root)
			if err != nil {
				t.Fatal("outer lock")
			}
			defer outer.Close()
			done := make(chan error, 1)
			go func() {
				var inner *controlFileLock
				var err error
				switch kind {
				case "legacy":
					inner, err = m.controlLock()
				case "dispatch":
					inner, err = m.nativeDispatchControlLock()
				case "admission":
					inner, err = m.admissionControlLock()
				case "reader":
					inner, err = laneControlReadLock(root)
				case "native":
					inner, err = laneControlLock(root, unix.LOCK_EX)
				}
				if inner != nil {
					inner.Close()
				}
				done <- err
			}()
			select {
			case err := <-done:
				if laneControlCode(err) != "locked" {
					t.Error("nested acquisition must refuse without changing outer ownership")
				}
				snapshot := m.laneControlFailureSnapshot(nil, err)
				if !strings.Contains(snapshot.Lanes[0].Explanation, "in this process") {
					t.Error("local owner was described as an unknown external holder")
				}
			case <-time.After(600 * time.Millisecond):
				t.Error("nested acquisition exceeded its bound")
				outer.Close()
				cancel()
				p0TestJoin(t, done)
				return
			}
			// A failed inner attempt must not unlock or convert the outer SH lock.
			probe, err := openLaneControlLockFile(root, false)
			if err != nil {
				t.Fatal("probe")
			}
			err = unix.Flock(int(probe.Fd()), unix.LOCK_EX|unix.LOCK_NB)
			probe.File.Close()
			if err != unix.EWOULDBLOCK {
				t.Fatal("inner attempt dropped outer exclusion")
			}
			outer.Close()
			guard, err := m.admissionControlLock()
			if err != nil {
				t.Fatal("released outer still blocks admission")
			}
			guard.Close()
		})
	}
}

func TestControlOwnerAdmissionBriefWait(t *testing.T) {
	m, store := laneControlCommitFixture(t)
	m.Close()
	release := oldControlHolder(t, store)
	done := make(chan error, 1)
	go func() {
		guard, err := m.admissionControlLock()
		if guard != nil {
			guard.Close()
		}
		done <- err
	}()
	// The child is confirmed locked before admission begins.
	time.Sleep(50 * time.Millisecond)
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("brief cross-process contention refused admission")
		}
	case <-time.After(time.Second):
		t.Fatal("admission exceeded its bound")
	}
}

func TestControlOwnerAuditContention(t *testing.T) {
	m, store := laneControlCommitFixture(t)
	m.Close()
	f, err := os.OpenFile(filepath.Join(store, "audit.jsonl"), os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		t.Fatal("audit fixture")
	}
	defer f.Close()
	if unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
		t.Fatal("audit holder")
	}
	defer unix.Flock(int(f.Fd()), unix.LOCK_UN)
	done := make(chan error, 1)
	go func() { done <- m.Freeze("offline audit contention") }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("audit contention was silently accepted")
		}
	case <-time.After(600 * time.Millisecond):
		t.Error("audit wait retained the control owner indefinitely")
		unix.Flock(int(f.Fd()), unix.LOCK_UN)
		<-done
		return
	}
	guard, err := m.admissionControlLock()
	if err != nil {
		t.Fatal("audit failure did not release the control owner")
	}
	guard.Close()
}

func TestControlOwnerMonitoredCrossProcessDispatch(t *testing.T) {
	for _, phase := range []string{"admission", "startup"} {
		t.Run(phase, func(t *testing.T) {
			m, store := laneControlCommitFixture(t)
			if _, err := m.Dispatch("claude", "CONTROL-MONITORED", "wait"); err != nil {
				t.Fatal("monitored run start")
			}
			other, err := NewManager(m.config, store)
			if err != nil {
				t.Fatal("second manager")
			}
			defer other.Close()
			w, err := NewWeb(m, "127.0.0.1:8123")
			if err != nil {
				t.Fatal("web")
			}
			observe, cancelObserve := context.WithTimeout(t.Context(), 30*time.Second)
			done := make(chan error, 1)
			go func() {
				for {
					select {
					case <-observe.Done():
						done <- nil
						return
					default:
					}
					if _, err := m.statusContext(observe, true); err != nil {
						done <- err
						return
					}
					req := httptest.NewRequestWithContext(observe, "GET", "http://localhost:8123/data", nil)
					req.Header.Set("X-Nuntius-Token", w.token)
					out := httptest.NewRecorder()
					w.ServeHTTP(out, req)
					if out.Code != 200 {
						done <- context.Canceled
						return
					}
					if _, err := callContext(observe, other, "nuntius_status", json.RawMessage(`{}`)); err != nil {
						done <- err
						return
					}
					_ = mcpLaneControlSnapshotContext(observe, other)
					if !p0MonitorPause(observe, 5*time.Millisecond) {
						done <- nil
						return
					}
				}
			}()
			defer func() {
				cancelObserve()
				select {
				case err := <-done:
					if err != nil && !errors.Is(err, context.Canceled) {
						t.Error("observation loop failed")
					}
				case <-time.After(2 * time.Second):
					t.Error("observation loop stalled")
				}
			}()
			path := filepath.Join(filepath.Dir(store), "lanes.json")
			b, _ := json.Marshal(m.config)
			if os.WriteFile(path, b, 0600) != nil {
				t.Fatal("config")
			}
			// Keep both watchers and the active follow loop alive across several ticks.
			if !p0MonitorPause(observe, 1200*time.Millisecond) {
				t.Fatal("observer fixture deadline")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, cliBinary, "--config", path, "--store", store, "mcp")
			cmd.WaitDelay = 100 * time.Millisecond
			cmd.Dir = filepath.Dir(store)
			in, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal("process input")
			}
			out, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal("process output")
			}
			var startupRelease func()
			if phase == "startup" {
				startupRelease = oldControlHolder(t, store)
			}
			if cmd.Start() != nil {
				t.Fatal("second process")
			}
			defer func() { in.Close(); cancel(); _ = cmd.Wait() }()
			deadline, _ := ctx.Deadline()
			input, inputOK := in.(*os.File)
			output, outputOK := out.(*os.File)
			if !inputOK || !outputOK || input.SetWriteDeadline(deadline) != nil || output.SetReadDeadline(deadline) != nil {
				t.Fatal("fixture pipe deadlines")
			}
			decoder := json.NewDecoder(out)
			exchange := func(request string) bool {
				if _, err := io.WriteString(in, request+"\n"); err != nil {
					t.Fatal("process write")
				}
				var response struct {
					ID     int `json:"id"`
					Result struct {
						IsError bool `json:"isError"`
					} `json:"result"`
					Error any `json:"error"`
				}
				if err := decoder.Decode(&response); err != nil {
					t.Fatal("response encoding or request deadline")
				}
				return response.Error == nil && !response.Result.IsError
			}
			if !exchange(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`) {
				t.Fatal("initialize")
			}
			if _, err := io.WriteString(in, `{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n"); err != nil {
				t.Fatal("initialized")
			}
			var released chan struct{}
			if phase == "startup" {
				if exchange(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"nuntius_dispatch","arguments":{"lane":"codex","goal_id":"CONTROL-SECOND","text":"wait"}}}`) {
					t.Fatal("startup contention admitted a child")
				}
				startupRelease()
				if exchange(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"nuntius_dispatch","arguments":{"lane":"codex","goal_id":"CONTROL-SECOND","text":"wait","unexpected":true}}}`) {
					t.Fatal("malformed admission accepted")
				}
			} else {
				// Admission begins after startup. Release genuine contention after
				// the request so this exercises retry rather than startup timing.
				release := oldControlHolder(t, store)
				released = make(chan struct{})
				go func() { _ = p0MonitorPause(ctx, 50*time.Millisecond); release(); close(released) }()
				defer func() { cancel(); p0TestJoin(t, released) }()
			}
			accepted := exchange(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"nuntius_dispatch","arguments":{"lane":"codex","goal_id":"CONTROL-SECOND","text":"wait"}}}`)
			if released != nil {
				p0TestJoin(t, released)
			}
			if !accepted {
				t.Fatal("second process could not dispatch under active monitoring and observation")
			}
			if lockContext(ctx, &m.mu) != nil {
				t.Fatal("fixture final read deadline")
			}
			run, err := m.latestContext(ctx, "codex")
			m.mu.Unlock()
			if err != nil || run == nil || run.GoalID != "CONTROL-SECOND" || !sameProcess(*run) {
				t.Fatal("dispatch did not start a fixture child")
			}
		})
	}
}
