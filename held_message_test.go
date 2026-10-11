package nuntius

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const reviewedText = "original reviewed message only"

func heldReviewFixture(t *testing.T, session string) (*Manager, heldMessage) {
	t.Helper()
	m, _ := laneControlCommitFixture(t)
	r := seedCrashRun(t, m, strings.Repeat("a", 32), "ORIGINAL-GOAL", "completed")
	r.Kind, r.State, r.SessionID = "codex", "ended", session
	r.SHA256 = laneControlDigest([]byte("original brief"))
	now := time.Now().UTC()
	r.Ended = &now
	if err := m.save(&r); err != nil {
		t.Fatal(err)
	}
	if err := atomicPrivate(filepath.Join(m.store, "runs", r.ID, "events.jsonl"), nil); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]string{"text": reviewedText})
	if err := atomicPrivate(filepath.Join(m.store, "runs", r.ID, "message.json"), b); err != nil {
		t.Fatal(err)
	}
	successorPause(t, m)
	b, err := os.ReadFile(filepath.Join(m.store, "runs", r.ID, "message-hold.json"))
	var h heldMessage
	if err != nil || json.Unmarshal(b, &h) != nil {
		t.Fatal("reservation unavailable")
	}
	if _, err := successorControl(m, "codex", false, nil); err != nil {
		t.Fatal(err)
	}
	return m, h
}

func heldReviewChildren(t *testing.T, m *Manager, h heldMessage, want int) string {
	t.Helper()
	runs, err := m.history()
	if err != nil {
		t.Fatal(err)
	}
	children := 0
	id := ""
	for _, r := range runs {
		if r.ID == h.RunID {
			continue
		}
		children++
		id = r.ID
		b, e := os.ReadFile(filepath.Join(m.store, "runs", r.ID, "goal.txt"))
		if e != nil || string(b) != reviewedText || r.ParentRunID != h.RunID || r.Lane != h.Lane || r.GoalID != h.GoalID || r.SessionID != h.SessionID || r.SHA256 != h.ContentSHA256 {
			t.Fatal("child lost original goal/session/content binding")
		}
	}
	if children != want {
		t.Fatalf("identified children: got %d want %d", children, want)
	}
	return id
}

func heldReviewAudit(t *testing.T, m *Manager, tool string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(m.store, "audit.jsonl"))
	if err != nil || !strings.Contains(string(b), tool) || strings.Contains(string(b), reviewedText) {
		t.Fatal("missing or unsafe reviewed audit")
	}
}

func heldReviewRetained(t *testing.T, m *Manager, h heldMessage) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(m.store, "runs", h.RunID, "message-hold.json"))
	var got heldMessage
	if err != nil || json.Unmarshal(b, &got) != nil || got != h {
		t.Fatal("failed send changed reviewed reservation")
	}
	if _, err := m.Dispatch(h.Lane, "OTHER-GOAL", "unreviewed replacement"); err == nil {
		t.Fatal("held reservation allowed unreviewed dispatch")
	}
}

func heldReviewFault(stage string) *laneControlIO {
	return &laneControlIO{checkpoint: func(s string) error {
		if s == stage {
			return errors.New("synthetic crash")
		}
		return nil
	}}
}

