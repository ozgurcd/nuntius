package nuntius

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func successorControl(m *Manager, lane string, paused bool, ops *laneControlIO) (laneControlResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		return laneControlResult{}, err
	}
	defer root.Close()
	guard, err := laneControlLock(root, unix.LOCK_EX)
	if err != nil {
		return laneControlResult{}, err
	}
	defer guard.Close()
	return m.setLaneControlLocked(root, laneControlChange{Lane: lane, Paused: paused}, "cli", ops)
}

func successorPause(t *testing.T, m *Manager) {
	t.Helper()
	result, err := successorControl(m, "codex", true, nil)
	if err != nil || !result.Available || !result.Paused {
		t.Fatal("pause failed")
	}
}

func successorRun(t *testing.T, m *Manager, id string) *Run {
	t.Helper()
	r, err := loadRun(filepath.Join(m.store, "runs", id), id)
	if err != nil || r == nil {
		t.Fatal("run unavailable")
	}
	return r
}

func successorHeld(t *testing.T, m *Manager, id, text string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(m.store, "runs", id, "message-hold.json"))
	var hold struct {
		Version       int    `json:"version"`
		Lane          string `json:"lane"`
		RunID         string `json:"run_id"`
		GoalID        string `json:"goal_id"`
		SessionID     string `json:"session_id"`
		RunSHA256     string `json:"run_sha256"`
		ContentSHA256 string `json:"content_sha256"`
		Revision      uint64 `json:"revision"`
		Origin        string `json:"origin"`
	}
	r := successorRun(t, m, id)
	if err != nil || json.Unmarshal(b, &hold) != nil || hold.Version != 1 || hold.Lane != r.Lane || hold.RunID != id || hold.GoalID != r.GoalID || hold.RunSHA256 != r.SHA256 || hold.ContentSHA256 != laneControlDigest([]byte(text)) || hold.Revision == 0 || hold.Origin == "" || hold.SessionID == "" {
		t.Fatal("pending message lacks original run/content binding")
	}
}

func TestLaneSuccessorsHeldSurvives(t *testing.T) {
	m, store := laneControlCommitFixture(t)
	id, err := m.Dispatch("codex", "ADMISSION-RUN", "admission-barrier")
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, m, "codex")
	if err = m.SendMessage("codex", "reviewed later", false); err != nil {
		t.Fatal(err)
	}
	successorPause(t, m)
	successorHeld(t, m, id, "reviewed later")
	if _, err = successorControl(m, "codex", false, nil); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(m.config.ReportsDir, "admission-release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	waitState(t, m, "codex", "ended")
	r := successorRun(t, m, id)
	if r.ContinueRunID != "" || r.StopReason != "completed" || r.Ended == nil {
		t.Fatal("held successor damaged end bookkeeping")
	}
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := NewManager(m.config, store)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	successorHeld(t, next, id, "reviewed later")
	if err = next.Recover(); err != nil {
		t.Fatal(err)
	}
	if _, err = next.Dispatch("codex", "UNRELATED", "hello"); err == nil || !strings.Contains(err.Error(), "pending_message_requires_review") {
		t.Fatal("unreviewed message allowed unrelated dispatch")
	}
	if _, err = next.Reply("codex", "replacement"); err == nil {
		t.Fatal("reply bypassed held review")
	}
	runs, _ := next.history()
	if len(runs) != 1 {
		t.Fatal("held message replayed after restart/resume")
	}
}

func TestLaneSuccessorsPauseBeforeFollow(t *testing.T) {
	m, _ := laneControlCommitFixture(t)
	id, err := m.Dispatch("claude", "ACK-HOLD", "ack-goal ack-barrier")
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, m, "claude")
	if err = m.SendMessage("claude", "ack-goal", false); err != nil {
		t.Fatal(err)
	}
	if _, err = successorControl(m, "claude", true, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = successorControl(m, "claude", false, nil); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(m.config.ReportsDir, "admission-release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	waitState(t, m, "claude", "ended")
	if !m.noWork(id) {
		t.Fatal("acknowledgement fixture did not exercise continuation eligibility")
	}
	successorHeld(t, m, id, "ack-goal")
	if successorRun(t, m, id).ContinueRunID != "" {
		t.Fatal("message/ack successor after hold")
	}
}

func TestLaneSuccessorsFollowBeforePause(t *testing.T) {
	m, _ := laneControlCommitFixture(t)
	id, err := m.Dispatch("codex", "FIRST", "wait")
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, m, "codex")
	if err = m.SendMessage("codex", "admission-barrier", true); err != nil {
		t.Fatal(err)
	}
	var child *Run
	for deadline := time.Now().Add(4 * time.Second); time.Now().Before(deadline); {
		child, _ = m.latest("codex")
		if child != nil && child.ID != id && child.State == "running" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if child == nil || child.ID == id {
		t.Fatal("follow did not win launch")
	}
	waitReady(t, m, "codex")
	if err = m.SendMessage("codex", "second held", false); err != nil {
		t.Fatal(err)
	}
	successorPause(t, m)
	successorHeld(t, m, child.ID, "second held")
	if !sameProcess(*successorRun(t, m, child.ID)) {
		t.Fatal("winning child interrupted")
	}
	if err = os.WriteFile(filepath.Join(m.config.ReportsDir, "admission-release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	waitState(t, m, "codex", "ended")
	if successorRun(t, m, child.ID).ContinueRunID != "" {
		t.Fatal("second successor launched")
	}
}

func TestLaneSuccessorsRecoveryOrder(t *testing.T) {
	for _, pauseFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "pause-before-Recover", false: "Recover-before-pause"}[pauseFirst], func(t *testing.T) {
			m, _ := laneControlCommitFixture(t)
			r := seedCrashRun(t, m, strings.Repeat("d", 32), "RECOVERY-HOLD", "")
			r.Brief = "wait"
			if err := m.save(&r); err != nil {
				t.Fatal(err)
			}
			if pauseFirst {
				successorPause(t, m)
			}
			err := m.Recover()
			if err != nil {
				t.Fatal("Recover must retain waiting work without error")
			}
			if !pauseFirst {
				child, _ := m.latest("codex")
				if child.ID == r.ID || child.ResumeCount != 1 {
					t.Fatal("recovery did not win")
				}
				waitReady(t, m, "codex")
				if err = m.SendMessage("codex", "held on recovery", false); err != nil {
					t.Fatal(err)
				}
				successorPause(t, m)
				successorHeld(t, m, child.ID, "held on recovery")
				if !sameProcess(*successorRun(t, m, child.ID)) {
					t.Fatal("recovery winner stopped")
				}
				return
			}
			for i := 0; i < 3; i++ {
				if err = m.Recover(); err != nil {
					t.Fatal(err)
				}
			}
			old := successorRun(t, m, r.ID)
			b, _ := json.Marshal(old)
			var fields map[string]any
			_ = json.Unmarshal(b, &fields)
			if old.State != "interrupted" || old.StopReason != "interrupted" || old.ResumeCount != 0 || old.ContinueRunID != "" || !old.WaitingToResume || fields["resume_blocked_reason"] != "lane_paused" {
				t.Fatal("paused recovery lost waiting identity or spent attempt")
			}
			if _, err = successorControl(m, "codex", false, nil); err != nil {
				t.Fatal(err)
			}
			runs, _ := m.history()
			if len(runs) != 1 {
				t.Fatal("resume launched recovery")
			}
		})
	}
}

