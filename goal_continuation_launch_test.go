package nuntius

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

func linkedResolver(ctx goalDispatchContext) goalContinuationResolver {
	return func(q goalContinuationQuery) (goalContinuationContext, error) {
		fresh := ctx
		fresh.Environment.Capabilities = map[string]goalAdmissionCapability{}
		for lane, capability := range ctx.Environment.Capabilities {
			fresh.Environment.Capabilities[lane] = capability
		}
		return goalContinuationContext{Dispatch: fresh, ObservedAt: time.Now().UTC()}, nil
	}
}

// Serialize this new lifecycle book under the existing four-slot ceiling.
// Native journal race instrumentation is expensive; lifecycle event deadlines
// must not be made into a contention benchmark or extended to hide failures.
var linkedBookSlot = make(chan struct{}, 1)

func linkedParallel(t *testing.T) {
	t.Helper()
	goalEndParallel(t)
	linkedBookSlot <- struct{}{}
	t.Cleanup(func() { <-linkedBookSlot })
}

func linkedControl(t *testing.T, m *Manager, paused bool) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	guard, err := m.controlLock()
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	result, err := m.setLaneControlLocked(root, laneControlChange{Lane: "codex", Paused: paused}, "cli", nil)
	if err != nil || !result.Available || result.Paused != paused {
		t.Fatal("control barrier failed")
	}
}

func linkedPending(t *testing.T, base *Manager, parent Run, ctx goalDispatchContext) (*Manager, Run) {
	t.Helper()
	m, r := goalEndClone(t, base, parent)
	m.config.AutoResume = nil
	m.config.Lanes = append([]Lane(nil), m.config.Lanes...)
	workdir := t.TempDir()
	for i := range m.config.Lanes {
		m.config.Lanes[i].Workdir = workdir
	}
	m.continuationResolver = linkedResolver(ctx)
	linkedCleanup(t, m)
	if err := goalEndPublish(t, m, &r, goalCommitIO{}); err != nil {
		t.Fatal(err)
	}
	return m, r
}

func linkedReservations(t *testing.T, m *Manager, ctx goalDispatchContext, want int) {
	t.Helper()
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	p, err := replayGoalJournalRoot(root)
	if err != nil || !goalDispatchPopulationValid(p) {
		t.Fatal("invalid reservation population")
	}
	runs, err := m.history()
	if err != nil {
		t.Fatal(err)
	}
	rows, unknown := goalAdmissionReservations(p, runs, root, ctx.Environment)
	if unknown || len(rows) != want {
		t.Fatal("chain did not retain exactly one canonical reservation")
	}
}

func TestNativeContinuationFreshGuard(t *testing.T) {
	linkedParallel(t)
	var ctx goalDispatchContext
	base, parent := goalEndSavedFixture(t, &ctx)
	for _, failure := range []string{"missing", "cached", "future", "source", "source-unavailable", "capability", "profile", "source-name", "second-source"} {
		t.Run(failure, func(t *testing.T) {
			m, r := linkedPending(t, base, parent, ctx)
			calls := 0
			resolver := linkedResolver(ctx)
			m.continuationResolver = func(q goalContinuationQuery) (goalContinuationContext, error) {
				calls++
				fresh, _ := resolver(q)
				switch failure {
				case "cached":
					fresh.ObservedAt = q.ObservedAfter.Add(-time.Nanosecond)
				case "future":
					fresh.ObservedAt = q.ObservedAfter.Add(time.Hour)
				case "source":
					fresh.Dispatch.Environment.SourceState = func(nativeGoalSpec) (string, error) { return "changed", nil }
				case "source-unavailable":
					fresh.Dispatch.Environment.SourceState = nil
				case "capability":
					fresh.Dispatch.Environment.Capabilities = map[string]goalAdmissionCapability{}
				case "profile":
					fresh.Dispatch.Profile.CodePointCap++
				case "source-name":
					fresh.Dispatch.Source = "web"
				case "second-source":
					if calls == 2 {
						fresh.Dispatch.Environment.SourceState = nil
					}
				}
				return fresh, nil
			}
			if failure == "missing" {
				m.continuationResolver = nil
			}
			if err := m.Recover(); err != nil {
				t.Fatal(err)
			}
			_, data := dispatchRecord(t, m, r.GoalID)
			if data.Attempt == nil || !data.Attempt.ActiveSuccessor || data.Attempt.ID != r.ID {
				t.Fatal("fresh refusal released ownership")
			}
			if failure == "second-source" {
				if calls != 2 || len(data.Dispatches) != 2 || data.Dispatches[1].Phase != "reserved" {
					t.Fatal("final guard did not hold allocated intent")
				}
			} else if len(data.Dispatches) != 1 {
				t.Fatal("selection refusal allocated a child")
			}
			p, _ := loadRun(filepath.Join(m.store, "runs", r.ID), r.ID)
			if p.ResumeCount != 0 {
				t.Fatal("refusal spent recovery budget")
			}
			linkedStarts(t, m, 0)
			linkedReservations(t, m, ctx, 1)
		})
	}
}

