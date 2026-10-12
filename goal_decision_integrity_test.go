package nuntius

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// Construct a complete, correctly checksummed candidate independently of the
// writer's history guard. Replay must check its semantics, not just its frame.
func goalDecisionReplayCandidate(t *testing.T, m *Manager, old goalReplayRecord, record []byte) {
	t.Helper()
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	ref := goalBlobName("goal-record-", record)
	if err := stageGoalBlob(root, ref, record, goalCommitIO{}); err != nil {
		t.Fatal(err)
	}
	prefix, err := readGoalPrivate(root, goalJournalName, goalJournalMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	p, err := replayGoalJournalData(root, prefix)
	if err != nil {
		t.Fatal(err)
	}
	specRef := goalBlobName("goal-spec-", []byte(old.spec))
	spec, err := decodeNativeGoalSpec([]byte(old.spec))
	if err != nil {
		t.Fatal(err)
	}
	mutation := strings.Repeat("a", 64)
	event := goalJournalEvent{Kind: "goal.spec", GoalID: spec.ID, Revision: old.revision + 1, SpecRef: specRef, SealRef: old.sealRef, RecordRef: ref, OperationID: "counterfeit", MutationSHA256: mutation}
	event.InputSHA256 = goalRecordFingerprint(goalInputFingerprint(spec.ID, old.revision, specRef, old.sealRef), ref, mutation)
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	before := goalCommitTree(t, m.store)
	got, err := replayGoalJournalData(root, append(prefix[:len(prefix):len(prefix)], encodeGoalFrame(p.sequence+1, payload)...))
	if !errors.Is(err, errGoalReplay) || got != nil {
		t.Error("replay accepted contradictory decision or returned a projection")
	}
	if !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
		t.Fatal("replay changed saved history")
	}
}

func TestGoalDecisionIntegrity(t *testing.T) {
	for _, alternate := range []bool{false, true} {
		mode := "report"
		if alternate {
			mode = "alternate"
		}
		for _, scenario := range []string{"stale-attempt", "stale-digest", "active-successor", "unconfirmed-launch", "not-ended", "missing-accounting"} {
			if alternate && scenario == "stale-digest" {
				continue // Alternate evidence intentionally does not bind a report.
			}
			t.Run(mode+"/"+scenario, func(t *testing.T) {
				a := &goalDecisionAttempt{ID: "current", Ended: true, ReportDigest: strings.Repeat("b", 64)}
				switch scenario {
				case "active-successor":
					a.ActiveSuccessor = true
				case "unconfirmed-launch":
					a.LaunchUnconfirmed = true
				case "not-ended":
					a.Ended = false
				case "missing-accounting":
					a = nil
				}
				m, req := goalDecisionFixture(t, nativeGoalEnded, a)
				req.Action, req.AttemptID, req.ReportDigest = "verify", "current", strings.Repeat("b", 64)
				req.AlternateEvidence = alternate
				if alternate {
					req.ReportDigest = ""
				}
				if scenario == "stale-attempt" {
					req.AttemptID = "previous"
				}
				if scenario == "stale-digest" {
					req.ReportDigest = strings.Repeat("c", 64)
				}
				before := goalCommitTree(t, m.store)
				if _, err := m.decideGoal(req); err == nil || !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
					t.Fatal("ordinary writer control failed to refuse unchanged")
				}
				p, _, err := readGoalRecords(m.store)
				if err != nil {
					t.Fatal(err)
				}
				old := p.records[req.GoalID]
				data, err := goalRecordMetadata(old)
				if err != nil {
					t.Fatal(err)
				}
				data.State = nativeGoalVerified
				data.Decisions = append(data.Decisions, goalDecisionEvidence{Action: req.Action, Revision: old.revision, Reason: req.Reason, Authority: req.Authority, Evidence: req.Evidence, AttemptID: req.AttemptID, ReportDigest: req.ReportDigest, AlternateEvidence: req.AlternateEvidence})
				record, err := encodeGoalRecord(data)
				if err != nil {
					t.Fatal(err)
				}
				_, projection, err := m.commitGoal(goalCommitRequest{OperationID: "counterfeit", ExpectedRevision: old.revision, Spec: []byte(old.spec), Seal: []byte("sealed fixture"), Record: record, MutationSHA256: strings.Repeat("a", 64)}, goalCommitIO{})
				if err == nil || projection != nil || !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
					t.Error("lower-level writer accepted decision or changed history")
				}
				// A separate fixture ensures replay is tested even when a broken
				// lower-level writer has already accepted this candidate.
				fresh, _ := goalDecisionFixture(t, nativeGoalEnded, a)
				goalDecisionReplayCandidate(t, fresh, old, record)
			})
		}
	}
}

