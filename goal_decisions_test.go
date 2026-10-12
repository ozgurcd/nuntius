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

func goalDecisionFixture(t *testing.T, state nativeGoalState, attempt *goalDecisionAttempt) (*Manager, goalDecisionRequest) {
	t.Helper()
	m, create := goalRecordsFixture(t)
	spec, _ := json.Marshal(create.Spec)
	data := goalRecordData{Version: 1, State: state, Notes: []goalRecordNote{}, Attempt: attempt}
	if state == nativeGoalEnded || state == nativeGoalVerified {
		data.Outcome = nativeOutcomeCompleted
	}
	if state == nativeGoalApproved {
		data.ApprovalRevision = 1
	}
	record, err := encodeGoalRecord(data)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = m.commitGoal(goalCommitRequest{OperationID: "fixture", Spec: spec, Seal: []byte("sealed fixture"), Record: record, MutationSHA256: strings.Repeat("a", 64)}, goalCommitIO{})
	if err != nil {
		t.Fatal(err)
	}
	return m, goalDecisionRequest{OperationID: "decision", ExpectedRevision: 1, GoalID: create.Spec.ID, Action: "park", Reason: "Review before continuing", Authority: "approved-plan", Evidence: "review-record"}
}

func TestGoalTransitions(t *testing.T) {
	states := []nativeGoalState{nativeGoalDrafted, nativeGoalApproved, nativeGoalRunning, nativeGoalEnded, nativeGoalVerified, nativeGoalCancelled, nativeGoalParked}
	// Independent D2 oracle: includes future lifecycle edges, without exposing
	// dispatch/end/continuation as judge decisions in this component.
	edges := map[nativeGoalState][]nativeGoalState{
		nativeGoalDrafted:  {nativeGoalApproved, nativeGoalParked, nativeGoalCancelled},
		nativeGoalApproved: {nativeGoalRunning, nativeGoalParked, nativeGoalCancelled},
		nativeGoalRunning:  {nativeGoalEnded},
		nativeGoalEnded:    {nativeGoalVerified, nativeGoalParked, nativeGoalCancelled, nativeGoalRunning},
		nativeGoalParked:   {nativeGoalApproved, nativeGoalCancelled},
	}
	for _, from := range states {
		for _, to := range states {
			want := false
			for _, candidate := range edges[from] {
				if candidate == to {
					want = true
				}
			}
			if got := goalTransitionAllowed(from, to); got != want {
				t.Errorf("D2 %s -> %s = %v, want %v", from, to, got, want)
			}
		}
	}
	for _, from := range states {
		for _, action := range []string{"approve", "verify", "park", "cancel"} {
			t.Run(string(from)+"/"+action, func(t *testing.T) {
				m, req := goalDecisionFixture(t, from, &goalDecisionAttempt{ID: "attempt-2", Ended: from != nativeGoalRunning, ReportDigest: strings.Repeat("b", 64)})
				p, _, _ := readGoalRecords(m.store)
				old := p.records[req.GoalID]
				data, _ := goalRecordMetadata(old)
				spec, _ := p.spec(req.GoalID)
				req.Action = action
				if action == "verify" {
					req.AttemptID, req.ReportDigest = "attempt-2", strings.Repeat("b", 64)
				}
				seal := &goalDecisionSeal{Revision: 1, SpecRef: goalBlobName("goal-spec-", []byte(old.spec)), SealRef: old.sealRef, Authority: req.Authority}
				got, err := applyGoalDecision(old, spec, data, req, seal)
				to := map[string]nativeGoalState{"approve": nativeGoalApproved, "verify": nativeGoalVerified, "park": nativeGoalParked, "cancel": nativeGoalCancelled}[action]
				want := false
				for _, candidate := range edges[from] {
					if candidate == to {
						want = true
					}
				}
				if want {
					if err != nil || got.State != to || len(got.Decisions) != 1 || got.Decisions[0].Reason != req.Reason || got.Decisions[0].Authority != req.Authority || got.Decisions[0].Evidence != req.Evidence {
						t.Fatal("allowed decision lost state or review evidence", err)
					}
					if got.Outcome != data.Outcome || got.GrantBinding != "" {
						t.Fatal("decision changed outcome or granted automation")
					}
				} else if err == nil || !reflect.DeepEqual(got, data) {
					t.Fatal("refused decision changed data")
				}
			})
		}
	}
}

