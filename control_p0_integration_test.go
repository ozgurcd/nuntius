//go:build integration

package nuntius

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const p0Version034 = "caaafaff01aedccd608436aa0c162e9c8f04f2f3"
const p0Version035 = "ee2c5d25f44117e88cdc7d313e5e4309585af9b5"

// Only immutable source/embedded assets are extracted. No environment files,
// settings, binaries, credentials or owner stores are copied into a fixture.
func p0PinnedBinary(t *testing.T, revision string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	dir := t.TempDir()
	archive := exec.CommandContext(ctx, "git", "archive", revision, "--", "*.go", "go.mod", "go.sum", "index.html", "app.css", "app.js", "assets/brand/*.svg")
	archive.WaitDelay = 100 * time.Millisecond
	b, err := archive.Output()
	if err != nil || len(b) > 32<<20 {
		t.Fatal("pinned source archive unavailable")
	}
	reader := tar.NewReader(bytes.NewReader(b))
	for {
		if ctx.Err() != nil {
			t.Fatal("pinned source extraction timed out")
		}
		h, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal("pinned source archive invalid")
		}
		name := filepath.Clean(h.Name)
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			t.Fatal("pinned archive escaped fixture")
		}
		if h.Typeflag == tar.TypeDir {
			continue
		}
		if h.Typeflag != tar.TypeReg || h.Size > 4<<20 {
			t.Fatal("pinned archive contains unsupported entry")
		}
		allowed := strings.HasSuffix(name, ".go") || name == "go.mod" || name == "go.sum" || name == "index.html" || name == "app.css" || name == "app.js" || strings.HasPrefix(name, "assets/brand/") && strings.HasSuffix(name, ".svg")
		if !allowed {
			t.Fatal("pinned archive contains unexpected file")
		}
		path := filepath.Join(dir, name)
		if os.MkdirAll(filepath.Dir(path), 0700) != nil {
			t.Fatal("fixture directory")
		}
		if ctx.Err() != nil {
			t.Fatal("pinned extraction canceled")
		}
		data, err := io.ReadAll(io.LimitReader(reader, (4<<20)+1))
		if err != nil || int64(len(data)) != h.Size || os.WriteFile(path, data, 0600) != nil {
			t.Fatal("fixture source extraction")
		}
	}
	binary := filepath.Join(dir, "pinned-nuntius")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/nuntius")
	build.Dir = dir
	build.Env = append(os.Environ(), "GOWORK=off", "GOTOOLCHAIN=local", "GOPROXY=off")
	build.WaitDelay = 100 * time.Millisecond
	if out, err := build.CombinedOutput(); err != nil {
		t.Logf("pinned_build_bytes=%d", len(out))
		t.Fatal("offline pinned binary build failed")
	}
	data, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal("pinned binary fingerprint")
	}
	t.Logf("pinned_revision=%s binary_sha256=%x", revision, sha256.Sum256(data))
	return binary
}

type p0Peer struct {
	ctx     context.Context
	cancel  context.CancelFunc
	cmd     *exec.Cmd
	in, out *os.File
	id      int
	closed  bool
	control struct {
		Code     string  `json:"code"`
		Changed  bool    `json:"changed"`
		Revision *uint64 `json:"revision"`
	}
}

func p0StartPeer(t *testing.T, binary, config, store string) *p0Peer {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	cmd := exec.CommandContext(ctx, binary, "--config", config, "--store", store, "mcp")
	cmd.WaitDelay = 100 * time.Millisecond
	in, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal("offline MCP input")
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		in.Close()
		cancel()
		t.Fatal("offline MCP output")
	}
	p := &p0Peer{ctx: ctx, cancel: cancel, cmd: cmd, in: in.(*os.File), out: out.(*os.File)}
	if cmd.Start() != nil {
		in.Close()
		out.Close()
		cancel()
		t.Fatal("offline MCP start")
	}
	t.Cleanup(func() {
		if p.closed {
			return
		}
		p.in.Close()
		// CommandContext owns cancellation and bounds Wait; no waiter goroutine.
		err := p.cmd.Wait()
		p.cancel()
		p.out.Close()
		p.closed = true
		if err != nil {
			t.Error("offline MCP did not close cleanly")
		}
	})
	p.send(t, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"})
	if p.receive(t)["error"] != nil {
		t.Fatal("offline MCP initialize")
	}
	p.send(t, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	p.id = 1
	return p
}

func (p *p0Peer) send(t *testing.T, request any) {
	t.Helper()
	if p.ctx.Err() != nil {
		t.Fatal("offline MCP canceled")
	}
	deadline := time.Now().Add(5 * time.Second)
	if d, ok := p.ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if p.in.SetWriteDeadline(deadline) != nil || json.NewEncoder(p.in).Encode(request) != nil {
		t.Fatal("offline MCP bounded write")
	}
}

func (p *p0Peer) receive(t *testing.T) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	if d, ok := p.ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if p.out.SetReadDeadline(deadline) != nil {
		t.Fatal("offline MCP read deadline")
	}
	var response map[string]any
	if json.NewDecoder(p.out).Decode(&response) != nil {
		t.Fatal("offline MCP bounded read")
	}
	return response
}

