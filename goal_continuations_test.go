package nuntius

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// These cases prove the independent end/ownership component. Successor
// launching still requires a trusted fresh admission context.
func TestNativeContinuationAccounting(t *testing.T) {
	goalEndParallel(t)
	t.Run("paused-interruption-recovery-pending", func(t *testing.T) {
		base, original := goalEndSavedFixture(t)
		m, r := goalEndClone(t, base, original)
		m.config.AutoResume = nil
		successorPause(t, m)
		if err := goalEndPublish(t, m, &r, goalCommitIO{}); err != nil {
			t.Fatal(err)
		}
		_, data := dispatchRecord(t, m, r.GoalID)
		if !r.WaitingToResume || r.ResumeCount != 0 || data.State != nativeGoalEnded || data.Attempt == nil || !data.Attempt.ActiveSuccessor || !data.Ends[0].Held {
			t.Fatal("native interruption released pending recovery or spent a recovery attempt")
		}
		root, err := openLaneControlRoot(m.store)
		if err != nil {
			t.Fatal(err)
		}
		p, err := replayGoalJournalRoot(root)
		if err != nil {
			t.Fatal(err)
		}
		reservations, unknown := goalAdmissionReservations(p, []Run{r}, root, goalAdmissionEnvironment{})
		root.Close()
		if unknown || len(reservations) != 1 || reservations[0].id != r.GoalID || !reflect.DeepEqual(reservations[0].resources, []string{"dispatch-resource"}) {
			t.Fatal("pending interruption lost its single reservation")
		}
		n := dispatchReopen(t, m)
		before := goalCommitTree(t, n.store)
		if err := n.Recover(); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(before, goalCommitTree(t, n.store)) {
			t.Fatal("pending recovery maintenance rewrote immutable history")
		}
		old, _ := dispatchRecord(t, n, r.GoalID)
		for _, alternate := range []bool{false, true} {
			req := goalDecisionRequest{OperationID: "pending-review", GoalID: r.GoalID, ExpectedRevision: old.revision, Action: "verify", Reason: "Reviewed work", Authority: "approved-plan", Evidence: "review", AttemptID: r.ID, AlternateEvidence: alternate}
			if !alternate {
				req.ReportDigest = strings.Repeat("a", 64)
			}
			if _, err := n.decideGoal(req); !errors.Is(err, errGoalDecisionBusy) {
				t.Fatal("pending recovery bypassed review refusal", err)
			}
		}
		if _, err := successorControl(n, r.Lane, false, nil); err != nil {
			t.Fatal(err)
		}
		runs, err := n.history()
		if err != nil || len(runs) != 1 || runs[0].ContinueRunID != "" || runs[0].ResumeCount != 0 || !runs[0].WaitingToResume {
			t.Fatal("Resume launched or forgot pending recovery")
		}
		dispatchStarts(t, n, 1)
	})
	t.Run("claude-acknowledgement-pending", func(t *testing.T) {
		m, req, ctx := dispatchFixtureBody(t, "ack-goal ack-barrier\n", "claude")
		if _, err := m.dispatchGoal(req, ctx, goalCommitIO{}); err != nil {
			t.Fatal(err)
		}
		waitReady(t, m, req.Lane)
		if err := os.WriteFile(filepath.Join(m.config.Lanes[0].Workdir, "admission-release"), nil, 0600); err != nil {
			t.Fatal(err)
		}
		waitState(t, m, req.Lane, "ended")
		_, data := dispatchRecord(t, m, req.GoalID)
		if data.Attempt == nil || !data.Attempt.ActiveSuccessor || len(data.Ends) != 1 || !data.Ends[0].Held {
			t.Fatal("eligible native acknowledgement released its reservation before continuation selection")
		}
		dispatchStarts(t, m, 1)
	})
}

func TestNativeContinuationPendingRepair(t *testing.T) {
	goalEndParallel(t)
	base, original := goalEndSavedFixture(t)
	for _, stage := range []string{"end_meta_saved", "before_append", "partial_write", "journal_sync", "journal_directory_sync", "publication", "native_intent.write", "native_receipt.write"} {
		t.Run(stage, func(t *testing.T) {
			m, r := goalEndClone(t, base, original)
			m.config.AutoResume = nil
			if err := goalEndPublish(t, m, &r, dispatchOneFault(stage)); err == nil {
				t.Fatal("injected publication did not fail")
			}
			n := dispatchReopen(t, m)
			old, data := dispatchRecord(t, n, r.GoalID)
			if len(data.Ends) != 1 || data.Ends[0].Pending != "recovery_pending" || !data.Ends[0].Held || data.Attempt == nil || !data.Attempt.ActiveSuccessor || !data.Ends[0].Ended.Equal(*r.Ended) {
				t.Fatal("repair lost first-observation pending ownership")
			}
			if err := n.Recover(); err != nil {
				t.Fatal(err)
			}
			current, _ := dispatchRecord(t, n, r.GoalID)
			if current.revision != old.revision {
				t.Fatal("repair repeated native end")
			}
			runs, err := n.history()
			if err != nil || len(runs) != 1 || runs[0].ResumeCount != 0 || !runs[0].WaitingToResume || runs[0].ContinueRunID != "" {
				t.Fatal("repair spent recovery or created a successor")
			}
		})
	}
}