func TestGoalTransitionsApproval(t *testing.T) {
	for _, state := range []nativeGoalState{nativeGoalDrafted, nativeGoalParked} {
		m, req := goalDecisionFixture(t, state, nil)
		req.Action = "approve"
		before, _ := os.ReadFile(filepath.Join(m.store, goalJournalName))
		if _, err := m.decideGoal(req); !errors.Is(err, errGoalApprovalUnavailable) {
			t.Fatal("production accepted opaque seal", err)
		}
		after, _ := os.ReadFile(filepath.Join(m.store, goalJournalName))
		if !reflect.DeepEqual(before, after) {
			t.Fatal("refused approval appended history")
		}
		p, _, _ := readGoalRecords(m.store)
		old := p.records[req.GoalID]
		data, _ := goalRecordMetadata(old)
		spec, _ := p.spec(req.GoalID)
		valid := goalDecisionSeal{Revision: 1, SpecRef: goalBlobName("goal-spec-", []byte(old.spec)), SealRef: old.sealRef, Authority: req.Authority}
		for _, field := range []string{"revision", "spec", "seal", "authority"} {
			bad := valid
			switch field {
			case "revision":
				bad.Revision++
			case "spec":
				bad.SpecRef += "bad"
			case "seal":
				bad.SealRef = ""
			case "authority":
				bad.Authority = "other"
			}
			if _, err := applyGoalDecision(old, spec, data, req, &bad); err == nil {
				t.Fatal("accepted mismatched sealed fixture", field)
			}
		}
		spec.OwnerHold = &nativeOwnerHold{Categories: []string{"release"}, Reason: "Owner approval required"}
		req.Authority = "source-owner"
		valid.Authority = req.Authority
		if _, err := applyGoalDecision(old, spec, data, req, &valid); !errors.Is(err, errGoalOwnerHold) || spec.OwnerHold == nil {
			t.Fatal("judge reference cleared owner hold")
		}
	}
}

func TestGoalTransitionsReview(t *testing.T) {
	for _, scenario := range []string{"report-alone", "stale-attempt", "stale-report", "active-successor", "uncertain-launch", "not-ended", "no-accounting", "alternate", "current-review", "alternate-without-run"} {
		t.Run(scenario, func(t *testing.T) {
			a := &goalDecisionAttempt{ID: "attempt-2", Ended: true, ReportDigest: strings.Repeat("b", 64)}
			if scenario == "no-accounting" {
				a = nil
			}
			if scenario == "active-successor" {
				a.ActiveSuccessor = true
			}
			if scenario == "uncertain-launch" {
				a.LaunchUnconfirmed = true
			}
			if scenario == "not-ended" {
				a.Ended = false
			}
			if scenario == "alternate-without-run" {
				a.ID, a.ReportDigest = "", ""
			}
			m, req := goalDecisionFixture(t, nativeGoalEnded, a)
			req.Action, req.AttemptID, req.ReportDigest = "verify", "attempt-2", strings.Repeat("b", 64)
			if scenario == "report-alone" {
				req.Evidence = ""
			}
			if scenario == "stale-attempt" {
				req.AttemptID = "attempt-1"
			}
			if scenario == "stale-report" {
				req.ReportDigest = strings.Repeat("c", 64)
			}
			if scenario == "alternate" || scenario == "alternate-without-run" {
				req.AlternateEvidence, req.ReportDigest = true, ""
			}
			if scenario == "alternate-without-run" {
				req.AttemptID = ""
			}
			before, _ := os.ReadFile(filepath.Join(m.store, goalJournalName))
			_, err := m.decideGoal(req)
			want := scenario == "alternate" || scenario == "current-review" || scenario == "alternate-without-run"
			if want {
				view, found, readErr := m.readGoal(req.GoalID)
				if err != nil || readErr != nil || !found || view.State != nativeGoalVerified || len(view.Decisions) != 1 {
					t.Fatal("review did not persist verification", err, readErr)
				}
			} else {
				if err == nil {
					t.Fatal("unsafe review accepted")
				}
				after, _ := os.ReadFile(filepath.Join(m.store, goalJournalName))
				if !reflect.DeepEqual(before, after) {
					t.Fatal("refusal changed history")
				}
			}
		})
	}
}

