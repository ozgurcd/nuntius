package nuntius

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func briefHistoryFixture(t *testing.T, state string) (*Manager, goalRecordRequest, goalReplayRecord, goalRecordData, []byte) {
	t.Helper()
	m, req := goalRecordsFixture(t)
	_, in := briefFixture(t)
	if state == "grant" {
		// Explicit initial bookkeeping remains supported. Advance it with a
		// note so the attack is against an existing, noninitial history.
		s, err := assembleGoalBrief(req.Spec, 1, in)
		if err != nil {
			t.Fatal(err)
		}
		seal, _ := json.Marshal(s)
		spec, _ := json.Marshal(req.Spec)
		data := goalRecordData{Version: 1, State: nativeGoalApproved, ApprovalRevision: 1, GrantBinding: "exact-grant", Notes: []goalRecordNote{}}
		record, err := encodeGoalRecord(data)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := m.commitGoal(goalCommitRequest{OperationID: "fixture", Spec: spec, Seal: seal, Record: record, MutationSHA256: strings.Repeat("a", 64)}, goalCommitIO{}); err != nil {
			t.Fatal(err)
		}
		if _, err := m.noteGoal(goalNoteRequest{OperationID: "fixture-note", ExpectedRevision: 1, GoalID: req.Spec.ID, Note: goalRecordNote{Text: "Retain initial observation"}}); err != nil {
			t.Fatal(err)
		}
	} else {
		if _, err := m.createGoal(req); err != nil {
			t.Fatal(err)
		}
		req.OperationID, req.ExpectedRevision = "seal", 1
		if _, err := m.sealGoalBrief(req, in); err != nil {
			t.Fatal(err)
		}
		if state != "drafted" {
			action := "approve"
			source := in.SourceState
			if state == "parked" {
				action = "park"
				source = ""
			}
			if _, err := m.decideGoal(goalDecisionRequest{OperationID: action, GoalID: req.Spec.ID, ExpectedRevision: 2, Action: action, Reason: "Reviewed", Authority: in.Authority, Evidence: "review", SourceState: source}); err != nil {
				t.Fatal(err)
			}
		}
	}
	p, err := replayGoalJournal(m.store)
	if err != nil {
		t.Fatal(err)
	}
	old := p.records[req.Spec.ID]
	data, err := goalRecordMetadata(old)
	if err != nil || old.revision < 2 {
		t.Fatal("fixture needs retained history", err)
	}
	seal, err := os.ReadFile(filepath.Join(m.store, old.sealRef))
	if err != nil {
		t.Fatal(err)
	}
	return m, req, old, data, seal
}

func TestBriefHistoryRetainedApproval(t *testing.T) {
	for _, scenario := range []string{"scope-approval", "scope-grant", "scope-reset-kept-outcome", "scope-kept-approval-cleared-grant", "drafted-approval", "parked-approval", "approval-revision", "removed-approval-revision", "invented-grant"} {
		for _, transport := range []string{"save", "replay"} {
			t.Run(scenario+"/"+transport, func(t *testing.T) {
				state := "approved"
				if strings.Contains(scenario, "grant") && scenario != "invented-grant" {
					state = "grant"
				} else if scenario == "drafted-approval" {
					state = "drafted"
				} else if scenario == "parked-approval" {
					state = "parked"
				}
				m, req, old, data, seal := briefHistoryFixture(t, state)
				spec := []byte(old.spec)
				if strings.HasPrefix(scenario, "scope-") {
					if req.Spec.Size != nativeGoalSize("S") {
						t.Fatal("scope control must change size S to M")
					}
					req.Spec.Size = "M"
					spec, _ = json.Marshal(req.Spec)
					if scenario == "scope-reset-kept-outcome" {
						data.State, data.ApprovalRevision, data.GrantBinding, data.Outcome = nativeGoalDrafted, 0, "", nativeOutcomeFailed
					} else if scenario == "scope-kept-approval-cleared-grant" {
						data.GrantBinding = ""
					}
				} else if scenario == "approval-revision" {
					data.ApprovalRevision = old.revision
				} else if scenario == "removed-approval-revision" {
					data.ApprovalRevision = 0
				} else if scenario == "invented-grant" {
					data.GrantBinding = "invented-grant"
				} else {
					// The ordinary approval operation rejects the wrong source.
					if _, err := m.decideGoal(goalDecisionRequest{OperationID: "wrong-source", GoalID: req.Spec.ID, ExpectedRevision: old.revision, Action: "approve", Reason: "Reviewed", Authority: "approved-plan", Evidence: "review", SourceState: "wrong"}); !errors.Is(err, errGoalPreflight) && scenario == "drafted-approval" {
						t.Fatal("ordinary approval accepted stale source", err)
					}
					data.State, data.ApprovalRevision = nativeGoalApproved, old.revision
				}
				record, err := encodeGoalRecord(data)
				if err != nil {
					t.Fatal(err)
				}
				if transport == "save" {
					before := goalCommitTree(t, m.store)
					_, projection, err := m.commitGoal(goalCommitRequest{OperationID: "counterfeit", ExpectedRevision: old.revision, Spec: spec, Seal: seal, Record: record, MutationSHA256: strings.Repeat("c", 64)}, goalCommitIO{})
					if !errors.Is(err, errGoalRecord) || projection != nil {
						t.Error("save accepted obsolete or unreviewed approval/grant", err)
					}
					if !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
						t.Error("refused save changed stored files")
					}
				} else {
					briefHistoryReplayCandidate(t, m, req.Spec.ID, old, spec, record)
				}
			})
		}
	}
}