func TestNativeContinuationPendingHistory(t *testing.T) {
	goalEndParallel(t)
	base, original := goalEndSavedFixture(t)
	for _, mutation := range []string{"approval", "grant", "scope", "held", "pending", "attempt", "notes", "report"} {
		t.Run(mutation, func(t *testing.T) {
			m, r := goalEndClone(t, base, original)
			m.config.AutoResume = nil
			if err := goalEndPublish(t, m, &r, goalCommitIO{}); err != nil {
				t.Fatal(err)
			}
			old, data := dispatchRecord(t, m, r.GoalID)
			spec := []byte(old.spec)
			switch mutation {
			case "approval":
				data.ApprovalRevision++
			case "grant":
				data.GrantBinding = "invented"
			case "scope":
				s, _ := decodeNativeGoalSpec(spec)
				s.Resources = []string{"different"}
				spec, _ = json.Marshal(s)
			case "held":
				data.Ends[0].Held = false
			case "pending":
				data.Ends[0].Pending = ""
			case "attempt":
				data.Attempt.ActiveSuccessor = false
			case "notes":
				data.Ends[0].Pending = "message_pending"
				data.Notes = append(data.Notes, goalRecordNote{Text: "unrelated change"})
			case "report":
				data.Attempt.ReportDigest = strings.Repeat("a", 64)
			}
			record, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			seal, err := os.ReadFile(filepath.Join(m.store, old.sealRef))
			if err != nil {
				t.Fatal(err)
			}
			before := goalCommitTree(t, m.store)
			if _, _, err := m.commitGoal(goalCommitRequest{OperationID: "counterfeit", ExpectedRevision: old.revision, Spec: spec, Seal: seal, Record: record, MutationSHA256: strings.Repeat("a", 64)}, goalCommitIO{}); err == nil {
				t.Fatal("candidate changed pending authority/history")
			}
			if !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
				t.Fatal("refused candidate wrote bytes")
			}
			briefHistoryReplayCandidate(t, m, r.GoalID, old, spec, record)
		})
	}
}

func TestNativeContinuationPendingCodec(t *testing.T) {
	when := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	e := goalAttemptEnd{RunID: strings.Repeat("a", 32), Lane: "codex", PID: 2, ProcessStart: "identity", Started: when, Ended: when, Outcome: nativeOutcomeEndedNoReport, Held: true, Pending: "recovery_pending"}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	wrap := func(b []byte) []byte { return append(append([]byte(`{"native_end":`), b...), '}') }
	if !validGoalRunEndJSON(wrap(b)) || len(b) > 4096 {
		t.Fatal("canonical pending end exceeded codec bound")
	}
	for _, change := range []string{`"Pending":"recovery_pending"`, `"pending":null`, `"pending":false`, `"pending":"other"`, `"pending":"recovery_pending","pending":"recovery_pending"`, `"pending":"recovery_pending","p\u0065nding":"recovery_pending"`} {
		candidate := strings.Replace(string(b), `"pending":"recovery_pending"`, change, 1)
		if validGoalRunEndJSON(wrap([]byte(candidate))) {
			t.Fatal("noncanonical pending codec accepted")
		}
	}
	escaped := strings.Replace(string(b), `"pending"`, `"p\u0065nding"`, 1)
	if !validGoalRunEndJSON(wrap([]byte(escaped))) {
		t.Fatal("escaped exact canonical key refused")
	}
	for _, separator := range []string{"\u2028", "\u2029", "\u0085"} {
		if goalBriefLine("one" + separator + "two") {
			t.Fatal("one-line reference accepted a Unicode line separator")
		}
	}
	e.Held = false
	if validGoalAttemptEnd(e) {
		t.Fatal("pending end without reservation accepted")
	}
}