func TestGoalDecisionIntegrityOtherActions(t *testing.T) {
	for _, action := range []string{"approve", "park", "cancel"} {
		for _, scenario := range []string{"active-successor", "unconfirmed-launch", "not-ended", "opaque-seal", "approval-change", "grant-change"} {
			if action != "approve" && scenario == "opaque-seal" {
				continue
			}
			t.Run(action+"/"+scenario, func(t *testing.T) {
				a := &goalDecisionAttempt{ID: "current", Ended: true}
				switch scenario {
				case "active-successor":
					a.ActiveSuccessor = true
				case "unconfirmed-launch":
					a.LaunchUnconfirmed = true
				case "not-ended":
					a.Ended = false
				}
				m, req := goalDecisionFixture(t, nativeGoalDrafted, a)
				req.Action = action
				p, _, err := readGoalRecords(m.store)
				if err != nil {
					t.Fatal(err)
				}
				old := p.records[req.GoalID]
				data, err := goalRecordMetadata(old)
				if err != nil {
					t.Fatal(err)
				}
				data.State = map[string]nativeGoalState{"approve": nativeGoalApproved, "park": nativeGoalParked, "cancel": nativeGoalCancelled}[action]
				if action == "approve" || scenario == "approval-change" || scenario == "grant-change" {
					data.ApprovalRevision = old.revision
				}
				if scenario == "grant-change" {
					data.GrantBinding = "invented-grant"
				}
				data.Decisions = append(data.Decisions, goalDecisionEvidence{Action: action, Revision: old.revision, Reason: req.Reason, Authority: req.Authority, Evidence: req.Evidence})
				record, err := encodeGoalRecord(data)
				if err != nil {
					t.Fatal(err)
				}
				before := goalCommitTree(t, m.store)
				_, projection, err := m.commitGoal(goalCommitRequest{OperationID: "counterfeit", ExpectedRevision: old.revision, Spec: []byte(old.spec), Seal: []byte("sealed fixture"), Record: record, MutationSHA256: strings.Repeat("a", 64)}, goalCommitIO{})
				if err == nil || projection != nil || !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
					t.Error("lower-level writer accepted unsafe action or changed history")
				}
				fresh, _ := goalDecisionFixture(t, nativeGoalDrafted, a)
				goalDecisionReplayCandidate(t, fresh, old, record)
			})
		}
	}
}

func TestGoalDecisionOneLine(t *testing.T) {
	for _, action := range []string{"park", "verify", "cancel"} {
		for name, separator := range map[string]string{"line": "\u2028", "paragraph": "\u2029"} {
			t.Run(action+"/"+name, func(t *testing.T) {
				m, req := goalDecisionFixture(t, nativeGoalEnded, &goalDecisionAttempt{ID: "current", Ended: true, ReportDigest: strings.Repeat("b", 64)})
				req.Action, req.Reason = action, "First line"+separator+"Second line"
				if action == "verify" {
					req.AttemptID, req.ReportDigest = "current", strings.Repeat("b", 64)
				}
				before := goalCommitTree(t, m.store)
				if _, err := m.decideGoal(req); !errors.Is(err, errGoalDecision) || !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
					t.Error("multi-line reason persisted or lacked invalid-field explanation")
				}
				// Raw JSON deliberately bypasses encodeGoalRecord's validation.
				data := goalRecordData{Version: 1, State: nativeGoalEnded, Notes: []goalRecordNote{}, Decisions: []goalDecisionEvidence{{Action: action, Revision: 1, Reason: req.Reason, Authority: req.Authority, Evidence: req.Evidence, AttemptID: req.AttemptID, ReportDigest: req.ReportDigest}}}
				raw, err := json.Marshal(data)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := decodeGoalRecord(raw); !errors.Is(err, errGoalRecord) {
					t.Error("retained multi-line decision reason decoded")
				}
			})
		}
	}
}

func TestGoalDecisionInitialHistory(t *testing.T) {
	for _, action := range []string{"approve", "verify", "park", "cancel"} {
		t.Run(action, func(t *testing.T) {
			m, create := goalRecordsFixture(t)
			spec, err := json.Marshal(create.Spec)
			if err != nil {
				t.Fatal(err)
			}
			data := goalRecordData{Version: 1, State: nativeGoalParked, Notes: []goalRecordNote{}, Decisions: []goalDecisionEvidence{{Action: action, Revision: 1, Reason: "Reviewed", Authority: "plan", Evidence: "review"}}}
			if action == "verify" {
				data.State = nativeGoalVerified
				data.Attempt = &goalDecisionAttempt{ID: "current", Ended: true, ReportDigest: strings.Repeat("b", 64)}
				data.Decisions[0].AttemptID, data.Decisions[0].ReportDigest = "current", data.Attempt.ReportDigest
			}
			if action == "approve" {
				data.State, data.ApprovalRevision = nativeGoalApproved, 1
			}
			if action == "cancel" {
				data.State = nativeGoalCancelled
			}
			record, err := encodeGoalRecord(data)
			if err != nil {
				t.Fatal(err)
			}
			// Initialize the shared lock without a journal event, so the
			// refusal measurement covers history rather than lock creation.
			if _, _, err := m.commitGoal(goalCommitRequest{OperationID: "prepare-lock", ExpectedRevision: 1, Spec: spec}, goalCommitIO{}); !errors.Is(err, errGoalConflict) {
				t.Fatal("lock preparation must leave the journal absent", err)
			}
			before := goalCommitTree(t, m.store)
			_, projection, err := m.commitGoal(goalCommitRequest{OperationID: "counterfeit", Spec: spec, Record: record, MutationSHA256: strings.Repeat("a", 64)}, goalCommitIO{})
			if err == nil || projection != nil || !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
				t.Fatal("first history frame invented a prior reviewed revision")
			}
		})
	}
}
