package nuntius

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func dispatchFixture(t *testing.T) (*Manager, goalDispatchRequest, goalDispatchContext) {
	return dispatchFixtureBody(t, "native-dispatch wait\n", "codex")
}

func dispatchFixtureBody(t *testing.T, body, lane string) (*Manager, goalDispatchRequest, goalDispatchContext) {
	t.Helper()
	m, c, _ := manager(t)
	s, in := briefFixture(t)
	s.ID, s.PreferredLane, s.LaneBinding = "DISPATCH", lane, "required"
	s.After, s.Writes, s.Resources = []string{}, []string{}, []string{"dispatch-resource"}
	in.Body = []byte("GOAL: DISPATCH (size S)\n" + body)
	goalFixtureSave(t, "dispatch create", func() error {
		_, err := m.createGoal(goalRecordRequest{OperationID: "dispatch-create", Spec: s})
		return err
	})
	goalFixtureSave(t, "dispatch seal", func() error {
		_, err := m.sealGoalBrief(goalRecordRequest{OperationID: "dispatch-seal", ExpectedRevision: 1, Spec: s}, in)
		return err
	})
	goalFixtureSave(t, "dispatch approve", func() error {
		_, err := m.decideGoal(goalDecisionRequest{OperationID: "dispatch-approve", GoalID: s.ID, ExpectedRevision: 2, Action: "approve", Reason: "Reviewed scope", Authority: in.Authority, Evidence: "review", SourceState: in.SourceState})
		return err
	})
	goalFixtureSave(t, "dispatch mirror", func() error { _, err := m.mirrorNativeAudit("cli", goalCommitIO{}); return err })
	ctx := goalDispatchContext{Source: "cli", Profile: goalDeliveryProfile{Engine: "codex", CodePointCap: 4000, Root: deliveryRoot(t)}, Environment: goalAdmissionEnvironment{
		Capabilities: map[string]goalAdmissionCapability{"codex": {Kind: "codex", WorkKinds: []nativeWorkKind{"review"}}, "claude": {Kind: "claude", WorkKinds: []nativeWorkKind{"review"}}},
		SourceState:  func(nativeGoalSpec) (string, error) { return in.SourceState, nil },
	}}
	_ = c
	ctx.Profile.Engine = nativeEngineKind(lane)
	return m, goalDispatchRequest{GoalID: s.ID, ExpectedRevision: 3, Key: "dispatch-one", Lane: lane}, ctx
}

func TestDispatchGoalIDApprovedBytes(t *testing.T) {
	m, req, ctx := dispatchFixture(t)
	r, err := m.dispatchGoal(req, ctx, goalCommitIO{})
	if err != nil || r.Phase != "running" || !runPattern.MatchString(r.RunID) {
		t.Fatal("approved goal did not publish a bound running result", err)
	}
	p, err := replayGoalJournal(m.store)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := goalRecordMetadata(p.records[req.GoalID])
	if d.State != nativeGoalRunning || d.Attempt == nil || d.Attempt.ID != r.RunID {
		t.Fatal("running result lacks durable attempt")
	}
	sealBytes, err := os.ReadFile(filepath.Join(m.store, p.records[req.GoalID].sealRef))
	if err != nil {
		t.Fatal(err)
	}
	var seal goalBriefSeal
	if json.Unmarshal(sealBytes, &seal) != nil {
		t.Fatal("seal")
	}
	_, prompt := seal.deliveryText()
	b, err := os.ReadFile(filepath.Join(m.store, "runs", r.RunID, "goal.txt"))
	if err != nil || string(b) != prompt {
		t.Fatal("launch replaced approved bytes")
	}
}

func TestDispatchGoalIDRefusesOverride(t *testing.T) {
	m, req, ctx := dispatchFixture(t)
	req.Text = "private-replacement wait"
	if _, err := m.dispatchGoal(req, ctx, goalCommitIO{}); err == nil || strings.Contains(err.Error(), "private-replacement") {
		t.Fatal("native override was not safely refused")
	}
}

func dispatchRecord(t *testing.T, m *Manager, id string) (goalReplayRecord, goalRecordData) {
	t.Helper()
	p, err := replayGoalJournal(m.store)
	if err != nil {
		t.Fatal("dispatch history unavailable", err)
	}
	old := p.records[id]
	data, err := goalRecordMetadata(old)
	if err != nil {
		t.Fatal(err)
	}
	return old, data
}
func dispatchStarts(t *testing.T, m *Manager, want int) {
	t.Helper()
	cwd := m.config.Lanes[0].Workdir
	deadline := time.Now().Add(5 * time.Second)
	for {
		entries, err := os.ReadDir(cwd)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "native-start-") {
				count++
			}
		}
		if count == want {
			return
		}
		if count > want || time.Now().After(deadline) {
			t.Fatalf("offline start oracle got %d, want %d", count, want)
		}
		time.Sleep(time.Millisecond)
	}
}
func dispatchReopen(t *testing.T, m *Manager) *Manager {
	t.Helper()
	n, err := NewManager(m.config, m.store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = n.Close() })
	return n
}
func dispatchOneFault(stage string) goalCommitIO {
	return goalCommitIO{checkpoint: func(s string) error {
		if s == stage {
			return errors.New("injected boundary failure")
		}
		return nil
	}}
}