func TestNativeContinuationPhysicalScopeAndClaims(t *testing.T) {
	linkedParallel(t)
	var ctx goalDispatchContext
	base, parent := canonicalEndedFixture(t, &ctx)
	for _, caseName := range []string{"valid", "missing-claim", "stale-claim", "held-claim", "changed-root"} {
		t.Run(caseName, func(t *testing.T) {
			m, r := linkedPending(t, base, parent, ctx)
			fresh := ctx
			switch caseName {
			case "missing-claim":
				fresh.Environment.Claims = nil
			case "stale-claim":
				fresh.Environment.Claims = func(roots []string) ([]goalAdmissionClaim, error) {
					return []goalAdmissionClaim{{Root: roots[0], ObservedAt: time.Now().Add(-time.Minute)}}, nil
				}
			case "held-claim":
				fresh.Environment.Claims = func(roots []string) ([]goalAdmissionClaim, error) {
					return []goalAdmissionClaim{{Root: roots[0], ObservedAt: time.Now(), Holder: "another-goal"}}, nil
				}
			case "changed-root":
				fresh.Environment.Roots = map[string]string{"sample": t.TempDir()}
			}
			m.continuationResolver = linkedResolver(fresh)
			if err := m.Recover(); err != nil {
				t.Fatal(err)
			}
			_, data := dispatchRecord(t, m, r.GoalID)
			if caseName == "valid" {
				waitReady(t, m, "codex")
				if len(data.Dispatches) != 2 || !reflect.DeepEqual(data.Dispatches[0].Scope, data.Dispatches[1].Scope) || !reflect.DeepEqual(data.Dispatches[0].Writes, data.Dispatches[1].Writes) {
					t.Fatal("canonical binding changed across child")
				}
				linkedStarts(t, m, 1)
			} else {
				if len(data.Dispatches) != 1 {
					t.Fatal("unavailable claim/root admitted a child")
				}
				linkedStarts(t, m, 0)
			}
			linkedReservations(t, m, ctx, 1)
		})
	}
}

func TestNativeContinuationPausedRecoveryAndLimit(t *testing.T) {
	linkedParallel(t)
	var ctx goalDispatchContext
	base, parent := goalEndSavedFixture(t, &ctx)
	m, r := linkedPending(t, base, parent, ctx)
	linkedControl(t, m, true)
	for i := 0; i < 2; i++ {
		if err := m.Recover(); err != nil {
			t.Fatal(err)
		}
	}
	_, data := dispatchRecord(t, m, r.GoalID)
	if len(data.Dispatches) != 1 || data.Ends[0].Pending != "recovery_pending" || !data.Attempt.ActiveSuccessor {
		t.Fatal("pause lost pending recovery")
	}
	linkedReservations(t, m, ctx, 1)
	linkedStarts(t, m, 0)
	linkedControl(t, m, false)
	if _, err := m.Status(); err != nil {
		t.Fatal(err)
	}
	linkedStarts(t, m, 0)
	for count := 1; count <= 2; count++ {
		if err := m.Recover(); err != nil {
			t.Fatal(err)
		}
		waitReady(t, m, "codex")
		_, data = dispatchRecord(t, m, r.GoalID)
		child := successorRun(t, m, data.Attempt.ID)
		if !child.RecoveryRun || !child.Resumed || child.ResumeCount != count || child.Brief != r.Brief {
			t.Fatal("recovery ordinal/original brief changed")
		}
		linkedReservations(t, m, ctx, 1)
		done := linkedDone(t, m, "codex")
		if err := signalRun(*child, syscall.SIGINT); err != nil {
			t.Fatal(err)
		}
		linkedWait(t, done)
	}
	if err := m.Recover(); err != nil {
		t.Fatal(err)
	}
	_, data = dispatchRecord(t, m, r.GoalID)
	if len(data.Dispatches) != 3 || data.Attempt.ActiveSuccessor || !data.Attempt.Ended {
		t.Fatal("automatic recovery exceeded two or failed settlement")
	}
	linkedStarts(t, m, 2)
	linkedReservations(t, m, ctx, 0)
}

