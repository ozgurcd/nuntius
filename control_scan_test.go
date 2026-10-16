package nuntius

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestP0AdmissionGuardReplacement(t *testing.T) {
	ctx, m, _ := p0MetadataFixture(t)
	guard, err := m.admissionControlLockWithin(dispatchControlLockWait, ctx)
	if err != nil {
		t.Fatal("admission fixture guard")
	}
	defer guard.Close()
	path := filepath.Join(m.store, "locks", "control.lock")
	if os.Rename(path, path+".preserved") != nil || os.WriteFile(path, nil, 0600) != nil {
		t.Fatal("coordination replacement fixture")
	}
	a := dispatchAdmission{manager: m, guard: guard, lane: "codex", ctx: ctx}
	if a.consume(m, "codex") == nil {
		t.Fatal("admission survived coordination inode replacement")
	}
}

func TestP0AdmissionGuardLifetime(t *testing.T) {
	ctx, m, _ := p0MetadataFixture(t)
	guard, err := m.admissionControlLockWithin(dispatchControlLockWait, ctx)
	if err != nil {
		t.Fatal("admission fixture guard")
	}
	defer guard.Close()
	a := dispatchAdmission{manager: m, guard: guard, lane: "codex", ctx: ctx}
	if a.consume(m, "claude") == nil || a.consume(&Manager{}, "codex") == nil {
		t.Fatal("admission crossed manager or lane")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	a.ctx = cancelled
	if laneControlCode(a.consume(m, "codex")) != "locked" {
		t.Fatal("cancelled admission code")
	}
	a.ctx = ctx
	if a.consume(m, "codex") != nil || a.consume(m, "codex") == nil {
		t.Fatal("admission was not single use")
	}
	guard.Close()
	a.used = false
	if a.consume(m, "codex") == nil {
		t.Fatal("admission outlived its guard")
	}
	result := mcpToolResult("nuntius_dispatch", nil, &laneControlFault{Code: "locked"})
	b, err := json.Marshal(result["structuredContent"])
	var fault struct {
		Code      string `json:"code"`
		Retryable bool   `json:"retryable"`
	}
	if err != nil || json.Unmarshal(b, &fault) != nil || fault.Code != "locked" || !fault.Retryable {
		t.Fatal("refused receipt omitted lock code")
	}
}

func TestP0WatcherBusyDoesNotScan(t *testing.T) {
	ctx, m, _ := p0MetadataFixture(t)
	m.readOnly = false
	defer oldControlHolder(t, m.store)()
	before := m.metadata.opens.Load()
	_, err := m.statusPass(ctx, false, false)
	if laneControlCode(err) != "locked" || m.metadata.opens.Load() != before {
		t.Fatal("busy watcher performed a public fallback")
	}
}

func TestP0SingleAdmissionScan(t *testing.T) {
	ctx, m, _ := p0MetadataFixture(t)
	m.readOnly = false
	before := m.admissionScans.Load()
	historyBefore := m.historyScans.Load()
	if _, err := m.DispatchContext(ctx, "codex", "P0-SINGLE-SCAN", "ack-goal"); err != nil {
		t.Fatal("single-scan dispatch")
	}
	if m.admissionScans.Load()-before != 1 {
		t.Fatal("dispatch repeated authoritative admission scan")
	}
	if m.historyScans.Load() != historyBefore {
		t.Fatal("dispatch rescanned admission metadata for history")
	}
}

func TestP0RecoverySingleHistory(t *testing.T) {
	_, m, parent := p0MetadataFixture(t)
	m.readOnly = false
	parent.State, parent.StopReason = "interrupted", "interrupted"
	if m.save(&parent) != nil {
		t.Fatal("recovery parent fixture")
	}
	child := seedCrashRun(t, m, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", parent.GoalID, "completed")
	child.State, child.ParentRunID = "ended", parent.ID
	if m.save(&child) != nil {
		t.Fatal("recovery child fixture")
	}
	before := m.historyScans.Load()
	if err := m.Recover(); err != nil {
		t.Fatal("recovery fixture")
	}
	current, err := loadRun(filepath.Join(m.store, "runs", parent.ID), parent.ID)
	if err != nil || current == nil || current.ContinueRunID != child.ID || current.WaitingToResume {
		t.Fatal("recovery lost its own parent-link repair")
	}
	if m.historyScans.Load()-before != 1 {
		t.Fatal("recovery repeated unchanged history scan")
	}
}

func TestP0DecisionOutlivesWaitBudget(t *testing.T) {
	_, m, _ := p0MetadataFixture(t)
	m.readOnly = false
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()
	if lockContext(ctx, &m.metadata.mu, 4) != nil {
		t.Fatal("decision fixture cache lock")
	}
	held := true
	type result struct {
		id  string
		err error
	}
	out := make(chan result, 1)
	done := make(chan struct{})
	before := m.admissionScans.Load()
	go func() {
		defer close(done)
		id, err := m.DispatchContext(ctx, "codex", "P0-DECISION-BUDGET", "ack-goal")
		out <- result{id, err} // single buffered result; the test owns and joins it.
	}()
	defer func() {
		if held {
			m.metadata.mu.Unlock()
		}
		cancel()
		join, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		select {
		case <-done:
		case <-join.Done():
			t.Error("decision fixture did not join")
		}
	}()
	for m.admissionScans.Load() == before {
		if !p0MonitorPause(ctx, time.Millisecond) {
			t.Fatal("decision did not acquire its guard")
		}
	}
	if !p0MonitorPause(ctx, dispatchControlLockWait+75*time.Millisecond) {
		t.Fatal("decision fixture deadline")
	}
	m.metadata.mu.Unlock()
	held = false
	select {
	case got := <-out:
		if got.err != nil || got.id == "" {
			t.Fatal("decision reused the expired acquisition deadline")
		}
	case <-ctx.Done():
		t.Fatal("decision ignored outer request deadline")
	}
}