func TestGoalTransitionsDurable(t *testing.T) {
	m, req := goalDecisionFixture(t, nativeGoalDrafted, nil)
	first, err := m.decideGoal(req)
	if err != nil {
		t.Fatal(err)
	}
	view, _, err := m.readGoal(req.GoalID)
	if err != nil || view.State != nativeGoalParked || len(view.Decisions) != 1 {
		t.Fatal("park not visible", err)
	}
	view.Decisions[0].Reason = "mutated returned value"
	p, _, err := readGoalRecords(m.store)
	if err != nil {
		t.Fatal(err)
	}
	durable, _ := goalRecordMetadata(p.records[req.GoalID])
	if durable.Decisions[0].Reason != req.Reason {
		t.Fatal("view shared decision data")
	}
	restarted := &Manager{store: m.store}
	if again, err := restarted.decideGoal(req); err != nil || !reflect.DeepEqual(again, first) {
		t.Fatal("restart lost decision receipt", err)
	}
	changed := req
	changed.Reason = "Different judgment"
	if _, err := restarted.decideGoal(changed); !errors.Is(err, errGoalOperation) {
		t.Fatal("operation identity accepted changed decision", err)
	}
	if again, err := m.decideGoal(req); err != nil || !reflect.DeepEqual(again, first) {
		t.Fatal("retry duplicated or lost receipt", err)
	}
	stale := req
	stale.OperationID = "stale"
	if _, err := m.decideGoal(stale); !errors.Is(err, errGoalConflict) {
		t.Fatal("stale decision accepted", err)
	}
	cancel := req
	cancel.OperationID, cancel.ExpectedRevision, cancel.Action = "cancel", 2, "cancel"
	if _, err := m.decideGoal(cancel); err != nil {
		t.Fatal(err)
	}
	if again, err := m.decideGoal(req); err != nil || !reflect.DeepEqual(again, first) {
		t.Fatal("old receipt changed after terminal decision", err)
	}
	reopen := req
	reopen.OperationID, reopen.ExpectedRevision, reopen.Action = "reopen", 3, "approve"
	if _, err := m.decideGoal(reopen); !errors.Is(err, errGoalTransition) {
		t.Fatal("terminal reopened", err)
	}
	note := goalNoteRequest{OperationID: "correction", ExpectedRevision: 3, GoalID: req.GoalID, Note: goalRecordNote{Text: "Correction evidence", Evidence: "review-addendum"}}
	if _, err := m.noteGoal(note); err != nil {
		t.Fatal(err)
	}
	got, _, err := m.readGoal(req.GoalID)
	if err != nil || got.State != nativeGoalCancelled || len(got.Decisions) != 2 || len(got.Notes) != 1 {
		t.Fatal("permanent result or history lost", err)
	}
	for _, name := range []string{"lane-control.json", "lane-control-intent.json", "queue", "runs"} {
		if _, err := os.Stat(filepath.Join(m.store, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("bookkeeping created operational artifact", name)
		}
	}
}

func TestGoalTransitionsNoProcessStop(t *testing.T) {
	m, _ := laneControlCommitFixture(t)
	m.mu.Lock()
	m.Grace = 10 * time.Millisecond
	m.mu.Unlock()
	if _, err := m.Dispatch("codex", "LIVE-FIXTURE", "ignore-int"); err != nil {
		t.Fatal(err)
	}
	waitReady(t, m, "codex")
	r, err := m.latest("codex")
	if err != nil || !sameProcess(*r) {
		t.Fatal("fake child not live")
	}
	defer func() {
		if err := m.Cancel("codex"); err != nil {
			t.Error("fixture cancel failed")
		}
		waitState(t, m, "codex", "ended")
		if sameProcess(*r) {
			t.Error("offline fake child survived teardown")
		}
		t.Log("offline fake child ended and verified inside test")
	}()
	spec, _ := decodeNativeGoalSpec(nativeSchemaBytes(t, nativeSchemaFixture()))
	data := goalRecordData{Version: 1, State: nativeGoalRunning, Notes: []goalRecordNote{}, Attempt: &goalDecisionAttempt{ID: "attempt-live"}}
	record, _ := encodeGoalRecord(data)
	specBytes, _ := json.Marshal(spec)
	if _, _, err := m.commitGoal(goalCommitRequest{OperationID: "fixture", Spec: specBytes, Record: record, MutationSHA256: strings.Repeat("a", 64)}, goalCommitIO{}); err != nil {
		t.Fatal(err)
	}
	before := admissionArtifacts(t, m.store)
	for _, action := range []string{"park", "cancel"} {
		req := goalDecisionRequest{OperationID: action, ExpectedRevision: 1, GoalID: spec.ID, Action: action, Reason: "Review the live fixture", Authority: "fixture-plan", Evidence: "fixture-review"}
		if _, err := m.decideGoal(req); !errors.Is(err, errGoalDecisionBusy) {
			t.Fatal("running bookkeeping did not refuse", err)
		}
		current, err := m.latest("codex")
		if err != nil || current.ID != r.ID || current.StopAt != nil || !sameProcess(*r) || !reflect.DeepEqual(before, admissionArtifacts(t, m.store)) {
			t.Fatal("bookkeeping interrupted or changed live run")
		}
	}
}

func TestGoalTransitionsStrictAndHistory(t *testing.T) {
	m, req := goalDecisionFixture(t, nativeGoalEnded, &goalDecisionAttempt{ID: "attempt-current", Ended: true})
	for _, field := range []string{"reason-empty", "reason-newline", "reason-limit", "reason-utf8", "authority", "evidence", "action", "irrelevant-review"} {
		bad := req
		switch field {
		case "reason-empty":
			bad.Reason = " "
		case "reason-newline":
			bad.Reason = "one\ntwo"
		case "reason-limit":
			bad.Reason = strings.Repeat("é", 257)
		case "reason-utf8":
			bad.Reason = string([]byte{0xff})
		case "authority":
			bad.Authority = strings.Repeat("a", 129)
		case "evidence":
			bad.Evidence = ""
		case "action":
			bad.Action = "dispatch"
		case "irrelevant-review":
			bad.AlternateEvidence = true
		}
		before := goalCommitTree(t, m.store)
		if _, err := m.decideGoal(bad); !errors.Is(err, errGoalDecision) || !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
			t.Fatal("invalid decision wrote files", field, err)
		}
	}
	if _, err := m.decideGoal(req); err != nil {
		t.Fatal(err)
	}
	p, _, _ := readGoalRecords(m.store)
	old := p.records[req.GoalID]
	data, _ := goalRecordMetadata(old)
	for _, mutation := range []string{"erase", "rewrite", "wrong-revision", "wrong-transition"} {
		changed := data
		changed.Decisions = append([]goalDecisionEvidence(nil), data.Decisions...)
		switch mutation {
		case "erase":
			changed.Decisions = nil
		case "rewrite":
			changed.Decisions[0].Reason = "Rewritten history"
		case "wrong-revision":
			d := changed.Decisions[0]
			d.Revision = 1
			changed.Decisions = append(changed.Decisions, d)
		case "wrong-transition":
			d := changed.Decisions[0]
			d.Revision, d.Action = 2, "park"
			changed.Decisions = append(changed.Decisions, d)
		}
		b, _ := json.Marshal(changed)
		next := old
		next.revision++
		next.record = string(b)
		if goalRecordHistoryValid(old, next) {
			t.Fatal("accepted counterfeit decision history", mutation)
		}
	}
	base := `{"version":1,"state":"ended","notes":[],"attempt":{"id":"a","ended":true,"active_successor":false,"launch_unconfirmed":false}}`
	for _, b := range []string{
		strings.Replace(base, `"ended":true`, `"ended":"true"`, 1),
		strings.Replace(base, `"ended":true`, `"ended":null`, 1),
		strings.Replace(base, `"id":"a"`, `"id":"a","id":"b"`, 1),
		strings.Replace(base, `"id":"a"`, `"id":"a","source":"owner"`, 1),
		strings.Replace(base, `"id":"a"`, `"id":"a","report_digest":null`, 1),
	} {
		if _, err := decodeGoalRecord([]byte(b)); err == nil {
			t.Fatal("invalid attempt observation accepted")
		}
	}
}