func linkedHeld(t *testing.T) (*Manager, Run, heldMessage, goalDispatchContext) {
	t.Helper()
	m, req, ctx := dispatchFixture(t)
	m.continuationResolver = linkedResolver(ctx)
	linkedCleanup(t, m)
	result, err := m.dispatchGoal(req, ctx, goalCommitIO{})
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, m, "codex")
	r := successorRun(t, m, result.RunID)
	done := linkedDone(t, m, "codex")
	if err = m.SendMessage("codex", "wait", false); err != nil {
		t.Fatal(err)
	}
	linkedControl(t, m, true)
	b, err := os.ReadFile(filepath.Join(m.store, "runs", r.ID, "message-hold.json"))
	var h heldMessage
	if err != nil || json.Unmarshal(b, &h) != nil {
		t.Fatal("held content not bound")
	}
	if err = signalRun(*r, syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	linkedWait(t, done)
	return m, *successorRun(t, m, r.ID), h, ctx
}

func TestNativeContinuationHeldSendAndDiscard(t *testing.T) {
	linkedParallel(t)
	for _, action := range []string{"send", "discard"} {
		t.Run(action, func(t *testing.T) {
			m, r, h, ctx := linkedHeld(t)
			_, data := dispatchRecord(t, m, r.GoalID)
			if data.State != nativeGoalEnded || data.Ends[0].Pending != "message_pending" || data.Ends[0].Message == nil {
				t.Fatal("held actual end missing immutable content binding")
			}
			linkedControl(t, m, false)
			if err := m.Recover(); err != nil {
				t.Fatal(err)
			}
			linkedStarts(t, m, 1)
			wrong := h
			wrong.Revision++
			if _, err := m.sendHeld(wrong, nil); err == nil {
				t.Fatal("stale held review sent")
			}
			if action == "send" {
				id, err := m.sendHeld(h, nil)
				if err != nil {
					t.Fatal(err)
				}
				waitReady(t, m, "codex")
				if again, err := m.sendHeld(h, nil); err != nil || again != id {
					t.Fatal("duplicate Send did not resolve same child")
				}
				if err := m.discardHeld(h, nil); err != nil {
					t.Fatal(err)
				}
				linkedReservations(t, m, ctx, 1)
				linkedStarts(t, m, 2)
			} else {
				if err := m.discardHeld(h, nil); err != nil {
					t.Fatal(err)
				}
				if err := m.discardHeld(h, nil); err != nil {
					t.Fatal(err)
				}
				_, data = dispatchRecord(t, m, r.GoalID)
				if len(data.Settlements) != 1 || data.Attempt.ActiveSuccessor || !data.Ends[0].Held {
					t.Fatal("discard rewrote end or failed explicit settlement")
				}
				linkedReservations(t, m, ctx, 0)
				linkedStarts(t, m, 1)
				id, err := m.Reply("codex", "wait")
				if err != nil || id == "" {
					t.Fatal("settled explicit native reply refused", err)
				}
				waitReady(t, m, "codex")
				linkedReservations(t, m, ctx, 1)
			}
		})
	}
}

func linkedLaunchFault(t *testing.T, m *Manager, r Run, ctx goalDispatchContext, fault goalCommitIO) (string, error) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	guard, err := m.controlLock()
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	lock, err := m.lock(r.Lane)
	if err != nil {
		t.Fatal(err)
	}
	id, err := m.launchGoalContinuationLocked(m.config.Lanes[1], nativeRecoveryText(r), r.SessionID, lock, &r, nil, nil, nil, fault, "recovery")
	if m.active[r.Lane] == nil {
		lock.Close()
	}
	return id, err
}