func briefHistoryReplayCandidate(t *testing.T, m *Manager, id string, old goalReplayRecord, spec, record []byte, beforePublish ...func()) {
	t.Helper()
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		t.Fatal(err)
	}
	specRef, recordRef := goalBlobName("goal-spec-", spec), goalBlobName("goal-record-", record)
	if err := stageGoalBlob(root, specRef, spec, goalCommitIO{}); err != nil {
		t.Fatal(err)
	}
	if err := stageGoalBlob(root, recordRef, record, goalCommitIO{}); err != nil {
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
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	mutation := strings.Repeat("c", 64)
	event := goalJournalEvent{Kind: "goal.spec", GoalID: id, Revision: old.revision + 1, SpecRef: specRef, SealRef: old.sealRef, RecordRef: recordRef, OperationID: "counterfeit", MutationSHA256: mutation}
	event.InputSHA256 = goalRecordFingerprint(goalInputFingerprint(id, old.revision, specRef, old.sealRef), recordRef, mutation)
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	// Construct and checksum the frame independently of every writer. Reopen
	// the on-disk journal so no writer projection or cached reader is involved.
	// The fixture manager still watches the store. An in-place rewrite briefly
	// exposes an empty journal, letting it recover the prepared run as legacy.
	// Publish a complete candidate so readers see either complete history.
	candidate := goalJournalName + ".candidate"
	goalReplayWrite(t, m.store, candidate, append(prefix[:len(prefix):len(prefix)], goalReplayFrame(p.sequence+1, payload)...))
	for _, observe := range beforePublish {
		observe()
	}
	if err := t.Context().Err(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(m.store, candidate), filepath.Join(m.store, goalJournalName)); err != nil {
		t.Fatal("fixture publication failed")
	}
	before := goalCommitTree(t, m.store)
	got, err := replayGoalJournal(m.store)
	if !errors.Is(err, errGoalReplay) || got != nil {
		t.Error("fresh replay accepted obsolete or unreviewed approval/grant", err)
	}
	if !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
		t.Fatal("replay changed saved history")
	}
}

func TestBriefHistoryLegitimateUpdates(t *testing.T) {
	for _, state := range []string{"approved", "grant"} {
		t.Run(state, func(t *testing.T) {
			m, req, old, data, seal := briefHistoryFixture(t, state)
			note := goalNoteRequest{OperationID: "append-note", ExpectedRevision: old.revision, GoalID: req.Spec.ID, Note: goalRecordNote{Text: "Keep reviewed history"}}
			result, err := m.noteGoal(note)
			if err != nil {
				t.Fatal(err)
			}
			if retry, err := m.noteGoal(note); err != nil || retry != result {
				t.Fatal("note receipt changed", err)
			}
			req.Spec.Title, req.Spec.Order = "Presentation only", req.Spec.Order+1
			req.OperationID, req.ExpectedRevision, req.Seal = "presentation", result.Revision, seal
			if _, err := m.updateGoal(req); err != nil {
				t.Fatal(err)
			}
			p, err := replayGoalJournal(m.store)
			if err != nil {
				t.Fatal(err)
			}
			got, err := goalRecordMetadata(p.records[req.Spec.ID])
			data.Notes = append(data.Notes, note.Note)
			if err != nil || !reflect.DeepEqual(data, got) {
				t.Fatal("notes or presentation changed approval, grant or decision history", err)
			}
			req.Spec.Size = "M"
			req.OperationID, req.ExpectedRevision = "scope-update", p.records[req.Spec.ID].revision
			if _, err := m.updateGoal(req); err != nil {
				t.Fatal(err)
			}
			p, err = replayGoalJournal(m.store)
			if err != nil {
				t.Fatal(err)
			}
			got, err = goalRecordMetadata(p.records[req.Spec.ID])
			data.State, data.Outcome, data.ApprovalRevision, data.GrantBinding = nativeGoalDrafted, "", 0, ""
			if err != nil || !reflect.DeepEqual(data, got) || p.records[req.Spec.ID].sealRef != old.sealRef {
				t.Fatal("scope update failed to revoke approval/grant while retaining history and seal", err)
			}
		})
	}
}
