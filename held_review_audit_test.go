package nuntius

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestHeldReviewTerminalAuditRetry(t *testing.T) {
	for _, action := range []string{"send", "discard"} {
		for _, boundary := range []string{"append", "fsync", "dirsync"} {
			for _, retry := range []string{"action", "reconcile", "direct"} {
				t.Run(action+"/"+boundary+"/"+retry, func(t *testing.T) {
					m, h := heldReviewFixture(t, "fixture-thread")
					retained := func() {
						t.Helper()
						b, err := os.ReadFile(filepath.Join(m.store, "runs", h.RunID, "message-hold.json"))
						var got heldMessage
						if err != nil || json.Unmarshal(b, &got) != nil || got != h {
							t.Fatal("audit uncertainty lost original hold")
						}
						b, err = os.ReadFile(filepath.Join(m.store, "runs", h.RunID, "message.json"))
						text, e := pendingText(b)
						if err != nil || e != nil || text != reviewedText {
							t.Fatal("audit uncertainty lost original content")
						}
					}
					result := "discarded"
					if action == "send" {
						result = "consumed"
					}
					invoke := func(ops *laneControlIO) error {
						if action == "send" {
							_, err := m.sendHeld(h, ops)
							return err
						}
						return m.discardHeld(h, ops)
					}
					stage := "review-audit-" + result + "." + boundary
					if invoke(heldReviewFault(stage)) == nil {
						t.Fatal("outcome fault missed")
					}
					retained()
					child := ""
					if action == "send" {
						child = heldReviewChildren(t, m, h, 1)
					}
					// Restart before repair; a visible audit row is not proof of sync.
					if err := m.Close(); err != nil {
						t.Fatal(err)
					}
					next, err := NewManager(m.config, m.store)
					if err != nil {
						t.Fatal(err)
					}
					m = next
					defer m.Close()
					repair := invoke
					if retry == "reconcile" {
						repair = func(ops *laneControlIO) error { return m.reconcileHeld(h, ops) }
					}
					if retry != "direct" {
						if repair(heldReviewFault(stage)) == nil {
							t.Fatal("uncertain audit repair allowed terminal cleanup")
						}
						retained()
					}
					if err := repair(nil); err != nil {
						t.Fatal("terminal audit repair failed")
					}
					for _, name := range []string{"message.json", "message-hold.json"} {
						if _, err := os.Stat(filepath.Join(m.store, "runs", h.RunID, name)); !os.IsNotExist(err) {
							t.Fatal("durable outcome did not permit cleanup")
						}
					}
					if err := repair(nil); err != nil {
						t.Fatal("completed retry failed")
					}
					if action == "send" {
						id, err := m.sendHeld(h, nil)
						if err != nil || id != child || heldReviewChildren(t, m, h, 1) != child {
							t.Fatal("terminal retry changed child")
						}
					}
					b, err := os.ReadFile(filepath.Join(m.store, "audit.jsonl"))
					if err != nil || bytes.Contains(b, []byte(reviewedText)) || bytes.Contains(b, []byte(h.SessionID)) {
						t.Fatal("audit unavailable or contains private content")
					}
					intents, outcomes := 0, 0
					for _, line := range bytes.Split(b, []byte("\n")) {
						var row struct {
							Tool, Result, Lane, Reason, Origin string
							RunID                              string `json:"run_id"`
							GoalID                             string `json:"goal_id"`
							SHA256                             string `json:"sha256"`
							Revision                           uint64 `json:"message_revision"`
						}
						if json.Unmarshal(line, &row) != nil || row.Tool != "held_message_"+action || row.RunID != h.RunID {
							continue
						}
						if row.Result == "intent" {
							intents++
						}
						if row.Result == result {
							outcomes++
							if row.Lane != h.Lane || row.GoalID != h.GoalID || row.SHA256 != h.ContentSHA256 || row.Revision != h.Revision || row.Origin != h.Origin || row.Reason != child {
								t.Fatal("outcome lost original correlation")
							}
						}
					}
					if intents == 0 || outcomes != 1 {
						t.Fatal("terminal retry lost or duplicated its outcome")
					}
				})
			}
		}
	}
}

func TestHeldReviewTerminalAuditTorn(t *testing.T) {
	for _, action := range []string{"send", "discard"} {
		t.Run(action, func(t *testing.T) {
			m, h := heldReviewFixture(t, "fixture-thread")
			result := "discarded"
			if action == "send" {
				result = "consumed"
				_, _ = m.sendHeld(h, heldReviewFault("review-audit-"+result+".append"))
			} else {
				_ = m.discardHeld(h, heldReviewFault("review-audit-"+result+".append"))
			}
			path := filepath.Join(m.store, "audit.jsonl")
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			b = append(b, []byte(`{"tool":`)...)
			if err := os.WriteFile(path, b, 0600); err != nil {
				t.Fatal(err)
			}
			if err := m.reconcileHeld(h, nil); err == nil {
				t.Fatal("torn audit permitted terminal cleanup")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(b, after) {
				t.Fatal("repair rewrote uncertain audit")
			}
			for _, name := range []string{"message.json", "message-hold.json"} {
				if _, err := os.Stat(filepath.Join(m.store, "runs", h.RunID, name)); err != nil {
					t.Fatal("torn audit lost retained content")
				}
			}
		})
	}
}

func TestHeldReviewDiscardRepairsConsumedAudit(t *testing.T) {
	m, h := heldReviewFixture(t, "fixture-thread")
	if _, err := m.sendHeld(h, heldReviewFault("review-audit-consumed.append")); err == nil {
		t.Fatal("send outcome fault missed")
	}
	child := heldReviewChildren(t, m, h, 1)
	if err := m.discardHeld(h, heldReviewFault("review-audit-consumed.fsync")); err == nil {
		t.Fatal("discard cleaned up before repairing send outcome")
	}
	for _, name := range []string{"message.json", "message-hold.json"} {
		if _, err := os.Stat(filepath.Join(m.store, "runs", h.RunID, name)); err != nil {
			t.Fatal("failed outcome repair lost content")
		}
	}
	if err := m.discardHeld(h, nil); err != nil {
		t.Fatal(err)
	}
	if err := m.reconcileHeld(h, nil); err != nil {
		t.Fatal(err)
	}
	if heldReviewChildren(t, m, h, 1) != child {
		t.Fatal("discard changed launched child")
	}
	b, err := os.ReadFile(filepath.Join(m.store, "audit.jsonl"))
	if err != nil || bytes.Contains(b, []byte(reviewedText)) {
		t.Fatal("unsafe audit")
	}
	for _, result := range []string{"consumed", "discarded"} {
		count := 0
		for _, line := range bytes.Split(b, []byte("\n")) {
			var row struct {
				Result string
				RunID  string `json:"run_id"`
				Reason string
			}
			if json.Unmarshal(line, &row) == nil && row.RunID == h.RunID && row.Result == result && row.Reason == child {
				count++
			}
		}
		if count != 1 {
			t.Fatal("discard lost or duplicated original outcome")
		}
	}
}
