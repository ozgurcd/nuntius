package nuntius

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Each group owns its store, configuration and compiled-fake children. Bound
// concurrency through cleanup so the complete lifecycle/fault matrix does not
// add all fixture setup and teardown times to the package's unchanged deadline.
var goalEndTestSlots = make(chan struct{}, 4)

func goalEndParallel(t *testing.T) {
	t.Helper()
	t.Parallel()
	goalEndTestSlots <- struct{}{}
	t.Cleanup(func() { <-goalEndTestSlots })
}

func TestGoalAutomaticEnd(t *testing.T) {
	goalEndParallel(t)
	for _, scenario := range []string{"normal", "no-report", "instant", "cancel", "timeout", "unknown-exit"} {
		t.Run(scenario, func(t *testing.T) {
			body := "ack-goal ack-barrier\n"
			lane := "claude"
			if scenario == "instant" {
				body = "ack-goal\n"
			}
			if scenario == "cancel" {
				body, lane = "wait\n", "codex"
			}
			if scenario == "timeout" {
				body, lane = "ignore-int\n", "codex"
			}
			if scenario == "unknown-exit" {
				body, lane = "exit-seven\n", "codex"
			}
			m, req, ctx := dispatchFixtureBody(t, body, lane)
			if scenario == "timeout" {
				m.runtime, m.Grace = 300*time.Millisecond, 20*time.Millisecond
			}
			result, err := m.dispatchGoal(req, ctx, goalCommitIO{})
			if err != nil && !errors.Is(err, errGoalLaunchHeld) {
				t.Fatal(err)
			}
			if scenario == "normal" {
				if err := os.WriteFile(filepath.Join(m.config.ReportsDir, req.GoalID+".txt"), []byte("Reviewed fixture report\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "normal" || scenario == "no-report" {
				if err := os.WriteFile(filepath.Join(m.config.Lanes[0].Workdir, "admission-release"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "cancel" {
				waitReady(t, m, req.Lane)
				if err := m.Cancel(req.Lane); err != nil {
					t.Fatal(err)
				}
			}
			waitState(t, m, req.Lane, "ended")
			_, data := dispatchRecord(t, m, req.GoalID)
			if data.State != nativeGoalEnded || data.Attempt == nil || !data.Attempt.Ended || data.Attempt.ID != result.RunID {
				t.Fatal("actual child end was not durably recorded as an open native ended attempt")
			}
			want := nativeOutcomeEndedNoReport
			if scenario == "normal" {
				want = nativeOutcomeCompleted
			}
			if scenario == "cancel" {
				want = nativeOutcomeCancelled
			}
			if scenario == "timeout" {
				want = nativeOutcomeTimedOut
			}
			if data.Outcome != want {
				t.Fatalf("outcome = %s, want %s", data.Outcome, want)
			}
			if scenario == "normal" && data.Attempt.ReportDigest == "" {
				t.Fatal("report was not bound to the attempt")
			}
			list, err := m.listGoals(0, 100)
			if err != nil || len(list.Open) != 1 || len(list.Terminal) != 0 {
				t.Fatal("ended work was automatically closed", err)
			}
			n := dispatchReopen(t, m)
			_, replayed := dispatchRecord(t, n, req.GoalID)
			if replayed.State != nativeGoalEnded {
				t.Fatal("fresh replay lost end")
			}
			dispatchStarts(t, m, 1)
		})
	}
}

// Detach only this fixture's supervisor, then wait for this exact child to
// disappear. Keep its records so faults and fresh managers use the real B11
// reservation and identity, rather than initial bookkeeping exceptions.
func goalEndSavedFixture(t *testing.T, capture ...*goalDispatchContext) (*Manager, Run) {
	t.Helper()
	return goalEndSavedFixtureSetup(t, nil, capture...)
}

func goalEndSavedFixtureSetup(t *testing.T, setup func(*Manager), capture ...*goalDispatchContext) (*Manager, Run) {
	t.Helper()
	m, req, ctx := dispatchFixture(t)
	if setup != nil {
		setup(m)
	}
	if len(capture) == 1 {
		*capture[0] = ctx
	}
	// These B12 cases test settled ends. B13 interruption cases explicitly
	// enable recovery on their own isolated clone before first publication.
	noRecovery := false
	m.config.AutoResume = &noRecovery
	stages := &dispatchStages{}
	result, err := m.dispatchGoal(req, ctx, stages.io())
	if err != nil {
		goalDispatchDiagnosis(t, m, req, result, stages, err)
		t.Fatal(err)
	}
	waitReady(t, m, req.Lane)
	r, err := loadRun(filepath.Join(m.store, "runs", result.RunID), result.RunID)
	if err != nil || r == nil {
		t.Fatal("fixture identity")
	}
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
	if err = signalRun(*r, syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(4 * time.Second); !goalAttemptDefinitelyGone(*r); {
		if time.Now().After(deadline) {
			t.Fatal("offline child did not end")
		}
		time.Sleep(time.Millisecond)
	}
	now := time.Now().UTC()
	r.Ended = &now
	r.State = "ended"
	r.StopReason = "ended_no_report"
	if err = m.save(r); err != nil {
		t.Fatal(err)
	}
	t.Log("offline child ended and identity absence verified before end repair")
	return m, *r
}

func goalEndPublish(t *testing.T, m *Manager, r *Run, fault goalCommitIO) error {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	guard, err := m.controlLock()
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	return m.publishGoalAttemptEndLocked(r, fault)
}

// The parent test starts and verifies the real offline child. Each table case
// gets an independent private copy of its noninitial approved/dispatched/end
// history, so faults cannot share receipts or repairs. No child is fabricated
// or launched for a copy; all copies refer to the verified absent full identity.
func goalEndClone(t *testing.T, base *Manager, original Run) (*Manager, Run) {
	t.Helper()
	store := filepath.Join(t.TempDir(), "store")
	if err := os.Mkdir(store, 0700); err != nil {
		t.Fatal(err)
	}
	err := filepath.WalkDir(base.store, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == base.store {
			return nil
		}
		relative, err := filepath.Rel(base.store, path)
		if err != nil {
			return err
		}
		target := filepath.Join(store, relative)
		if entry.IsDir() {
			return os.Mkdir(target, 0700)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("fixture history contains a nonregular file")
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0600)
	})
	if err != nil {
		t.Fatal(err)
	}
	m, err := newReadManager(base.config, store)
	if err != nil {
		t.Fatal(err)
	}
	m.readOnly = false
	t.Cleanup(func() { _ = m.Close() })
	if !goalAttemptDefinitelyGone(original) {
		t.Fatal("copied fixture identity is still live")
	}
	return m, original
}

func TestGoalAutomaticEndRestartAndReadOnly(t *testing.T) {
	goalEndParallel(t)
	m, r := goalEndSavedFixture(t)
	before := goalCommitTree(t, m.store)
	n, err := newReadManager(m.config, m.store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = n.Status(); err != nil {
		t.Fatal(err)
	}
	if _, err = n.listGoals(0, 100); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
		t.Fatal("read-only lookup repaired history")
	}
	fresh := dispatchReopen(t, m)
	p, err := replayGoalJournal(fresh.store)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := goalRecordMetadata(p.records[r.GoalID])
	if data.State != nativeGoalEnded || len(data.Ends) != 1 || !data.Ends[0].Ended.Equal(*r.Ended) || data.Ends[0].RunID != r.ID {
		t.Fatal("restart skipped saved end")
	}
	sequence := p.sequence
	if err = fresh.Recover(); err != nil {
		t.Fatal(err)
	}
	p, err = replayGoalJournal(fresh.store)
	if err != nil || p.sequence != sequence {
		t.Fatal("restart duplicated end", err)
	}
	dispatchStarts(t, m, 1)
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	_, rows, _, _, err := readNativeAuditRecords(root)
	if err != nil || rows[len(rows)-1].Result != "attempt_ended" {
		t.Fatal("durable audit did not identify attempt end", err)
	}
}

func TestGoalAutomaticEndJournalFaults(t *testing.T) {
	goalEndParallel(t)
	base, original := goalEndSavedFixture(t)
	for _, stage := range []string{"end_meta_saved", "blob_write", "blob_sync", "blob_publish", "blobs_sync", "before_append", "partial_write", "journal_sync", "journal_directory_sync", "publication", "native_intent.write", "native_intent.dirsync", "native_receipt.write", "native_receipt.dirsync"} {
		t.Run(stage, func(t *testing.T) {
			m, r := goalEndClone(t, base, original)
			fault := goalCommitIO{checkpoint: func(s string) error {
				if s == stage {
					return errors.New("injected end delivery failure")
				}
				return nil
			}}
			if err := goalEndPublish(t, m, &r, fault); err == nil {
				t.Fatal("fault did not hold end delivery", stage)
			}
			// Fresh operation must reconstruct the end even without an in-memory
			// pending queue; a complete uncertain frame keeps its original receipt.
			n := dispatchReopen(t, m)
			p, err := replayGoalJournal(n.store)
			if err != nil {
				t.Fatal(err)
			}
			data, _ := goalRecordMetadata(p.records[r.GoalID])
			if len(data.Ends) != 1 || data.State != nativeGoalEnded || data.Ends[0].RunID != r.ID || !data.Ends[0].Ended.Equal(*r.Ended) {
				t.Fatal("end lost during repair", stage)
			}
			seq := p.sequence
			if err = n.Recover(); err != nil {
				t.Fatal(err)
			}
			p, err = replayGoalJournal(n.store)
			if err != nil || p.sequence != seq {
				t.Fatal("repair repeated end", err)
			}
			dispatchStarts(t, m, 1)
		})
	}
}

func TestGoalAutomaticEndLateReportAndBounds(t *testing.T) {
	goalEndParallel(t)
	m, r := goalEndSavedFixture(t)
	if err := goalEndPublish(t, m, &r, goalCommitIO{}); err != nil {
		t.Fatal(err)
	}
	p, _ := replayGoalJournal(m.store)
	seq := p.sequence
	path := filepath.Join(m.config.ReportsDir, r.GoalID+".txt")
	for _, scenario := range []string{"stale", "symlink", "boundary", "oversize", "changed", "current"} {
		t.Run(scenario, func(t *testing.T) {
			_ = os.Remove(path)
			body := []byte("late fixture report\n")
			if scenario == "oversize" {
				body = []byte(strings.Repeat("x", goalAttemptReportMaxBytes+1))
			}
			if scenario == "boundary" {
				body = []byte(strings.Repeat("x", goalAttemptReportMaxBytes))
			}
			if err := os.WriteFile(path, body, 0600); err != nil {
				t.Fatal(err)
			}
			if scenario == "stale" {
				when := r.Started.Add(-time.Minute)
				if err := os.Chtimes(path, when, when); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "symlink" {
				if err := os.Rename(path, path+".target"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".target", path); err != nil {
					t.Fatal(err)
				}
			}
			var change func() error
			if scenario == "changed" {
				change = func() error { return os.WriteFile(path, []byte("changed fixture bytes"), 0600) }
			}
			observation := m.observeGoalAttemptReport(r, change)
			if scenario == "boundary" {
				if observation == nil || observation.Bytes != goalAttemptReportMaxBytes || observation.Digest != goalBriefDigest(body) {
					t.Fatal("complete report at byte bound refused")
				}
				return
			}
			if scenario != "current" {
				if observation != nil {
					t.Fatal("unsafe report became evidence", scenario)
				}
				return
			}
			if observation == nil || observation.Digest != goalBriefDigest(body) {
				t.Fatal("stable full report digest absent")
			}
			if err := m.publishGoalAttemptReportLocked(r, goalCommitIO{}); err != nil {
				t.Fatal(err)
			}
			p, err := replayGoalJournal(m.store)
			if err != nil {
				t.Fatal(err)
			}
			data, _ := goalRecordMetadata(p.records[r.GoalID])
			if p.sequence != seq+1 || len(data.Ends) != 1 || len(data.Reports) != 1 || data.State != nativeGoalEnded || data.Attempt.ReportDigest != observation.Digest {
				t.Fatal("late report repeated end or verified goal")
			}
		})
	}
}

func TestGoalAutomaticEndHistoryIntegrity(t *testing.T) {
	goalEndParallel(t)
	base, original := goalEndSavedFixture(t)
	for _, mutation := range []string{"approval", "grant", "scope", "seal", "notes", "verified", "identity", "outcome", "report", "rewrite"} {
		t.Run(mutation, func(t *testing.T) {
			m, r := goalEndClone(t, base, original)
			old, data := dispatchRecord(t, m, r.GoalID)
			end := goalAttemptEnd{RunID: r.ID, Lane: r.Lane, PID: r.PID, ProcessStart: r.ProcessStart, Job: r.LaunchdLabel, Started: r.Started, Ended: *r.Ended, Outcome: nativeOutcomeEndedNoReport}
			next, err := applyGoalAttemptEnd(data, end)
			if err != nil {
				t.Fatal(err)
			}
			spec := []byte(old.spec)
			switch mutation {
			case "approval":
				next.ApprovalRevision++
			case "grant":
				next.GrantBinding = "invented"
			case "scope":
				s, _ := decodeNativeGoalSpec(spec)
				s.Resources = []string{"different"}
				spec, _ = json.Marshal(s)
			case "seal":
				next.Dispatches = append([]goalDispatchRecord(nil), next.Dispatches...)
				next.Dispatches[0].SealRef = goalBlobName("goal-seal-", []byte("invented"))
			case "notes":
				next.Notes = append(next.Notes, goalRecordNote{Text: "unrelated change"})
			case "verified":
				next.State = nativeGoalVerified
			case "identity":
				next.Ends[0].PID++
			case "outcome":
				next.Outcome = nativeOutcomeFailed
			case "report":
				next.Attempt.ReportDigest = strings.Repeat("a", 64)
			case "rewrite":
				if err = goalEndPublish(t, m, &r, goalCommitIO{}); err != nil {
					t.Fatal(err)
				}
				old, next = dispatchRecord(t, m, r.GoalID)
				next.Ends[0].Outcome = nativeOutcomeFailed
				next.Outcome = nativeOutcomeFailed
			}
			record, _ := json.Marshal(next)
			seal, err := os.ReadFile(filepath.Join(m.store, old.sealRef))
			if err != nil {
				t.Fatal(err)
			}
			before := goalCommitTree(t, m.store)
			if _, _, err = m.commitGoal(goalCommitRequest{OperationID: "counterfeit", ExpectedRevision: old.revision, Spec: spec, Seal: seal, Record: record, MutationSHA256: strings.Repeat("a", 64)}, goalCommitIO{}); err == nil {
				t.Fatal("writer accepted unrelated end mutation", mutation)
			}
			if !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
				t.Fatal("refusal wrote history")
			}
			briefHistoryReplayCandidate(t, m, r.GoalID, old, spec, record)
		})
	}
}

func TestGoalAutomaticEndPausedCurrent(t *testing.T) {
	goalEndParallel(t)
	for _, after := range []bool{false, true} {
		t.Run(map[bool]string{false: "pause", true: "after-current"}[after], func(t *testing.T) {
			m, req, ctx := dispatchFixtureBody(t, "admission-barrier\n", "codex")
			result, err := m.dispatchGoal(req, ctx, goalCommitIO{})
			if err != nil {
				t.Fatal(err)
			}
			waitReady(t, m, req.Lane)
			before, _ := m.latest(req.Lane)
			// Use the existing locked A commit after acquiring the same manager
			// and control locks; public TryLock contention is a separate contract.
			m.mu.Lock()
			guard, pauseErr := m.controlLock()
			if pauseErr == nil {
				root, openErr := openLaneControlRoot(m.store)
				if openErr == nil {
					_, pauseErr = m.setLaneControlLocked(root, laneControlChange{Lane: req.Lane, Paused: true, AfterCurrent: after}, "cli", nil)
					root.Close()
				} else {
					pauseErr = openErr
				}
				guard.Close()
			}
			m.mu.Unlock()
			if pauseErr != nil {
				t.Fatal("locked pause failed", pauseErr)
			}
			control, err := m.readLaneControlsLocked()
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(m.config.ReportsDir, req.GoalID+".txt"), []byte("current fixture report"), 0600); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(m.config.ReportsDir, "admission-release"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			waitState(t, m, req.Lane, "ended")
			afterControl, err := m.readLaneControlsLocked()
			if err != nil || !reflect.DeepEqual(control, afterControl) {
				t.Fatal("end bookkeeping changed lane control")
			}
			r, _ := m.latest(req.Lane)
			_, data := dispatchRecord(t, m, req.GoalID)
			if r.ID != result.RunID || r.ProcessStart != before.ProcessStart || r.StopAt != nil || data.State != nativeGoalEnded || data.Outcome != nativeOutcomeCompleted {
				t.Fatal("pause changed current run or blocked native end")
			}
			dispatchStarts(t, m, 1)
		})
	}
}

func TestGoalAutomaticEndResourceSafety(t *testing.T) {
	goalEndParallel(t)
	for _, held := range []bool{false, true} {
		t.Run(map[bool]string{false: "settled", true: "recovery-pending"}[held], func(t *testing.T) {
			m, r := goalEndSavedFixture(t)
			r.WaitingToResume = held
			if err := m.save(&r); err != nil {
				t.Fatal(err)
			}
			if err := goalEndPublish(t, m, &r, goalCommitIO{}); err != nil {
				t.Fatal(err)
			}
			p, err := replayGoalJournal(m.store)
			if err != nil {
				t.Fatal(err)
			}
			root, err := openLaneControlRoot(m.store)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			reservations, unknown := goalAdmissionReservations(p, []Run{r}, root, goalAdmissionEnvironment{})
			if unknown || (len(reservations) > 0) != held {
				t.Fatal("end released pending work or retained settled resource")
			}
			data, _ := goalRecordMetadata(p.records[r.GoalID])
			if data.Attempt.ActiveSuccessor != held {
				t.Fatal("judge review did not retain recovery blocker")
			}
			before := p.sequence
			if err := goalEndPublish(t, m, &r, goalCommitIO{}); err != nil {
				t.Fatal(err)
			}
			p, err = replayGoalJournal(m.store)
			if err != nil || p.sequence != before {
				t.Fatal("duplicate end appended history")
			}
		})
	}
}

func TestGoalAutomaticEndClosedCodec(t *testing.T) {
	goalEndParallel(t)
	m, r := goalEndSavedFixture(t)
	if err := goalEndPublish(t, m, &r, goalCommitIO{}); err != nil {
		t.Fatal(err)
	}
	_, data := dispatchRecord(t, m, r.GoalID)
	b, _ := json.Marshal(data)
	base := string(b)
	for _, bad := range []string{
		strings.Replace(base, `"held":false`, `"held":null`, 1),
		strings.Replace(base, `"held":false`, `"held":"false"`, 1),
		strings.Replace(base, `"held":false`, `"Held":false`, 1),
		strings.Replace(base, `"held":false`, `"held":false,"Held":false`, 1),
		strings.Replace(base, `"held":false`, `"held":false,"\u0068eld":false`, 1),
		strings.Replace(base, `"ends":[`, `"ends":null,"ignored":[`, 1),
		strings.Replace(base, `"lane":"codex"`, `"lane":"codex\u2028"`, -1),
		strings.Replace(base, `"lane":"codex"`, `"lane":"codex\u2029"`, -1),
		strings.Replace(base, `"lane":"codex"`, `"lane":"codex\u0085"`, -1),
	} {
		if _, err := decodeGoalRecord([]byte(bad)); err == nil {
			t.Fatal("noncanonical end key or unsafe text accepted")
		}
	}
	if _, err := decodeGoalRecord([]byte(strings.Replace(base, `"held":false`, `"\u0068eld":false`, 1))); err != nil {
		t.Fatal("canonical escaped key refused")
	}
	meta, _ := json.Marshal(r)
	if !validGoalRunEndJSON(meta) {
		t.Fatal("valid saved end rejected")
	}
	if !validGoalRunEndJSON([]byte(strings.Replace(string(meta), `"native_end":`, `"\u006eative_end":`, 1))) {
		t.Fatal("escaped canonical run-end key refused")
	}
	for _, bad := range []string{
		strings.Replace(string(meta), `"native_end":`, `"Native_End":`, 1),
		strings.Replace(string(meta), `"native_end":`, `"nAtIvE_eNd":`, 1),
		strings.Replace(string(meta), `"native_end":`, `"\u004eative_End":`, 1),
		strings.Replace(string(meta), `"held":false`, `"held":null`, 1),
		strings.Replace(string(meta), `"held":false`, `"held":false,"Held":false`, 1),
	} {
		if validGoalRunEndJSON([]byte(bad)) {
			t.Fatal("saved end recognizer disagreed with decoder")
		}
	}
}

func TestGoalAutomaticEndUnknownOutcome(t *testing.T) {
	goalEndParallel(t)
	m, r := goalEndSavedFixture(t)
	r.StopReason = "unknown_exit"
	if err := m.save(&r); err != nil {
		t.Fatal(err)
	}
	if err := goalEndPublish(t, m, &r, goalCommitIO{}); err != nil {
		t.Fatal(err)
	}
	_, data := dispatchRecord(t, m, r.GoalID)
	if data.Outcome != nativeOutcomeUnknownExit || r.Exit != nil {
		t.Fatal("unknown exit inferred a numeric success")
	}
}

func TestGoalAutomaticEndTerminalDelivery(t *testing.T) {
	goalEndParallel(t)
	m, _, _ := manager(t)
	m.mu.Lock()
	m.config.Lanes[1].Kind = "opencode"
	m.config.Lanes[1].Model = "opencode/fixture-free"
	m.mu.Unlock()
	s, in := briefFixture(t)
	s.ID, s.PreferredLane, s.LaneBinding = "DISPATCH", "codex", "required"
	s.LaneKind, s.Kinds = "opencode", []nativeEngineKind{"opencode"}
	s.After, s.Writes, s.Resources = []string{}, []string{}, []string{"dispatch-resource"}
	in.Body = []byte("GOAL: DISPATCH (size S)\nterminal-hold\n")
	if _, err := m.createGoal(goalRecordRequest{OperationID: "create", Spec: s}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.sealGoalBrief(goalRecordRequest{OperationID: "seal", ExpectedRevision: 1, Spec: s}, in); err != nil {
		t.Fatal(err)
	}
	if _, err := m.decideGoal(goalDecisionRequest{OperationID: "approve", GoalID: s.ID, ExpectedRevision: 2, Action: "approve", Reason: "Reviewed scope", Authority: in.Authority, Evidence: "review", SourceState: in.SourceState}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.mirrorNativeAudit("cli", goalCommitIO{}); err != nil {
		t.Fatal(err)
	}
	req := goalDispatchRequest{GoalID: s.ID, ExpectedRevision: 3, Key: "dispatch-one", Lane: "codex"}
	ctx := goalDispatchContext{Source: "cli", Profile: goalDeliveryProfile{Engine: "opencode", CodePointCap: 4000, Root: deliveryRoot(t)}, Environment: goalAdmissionEnvironment{Capabilities: map[string]goalAdmissionCapability{"codex": {Kind: "opencode", WorkKinds: []nativeWorkKind{"review"}}}, SourceState: func(nativeGoalSpec) (string, error) { return in.SourceState, nil }}}
	result, err := m.dispatchGoal(req, ctx, goalCommitIO{})
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, m, req.Lane)
	r, _ := m.latest(req.Lane)
	if err = m.Recover(); err != nil {
		t.Fatal(err)
	}
	_, data := dispatchRecord(t, m, req.GoalID)
	if data.State != nativeGoalRunning || !sameProcess(*r) || len(data.Ends) != 0 {
		t.Fatal("terminal event released a live child")
	}
	if err = os.WriteFile(filepath.Join(m.config.ReportsDir, "terminal-release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	waitState(t, m, req.Lane, "ended")
	_, data = dispatchRecord(t, m, req.GoalID)
	if data.State != nativeGoalEnded || data.Outcome != nativeOutcomeReportDeliveryFailed || data.Attempt.ID != result.RunID {
		t.Fatal("terminal delivery failure lost native end")
	}
	if err = m.Recover(); err != nil {
		t.Fatal(err)
	}
	runs, err := m.history()
	if err != nil || len(runs) != 1 || runs[0].ContinueRunID != "" {
		t.Fatal("terminal end launched recovery")
	}
}

func TestGoalAutomaticEndEarlyPublicationRetry(t *testing.T) {
	goalEndParallel(t)
	m, req, ctx := dispatchFixtureBody(t, "ack-goal\n", "claude")
	result, err := m.dispatchGoal(req, ctx, dispatchOneFault("after_os"))
	if err != nil && !errors.Is(err, errGoalLaunchHeld) {
		t.Fatal(err)
	}
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(4 * time.Second); ; {
		r, _ := loadRun(filepath.Join(m.store, "runs", result.RunID), result.RunID)
		if r != nil && goalAttemptDefinitelyGone(*r) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("instant offline child did not end")
		}
		time.Sleep(time.Millisecond)
	}
	n := dispatchReopen(t, m)
	again, err := n.dispatchGoal(req, ctx, goalCommitIO{})
	if err != nil || again.RunID != result.RunID || again.Phase != "ended" {
		t.Fatal("early end retry overwrote the completed attempt", err)
	}
	_, data := dispatchRecord(t, n, req.GoalID)
	if len(data.Ends) != 1 || data.State != nativeGoalEnded {
		t.Fatal("early end lost durable identity")
	}
	dispatchStarts(t, m, 1)
}

func TestGoalAutomaticEndConcurrentRepair(t *testing.T) {
	goalEndParallel(t)
	m, r := goalEndSavedFixture(t)
	a, err := newReadManager(m.config, m.store)
	if err != nil {
		t.Fatal(err)
	}
	a.readOnly = false
	b, err := newReadManager(m.config, m.store)
	if err != nil {
		t.Fatal(err)
	}
	b.readOnly = false
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, writer := range []*Manager{a, b} {
		wg.Go(func() { <-start; copy := r; results <- goalEndPublish(t, writer, &copy, goalCommitIO{}) })
	}
	close(start)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	p, err := replayGoalJournal(m.store)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := goalRecordMetadata(p.records[r.GoalID])
	if len(data.Ends) != 1 || data.State != nativeGoalEnded {
		t.Fatal("cooperating writers duplicated or lost end")
	}
}

func TestGoalAutomaticEndFramePrefixes(t *testing.T) {
	goalEndParallel(t)
	m, r := goalEndSavedFixture(t)
	if err := goalEndPublish(t, m, &r, goalCommitIO{}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(m.store, goalJournalName)
	full, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	start := 0
	for offset := 0; offset < len(full); {
		start = offset
		offset += goalJournalHeaderBytes + int(binary.BigEndian.Uint32(full[offset+16:offset+20]))
	}
	last := full[start:]
	before, _ := replayGoalJournal(m.store)
	independent := goalReplayFrame(before.sequence, last[goalJournalHeaderBytes:])
	if !reflect.DeepEqual(last, independent) {
		t.Fatal("end frame disagrees with independent checksums")
	}
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	treeBefore := goalCommitTree(t, m.store)
	var readers sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		readers.Go(func() {
			for cut := worker; cut < len(last); cut += 4 {
				candidate := append(append([]byte(nil), full[:start]...), independent[:cut]...)
				p, err := replayGoalJournalData(root, candidate)
				if err != nil || p.sequence != before.sequence-1 {
					t.Error("proper final end prefix exposed a partial end", cut, err)
					continue
				}
				data, _ := goalRecordMetadata(p.records[r.GoalID])
				if data.State == nativeGoalEnded || len(data.Ends) != 0 {
					t.Error("partial end released execution", cut)
				}
			}
		})
	}
	readers.Wait()
	if !reflect.DeepEqual(treeBefore, goalCommitTree(t, m.store)) {
		t.Fatal("prefix replay changed saved history")
	}
	corrupt := append([]byte(nil), full...)
	corrupt[goalJournalHeaderBytes] ^= 1
	if err = os.WriteFile(path, corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	if p, err := replayGoalJournal(m.store); p != nil || !errors.Is(err, errGoalReplay) {
		t.Fatal("interior corruption accepted")
	}
	if err = os.WriteFile(path, full[:start+len(last)/2], 0600); err != nil {
		t.Fatal(err)
	}
	n := dispatchReopen(t, m)
	p, err := replayGoalJournal(n.store)
	if err != nil || p.sequence != before.sequence {
		t.Fatal("writer repair lost original end", err)
	}
}

func TestGoalAutomaticEndLegacySignalFailure(t *testing.T) {
	goalEndParallel(t)
	m, r := goalEndSavedFixture(t)
	path := filepath.Join(m.store, "signals")
	if _, err := os.Stat(path); err == nil {
		if err = os.Rename(path, path+".saved"); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	guard, err := m.controlLock()
	if err != nil {
		m.mu.Unlock()
		t.Fatal(err)
	}
	err = m.endRun(&r)
	guard.Close()
	m.mu.Unlock()
	if err == nil {
		t.Fatal("legacy signal failure was not reported")
	}
	_, data := dispatchRecord(t, m, r.GoalID)
	if data.State != nativeGoalEnded || len(data.Ends) != 1 {
		t.Fatal("signal failure suppressed native end")
	}
}

func TestGoalAutomaticEndReviewAndStaleIdentity(t *testing.T) {
	goalEndParallel(t)
	m, r := goalEndSavedFixture(t)
	if err := goalEndPublish(t, m, &r, goalCommitIO{}); err != nil {
		t.Fatal(err)
	}
	m = dispatchReopen(t, m)
	old, data := dispatchRecord(t, m, r.GoalID)
	decision := goalDecisionRequest{OperationID: "review", GoalID: r.GoalID, ExpectedRevision: old.revision, Action: "verify", Reason: "Reviewed completed work", Authority: "approved-plan", Evidence: "review", AttemptID: r.ID, ReportDigest: strings.Repeat("a", 64)}
	if _, err := m.decideGoal(decision); !errors.Is(err, errGoalReview) {
		t.Fatal("absent report verified goal", err)
	}
	decision.ReportDigest = ""
	decision.AlternateEvidence = true
	if _, err := m.decideGoal(decision); err != nil {
		t.Fatal("explicit alternate review refused", err)
	}
	p, _ := replayGoalJournal(m.store)
	verified, _ := goalRecordMetadata(p.records[r.GoalID])
	if verified.State != nativeGoalVerified {
		t.Fatal("judge review did not close goal")
	}
	// B11 has no linked-successor schema. Prove the shared stale-current guard
	// without enabling B13's launcher or fabricating a durable successor.
	data.Ends = nil
	data.State = nativeGoalRunning
	data.Attempt = &goalDecisionAttempt{ID: strings.Repeat("f", 32)}
	if _, err := applyGoalAttemptEnd(data, *r.NativeEnd); err == nil {
		t.Fatal("old end overwrote a newer current attempt")
	}
}

func TestGoalAutomaticEndReplayGuard(t *testing.T) {
	goalEndParallel(t)
	m, r := goalEndSavedFixture(t)
	old, data := dispatchRecord(t, m, r.GoalID)
	end := goalAttemptEnd{RunID: r.ID, Lane: r.Lane, PID: r.PID, ProcessStart: r.ProcessStart, Job: r.LaunchdLabel, Started: r.Started, Ended: *r.Ended, Outcome: nativeOutcomeEndedNoReport}
	next, err := applyGoalAttemptEnd(data, end)
	if err != nil {
		t.Fatal(err)
	}
	next.ApprovalRevision++
	record, err := json.Marshal(next)
	if err != nil {
		t.Fatal(err)
	}
	briefHistoryReplayCandidate(t, m, r.GoalID, old, []byte(old.spec), record)
}

func TestGoalAutomaticEndAuditPresentation(t *testing.T) {
	goalEndParallel(t)
	m, r := goalEndSavedFixture(t)
	if err := goalEndPublish(t, m, &r, goalCommitIO{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.config.ReportsDir, r.GoalID+".txt"), []byte("late fixture report"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.publishGoalAttemptReportLocked(r, goalCommitIO{}); err != nil {
		t.Fatal(err)
	}
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	_, rows, _, _, err := readNativeAuditRecords(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows[len(rows)-2:] {
		row.Source = "native"
		row.Time = time.Now().UTC()
		b, _ := json.Marshal(row)
		entries := parseAudit(b, time.Time{})
		if len(entries) != 1 || entries[0].Result != "ok" || entries[0].Reason == row.Result || entries[0].Reason == "" || entries[0].Summary == "Saved native goal "+r.GoalID {
			t.Fatal("successful native observation displayed as a refusal or bare code", row.Result)
		}
	}
}

// Only the explicitly named leftover fixture under this invocation's TMPDIR
// is eligible. This helper verifies teardown after a package-wide alarm, whose
// terminated process cannot run its registered test cleanup. It never inspects
// the owner's store, accepts a guessed identity, or starts replacement work.
func TestGoalAutomaticEndTimedRunTeardown(t *testing.T) {
	root := os.Getenv("NUNTIUS_B12_TEARDOWN_ROOT")
	if root == "" {
		return
	}
	rel, err := filepath.Rel(os.TempDir(), root)
	parts := strings.Split(rel, string(os.PathSeparator))
	namedFixture := len(parts) == 3 && (strings.HasPrefix(parts[0], "TestReportOverviewAttemptAndPrivacy") || strings.HasPrefix(parts[0], "TestTerminalLiveSupervisionAndReportstale"))
	if err != nil || !namedFixture || parts[1] != "001" || parts[2] != "store" {
		t.Fatal("teardown fixture is outside the named temporary test scope")
	}
	entries, err := os.ReadDir(filepath.Join(root, "runs"))
	if err != nil {
		t.Fatal("teardown run inventory unavailable")
	}
	observed := false
	for _, entry := range entries {
		if !runPattern.MatchString(entry.Name()) || !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		r, err := loadRun(filepath.Join(root, "runs", entry.Name()), entry.Name())
		if err != nil || r == nil || r.Integrity == "" || r.PID <= 1 || r.ProcessStart == "" {
			t.Fatal("timed run has no trustworthy full identity")
		}
		if sameProcess(*r) {
			if strings.HasPrefix(parts[0], "TestTerminalLiveSupervisionAndReportstale") {
				if err = os.WriteFile(filepath.Join(filepath.Dir(root), "terminal-release"), nil, 0600); err != nil {
					t.Fatal("test-owned terminal release failed")
				}
			} else if err = signalRun(*r, syscall.SIGKILL); err != nil {
				t.Fatal("test-owned child teardown failed", err)
			}
		}
		for deadline := time.Now().Add(4 * time.Second); !goalAttemptDefinitelyGone(*r); {
			if time.Now().After(deadline) {
				t.Fatal("timed run child absence remains uncertain")
			}
			time.Sleep(time.Millisecond)
		}
		observed = true
	}
	if !observed {
		t.Fatal("timed run supplied no child teardown evidence")
	}
	t.Log("original timed-out fixture child identity absence verified inside the test; records preserved")
}

func TestGoalAutomaticEndFailedRepairSafety(t *testing.T) {
	goalEndParallel(t)
	m, req, ctx := dispatchFixture(t)
	result, err := m.dispatchGoal(req, ctx, goalCommitIO{})
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, m, req.Lane)
	path := filepath.Join(m.store, goalJournalName)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.WriteFile(path, original, 0600); err != nil {
			t.Error("restore private fixture history failed")
		}
	})
	corrupt := append([]byte(nil), original...)
	corrupt[goalJournalHeaderBytes] ^= 1
	if err = os.WriteFile(path, corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	n, err := NewManager(m.config, m.store)
	if err != nil {
		t.Fatal("native repair failure disabled independent report and safety controls", err)
	}
	t.Cleanup(func() { _ = n.Close() })
	if err = os.WriteFile(filepath.Join(m.config.ReportsDir, req.GoalID+".txt"), []byte("safe fixture report"), 0600); err != nil {
		t.Fatal(err)
	}
	if text, err := n.Report(req.GoalID); err != nil || text != "safe fixture report" {
		t.Fatal("native failure blocked report access", err)
	}
	if _, err = n.admitGoals(ctx.Environment); err == nil {
		t.Fatal("native admission ignored corrupt authority")
	}
	if err = n.Cancel(req.Lane); err != nil {
		t.Fatal("native failure blocked identity-checked cancellation", err)
	}
	state := waitState(t, n, req.Lane, "ended")
	if state.RunID != result.RunID || state.Outcome != "cancelled" {
		t.Fatal("safety cancellation lost actual native run")
	}
	if err = os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err = n.Recover(); err != nil {
		t.Fatal(err)
	}
	_, data := dispatchRecord(t, n, req.GoalID)
	if data.State != nativeGoalEnded || data.Outcome != nativeOutcomeCancelled || len(data.Ends) != 1 {
		t.Fatal("repair lost end saved while journal was broken")
	}
}