func TestHeldReviewSendVersusPause(t *testing.T) {
	t.Run("pause-first", func(t *testing.T) {
		m, h := heldReviewFixture(t, "fixture-thread")
		successorPause(t, m)
		if _, err := m.sendHeld(h, nil); err == nil {
			t.Fatal("paused send admitted")
		}
		heldReviewRetained(t, m, h)
		heldReviewChildren(t, m, h, 0)
		if _, err := successorControl(m, h.Lane, false, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := m.sendHeld(h, nil); err != nil {
			t.Fatal(err)
		}
		heldReviewChildren(t, m, h, 1)
	})
	t.Run("send-first", func(t *testing.T) {
		m, h := heldReviewFixture(t, "fixture-thread")
		entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
		go func() {
			_, err := m.sendHeld(h, &laneControlIO{checkpoint: func(s string) error {
				if s == "review-validated" {
					close(entered)
					<-release
				}
				return nil
			}})
			done <- err
		}()
		select {
		case <-entered:
		case err := <-done:
			t.Fatalf("send never admitted: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatal("send barrier unavailable")
		}
		paused := make(chan error, 1)
		go func() { _, err := successorControl(m, h.Lane, true, nil); paused <- err }()
		close(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if err := <-paused; err != nil {
			t.Fatal(err)
		}
		heldReviewChildren(t, m, h, 1)
	})
}

func TestHeldReviewDuplicateSend(t *testing.T) {
	t.Run("ordinary-queued-receipt", func(t *testing.T) {
		m, _ := laneControlCommitFixture(t)
		id, err := m.Dispatch("codex", "ADMISSION-RUN", "admission-barrier")
		if err != nil {
			t.Fatal(err)
		}
		waitReady(t, m, "codex")
		if err = m.SendMessage("codex", reviewedText, false); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(m.config.ReportsDir, "admission-release"), nil, 0600); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(m.store, "runs", id, "message-receipt.json")
		var receipt struct {
			State   string      `json:"state"`
			Binding heldMessage `json:"binding"`
			ChildID string      `json:"child_id"`
		}
		for deadline := time.Now().Add(5 * time.Second); ; {
			b, _ := os.ReadFile(path)
			if json.Unmarshal(b, &receipt) == nil && receipt.State == "consumed" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("ordinary message lacks terminal receipt")
			}
			time.Sleep(10 * time.Millisecond)
		}
		if receipt.Binding.RunID != id || receipt.ChildID == "" {
			t.Fatal("ordinary receipt lost original child binding")
		}
		heldReviewChildren(t, m, receipt.Binding, 1)
		if _, err = m.sendHeld(receipt.Binding, nil); err != nil {
			t.Fatal("ordinary consume response retry failed")
		}
		heldReviewChildren(t, m, receipt.Binding, 1)
	})
	m, h := heldReviewFixture(t, "fixture-thread")
	id, err := m.sendHeld(h, nil)
	if err != nil {
		t.Fatal(err)
	}
	again, err := m.sendHeld(h, nil)
	if err != nil || again != id {
		t.Fatal("duplicate response not bound to original child")
	}
	if _, err := m.Reply(h.Lane, "replacement"); err == nil {
		t.Fatal("busy child accepted unrelated reply")
	}
	heldReviewChildren(t, m, h, 1)
	heldReviewAudit(t, m, "held_message_send")
}

func TestHeldReviewWrongOrStale(t *testing.T) {
	t.Run("discard-only-original", func(t *testing.T) {
		m, h := heldReviewFixture(t, "fixture-thread")
		r := seedCrashRun(t, m, strings.Repeat("b", 32), "SECOND-GOAL", "")
		r.Kind, r.SessionID, r.SHA256 = "codex", "fixture-thread", laneControlDigest([]byte("second"))
		if err := m.save(&r); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(map[string]string{"text": "second held message"})
		if err := atomicPrivate(filepath.Join(m.store, "runs", r.ID, "message.json"), b); err != nil {
			t.Fatal(err)
		}
		successorPause(t, m)
		before, err := os.ReadFile(filepath.Join(m.store, "runs", r.ID, "message-hold.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err = m.discardHeld(h, nil); err != nil {
			t.Fatal(err)
		}
		after, err := os.ReadFile(filepath.Join(m.store, "runs", r.ID, "message-hold.json"))
		if err != nil || string(before) != string(after) {
			t.Fatal("discard changed another goal reservation")
		}
		all, err := m.history()
		if err != nil || len(all) != 2 || successorRun(t, m, h.RunID).ContinueRunID != "" {
			t.Fatal("discard launched work")
		}
	})
	for _, field := range []string{"lane", "run", "goal", "session", "run-digest", "content", "revision", "origin", "disk-content", "disk-session"} {
		t.Run(field, func(t *testing.T) {
			m, h := heldReviewFixture(t, "fixture-thread")
			bad := h
			switch field {
			case "lane":
				bad.Lane = "claude"
			case "run":
				bad.RunID = strings.Repeat("b", 32)
			case "goal":
				bad.GoalID = "OTHER-GOAL"
			case "session":
				bad.SessionID = "other-thread"
			case "run-digest":
				bad.RunSHA256 = laneControlDigest([]byte("other"))
			case "content":
				bad.ContentSHA256 = laneControlDigest([]byte("other"))
			case "revision":
				bad.Revision++
			case "origin":
				bad.Origin = "other"
			case "disk-content":
				b, _ := json.Marshal(map[string]string{"text": "other"})
				if err := atomicPrivate(filepath.Join(m.store, "runs", h.RunID, "message.json"), b); err != nil {
					t.Fatal(err)
				}
			case "disk-session":
				r := successorRun(t, m, h.RunID)
				r.SessionID = "other-thread"
				if err := m.save(r); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := m.sendHeld(bad, nil); err == nil {
				t.Fatal("unreviewed send")
			}
			if err := m.discardHeld(bad, nil); err == nil {
				t.Fatal("wrong message discarded")
			}
			heldReviewRetained(t, m, h)
			heldReviewChildren(t, m, h, 0)
			if field == "disk-content" {
				b, _ := json.Marshal(map[string]string{"text": reviewedText})
				if err := atomicPrivate(filepath.Join(m.store, "runs", h.RunID, "message.json"), b); err != nil {
					t.Fatal(err)
				}
			}
			if field == "disk-session" {
				r := successorRun(t, m, h.RunID)
				r.SessionID = h.SessionID
				if err := m.save(r); err != nil {
					t.Fatal(err)
				}
			}
			if err := m.discardHeld(h, nil); err != nil {
				t.Fatal(err)
			}
			heldReviewAudit(t, m, "held_message_discard")
		})
	}
}

func TestHeldReviewMissingSession(t *testing.T) {
	for _, session := range []string{"", "unsafe session"} {
		t.Run(session, func(t *testing.T) {
			m, h := heldReviewFixture(t, session)
			if _, err := m.sendHeld(h, nil); err == nil {
				t.Fatal("unsafe session dispatched")
			}
			heldReviewRetained(t, m, h)
			if err := m.discardHeld(h, nil); err != nil {
				t.Fatal(err)
			}
			if err := m.discardHeld(h, nil); err != nil {
				t.Fatal("discard retry failed")
			}
			if _, err := os.Stat(filepath.Join(m.store, "runs", h.RunID, "message.json")); !os.IsNotExist(err) {
				t.Fatal("discard left message")
			}
			heldReviewAudit(t, m, "held_message_discard")
			heldReviewChildren(t, m, h, 0)
		})
	}
}

func TestHeldReviewLaunchFailure(t *testing.T) {
	t.Run("bootstrap-failure", func(t *testing.T) {
		m, h := heldReviewFixture(t, "fixture-thread")
		m.config.Lanes[1].Workdir = filepath.Join(t.TempDir(), "missing-fake-workdir")
		if _, err := m.sendHeld(h, nil); err == nil {
			t.Fatal("invalid offline workdir launched")
		}
		heldReviewRetained(t, m, h)
		if _, err := m.sendHeld(h, nil); err == nil {
			t.Fatal("failed bootstrap was resent")
		}
		heldReviewChildren(t, m, h, 1)
		if err := m.discardHeld(h, nil); err != nil {
			t.Fatal("uncertain reviewed content cannot discard")
		}
		if _, err := m.Dispatch(h.Lane, "OTHER-GOAL", "unreviewed replacement"); err == nil {
			t.Fatal("discard released uncertain child reservation")
		}
		if err := m.reconcileHeld(h, nil); err == nil {
			t.Fatal("missing launch identity reconciled")
		}
	})
	m, h := heldReviewFixture(t, "fixture-thread")
	if _, err := m.sendHeld(h, heldReviewFault("review-before-launch")); err == nil {
		t.Fatal("injected launch failure ignored")
	}
	heldReviewRetained(t, m, h)
	b, err := os.ReadFile(filepath.Join(m.store, "runs", h.RunID, "message-receipt.json"))
	if err != nil || !strings.Contains(string(b), "reserved") {
		t.Fatal("uncertain launch lacks durable reservation")
	}
	if _, err = m.sendHeld(h, nil); err == nil {
		t.Fatal("uncertain launch resent")
	}
	heldReviewChildren(t, m, h, 1) // Identified record, never a second launch.
}

func TestHeldReviewCrashAfterLaunch(t *testing.T) {
	t.Run("discard-retains-launch-reservation", func(t *testing.T) {
		m, h := heldReviewFixture(t, "fixture-thread")
		if _, err := m.sendHeld(h, heldReviewFault("review-after-launch")); err == nil {
			t.Fatal("crash fault missed")
		}
		r := successorRun(t, m, h.RunID)
		r.SessionID = ""
		if err := m.save(r); err != nil {
			t.Fatal(err)
		}
		if err := m.discardHeld(h, nil); err != nil {
			t.Fatal("missing-session content discard refused")
		}
		if _, err := m.Dispatch(h.Lane, "OTHER-GOAL", "replacement"); err == nil {
			t.Fatal("uncertain resources released by discard")
		}
		if _, err := m.sendHeld(h, nil); err == nil {
			t.Fatal("discarded content resent")
		}
		if _, err := successorControl(m, h.Lane, true, nil); err != nil {
			t.Fatal("discarded reservation broke pause repair")
		}
		if err := m.reconcileHeld(h, nil); err != nil {
			t.Fatal(err)
		}
		heldReviewChildren(t, m, h, 1)
		heldReviewAudit(t, m, "held_message_discard")
	})
	t.Run("consume-boundaries", func(t *testing.T) {
		for _, stage := range []string{"review-audit-intent.append", "review-audit-intent.fsync", "review-audit-intent.dirsync", "receipt-consumed.write", "receipt-consumed.fsync", "receipt-consumed.rename", "receipt-consumed.renamed", "receipt-consumed.dirsync", "review-audit-consumed.append"} {
			t.Run(stage, func(t *testing.T) {
				m, h := heldReviewFixture(t, "fixture-thread")
				if _, err := m.sendHeld(h, heldReviewFault(stage)); err == nil {
					t.Fatal("consume crash fault missed")
				}
				heldReviewChildren(t, m, h, 1)
				if _, err := m.sendHeld(h, nil); err != nil {
					t.Fatal("identified child could not reconcile")
				}
				heldReviewChildren(t, m, h, 1)
			})
		}
	})
	m, h := heldReviewFixture(t, "fixture-thread")
	if _, err := m.sendHeld(h, heldReviewFault("review-after-launch")); err == nil {
		t.Fatal("crash fault ignored")
	}
	heldReviewRetained(t, m, h)
	id := heldReviewChildren(t, m, h, 1)
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := NewManager(m.config, m.store)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	again, err := next.sendHeld(h, nil)
	if err != nil || again != id {
		t.Fatal("identified launch failed reconciliation")
	}
	if _, err := next.sendHeld(h, nil); err != nil {
		t.Fatal(err)
	}
	heldReviewChildren(t, next, h, 1)
}

func TestHeldReviewCrashAfterConsume(t *testing.T) {
	m, h := heldReviewFixture(t, "fixture-thread")
	if _, err := m.sendHeld(h, heldReviewFault("review-after-consume")); err == nil {
		t.Fatal("crash fault ignored")
	}
	id := heldReviewChildren(t, m, h, 1)
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := NewManager(m.config, m.store)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	again, err := next.sendHeld(h, nil)
	if err != nil || again != id {
		t.Fatal("consumed response retry changed child")
	}
	heldReviewChildren(t, next, h, 1)
	heldReviewAudit(t, next, "held_message_send")
}