func TestGoalTransitionsCapacity(t *testing.T) {
	goalEndParallel(t)
	m, create := goalRecordsFixture(t)
	if _, err := m.createGoal(create); err != nil {
		t.Fatal(err)
	}
	for revision := uint64(1); revision <= 32; revision++ {
		before, _ := os.ReadFile(filepath.Join(m.store, goalJournalName))
		beforeTree := goalCommitTree(t, m.store)
		note := goalNoteRequest{OperationID: "note-" + strings.Repeat("a", int(revision)), ExpectedRevision: revision, GoalID: create.Spec.ID, Note: goalRecordNote{Text: strings.Repeat("\"", 1024), Evidence: strings.Repeat("e", 128)}}
		_, err := m.noteGoal(note)
		if err != nil {
			if revision != 30 || !errors.Is(err, errGoalRecordCapacity) || errors.Is(err, errGoalRecord) {
				t.Fatal("encoded capacity lacks distinct refusal", revision, err)
			}
			after, _ := os.ReadFile(filepath.Join(m.store, goalJournalName))
			got, found, readErr := m.readGoal(create.Spec.ID)
			if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(beforeTree, goalCommitTree(t, m.store)) || readErr != nil || !found || len(got.Notes) != 29 {
				t.Fatal("capacity dropped history or reads")
			}
			for _, saved := range got.Notes {
				if saved.Text != strings.Repeat("\"", 1024) || saved.Evidence != strings.Repeat("e", 128) {
					t.Fatal("capacity changed retained fixture content")
				}
			}
			invalid := note
			invalid.Note.Text = "invalid\nline"
			if _, err := m.noteGoal(invalid); !errors.Is(err, errGoalRecord) || errors.Is(err, errGoalRecordCapacity) {
				t.Fatal("invalid fields confused with encoded capacity", err)
			}
			return
		}
	}
	t.Fatal("escaped metadata capacity was not enforced")
}