func TestNativeContinuationFaultsAndAdoption(t *testing.T) {
	t.Parallel()
	goalEndTestSlots <- struct{}{}
	setupHeld := true
	t.Cleanup(func() {
		if setupHeld {
			<-goalEndTestSlots
		}
	})
	var ctx goalDispatchContext
	base, parent := goalEndSavedFixture(t, &ctx)
	// The base child is confirmed gone. Transfer its slot to isolated cases;
	// at most four case fixtures can run, unchanged from the shared limit.
	<-goalEndTestSlots
	setupHeld = false
	for _, stage := range []string{"prepare_before", "prepare_directory", "prepare_prompt", "prepare_meta", "prepare_rename", "prepare_sync", "before_start_intent", "before_os", "after_os", "child_identity", "running_publication", "before_success"} {
		t.Run(stage, func(t *testing.T) {
			linkedParallel(t)
			m, r := linkedPending(t, base, parent, ctx)
			_, err := linkedLaunchFault(t, m, r, ctx, dispatchOneFault(stage))
			if err == nil {
				t.Fatal("injected boundary did not fail")
			}
			_, data := dispatchRecord(t, m, r.GoalID)
			if len(data.Dispatches) != 2 {
				t.Fatal("intent not retained")
			}
			id := data.Dispatches[1].RunID
			// Replace the manager while keeping its test-owned agent and store.
			if err := m.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, e := newReadManager(m.config, m.store)
			if e != nil {
				t.Fatal(e)
			}
			reopened.readOnly = false
			reopened.continuationResolver = linkedResolver(ctx)
			t.Cleanup(func() { _ = reopened.Close() })
			linkedCleanup(t, reopened)
			if _, e = reopened.Status(); e != nil {
				t.Fatal(e)
			}
			for n := 0; n < 2; n++ {
				if e = reopened.Recover(); e != nil {
					t.Fatal(e)
				}
			}
			_, saved := dispatchRecord(t, reopened, r.GoalID)
			if len(saved.Dispatches) != 2 || saved.Dispatches[1].RunID != id {
				t.Fatal("retry allocated another child")
			}
			started := stage == "after_os" || stage == "child_identity" || stage == "running_publication" || stage == "before_success"
			if started {
				waitReady(t, reopened, "codex")
				if saved.State != nativeGoalRunning || saved.Attempt.ID != id {
					t.Fatal("exact surviving child not current")
				}
				linkedStarts(t, reopened, 1)
			} else {
				linkedStarts(t, reopened, 0)
			}
			linkedReservations(t, reopened, ctx, 1)
		})
	}
}