func TestDispatchGoalIDRepeatedKey(t *testing.T) {
	m, req, ctx := dispatchFixture(t)
	r, err := m.dispatchGoal(req, ctx, goalCommitIO{})
	if err != nil {
		t.Fatal(err)
	}
	old, _ := dispatchRecord(t, m, req.GoalID)
	if _, err = m.noteGoal(goalNoteRequest{OperationID: "dispatch-note", GoalID: req.GoalID, ExpectedRevision: old.revision, Note: goalRecordNote{Text: "Observed running work"}}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []*Manager{m, dispatchReopen(t, m)} {
		retry, err := n.dispatchGoal(req, ctx, goalCommitIO{})
		if err != nil || retry != r {
			t.Fatal("retry lost original result", err)
		}
	}
	changed := req
	changed.ExpectedRevision++
	if _, err = m.dispatchGoal(changed, ctx, goalCommitIO{}); err == nil {
		t.Fatal("changed key input accepted")
	}
	changed = req
	changed.Lane = "claude"
	if _, err = m.dispatchGoal(changed, ctx, goalCommitIO{}); err == nil {
		t.Fatal("changed key lane accepted")
	}
	changed = req
	changed.GoalID = "UNRELATED"
	if _, err = m.dispatchGoal(changed, ctx, goalCommitIO{}); err == nil {
		t.Fatal("key reused for another goal")
	}
	altered := ctx
	altered.Profile.CodePointCap++
	if _, err = m.dispatchGoal(req, altered, goalCommitIO{}); err == nil {
		t.Fatal("key reused for another profile")
	}
	dispatchStarts(t, m, 1)
	_, data := dispatchRecord(t, m, req.GoalID)
	if len(data.Dispatches) != 1 || data.Attempt.ID != r.RunID {
		t.Fatal("retry invented another receipt")
	}
}

func TestDispatchGoalIDDuplicateTwoLanes(t *testing.T) {
	m, req, ctx := dispatchFixture(t)
	old, data := dispatchRecord(t, m, req.GoalID)
	s, _ := decodeNativeGoalSpec([]byte(old.spec))
	// Reapprove a scope whose eligible lanes include both; no fixture-only
	// initial approval shortcut.
	s.LaneBinding = "preferred"
	_, in := briefFixture(t)
	in.Body = []byte("GOAL: DISPATCH (size S)\nnative-dispatch wait\n")
	if _, err := m.sealGoalBrief(goalRecordRequest{OperationID: "both-seal", ExpectedRevision: old.revision, Spec: s}, in); err != nil {
		t.Fatal(err)
	}
	if _, err := m.decideGoal(goalDecisionRequest{OperationID: "both-approve", GoalID: s.ID, ExpectedRevision: old.revision + 1, Action: "approve", Reason: "Both lanes reviewed", Authority: in.Authority, Evidence: "review", SourceState: in.SourceState}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.mirrorNativeAudit("cli", goalCommitIO{}); err != nil {
		t.Fatal(err)
	}
	req.ExpectedRevision = old.revision + 2
	_ = data
	n := dispatchReopen(t, m)
	other := req
	other.Key = "second-lane"
	other.Lane = "claude"
	alternate := ctx
	alternate.Profile.Engine = "claude"
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan bool, 2)
	for _, f := range []func() bool{
		func() bool {
			_, err := m.dispatchGoal(req, ctx, goalCommitIO{})
			if err != nil {
				t.Log("first duplicate refusal", err)
			}
			return err == nil
		},
		func() bool {
			_, err := n.dispatchGoal(other, alternate, goalCommitIO{})
			if err != nil {
				t.Log("second duplicate refusal", err)
			}
			return err == nil
		},
	} {
		wg.Add(1)
		go func(f func() bool) { defer wg.Done(); <-start; results <- f() }(f)
	}
	close(start)
	wg.Wait()
	close(results)
	winners := 0
	for won := range results {
		if won {
			winners++
		}
	}
	if winners != 1 {
		t.Fatal("duplicate goal did not have exactly one winner")
	}
	dispatchStarts(t, m, 1)
	_, got := dispatchRecord(t, m, req.GoalID)
	if len(got.Dispatches) != 1 || got.Attempt == nil {
		t.Fatal("duplicate durable reservation")
	}
}

func TestDispatchGoalIDStaleRevision(t *testing.T) {
	m, req, ctx := dispatchFixture(t)
	if _, err := m.noteGoal(goalNoteRequest{OperationID: "advance-note", GoalID: req.GoalID, ExpectedRevision: 3, Note: goalRecordNote{Text: "A later note"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.mirrorNativeAudit("cli", goalCommitIO{}); err != nil {
		t.Fatal(err)
	}
	before := goalCommitTree(t, m.store)
	if _, err := m.dispatchGoal(req, ctx, goalCommitIO{}); !errors.Is(err, errGoalConflict) {
		t.Fatal("stale revision accepted", err)
	}
	if !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
		t.Fatal("stale revision wrote artifacts")
	}
	dispatchStarts(t, m, 0)
}

func TestDispatchGoalIDLaunchFailure(t *testing.T) {
	goalEndParallel(t)
	for _, stage := range []string{"prepare_before", "before_os"} {
		t.Run(stage, func(t *testing.T) {
			m, req, ctx := dispatchFixture(t)
			r, err := m.dispatchGoal(req, ctx, dispatchOneFault(stage))
			if err == nil {
				t.Fatal("fault returned success")
			}
			_, data := dispatchRecord(t, m, req.GoalID)
			expected := "failed"
			if stage == "before_os" {
				expected = "starting"
			}
			if data.State != nativeGoalApproved || len(data.Dispatches) != 1 || data.Dispatches[0].Phase != expected {
				t.Fatal("wrong failure reservation")
			}
			n := dispatchReopen(t, m)
			again, retryErr := n.dispatchGoal(req, ctx, goalCommitIO{})
			if retryErr == nil || again.RunID != r.RunID || again.Phase != expected {
				t.Fatal("failure retry was not bound", retryErr)
			}
			next := req
			next.Key = "new-after-failure"
			old, _ := dispatchRecord(t, m, req.GoalID)
			next.ExpectedRevision = old.revision
			if stage == "before_os" {
				if _, err = n.dispatchGoal(next, ctx, goalCommitIO{}); err == nil {
					t.Fatal("unknown invocation allowed replacement")
				}
				dispatchStarts(t, m, 0)
			} else {
				if _, err = n.dispatchGoal(next, ctx, goalCommitIO{}); err != nil {
					t.Fatal("confirmed nonlaunch did not release resources", err)
				}
				dispatchStarts(t, m, 1)
			}
		})
	}
}

func TestDispatchGoalIDReservationFaults(t *testing.T) {
	goalEndParallel(t)
	for _, stage := range []string{"blob_write", "blob_sync", "blob_publish", "blobs_sync", "before_append", "partial_write", "journal_sync", "journal_directory_sync", "publication", "native_intent.write", "native_receipt.write"} {
		t.Run(stage, func(t *testing.T) {
			m, req, ctx := dispatchFixture(t)
			r, err := m.dispatchGoal(req, ctx, dispatchOneFault(stage))
			// Unknown checkpoint names must not manufacture a passing fault proof.
			if err == nil {
				t.Fatal("reservation fault was not reached", stage)
			}
			dispatchStarts(t, m, 0)
			p, e := replayGoalJournal(m.store)
			if e != nil {
				t.Fatal(e)
			}
			if len(p.records) != 1 {
				t.Fatal("reservation damaged unrelated history")
			}
			_, data := dispatchRecord(t, m, req.GoalID)
			n := dispatchReopen(t, m)
			if len(data.Dispatches) > 0 {
				again, e := n.dispatchGoal(req, ctx, goalCommitIO{})
				if e == nil || again.RunID != r.RunID {
					t.Fatal("unconfirmed reservation lost identity", e)
				}
				dispatchStarts(t, m, 0)
			} else if !p.tornTail {
				if _, e := n.dispatchGoal(req, ctx, goalCommitIO{}); e != nil {
					t.Fatal("preappend refusal prevented a genuine first reservation", e)
				}
				dispatchStarts(t, m, 1)
			} else {
				if _, e := n.dispatchGoal(req, ctx, goalCommitIO{}); e == nil {
					t.Fatal("torn reservation allowed launch")
				}
			}
		})
	}
}

func TestDispatchGoalIDReservationPrefixes(t *testing.T) {
	m, req, ctx := dispatchFixture(t)
	prefix, err := os.ReadFile(filepath.Join(m.store, goalJournalName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.dispatchGoal(req, ctx, dispatchOneFault("publication")); err == nil {
		t.Fatal("publication fault")
	}
	full, err := os.ReadFile(filepath.Join(m.store, goalJournalName))
	if err != nil {
		t.Fatal(err)
	}
	frame := full[len(prefix):]
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	before := goalCommitTree(t, m.store)
	for i := 1; i < len(frame); i++ {
		p, e := replayGoalJournalData(root, append(bytes.Clone(prefix), frame[:i]...))
		if e != nil || !p.tornTail || p.sequence != 3 || p.records[req.GoalID].revision != 3 {
			t.Fatalf("torn reservation prefix %d became visible", i)
		}
	}
	if !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
		t.Fatal("prefix reader wrote")
	}
	dispatchStarts(t, m, 0)
}

func TestDispatchGoalIDPreparationFaults(t *testing.T) {
	for _, stage := range []string{"prepare_before", "prepare_directory", "prepare_prompt", "prepare_meta", "prepare_rename", "prepare_sync", "before_start_intent"} {
		t.Run(stage, func(t *testing.T) {
			m, req, ctx := dispatchFixture(t)
			r, e := m.dispatchGoal(req, ctx, dispatchOneFault(stage))
			if !errors.Is(e, errGoalNotLaunched) || r.Phase != "failed" {
				t.Fatal("unstarted preparation did not get confirmed failure", e)
			}
			n := dispatchReopen(t, m)
			again, e := n.dispatchGoal(req, ctx, goalCommitIO{})
			if !errors.Is(e, errGoalNotLaunched) || again != r {
				t.Fatal("preparation allocated a replacement", e)
			}
			dispatchStarts(t, m, 0)
			entries, err := os.ReadDir(filepath.Join(m.store, "runs"))
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if entry.Name() != r.RunID && entry.Name() != ".pending-"+r.RunID {
					t.Fatal("preparation used another full ID")
				}
			}
		})
	}
}

func TestDispatchGoalIDChildStartFaults(t *testing.T) {
	goalEndParallel(t)
	for _, stage := range []string{"before_os", "after_os", "child_identity"} {
		t.Run(stage, func(t *testing.T) {
			m, req, ctx := dispatchFixture(t)
			r, e := m.dispatchGoal(req, ctx, dispatchOneFault(stage))
			if !errors.Is(e, errGoalLaunchHeld) {
				t.Fatal("start fault was not held", e)
			}
			if stage == "before_os" {
				dispatchStarts(t, m, 0)
			} else {
				dispatchStarts(t, m, 1)
			}
			n := dispatchReopen(t, m)
			again, e := n.dispatchGoal(req, ctx, goalCommitIO{})
			if again.RunID != r.RunID {
				t.Fatal("retry changed child ID")
			}
			if stage == "before_os" && e == nil || stage != "before_os" && (e != nil || again.Phase != "running") {
				t.Fatal("wrong reconciliation", e)
			}
			if stage != "before_os" {
				dispatchStarts(t, m, 1)
			}
		})
	}
}

func TestDispatchGoalIDPublicationFaults(t *testing.T) {
	goalEndParallel(t)
	for _, stage := range []string{"running_publication", "journal_sync", "journal_directory_sync", "publication", "native_receipt.write", "before_success"} {
		t.Run(stage, func(t *testing.T) {
			m, req, ctx := dispatchFixture(t)
			publishing := false
			fault := goalCommitIO{checkpoint: func(s string) error {
				if s == "running_publication" {
					publishing = true
				}
				if s == stage {
					if publishing {
						return errors.New("publication failure")
					}
				}
				return nil
			}}
			r, e := m.dispatchGoal(req, ctx, fault)
			if !errors.Is(e, errGoalLaunchHeld) {
				t.Fatal("publication fault did not hold success", e)
			}
			dispatchStarts(t, m, 1)
			n := dispatchReopen(t, m)
			again, e := n.dispatchGoal(req, ctx, goalCommitIO{})
			if e != nil || again.RunID != r.RunID || again.Phase != "running" {
				t.Fatal("identified child not reconciled", e)
			}
			for j := 0; j < 2; j++ {
				retry, e := n.dispatchGoal(req, ctx, goalCommitIO{})
				if e != nil || retry != again {
					t.Fatal("publication duplicate", e)
				}
			}
			dispatchStarts(t, m, 1)
			_, data := dispatchRecord(t, m, req.GoalID)
			if len(data.Dispatches) != 1 || data.Dispatches[0].PID <= 1 {
				t.Fatal("missing exact launch identity")
			}
			// The restart adapter's periodic writer legitimately owns a separate
			// nonblocking store lock. Finish that adapter before the final audit
			// observation; all restart/duplicate/identity assertions ran above.
			if err := n.Close(); err != nil {
				t.Fatal("close restart adapter before audit observation", err)
			}
			audit, e := m.nativeAuditStatus()
			if e != nil || audit.Pending != 0 {
				if e != nil {
					goalSetupFailure(t, "running audit", e)
				}
				t.Fatalf("running audit unconfirmed: sequence=%d mirrored=%d pending=%d", audit.Sequence, audit.Mirrored, audit.Pending)
			}
		})
	}
}

func TestDispatchGoalIDFinalAdmission(t *testing.T) {
	for _, mode := range []string{"paused", "frozen", "source", "capability", "closed", "readonly", "audit", "removed"} {
		t.Run(mode, func(t *testing.T) {
			m, req, ctx := dispatchFixture(t)
			switch mode {
			case "paused":
				admissionPause(t, m, false)
			case "frozen":
				if err := m.Freeze("Owner review"); err != nil {
					t.Fatal(err)
				}
			case "source":
				ctx.Environment.SourceState = func(nativeGoalSpec) (string, error) { return "changed", nil }
			case "capability":
				ctx.Environment.Capabilities = map[string]goalAdmissionCapability{}
			case "closed":
				if err := m.Close(); err != nil {
					t.Fatal(err)
				}
			case "readonly":
				m.mu.Lock()
				m.readOnly = true
				m.mu.Unlock()
				t.Cleanup(func() { m.mu.Lock(); m.readOnly = false; m.mu.Unlock() })
			case "audit":
				if err := os.Remove(filepath.Join(m.store, nativeAuditReceiptName)); err != nil {
					t.Fatal(err)
				}
			case "removed":
				m.mu.Lock()
				m.config.Lanes = nil
				m.mu.Unlock()
			}
			before := goalCommitTree(t, m.store)
			if _, err := m.dispatchGoal(req, ctx, goalCommitIO{}); err == nil {
				t.Fatal("final guard allowed launch")
			}
			if !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
				t.Fatal("blocked admission wrote")
			}
			if mode != "removed" {
				dispatchStarts(t, m, 0)
			}
		})
	}
}

func TestDispatchGoalIDClosedCodecAndText(t *testing.T) {
	valid := `{"goal_id":"DISPATCH","expected_revision":3,"key":"retry-1","lane":"codex"}`
	if _, err := decodeGoalDispatchRequest([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	exact := strings.Replace(valid, `"goal_id"`, `"goal\u005fid"`, 1)
	if _, err := decodeGoalDispatchRequest([]byte(exact)); err != nil {
		t.Fatal("canonical escaped key refused", err)
	}
	for _, b := range []string{
		strings.Replace(valid, `"goal_id"`, `"Goal_ID"`, 1),
		strings.Replace(valid, `"goal_id"`, `"\u0047oal_id"`, 1),
		strings.Replace(valid, `"key"`, `"KEY"`, 1),
		strings.Replace(valid, `"lane"`, `"Lane"`, 1),
		strings.Replace(valid, `"expected_revision":3`, `"expected_revision":3.0`, 1),
		strings.Replace(valid, `"expected_revision":3`, `"expected_revision":3e0`, 1),
		strings.Replace(valid, `"expected_revision":3`, `"expected_revision":null`, 1),
		strings.Replace(valid, `"expected_revision":3`, `"expected_revision":"3"`, 1),
		strings.Replace(valid, `"key":"retry-1"`, `"key":"a","key":"b"`, 1),
		strings.Replace(valid, `"key":"retry-1"`, `"key":"a","KEY":"b"`, 1),
		strings.Replace(valid, `"key":"retry-1"`, `"key":"a","\u006bey":"b"`, 1),
		strings.Replace(valid, `"goal_id":"DISPATCH"`, `"goal_id":"../DISPATCH"`, 1),
		strings.TrimSuffix(valid, "}") + `,"text":"private-body"}`,
		strings.TrimSuffix(valid, "}") + `,"body":"private-body"}`,
		valid + "{}", strings.Repeat(" ", 4097) + valid,
	} {
		if _, err := decodeGoalDispatchRequest([]byte(b)); err == nil {
			t.Error("closed request accepted alias/type/override")
		}
	}
	for _, separator := range []string{"\u2028", "\u2029", "\u0085"} {
		for _, field := range []string{"key", "lane", "goal_id"} {
			var r map[string]any
			_ = json.Unmarshal([]byte(valid), &r)
			r[field] = "a" + separator + "b"
			b, _ := json.Marshal(r)
			if _, err := decodeGoalDispatchRequest(b); err == nil {
				t.Error("one-line request accepted separator")
			}
		}
	}
	m, req, ctx := dispatchFixture(t)
	if _, err := m.dispatchGoal(req, ctx, dispatchOneFault("before_os")); err == nil {
		t.Fatal("hold setup")
	}
	_, data := dispatchRecord(t, m, req.GoalID)
	canonical, _ := json.Marshal(data)
	var object map[string]any
	_ = json.Unmarshal(canonical, &object)
	dispatch := object["dispatches"].([]any)[0].(map[string]any)
	for _, field := range []string{"source", "root", "process_start", "job"} {
		original := dispatch[field]
		dispatch[field] = "a\u2028b"
		b, _ := json.Marshal(object)
		if _, err := decodeGoalRecord(b); err == nil {
			t.Error("retained field accepted line separator", field)
		}
		dispatch[field] = original
	}
	for key, value := range dispatch {
		delete(dispatch, key)
		dispatch[strings.ToUpper(key)] = value
		b, _ := json.Marshal(object)
		if _, err := decodeGoalRecord(b); err == nil {
			t.Error("retained record accepted key alias", key)
		}
		delete(dispatch, strings.ToUpper(key))
		dispatch[key] = value
	}
}

func TestDispatchGoalIDRetainedScopeAndApproval(t *testing.T) {
	for _, mutation := range []string{"erase", "release", "runid", "delivery", "approval", "state", "scope", "invent-running"} {
		t.Run(mutation, func(t *testing.T) {
			m, req, ctx := dispatchFixture(t)
			if _, err := m.dispatchGoal(req, ctx, dispatchOneFault("before_os")); err == nil {
				t.Fatal("setup")
			}
			old, data := dispatchRecord(t, m, req.GoalID)
			specBytes := []byte(old.spec)
			switch mutation {
			case "erase":
				data.Dispatches = nil
			case "release":
				data.Dispatches[0].Phase = "failed"
			case "runid":
				data.Dispatches[0].RunID = strings.Repeat("f", 32)
			case "delivery":
				data.Dispatches[0].Delivery.Prompt = "private-replacement"
			case "approval":
				data.ApprovalRevision++
			case "state":
				data.State = nativeGoalDrafted
				data.ApprovalRevision = 0
			case "scope":
				s, _ := decodeNativeGoalSpec(specBytes)
				s.Resources = []string{"other"}
				specBytes, _ = json.Marshal(s)
			case "invent-running":
				data.State = nativeGoalRunning
				data.Attempt = &goalDecisionAttempt{ID: strings.Repeat("f", 32)}
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
			_, _, err = m.commitGoal(goalCommitRequest{OperationID: "counterfeit", ExpectedRevision: old.revision, Spec: specBytes, Seal: seal, Record: record, MutationSHA256: strings.Repeat("a", 64)}, goalCommitIO{})
			if err == nil {
				t.Fatal("candidate writer accepted invented retained state")
			}
			if !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
				t.Fatal("candidate refusal changed history")
			}
			briefHistoryReplayCandidate(t, m, req.GoalID, old, specBytes, record)
		})
	}
}

func TestDispatchGoalIDDeliveryIdentity(t *testing.T) {
	goalEndParallel(t)
	for _, mode := range []string{"pointer", "replace", "mode", "digest", "symlink", "root-swap"} {
		t.Run(mode, func(t *testing.T) {
			m, req, ctx := dispatchFixtureBody(t, strings.Repeat("界", 5000)+"\nnative-dispatch wait\n", "codex")
			if mode == "pointer" {
				r, err := m.dispatchGoal(req, ctx, goalCommitIO{})
				if err != nil {
					t.Fatal(err)
				}
				dispatchStarts(t, m, 1)
				_, data := dispatchRecord(t, m, req.GoalID)
				d := data.Dispatches[0].Delivery
				starts, _ := filepath.Glob(filepath.Join(m.config.Lanes[0].Workdir, "native-start-*"))
				b, err := os.ReadFile(starts[0])
				if err != nil || string(b) != fmt.Sprintf("%s %s %d", d.PromptSHA256, d.FullSHA256, d.FullBytes) {
					t.Fatal("offline child did not consume full sealed artifact")
				}
				if !runPattern.MatchString(r.RunID) {
					t.Fatal("full ID")
				}
				return
			}
			r, err := m.dispatchGoal(req, ctx, dispatchOneFault("before_os"))
			if err == nil {
				t.Fatal("hold setup")
			}
			_, data := dispatchRecord(t, m, req.GoalID)
			d := data.Dispatches[0].Delivery
			b, err := os.ReadFile(d.Artifact)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "replace":
				if os.Rename(d.Artifact, d.Artifact+".old") != nil || os.WriteFile(d.Artifact, b, 0600) != nil {
					t.Fatal("replacement fixture")
				}
			case "mode":
				if os.Chmod(d.Artifact, 0644) != nil {
					t.Fatal("mode fixture")
				}
			case "digest":
				if os.WriteFile(d.Artifact, []byte("corrupt"), 0600) != nil {
					t.Fatal("digest fixture")
				}
			case "symlink":
				if os.Rename(d.Artifact, d.Artifact+".old") != nil || os.Symlink(d.Artifact+".old", d.Artifact) != nil {
					t.Fatal("link fixture")
				}
			case "root-swap":
				if os.Rename(ctx.Profile.Root, ctx.Profile.Root+".old") != nil || os.Mkdir(ctx.Profile.Root, 0700) != nil {
					t.Fatal("root fixture")
				}
			}
			n := dispatchReopen(t, m)
			again, err := n.dispatchGoal(req, ctx, goalCommitIO{})
			if err == nil || again.RunID != r.RunID {
				t.Fatal("retained delivery replacement was legitimized", err)
			}
			dispatchStarts(t, m, 0)
		})
	}
}

func TestDispatchGoalIDLegacyBoundaryAndImmediateExit(t *testing.T) {
	m, req, ctx := dispatchFixtureBody(t, "ack-goal\n", "claude")
	if _, err := m.Dispatch(req.Lane, req.GoalID, "private-replacement"); err == nil {
		t.Fatal("legacy free text bypassed native goal")
	}
	r, err := m.dispatchGoal(req, ctx, goalCommitIO{})
	if err != nil && !errors.Is(err, errGoalLaunchHeld) {
		t.Fatal(err)
	}
	waitState(t, m, req.Lane, "ended")
	if err = m.Recover(); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Reply(req.Lane, "private-replacement"); err == nil {
		t.Fatal("native reply bypass")
	}
	dispatchStarts(t, m, 1)
	_, data := dispatchRecord(t, m, req.GoalID)
	if len(data.Dispatches) != 1 || data.Dispatches[0].RunID != r.RunID {
		t.Fatal("immediate exit lost original attempt")
	}
}

type dispatchProcessFixture struct {
	Config  Config              `json:"config"`
	Store   string              `json:"store"`
	Request goalDispatchRequest `json:"request"`
	Root    string              `json:"root"`
}

func TestDispatchGoalIDProcessHelper(t *testing.T) {
	if len(os.Args) < 4 || os.Args[len(os.Args)-3] != "b11-fixture" {
		return
	}
	path, stage := os.Args[len(os.Args)-2], os.Args[len(os.Args)-1]
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f dispatchProcessFixture
	if json.Unmarshal(b, &f) != nil {
		t.Fatal("fixture codec")
	}
	m, err := newReadManager(f.Config, f.Store)
	if err != nil {
		t.Fatal(err)
	}
	m.readOnly = false
	defer m.Close()
	env := goalAdmissionEnvironment{Capabilities: map[string]goalAdmissionCapability{}, SourceState: func(nativeGoalSpec) (string, error) { return "observed-head-1", nil }}
	for _, lane := range f.Config.Lanes {
		env.Capabilities[lane.Name] = goalAdmissionCapability{Kind: nativeEngineKind(lane.Kind), WorkKinds: []nativeWorkKind{"review"}}
	}
	ctx := goalDispatchContext{Source: "cli", Profile: goalDeliveryProfile{Engine: "codex", CodePointCap: 4000, Root: f.Root}, Environment: env}
	hit := false
	fault := goalCommitIO{checkpoint: func(s string) error {
		if s == stage && !hit {
			hit = true
			fmt.Println("barrier")
			var byte [1]byte
			_, err := io.ReadFull(os.Stdin, byte[:])
			return err
		}
		return nil
	}}
	if stage == "no-barrier" {
		fmt.Println("request-ready")
	}
	_, err = m.dispatchGoal(f.Request, ctx, fault)
	if err != nil {
		var contention *laneControlFault
		if errors.As(err, &contention) && contention.Code == "locked" {
			fmt.Println("locked")
		} else {
			fmt.Println("held")
		}
	} else {
		fmt.Println("confirmed")
	}
}

func TestDispatchGoalIDExactlyOneAdoptedChild(t *testing.T) {
	goalEndParallel(t)
	for _, stage := range []string{"before_append", "before_os", "after_os", "child_identity", "before_success"} {
		t.Run(stage, func(t *testing.T) {
			m, req, ctx := dispatchFixture(t)
			f := dispatchProcessFixture{Config: m.config, Store: m.store, Request: req, Root: ctx.Profile.Root}
			path := filepath.Join(t.TempDir(), "dispatch-fixture.json")
			b, _ := json.Marshal(f)
			if os.WriteFile(path, b, 0600) != nil {
				t.Fatal("fixture config")
			}
			if err := m.Close(); err != nil {
				t.Fatal(err)
			}
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(exe, "-test.run=^TestDispatchGoalIDProcessHelper$", "--", "b11-fixture", path, stage)
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			cmd.Stderr = io.Discard
			started := time.Now().UTC()
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waited := false
			t.Cleanup(func() {
				if !waited {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
				_ = stdin.Close()
				if cmd.ProcessState == nil || !cmd.ProcessState.Exited() && !cmd.ProcessState.Sys().(syscall.WaitStatus).Signaled() {
					t.Error("crash helper not reaped")
				}
			})
			ready := make(chan bool, 1)
			go func() {
				scanner := bufio.NewScanner(stdout)
				for scanner.Scan() {
					if scanner.Text() == "barrier" {
						ready <- true
						return
					}
				}
				ready <- false
			}()
			select {
			case ok := <-ready:
				if !ok {
					t.Fatal("crash checkpoint not reached")
				}
			case <-time.After(15 * time.Second):
				t.Fatal("crash helper deadline")
			}
			if err = cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			err = cmd.Wait()
			waited = true
			_ = stdin.Close()
			if err == nil || cmd.ProcessState == nil {
				t.Fatal("helper was not killed and reaped")
			}
			t.Logf("offline crash helper %s start=%s end=%s teardown=waited", stage, started.Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano))
			n := dispatchReopen(t, m)
			// New manager observation is deliberately independent of dispatch replay.
			again, e := n.dispatchGoal(req, ctx, goalCommitIO{})
			if stage == "before_os" {
				if e == nil {
					t.Fatal("unidentified start invoked another child")
				}
				dispatchStarts(t, n, 0)
			} else if stage == "before_append" {
				if e != nil {
					t.Fatal("first request after preappend kill refused", e)
				}
				dispatchStarts(t, n, 1)
			} else {
				if e != nil || again.Phase != "running" {
					t.Fatal("surviving full-ID child not adopted", e)
				}
				dispatchStarts(t, n, 1)
				_, data := dispatchRecord(t, n, req.GoalID)
				if len(data.Dispatches) != 1 || data.Dispatches[0].PID <= 1 {
					t.Fatal("adoption did not bind actual identity")
				}
				another := dispatchReopen(t, n)
				retry, e := another.dispatchGoal(req, ctx, goalCommitIO{})
				if e != nil || retry != again {
					t.Fatal("second reconciler duplicated outcome", e)
				}
				dispatchStarts(t, n, 1)
			}
			if stage != "before_os" {
				if err := n.Cancel(req.Lane); err != nil {
					t.Fatal("adopted fixture cleanup", err)
				}
				waitState(t, n, req.Lane, "ended")
			}
		})
	}
}

func TestDispatchGoalIDPauseOrdering(t *testing.T) {
	m, req, ctx := dispatchFixture(t)
	// Exercise the actual locked A commit after winning the same mutex/inode,
	// without turning unrelated public TryLock contention into a flaky oracle.
	setLocked := func(paused bool) error {
		m.mu.Lock()
		defer m.mu.Unlock()
		guard, err := m.controlLock()
		if err != nil {
			return err
		}
		defer guard.Close()
		root, err := openLaneControlRoot(m.store)
		if err != nil {
			return err
		}
		defer root.Close()
		_, err = m.setLaneControlLocked(root, laneControlChange{Lane: "codex", Paused: paused}, "cli", nil)
		return err
	}
	at := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	fault := goalCommitIO{checkpoint: func(s string) error {
		if s == "running_publication" {
			close(at)
			<-release
		}
		return nil
	}}
	go func() { _, err := m.dispatchGoal(req, ctx, fault); done <- err }()
	select {
	case <-at:
	case <-time.After(10 * time.Second):
		t.Fatal("launch boundary")
	}
	pauseDone := make(chan error, 1)
	go func() {
		pauseDone <- setLocked(true)
	}()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-pauseDone; err != nil {
		t.Fatal(err)
	}
	_, data := dispatchRecord(t, m, req.GoalID)
	run := m.goalDispatchChild(data.Dispatches[0])
	if run == nil {
		t.Fatal("pause interrupted admitted child")
	}
	if err := setLocked(false); err != nil {
		t.Fatal(err)
	}
	dispatchStarts(t, m, 1)
}

func TestDispatchGoalIDFinalSourceObservation(t *testing.T) {
	m, req, ctx := dispatchFixture(t)
	changed := false
	ctx.Environment.SourceState = func(nativeGoalSpec) (string, error) {
		if changed {
			return "changed-source", nil
		}
		return "observed-head-1", nil
	}
	fault := goalCommitIO{checkpoint: func(stage string) error {
		if stage == "prepare_sync" {
			changed = true
		}
		return nil
	}}
	r, err := m.dispatchGoal(req, ctx, fault)
	if !errors.Is(err, errGoalNotLaunched) || r.Phase != "failed" {
		t.Fatal("final source change did not refuse before start", err)
	}
	dispatchStarts(t, m, 0)
}

func TestDispatchGoalIDUnknownIdentity(t *testing.T) {
	goalEndParallel(t)
	for _, mode := range []string{"pid-only", "mismatch", "job-unavailable"} {
		t.Run(mode, func(t *testing.T) {
			m, req, ctx := dispatchFixture(t)
			var original Run
			fault := goalCommitIO{checkpoint: func(stage string) error {
				if stage != "after_os" {
					return nil
				}
				runs, err := m.history()
				if err != nil || len(runs) != 1 {
					return errors.New("fixture record unavailable")
				}
				original = runs[0]
				t.Cleanup(func() {
					if sameProcess(original) {
						_ = signalRun(original, syscall.SIGKILL)
					}
					deadline := time.Now().Add(5 * time.Second)
					for sameProcess(original) && time.Now().Before(deadline) {
						time.Sleep(time.Millisecond)
					}
					if sameProcess(original) {
						t.Error("offline identity fixture child survived teardown")
					} else {
						t.Log("offline identity fixture teardown verified before removal")
					}
				})
				corrupt := original
				switch mode {
				case "pid-only":
					corrupt.ProcessStart = ""
				case "mismatch":
					corrupt.PID = os.Getpid()
				case "job-unavailable":
					corrupt.LaunchdLabel = "nuntius.run." + corrupt.ID
				}
				if err = m.save(&corrupt); err != nil {
					return err
				}
				return errors.New("identity publication failure")
			}}
			r, err := m.dispatchGoal(req, ctx, fault)
			if !errors.Is(err, errGoalLaunchHeld) {
				t.Fatal("unknown identity was not held", err)
			}
			dispatchStarts(t, m, 1)
			retry, err := m.dispatchGoal(req, ctx, goalCommitIO{})
			if err == nil || retry.RunID != r.RunID {
				t.Fatal("unknown identity retried launch")
			}
			if !sameProcess(original) {
				t.Fatal("unidentified process was signalled")
			}
			next := req
			next.Key = "second-unknown"
			old, _ := dispatchRecord(t, m, req.GoalID)
			next.ExpectedRevision = old.revision
			if _, err = m.dispatchGoal(next, ctx, goalCommitIO{}); err == nil {
				t.Fatal("unknown identity released resources")
			}
			dispatchStarts(t, m, 1)
		})
	}
}

func TestDispatchGoalIDDuplicateProcesses(t *testing.T) {
	goalEndParallel(t)
	m, req, ctx := dispatchFixture(t)
	f := dispatchProcessFixture{Config: m.config, Store: m.store, Request: req, Root: ctx.Profile.Root}
	path := filepath.Join(t.TempDir(), "dispatch-fixture.json")
	b, _ := json.Marshal(f)
	if os.WriteFile(path, b, 0600) != nil {
		t.Fatal("fixture")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestDispatchGoalIDProcessHelper$", "--", "b11-fixture", path, "before_append")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = io.Discard
	started := time.Now().UTC()
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		_ = stdin.Close()
		if cmd.ProcessState == nil {
			t.Error("duplicate holder not waited")
		}
	})
	ready := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if scanner.Text() == "barrier" {
				ready <- true
				return
			}
		}
		ready <- false
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("holder checkpoint")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("holder deadline")
	}
	// Independent exact retry reaches the same inode while the holder owns it.
	deadline, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	contender := exec.CommandContext(deadline, exe, "-test.run=^TestDispatchGoalIDProcessHelper$", "--", "b11-fixture", path, "no-barrier")
	secondStart := time.Now().UTC()
	secondOut, e := contender.StdoutPipe()
	if e != nil {
		t.Fatal(e)
	}
	contender.Stderr = io.Discard
	if e = contender.Start(); e != nil {
		t.Fatal(e)
	}
	secondWaited := false
	t.Cleanup(func() {
		if !secondWaited {
			_ = contender.Process.Kill()
			_ = contender.Wait()
		}
	})
	reader := bufio.NewReader(secondOut)
	line, e := reader.ReadString('\n')
	if e != nil || strings.TrimSpace(line) != "request-ready" {
		t.Fatal("contender did not reach request")
	}
	if _, err = stdin.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	_ = stdin.Close()
	if err = cmd.Wait(); err != nil {
		t.Fatal("holder did not finish", err)
	}
	waited = true
	if !cmd.ProcessState.Success() {
		t.Fatal("holder process state")
	}
	out, e := io.ReadAll(reader)
	if e != nil {
		t.Fatal(e)
	}
	e = contender.Wait()
	secondWaited = true
	if e != nil || contender.ProcessState == nil || !contender.ProcessState.Success() {
		t.Fatal("independent retry lost original receipt")
	}
	if bytes.HasPrefix(out, []byte("locked\n")) {
		// Bounded admission can expire while the original process finishes its
		// durable child publication. Retry the exact same request after that
		// process has returned; a refusal grants no authority to replace it.
		retry := exec.CommandContext(deadline, exe, "-test.run=^TestDispatchGoalIDProcessHelper$", "--", "b11-fixture", path, "no-barrier")
		retry.Stderr = io.Discard
		out, e = retry.Output()
		if e != nil {
			t.Fatal("independent exact retry failed after owner release")
		}
	}
	if !bytes.Contains(out, []byte("confirmed")) {
		t.Fatal("independent retry lost original receipt")
	}
	t.Logf("offline contender start=%s end=%s teardown=waited", secondStart.Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano))
	t.Logf("offline duplicate holder start=%s end=%s teardown=waited", started.Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano))
	n := dispatchReopen(t, m)
	r, err := n.dispatchGoal(req, ctx, goalCommitIO{})
	if err != nil || r.Phase != "running" {
		t.Fatal("holder outcome lost", err)
	}
	dispatchStarts(t, m, 1)
	if err = n.Cancel(req.Lane); err != nil {
		t.Fatal(err)
	}
	waitState(t, n, req.Lane, "ended")
}