func TestNativeContinuationPendingReadAndReport(t *testing.T) {
	goalEndParallel(t)
	base, original := goalEndSavedFixture(t)
	m, r := goalEndClone(t, base, original)
	m.config.AutoResume = nil
	if err := goalEndPublish(t, m, &r, goalCommitIO{}); err != nil {
		t.Fatal(err)
	}
	read, err := newReadManager(m.config, m.store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = read.Close() })
	before := goalCommitTree(t, m.store)
	if _, err := read.Status(); err != nil {
		t.Fatal(err)
	}
	if _, err := read.listGoals(0, 100); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
		t.Fatal("passive pending views changed store bytes")
	}
	if err := os.WriteFile(filepath.Join(m.config.ReportsDir, r.GoalID+".txt"), []byte("late current report\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.Recover(); err != nil {
		t.Fatal(err)
	}
	old, data := dispatchRecord(t, m, r.GoalID)
	if len(data.Ends) != 1 || len(data.Reports) != 1 || data.Ends[0].Pending != "recovery_pending" || data.Attempt.ReportDigest == "" || !data.Attempt.ActiveSuccessor || goalAttemptReleased(data, r.ID) {
		t.Fatal("late report released pending recovery")
	}
	for _, alternate := range []bool{false, true} {
		req := goalDecisionRequest{OperationID: "late-review", GoalID: r.GoalID, ExpectedRevision: old.revision, Action: "verify", Reason: "Reviewed report", Authority: "approved-plan", Evidence: "review", AttemptID: r.ID, AlternateEvidence: alternate}
		if !alternate {
			req.ReportDigest = data.Attempt.ReportDigest
		}
		if _, err := m.decideGoal(req); !errors.Is(err, errGoalDecisionBusy) {
			t.Fatal("report or alternate verification bypassed pending ownership", err)
		}
	}
}

func TestNativeContinuationPendingExclusions(t *testing.T) {
	goalEndParallel(t)
	base, original := goalEndSavedFixture(t)
	for _, scenario := range []string{"disabled", "limit", "cancelled", "timed-out", "known-nonzero", "terminal-delivery", "completed", "removed", "changed-kind"} {
		t.Run(scenario, func(t *testing.T) {
			m, r := goalEndClone(t, base, original)
			m.config.AutoResume = nil
			switch scenario {
			case "disabled":
				off := false
				m.config.AutoResume = &off
			case "limit":
				r.ResumeCount = 2
			case "cancelled":
				r.StopReason, r.StopAt = "cancelled", r.Ended
			case "timed-out":
				r.StopReason, r.StopAt = "timed_out", r.Ended
			case "known-nonzero":
				exit := 7
				r.Exit = &exit
			case "terminal-delivery":
				r.StopReason = "report_delivery_failed"
			case "completed":
				r.StopReason = "completed"
			case "removed":
				m.config.Lanes = m.config.Lanes[:1]
			case "changed-kind":
				m.config.Lanes[1].Kind = "claude"
			}
			if branch := m.goalContinuationPendingLocked(&r, "", false); branch != "" || r.WaitingToResume || r.ResumeCount > 2 {
				t.Fatal("excluded observation became new recovery work")
			}
		})
	}
	// A later configuration change never clears an existing saved pending hold.
	m, r := goalEndClone(t, base, original)
	r.WaitingToResume = true
	if branch := m.goalContinuationPendingLocked(&r, "", false); branch != "recovery_pending" {
		t.Fatal("disabled recovery cleared preexisting ownership")
	}
	if branch := m.goalContinuationPendingLocked(&r, "accepted text", false); branch != "message_pending" {
		t.Fatal("accepted text lost branch priority")
	}
}

func TestNativeContinuationCanonicalPending(t *testing.T) {
	goalEndParallel(t)
	base, original := canonicalEndedFixture(t)
	m, r := goalEndClone(t, base, original)
	m.config.AutoResume = nil
	_, before := dispatchRecord(t, m, r.GoalID)
	if err := goalEndPublish(t, m, &r, goalCommitIO{}); err != nil {
		t.Fatal(err)
	}
	_, after := dispatchRecord(t, m, r.GoalID)
	if len(after.Dispatches) != 1 || !goalDispatchScopeValid(after.Dispatches[0]) || !reflect.DeepEqual(before.Dispatches, after.Dispatches) || after.ApprovalRevision != before.ApprovalRevision || after.GrantBinding != before.GrantBinding || after.Ends[0].Pending != "recovery_pending" || goalAttemptReleased(after, r.ID) {
		t.Fatal("pending native end lost approved canonical ownership")
	}
}