func TestNativeContinuationRetainedHistoryAndCodec(t *testing.T) {
	linkedParallel(t)
	var ctx goalDispatchContext
	base, parent := goalEndSavedFixture(t, &ctx)
	m, r := linkedPending(t, base, parent, ctx)
	if err := m.Recover(); err != nil {
		t.Fatal(err)
	}
	waitReady(t, m, "codex")
	_, data := dispatchRecord(t, m, r.GoalID)
	child := successorRun(t, m, data.Attempt.ID)
	done := linkedDone(t, m, "codex")
	if err := m.Cancel("codex"); err != nil {
		t.Fatal(err)
	}
	linkedWait(t, done)
	for _, mutation := range []string{"parent", "input", "session", "branch", "ordinal", "approval", "grant", "scope", "source", "end", "settle", "notes"} {
		t.Run(mutation, func(t *testing.T) {
			clone, _ := goalEndClone(t, m, *child)
			old, data := dispatchRecord(t, clone, r.GoalID)
			c := data.Dispatches[1].Continuation
			switch mutation {
			case "parent":
				c.Parent = strings.Repeat("a", 32)
			case "input":
				c.InputSHA256 = strings.Repeat("a", 64)
			case "session":
				c.SessionSHA256 = strings.Repeat("b", 64)
			case "branch":
				c.Branch = "other"
			case "ordinal":
				c.RecoveryCount = 2
			case "approval":
				data.ApprovalRevision++
			case "grant":
				data.GrantBinding = "invented"
			case "scope":
				data.Dispatches[1].Writes = []string{"/different"}
			case "source":
				data.Dispatches[1].Source = "web"
			case "end":
				data.Ends[0].Held = false
			case "settle":
				data.Attempt.ActiveSuccessor = true
			case "notes":
				c.InputSHA256 = strings.Repeat("a", 64)
				data.Notes = append(data.Notes, goalRecordNote{Text: "unrelated"})
			}
			bytes, _ := json.Marshal(data)
			seal, e := os.ReadFile(filepath.Join(clone.store, old.sealRef))
			if e != nil {
				t.Fatal(e)
			}
			before := goalCommitTree(t, clone.store)
			if _, _, e = clone.commitGoal(goalCommitRequest{OperationID: "counterfeit", ExpectedRevision: old.revision, Spec: []byte(old.spec), Seal: seal, Record: bytes, MutationSHA256: strings.Repeat("a", 64)}, goalCommitIO{}); e == nil {
				t.Fatal("candidate changed linked authority")
			}
			if !reflect.DeepEqual(before, goalCommitTree(t, clone.store)) {
				t.Fatal("refusal wrote state")
			}
			briefHistoryReplayCandidate(t, clone, r.GoalID, old, []byte(old.spec), bytes)
		})
	}
	_, data = dispatchRecord(t, m, r.GoalID)
	bytes, _ := json.Marshal(data)
	for _, change := range []string{`"Continuation":`, `"continuation":null,"continuation":`, `"continuation":false,"unused":`} {
		bad := strings.Replace(string(bytes), `"continuation":`, change, 1)
		if _, e := decodeGoalRecord([]byte(bad)); e == nil {
			t.Fatal("continuation alias/duplicate/null accepted")
		}
	}
	escaped := strings.Replace(string(bytes), `"continuation":`, `"cont\u0069nuation":`, 1)
	if _, e := decodeGoalRecord([]byte(escaped)); e != nil {
		t.Fatal("escaped canonical continuation key refused")
	}
}

func linkedDone(t *testing.T, m *Manager, lane string) <-chan struct{} {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active[lane] == nil {
		t.Fatal("identified fixture supervisor absent")
	}
	return m.active[lane].done
}

func linkedWait(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("fixture supervision event unavailable")
	}
}

func linkedCleanup(t *testing.T, m *Manager) {
	t.Helper()
	t.Cleanup(func() {
		m.mu.Lock()
		var done <-chan struct{}
		lane := ""
		for name, active := range m.active {
			if m.closed {
				break
			} // replacement manager owns cancellation after detach
			lane, done = name, active.done
			break
		}
		m.mu.Unlock()
		if done != nil {
			if err := m.Cancel(lane); err != nil {
				t.Error("fixture cancellation unavailable", err)
			}
			linkedWait(t, done)
		}
		runs, err := m.history()
		if err != nil {
			t.Error("fixture history unavailable", err)
		}
		for _, r := range runs {
			if r.PID > 1 && !goalAttemptDefinitelyGone(r) {
				t.Error("fixture process identity not absent before records disappear")
			}
		}
	})
}

func linkedStarts(t *testing.T, m *Manager, want int) {
	t.Helper()
	entries, err := os.ReadDir(m.config.Lanes[0].Workdir)
	if err != nil {
		t.Fatal(err)
	}
	got := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "agent-start-") {
			got++
		}
	}
	if got != want {
		t.Fatalf("all-branch agent starts=%d want=%d", got, want)
	}
}

