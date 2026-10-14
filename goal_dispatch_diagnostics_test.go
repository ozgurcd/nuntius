package nuntius

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// dispatchStages records every checkpoint a fixture dispatch reaches without
// injecting a fault, so a held launch names the exit that discarded its cause.
type dispatchStages struct {
	mu     sync.Mutex
	stages []string
}

func (s *dispatchStages) io() goalCommitIO {
	return goalCommitIO{checkpoint: func(stage string) error {
		s.mu.Lock()
		s.stages = append(s.stages, stage)
		s.mu.Unlock()
		return nil
	}}
}

// goalDispatchDiagnosis reports the durable phase, child identity and bounded
// offline fixture output after a failed fixture dispatch.
func goalDispatchDiagnosis(t *testing.T, m *Manager, req goalDispatchRequest, result goalDispatchResult, stages *dispatchStages, err error) {
	t.Helper()
	stages.mu.Lock()
	reached := append([]string(nil), stages.stages...)
	stages.mu.Unlock()
	t.Logf("dispatch diagnosis: err=%v result_phase=%q stages=%s", err, result.Phase, strings.Join(reached, ","))
	p, e := replayGoalJournal(m.store)
	if e != nil {
		t.Logf("dispatch diagnosis: journal replay: %v", e)
		return
	}
	data, e := goalRecordMetadata(p.records[req.GoalID])
	if e != nil || len(data.Dispatches) == 0 {
		t.Logf("dispatch diagnosis: no dispatch record (%v)", e)
		return
	}
	d := data.Dispatches[len(data.Dispatches)-1]
	t.Logf("dispatch diagnosis: journal phase=%s state=%s ends=%d torn=%v", d.Phase, data.State, len(data.Ends), p.tornTail)
	dir := filepath.Join(m.store, "runs", d.RunID)
	r, e := loadRun(dir, d.RunID)
	if e != nil || r == nil {
		t.Logf("dispatch diagnosis: run record unavailable (%v)", e)
		return
	}
	start, startErr := processStart(r.PID)
	t.Logf("dispatch diagnosis: run state=%s stop=%s pid_set=%v start_set=%v same_process=%v start_matches=%v start_err=%v gone=%v attempt_run=%v child=%v",
		r.State, r.StopReason, r.PID > 1, r.ProcessStart != "", sameProcess(*r), startErr == nil && start == r.ProcessStart, startErr, goalAttemptDefinitelyGone(*r), m.goalAttemptRun(d) != nil, m.goalDispatchChild(d) != nil)
	for _, name := range []string{"stderr.log", "events.jsonl"} {
		b, e := os.ReadFile(filepath.Join(dir, name))
		if len(b) > 1024 {
			b = b[len(b)-1024:]
		}
		t.Logf("dispatch diagnosis: %s (%v): %s", name, e, fmt.Sprintf("%q", b))
	}
}
