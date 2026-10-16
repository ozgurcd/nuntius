package nuntius

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func oldControlHolder(t *testing.T, store string) func() {
	t.Helper()
	return controlFileHolder(t, filepath.Join(store, "locks", "control.lock"), filepath.Dir(store))
}

func controlFileHolder(t *testing.T, path, dir string) func() {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, fakeBinary, "__fixture_control_lock", path)
	cmd.WaitDelay = 100 * time.Millisecond
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal("holder input")
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal("holder output")
	}
	cmd.Cancel = in.Close
	if cmd.Start() != nil {
		t.Fatal("holder start")
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		cancel()
		_ = in.Close()
		_ = out.Close()
		began := time.Now()
		_ = cmd.Wait() // CommandContext + WaitDelay bound exit and inherited pipes.
		if time.Since(began) > 3*time.Second {
			t.Error("holder exceeded cleanup bound")
		}
	}
	t.Cleanup(release)
	pipe, ok := out.(*os.File)
	if !ok || pipe.SetReadDeadline(time.Now().Add(3*time.Second)) != nil {
		t.Fatal("holder readiness deadline")
	}
	s, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || s != "locked\n" {
		t.Fatal("holder not ready within bound")
	}
	return release
}

func TestLaneUpgradeProcessContention(t *testing.T) {
	m, store := laneControlCommitFixture(t)
	// Stop the watcher before holding the inode so the RED proof cannot hang cleanup.
	m.Close()
	if os.WriteFile(filepath.Join(store, "lane-control.json"), []byte(laneControlSample), 0600) != nil {
		t.Fatal("saved hold fixture")
	}
	release := oldControlHolder(t, store)
	root, err := openLaneControlRoot(store)
	if err != nil {
		t.Fatal("root")
	}
	defer root.Close()
	_, err = laneControlReadLock(root)
	if laneControlCode(err) != "locked" {
		t.Error("contention lost its locked state")
	}
	_, err = m.admissionControlLock()
	if laneControlCode(err) != "locked" {
		t.Error("admission lost its locked state")
	}
	for _, op := range []string{"dispatch", "reply", "send", "stop-and-send"} {
		if laneControlCode(admissionCall(m, op)) != "locked" {
			t.Error("public admission lost busy classification", op)
		}
	}
	var listed, refusal bytes.Buffer
	if m.laneCommand(laneCLIRequest{operation: "list", machine: true}, &listed, &refusal) != 1 || !bytes.Contains(listed.Bytes(), []byte(`"phase":"busy"`)) {
		t.Error("CLI list lost busy state")
	}
	result := make(chan []Status, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	go func() { rows, _ := m.statusContext(ctx, true); result <- rows }()
	select {
	case rows := <-result:
		if len(rows) != 2 || rows[0].LaneControl.Phase != "enabled" || !rows[0].LaneControl.DispatchEligible || strings.Contains(rows[0].LaneControl.Explanation, "new work is blocked") {
			t.Error("stable advisory eligibility lost behind writer")
		}
	case <-time.After(time.Second):
		t.Error("status blocked on another process")
		release()
		cancel()
		p0TestJoin(t, result)
	}
	w, err := NewWeb(m, "127.0.0.1:8123")
	if err != nil {
		t.Fatal("web")
	}
	req := httptest.NewRequest("GET", "http://localhost:8123/data", nil)
	req.Header.Set("X-Nuntius-Token", w.token)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	var data struct {
		Lanes    []webLane           `json:"lanes"`
		Controls laneControlSnapshot `json:"lane_control"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &data) != nil || len(data.Lanes) != 2 {
		t.Fatal("one control read broke dashboard refresh")
	}
	if data.Controls.Lanes[0].Phase != "enabled" || !data.Controls.Lanes[0].DispatchEligible {
		t.Error("dashboard stable advisory eligibility missing")
	}
	release()
	rows, err := m.Status()
	if err != nil || rows[0].LaneControl.Phase != "enabled" && rows[0].LaneControl.Phase != "paused" {
		t.Fatal("status did not recover after release")
	}
	state, err := os.ReadFile(filepath.Join(store, "lane-control.json"))
	if err != nil || string(state) != laneControlSample {
		t.Fatal("contended projections rewrote saved hold")
	}
	for _, row := range rows {
		if row.Lane == "codex" && (!row.LaneControl.Paused || row.LaneControl.DispatchEligible) {
			t.Fatal("release lost saved pause")
		}
	}
}

func TestLaneUpgradeStartupContention(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	m, store := laneControlCommitFixture(t)
	m.Close()
	_, err := m.setLaneControlIO(laneControlChange{Lane: "codex", Paused: true}, "cli", &laneControlIO{checkpoint: func(stage string) error {
		if stage == "state.renamed" {
			return errors.New("injected")
		}
		return nil
	}})
	if laneControlCode(err) != "save_unconfirmed" {
		t.Fatal("pending startup intent fixture")
	}
	release := oldControlHolder(t, store)
	result := make(chan *Manager, 1)
	go func() { restarted, _ := NewManager(m.config, store); result <- restarted }()
	select {
	case restarted := <-result:
		if restarted == nil {
			t.Fatal("startup lost independent controls")
		}
		if lockContext(ctx, &restarted.mu) != nil {
			t.Fatal("fixture recovery handoff deadline")
		}
		restarted.recovery = true
		restarted.mu.Unlock()
		// Stay contended through a real watcher tick. Recover must not hold
		// the manager mutex while waiting for this older process to release.
		if !p0MonitorPause(ctx, 1100*time.Millisecond) {
			t.Fatal("watcher fixture deadline")
		}
		responsive := make(chan []Status, 1)
		go func() { rows, _ := restarted.statusContext(ctx, true); responsive <- rows }()
		select {
		case rows := <-responsive:
			if len(rows) != 2 || rows[0].LaneControl.Phase != "unavailable" || rows[0].LaneControl.DispatchEligible || !strings.Contains(rows[0].LaneControl.Explanation, "change pending") {
				t.Error("pending intent lost its unavailable advisory projection")
			}
		case <-time.After(time.Second):
			t.Error("recovery watcher blocked independent status")
			release()
			cancel()
			p0TestJoin(t, responsive)
		}
		release()
		defer restarted.Close()
		deadline := time.Now().Add(3 * time.Second)
		for {
			_, err := os.Lstat(filepath.Join(store, "lane-control-intent.json"))
			if os.IsNotExist(err) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("startup retry left recoverable intent")
			}
			if !p0MonitorPause(ctx, 10*time.Millisecond) {
				t.Fatal("recovery fixture deadline")
			}
		}
		rows, err := restarted.Status()
		if err != nil {
			t.Fatal("recovered status")
		}
		for _, row := range rows {
			if row.Lane == "codex" && (!row.LaneControl.Paused || row.LaneControl.DispatchEligible) {
				t.Fatal("startup recovery reopened saved pause")
			}
		}
	case <-time.After(time.Second):
		t.Error("startup waited forever on old holder")
		release()
		if restarted := p0TestJoin(t, result); restarted != nil {
			restarted.Close()
		}
	}
}

func TestLaneUpgradeCorruptProjection(t *testing.T) {
	m, store := laneControlCommitFixture(t)
	m.Close()
	if os.WriteFile(filepath.Join(store, "lane-control.json"), []byte("{"), 0600) != nil {
		t.Fatal("corrupt fixture")
	}
	w, _ := NewWeb(m, "127.0.0.1:8123")
	req := httptest.NewRequest("GET", "http://localhost:8123/data", nil)
	req.Header.Set("X-Nuntius-Token", w.token)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != 200 || !bytes.Contains(rec.Body.Bytes(), []byte(`"phase":"unavailable"`)) {
		t.Fatal("corrupt controls disabled whole dashboard")
	}
}

func TestLaneUpgradeStartupDiagnostic(t *testing.T) {
	m, store := laneControlCommitFixture(t)
	m.Close()
	if os.WriteFile(filepath.Join(store, "lane-control.json"), []byte("{"), 0600) != nil {
		t.Fatal("fault setup")
	}
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	restarted, err := NewManager(m.config, store)
	if err != nil {
		t.Fatal("independent startup failed")
	}
	restarted.Close()
	if !strings.Contains(output.String(), "ERROR lane control startup recovery failed (lane_control_unavailable)") || strings.Contains(output.String(), store) {
		t.Fatal("startup omitted fixed safe failure reason")
	}
}

func TestLaneUpgradeHolderDiagnostics(t *testing.T) {
	dir := t.TempDir()
	if os.Symlink(fakeBinary, filepath.Join(dir, "lsof")) != nil {
		t.Fatal("offline diagnostic tool")
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, mode := range []string{"older", "same", "unknown", "oversize", "hang"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("NUNTIUS_FAKE_LSOF", mode)
			start := time.Now()
			reason := controlLockHolderExplanation(dir)
			if time.Since(start) > time.Second {
				t.Fatal("diagnostic exceeded bounded projection")
			}
			if mode == "older" && (!strings.Contains(reason, "older Nuntius process") || !strings.Contains(reason, "v0.3.4") || !strings.Contains(reason, "restart")) {
				t.Fatal("older holder not identified")
			}
			if mode == "same" && !strings.Contains(reason, "operation finishes") {
				t.Fatal("current holder mislabeled")
			}
			if mode != "same" && mode != "older" && !strings.Contains(reason, "unknown") {
				t.Fatal("unknown holder version guessed")
			}
			if strings.Contains(reason, "Caskroom") || strings.Contains(reason, dir) {
				t.Fatal("diagnostic leaked an executable path")
			}
		})
	}
}

func TestLaneUpgradeHolderParsing(t *testing.T) {
	for _, b := range []string{"p12345\ncnuntius\nf3\n", "p12345\ncother\nf3\nlW\n", "p0\ncnuntius\nlW\n", "p12345\ncnuntius\nlw\n", "p9999999999999\ncnuntius\nlW\n"} {
		if controlLockHolderPID([]byte(b)) != 0 {
			t.Fatal("open descriptor or foreign process mistaken for holder")
		}
	}
	if controlLockHolderPID([]byte("p12345\ncnuntius\nf3\nlW\n")) != 12345 {
		t.Fatal("exclusive holder missing")
	}
	for _, b := range []string{"n/arbitrary/0.3.4/nuntius\n", "n/opt/homebrew/Caskroom/other/0.3.4/bin/nuntius\n", "n/opt/homebrew/Caskroom/nuntius/untrusted/bin/nuntius\n"} {
		if controlLockHomebrewVersion([]byte(b)) != "" {
			t.Fatal("unproven version accepted")
		}
	}
	if olderControlLockVersion("v0.3.6") || olderControlLockVersion(Version) || !olderControlLockVersion("v0.3.4") {
		t.Fatal("version ordering differs")
	}
}

func TestLaneUpgradeMCPBusyStartup(t *testing.T) {
	busy := &laneControlFault{Code: "locked"}
	for _, name := range []string{"nuntius_status", "nuntius_history", "nuntius_report", "nuntius_tail", "nuntius_cancel"} {
		if !mcpStartupAllows(busy, name) {
			t.Error("control contention disabled independent MCP access", name)
		}
	}
	if mcpStartupAllows(busy, "nuntius_dispatch") || mcpStartupAllows(&laneControlFault{Code: "audit_unavailable"}, "nuntius_status") {
		t.Fatal("startup isolation granted unrelated authority")
	}
}