func TestNativeLinkedContinuation(t *testing.T) {
	linkedParallel(t)
	for _, branch := range []string{"message", "acknowledgement", "recovery"} {
		t.Run(branch, func(t *testing.T) {
			var m *Manager
			var parent Run
			if branch == "recovery" {
				var ctx goalDispatchContext
				base, r := goalEndSavedFixture(t, &ctx)
				m, parent = goalEndClone(t, base, r)
				m.config.AutoResume = nil
				m.continuationResolver = linkedResolver(ctx)
				linkedCleanup(t, m)
				if err := m.Recover(); err != nil {
					t.Fatal(err)
				}
			} else {
				body, lane := "wait\n", "codex"
				if branch == "acknowledgement" {
					body, lane = "ack-goal ack-barrier\n", "claude"
				}
				var req goalDispatchRequest
				var ctx goalDispatchContext
				m, req, ctx = dispatchFixtureBody(t, body, lane)
				m.continuationResolver = linkedResolver(ctx)
				if branch == "acknowledgement" {
					m.config.Lanes[0].ContinueText = "wait"
				}
				result, err := m.dispatchGoal(req, ctx, goalCommitIO{})
				if err != nil {
					t.Fatal(err)
				}
				waitReady(t, m, lane)
				r, _ := loadRun(filepath.Join(m.store, "runs", result.RunID), result.RunID)
				parent = *r
				done := linkedDone(t, m, lane)
				linkedCleanup(t, m)
				if branch == "message" {
					if err := m.SendMessage(lane, "wait", false); err != nil {
						t.Fatal(err)
					}
					if err := signalRun(parent, syscall.SIGINT); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(filepath.Join(m.config.Lanes[0].Workdir, "admission-release"), nil, 0600); err != nil {
					t.Fatal(err)
				}
				linkedWait(t, done)
			}
			runs, err := m.history()
			if err != nil || len(runs) != 2 {
				t.Fatal("native continuation did not link a successor before starting")
			}
			waitReady(t, m, parent.Lane)
			_, data := dispatchRecord(t, m, parent.GoalID)
			if len(data.Dispatches) != 2 || data.Attempt == nil || data.Attempt.ID == parent.ID || data.State != nativeGoalRunning {
				t.Fatal("successor not the current native attempt")
			}
			child, _ := loadRun(filepath.Join(m.store, "runs", data.Attempt.ID), data.Attempt.ID)
			p, _ := loadRun(filepath.Join(m.store, "runs", parent.ID), parent.ID)
			if child == nil || p == nil || child.ParentRunID != parent.ID || p.ContinueRunID != child.ID || child.Brief != parent.Brief || child.RecoveryRun != (branch == "recovery") {
				t.Fatal("full parent/child/original-brief binding absent")
			}
			linkedStarts(t, m, 2)
		})
	}
}

func TestNativeContinuationReconcileErrorPropagation(t *testing.T) {
	linkedParallel(t)
	var ctx goalDispatchContext
	base, parent := goalEndSavedFixture(t, &ctx)
	m, r := linkedPending(t, base, parent, ctx)
	id, err := linkedLaunchFault(t, m, r, ctx, dispatchOneFault("after_os"))
	if id == "" || !errors.Is(err, errGoalLaunchHeld) {
		t.Fatal("fixture did not retain the identified starting child")
	}
	_, before := dispatchRecord(t, m, r.GoalID)
	if len(before.Dispatches) != 2 || before.Dispatches[1].Phase != "starting" || m.goalDispatchChild(before.Dispatches[1]) == nil {
		t.Fatal("fixture child is not an exact live starting continuation")
	}
	reconcile := func(fault goalCommitIO) error {
		m.mu.Lock()
		defer m.mu.Unlock()
		guard, err := m.controlLock()
		if err != nil {
			return err
		}
		defer guard.Close()
		return m.reconcileGoalAttemptsLocked(fault)
	}
	if err := reconcile(dispatchOneFault("running_publication")); !errors.Is(err, errGoalLaunchHeld) {
		t.Fatal("starting-child reconciliation result was lost instead of propagated")
	}
	_, held := dispatchRecord(t, m, r.GoalID)
	if !reflect.DeepEqual(before, held) {
		t.Fatal("failed running publication changed retained native accounting")
	}
	linkedReservations(t, m, ctx, 1)
	if err := reconcile(goalCommitIO{}); err != nil {
		t.Fatal(err)
	}
	_, repaired := dispatchRecord(t, m, r.GoalID)
	if len(repaired.Dispatches) != 2 || repaired.Dispatches[1].RunID != id || repaired.Dispatches[1].Phase != "running" || repaired.Attempt == nil || repaired.Attempt.ID != id {
		t.Fatal("repair did not publish the same identified child")
	}
	linkedReservations(t, m, ctx, 1)
	linkedStarts(t, m, 1)
}