func TestLaneSuccessorsLatchUncertainty(t *testing.T) {
	m, _ := laneControlCommitFixture(t)
	id, err := m.Dispatch("codex", "ADMISSION-RUN", "admission-barrier")
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, m, "codex")
	if err = m.SendMessage("codex", "repair me", false); err != nil {
		t.Fatal(err)
	}
	_, err = successorControl(m, "codex", true, &laneControlIO{checkpoint: func(stage string) error {
		if stage == "held.rename" {
			return errors.New("fixture failure")
		}
		return nil
	}})
	if err == nil {
		t.Fatal("message latch uncertainty reported success")
	}
	if _, err = m.readLaneControlsLocked(); err == nil {
		t.Fatal("uncertain latch enabled delivery")
	}
	if err = os.WriteFile(filepath.Join(m.config.ReportsDir, "admission-release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	waitState(t, m, "codex", "ended")
	if r := successorRun(t, m, id); r.Ended == nil || r.StopReason != "completed" || r.ContinueRunID != "" {
		t.Fatal("unresolved hold lost current end bookkeeping")
	}
	if err = m.recoverLaneControls(nil); err != nil {
		t.Fatal(err)
	}
	successorHeld(t, m, id, "repair me")
}

func TestLaneSuccessorsAllMessagesAndContent(t *testing.T) {
	m, _ := laneControlCommitFixture(t)
	for _, id := range []string{strings.Repeat("a", 32), strings.Repeat("b", 32)} {
		r := seedCrashRun(t, m, id, "OLDER-HOLD", "")
		r.SessionID = "fixture-thread"
		r.SHA256 = laneControlDigest([]byte(id))
		if err := m.save(&r); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(map[string]string{"text": "original " + id})
		if err := atomicPrivate(filepath.Join(m.store, "runs", id, "message.json"), b); err != nil {
			t.Fatal(err)
		}
	}
	successorPause(t, m)
	for _, id := range []string{strings.Repeat("a", 32), strings.Repeat("b", 32)} {
		successorHeld(t, m, id, "original "+id)
	}
	path := filepath.Join(m.store, "runs", strings.Repeat("a", 32), "message-hold.json")
	before, _ := os.ReadFile(path)
	if _, err := successorControl(m, "codex", false, nil); err != nil {
		t.Fatal(err)
	}
	successorPause(t, m)
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("repeated pause replaced first message binding")
	}
	if _, err := successorControl(m, "codex", false, nil); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]string{"text": "changed content"})
	if err := atomicPrivate(filepath.Join(m.store, "runs", strings.Repeat("a", 32), "message.json"), b); err != nil {
		t.Fatal(err)
	}
	if _, err := successorControl(m, "codex", true, nil); err == nil {
		t.Fatal("changed content rebound to original receipt")
	}
	if _, err := m.readLaneControlsLocked(); err == nil {
		t.Fatal("changed held content released unresolved intent")
	}
}

func TestLaneSuccessorsRecoverAdoptsTimeout(t *testing.T) {
	m, store := laneControlCommitFixture(t)
	id, err := m.Dispatch("codex", "LIVE-OLD", "wait")
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, m, "codex")
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
	seedCrashRun(t, m, strings.Repeat("e", 32), "NEWER-WAITING", "")
	newer := successorRun(t, m, strings.Repeat("e", 32))
	newer.Started = time.Now().Add(time.Second).UTC()
	if err = m.save(newer); err != nil {
		t.Fatal(err)
	}
	next, err := NewManager(m.config, store)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	successorPause(t, next)
	if err = next.Recover(); err != nil {
		t.Fatal(err)
	}
	next.mu.Lock()
	supervised := next.active["codex"] != nil
	next.runtime = time.Millisecond
	next.Grace = time.Millisecond
	next.mu.Unlock()
	if !supervised {
		t.Fatal("Recover did not adopt surviving process behind latest history")
	}
	for deadline := time.Now().Add(4 * time.Second); time.Now().Before(deadline); {
		if successorRun(t, next, id).State == "timed_out" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("paused adopted process lost normal timeout supervision")
}