func (p *p0Peer) call(t *testing.T, name string, args any) bool {
	t.Helper()
	p.id++
	p.send(t, map[string]any{"jsonrpc": "2.0", "id": p.id, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	r := p.receive(t)
	result, ok := r["result"].(map[string]any)
	p.control.Code, p.control.Changed, p.control.Revision = "", false, nil
	if ok {
		b, _ := json.Marshal(result["structuredContent"])
		_ = json.Unmarshal(b, &p.control)
	}
	success := ok && r["error"] == nil && result["isError"] != true
	if !success && (name == "nuntius_lane_pause" || name == "nuntius_lane_resume") {
		code := "other"
		switch p.control.Code {
		case "locked", "revision_conflict", "audit_unavailable", "save_unconfirmed", "committed_audit_failed", "lane_control_unavailable":
			code = p.control.Code
		}
		t.Logf("pinned_control_refused operation=%s code=%s changed=%t revision_present=%t", name, code, p.control.Changed, p.control.Revision != nil)
	}
	return success
}

func (p *p0Peer) controlCall(t *testing.T, operation string, expected uint64) {
	t.Helper()
	name := ""
	switch operation {
	case "pause":
		name = "nuntius_lane_pause"
	case "resume":
		name = "nuntius_lane_resume"
	default:
		t.Fatal("invalid fixture control operation")
	}
	if expected >= laneControlMaxRevision {
		t.Fatal("invalid fixture revision")
	}
	parent := p.ctx
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	p.ctx = ctx
	defer func() { p.ctx = parent }()
	for {
		if p.call(t, name, map[string]any{"lane": "codex", "expected_revision": expected}) {
			if p.control.Code != "committed" || !p.control.Changed || p.control.Revision == nil || *p.control.Revision != expected+1 {
				t.Fatal("pinned control did not commit exactly the next revision")
			}
			return
		}
		// The pinned public primitive may refuse a concurrent watcher. Only
		// this typed pre-effect receipt permits replay of the same revision.
		if p.control.Code != "locked" || p.control.Changed || p.control.Revision != nil || !p0MonitorPause(ctx, time.Millisecond) {
			t.Fatal("pinned control refused beyond the fixture acquisition contract")
		}
	}
}

func p0WriteConfig(t *testing.T, c Config) string {
	t.Helper()
	off := false
	c.AutoResume, c.Notifications = &off, &off
	for i := range c.Lanes {
		c.Lanes[i].StartMode = "as_is"
	}
	path := filepath.Join(t.TempDir(), "lanes.json")
	b, err := json.Marshal(c)
	if err != nil || os.WriteFile(path, b, 0600) != nil {
		t.Fatal("offline configuration")
	}
	return path
}

func p0WaitGoal(t *testing.T, m *Manager, goal string, ended bool) Run {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		runs, err := m.historyContext(ctx)
		if err == nil {
			for _, r := range runs {
				if r.GoalID == goal && r.PID > 1 && (r.State != "running") == ended {
					return r
				}
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("offline goal did not reach expected state")
		case <-tick.C:
		}
	}
}

func TestP0MixedVersions(t *testing.T) {
	for _, pin := range []struct{ version, sha string }{{"v0.3.4", p0Version034}, {"v0.3.5", p0Version035}} {
		t.Run(pin.version, func(t *testing.T) {
			binary := p0PinnedBinary(t, pin.sha)
			m, c, store := manager(t)
			config := p0WriteConfig(t, c)
			old := p0StartPeer(t, binary, config, store)
			args := func(goal, text string) map[string]string {
				return map[string]string{"lane": "codex", "goal_id": goal, "text": text}
			}
			if !old.call(t, "nuntius_dispatch", args("P0-OLD-START", "wait")) {
				t.Fatal("old dispatch refused")
			}
			r := p0WaitGoal(t, m, "P0-OLD-START", false)
			if _, err := m.Dispatch("codex", "P0-OLD-DUPLICATE", "wait"); err == nil {
				t.Fatal("mixed duplicate accepted")
			}
			waitReady(t, m, "codex")
			if err := m.Cancel("codex"); err != nil {
				t.Fatal("new cancel of old run")
			}
			p0WaitGoal(t, m, r.GoalID, true)
			if _, err := m.Dispatch("codex", "P0-NEW-START", "wait"); err != nil {
				t.Fatal("new dispatch refused")
			}
			waitReady(t, m, "codex")
			if !old.call(t, "nuntius_cancel", map[string]string{"lane": "codex"}) {
				t.Fatal("old cancel of new run")
			}
			p0WaitGoal(t, m, "P0-NEW-START", true)
			if !old.call(t, "nuntius_freeze", map[string]string{"reason": "offline mixed freeze"}) {
				t.Fatal("old freeze")
			}
			if _, err := m.Dispatch("codex", "P0-FROZEN-NEW", "ack-goal"); err == nil {
				t.Fatal("new ignored old freeze")
			}
			if err := m.Unfreeze(); err != nil {
				t.Fatal("new unfreeze")
			}
			if err := m.Freeze("offline mixed freeze"); err != nil {
				t.Fatal("new freeze")
			}
			if old.call(t, "nuntius_dispatch", args("P0-FROZEN-OLD", "ack-goal")) {
				t.Fatal("old ignored new freeze")
			}
			if !old.call(t, "nuntius_unfreeze", map[string]any{}) {
				t.Fatal("old unfreeze")
			}
			pauseCtx, stopPause := context.WithTimeout(context.Background(), 2*time.Second)
			defer stopPause()
			for {
				result, err := m.setLaneControl(laneControlChange{Lane: "codex", Paused: true, ExpectedRevision: laneControlRevision(0)}, "mcp")
				if err == nil {
					if result.Revision != 1 || !result.Paused || !result.Changed || !result.Available {
						t.Fatal("new pause did not commit exactly revision one")
					}
					break
				}
				var fault *laneControlFault
				// Only locked proves no commit began. Never retry an uncertain
				// save, audit failure, revision conflict or another refusal.
				if !errors.As(err, &fault) || fault.Code != "locked" || !p0MonitorPause(pauseCtx, time.Millisecond) {
					t.Fatal("new pause failed within the fixture acquisition budget")
				}
			}
			accepted := old.call(t, "nuntius_dispatch", args("P0-MIXED-PAUSE", "ack-goal"))
			if pin.version == "v0.3.4" {
				if !accepted {
					t.Fatal("known v0.3.4 ignores-holds behavior changed")
				}
				p0WaitGoal(t, m, "P0-MIXED-PAUSE", true)
				t.Log("known_v034_ignores_holds=true")
			} else {
				if accepted {
					t.Fatal("v0.3.5 ignored new hold")
				}
				old.controlCall(t, "resume", 1)
				old.controlCall(t, "pause", 2)
				if _, err := m.Dispatch("codex", "P0-OLD-PAUSE", "ack-goal"); err == nil {
					t.Fatal("new ignored old hold")
				}
				for _, oldFirst := range []bool{true, false} {
					t.Run(fmt.Sprint(oldFirst), func(t *testing.T) { p0MixedHeld(t, binary, oldFirst) })
				}
			}
			t.Log("dispatch_cancel_freeze_both_directions=true duplicate_refused=true")
		})
	}
}

func p0MixedHeld(t *testing.T, binary string, oldFirst bool) {
	t.Helper()
	m, h := heldReviewFixture(t, "fixture-thread")
	config := p0WriteConfig(t, m.config)
	run := func(operation string) bool {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, "--config", config, "--store", m.store, "lane", "held", operation, h.Lane, h.RunID, "--revision", fmt.Sprint(h.Revision), "--digest", h.ContentSHA256)
		cmd.WaitDelay = 100 * time.Millisecond
		return cmd.Run() == nil
	}
	if oldFirst {
		if !run("send") {
			t.Fatal("old held send")
		}
	} else if _, err := m.sendHeld(h, nil); err != nil {
		t.Fatal("new held send")
	}
	if !run("send") {
		t.Fatal("old held retry")
	}
	if _, err := m.sendHeld(h, nil); err != nil {
		t.Fatal("new held retry")
	}
	if !run("discard") {
		t.Fatal("old terminal discard")
	}
	if err := m.discardHeld(h, nil); err != nil {
		t.Fatal("new terminal discard")
	}
	heldReviewChildren(t, m, h, 1)
	t.Log("mixed_held_deliveries=1 retry_and_discard_both_directions=true")
}

func TestP0LostClientReconciliation(t *testing.T) {
	m, c, store := manager(t)
	peer := p0StartPeer(t, cliBinary, p0WriteConfig(t, c), store)
	peer.send(t, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": "nuntius_dispatch", "arguments": map[string]string{"lane": "codex", "goal_id": "P0-LOST-CLIENT", "text": "slow-finish"}}})
	first := p0WaitGoal(t, m, "P0-LOST-CLIENT", false)
	// The requester disappears after durable acceptance and before consuming
	// any response. Kill only this test-owned MCP server; its child survives.
	peer.out.Close()
	peer.cancel()
	_ = peer.cmd.Wait()
	peer.in.Close()
	peer.closed = true
	first = p0WaitGoal(t, m, first.GoalID, true)
	rows, err := m.History(HistoryFilter{Limit: 20, Lane: "codex", GoalID: first.GoalID})
	if err != nil || len(rows) != 1 || rows[0].RunID != first.ID {
		t.Fatal("lost response not recoverable from history")
	}
	if _, err := m.Dispatch("codex", first.GoalID, "ack-goal"); err != nil {
		t.Fatal("explicit post-reconciliation repeat")
	}
	waitState(t, m, "codex", "ended")
	runs, err := m.history()
	if err != nil || len(runs) != 2 {
		t.Fatal("documented lack of request-id deduplication changed")
	}
	t.Log("lost_response_reconciled=true first_runs=1 forced_post_completion_runs=2")
}