func TestDispatchGoalIDSavedHistoryPolicy(t *testing.T) {
	for _, mutation := range []string{"erase", "release", "runid", "delivery", "invent-running"} {
		t.Run(mutation, func(t *testing.T) {
			m, req, ctx := dispatchFixture(t)
			if _, err := m.dispatchGoal(req, ctx, dispatchOneFault("before_os")); err == nil {
				t.Fatal("setup")
			}
			old, data := dispatchRecord(t, m, req.GoalID)
			switch mutation {
			case "erase":
				data.Dispatches = nil
			case "release":
				data.Dispatches[0].Phase = "failed"
			case "runid":
				data.Dispatches[0].RunID = strings.Repeat("f", 32)
			case "delivery":
				data.Dispatches[0].Delivery.Prompt = "private-replacement"
			case "invent-running":
				data.State = nativeGoalRunning
				data.Attempt = &goalDecisionAttempt{ID: strings.Repeat("f", 32)}
			}
			record, _ := json.Marshal(data)
			briefHistoryReplayCandidate(t, m, req.GoalID, old, []byte(old.spec), record)
		})
	}
}

func TestDispatchGoalIDReservationLaneReplay(t *testing.T) {
	m, req, ctx := dispatchFixture(t)
	old, _ := dispatchRecord(t, m, req.GoalID)
	prefix, err := os.ReadFile(filepath.Join(m.store, goalJournalName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.dispatchGoal(req, ctx, dispatchOneFault("publication")); err == nil {
		t.Fatal("reservation fixture")
	}
	_, data := dispatchRecord(t, m, req.GoalID)
	data.Dispatches[0].Request.Lane = "claude" // approved binding requires codex
	record, _ := json.Marshal(data)
	seal, err := os.ReadFile(filepath.Join(m.store, old.sealRef))
	if err != nil {
		t.Fatal(err)
	}
	// This test-owned independent history contains only its genuine prior prefix.
	goalReplayWrite(t, m.store, goalJournalName, prefix)
	_, _, err = m.commitGoal(goalCommitRequest{OperationID: "counterfeit", ExpectedRevision: old.revision, Spec: []byte(old.spec), Seal: seal, Record: record, MutationSHA256: strings.Repeat("a", 64)}, goalCommitIO{})
	if err == nil {
		t.Error("candidate writer invented a reservation on an unapproved required lane")
	}
	goalReplayWrite(t, m.store, goalJournalName, prefix)
	briefHistoryReplayCandidate(t, m, req.GoalID, old, []byte(old.spec), record)
	dispatchStarts(t, m, 0)
}

func TestDispatchGoalIDPausedBlockers(t *testing.T) {
	m, req, ctx := dispatchFixture(t)
	if _, err := m.noteGoal(goalNoteRequest{OperationID: "pending-note", GoalID: req.GoalID, ExpectedRevision: 3, Note: goalRecordNote{Text: "Pending audit"}}); err != nil {
		t.Fatal(err)
	}
	req.ExpectedRevision = 4
	admissionPause(t, m, false)
	if err := m.Freeze("Owner review"); err != nil {
		t.Fatal(err)
	}
	ctx.Environment.SourceState = func(nativeGoalSpec) (string, error) { return "changed-source", nil }
	_, err := m.dispatchGoal(req, ctx, goalCommitIO{})
	var fault *laneAdmissionFault
	if !errors.As(err, &fault) || fault.Code != "lane_paused" || !fault.Paused || fault.Retryable {
		t.Fatal("primary paused refusal lost")
	}
	for _, code := range []string{"lane_paused", "frozen", "source_changed", "native_audit_unavailable"} {
		found := false
		for _, blocker := range fault.Blockers {
			found = found || blocker == code
		}
		if !found {
			t.Error("native paused refusal hid safe blocker", code)
		}
	}
	dispatchStarts(t, m, 0)
}
