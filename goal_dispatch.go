package nuntius

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

var (
	errGoalDispatch    = errors.New("native goal dispatch refused; use the current approved goal, revision and a distinct retry key without replacement text")
	errGoalLaunchHeld  = errors.New("native launch is held for review; work may have started, so no replacement child will be started")
	errGoalNotLaunched = errors.New("native launch did not start a child; review the preparation failure before requesting a new dispatch")
)

// Internal only. Profile, environment and source are adapter-owned.
type goalDispatchRequest struct {
	GoalID           string `json:"goal_id"`
	ExpectedRevision uint64 `json:"expected_revision"`
	Key              string `json:"key"`
	Lane             string `json:"lane"`
	Text             string `json:"-"`
}
type goalDispatchContext struct {
	Environment goalAdmissionEnvironment
	Profile     goalDeliveryProfile
	Source      string
}
type goalDispatchResult struct {
	RunID string
	Phase string
}

// Permanent receipts live in the authoritative journal, never a second index.
type goalDispatchRecord struct {
	Request          goalDispatchRequest      `json:"request"`
	RunID            string                   `json:"run_id"`
	Phase            string                   `json:"phase"`
	Source           string                   `json:"source"`
	Root             string                   `json:"root"`
	Spec             string                   `json:"approved_spec"`
	SealRef          string                   `json:"seal_ref"`
	ApprovalRevision uint64                   `json:"approval_revision"`
	Delivery         goalBriefDelivery        `json:"delivery"`
	Writes           []string                 `json:"canonical_writes"`
	Scope            []goalWriteEvidence      `json:"scope_evidence"`
	PID              int                      `json:"pid"`
	ProcessStart     string                   `json:"process_start"`
	Job              string                   `json:"job"`
	Continuation     *goalContinuationBinding `json:"continuation,omitempty"`
}

func decodeGoalDispatchRequest(b []byte) (goalDispatchRequest, error) {
	var r goalDispatchRequest
	if !goalDispatchCanonical(b, &r, 4096) || !validGoalDispatchRequest(r) {
		return r, errGoalDispatch
	}
	return r, nil
}

// queueJSON rejects decoded duplicates; canonical comparison rejects every
// nested unknown/case key, null, wrong type and noncanonical numeric spelling.
func goalDispatchCanonical(b []byte, target any, limit int) bool {
	if len(b) > limit || !utf8.Valid(b) || !nativeJSONScalars(b) {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	raw, err := queueJSON(d, 1)
	if err != nil {
		return false
	}
	if _, err = d.Token(); err != io.EOF || json.Unmarshal(b, target) != nil {
		return false
	}
	canonical, err := json.Marshal(target)
	if err != nil {
		return false
	}
	c := json.NewDecoder(bytes.NewReader(canonical))
	c.UseNumber()
	want, err := queueJSON(c, 1)
	if err != nil {
		return false
	}
	a, _ := json.Marshal(raw)
	z, _ := json.Marshal(want)
	return bytes.Equal(a, z)
}
func validGoalDispatchRequest(r goalDispatchRequest) bool {
	return goalPattern.MatchString(r.GoalID) && r.ExpectedRevision > 0 && r.ExpectedRevision <= goalJournalMaxEvents && validGoalOperationID(r.Key) && namePattern.MatchString(r.Lane) && r.Text == ""
}
func validGoalDispatchJSON(m map[string]any) bool {
	v, ok := m["dispatches"]
	if !ok {
		return true
	}
	b, err := json.Marshal(v)
	var rows []goalDispatchRecord
	if err != nil || !goalDispatchCanonical(b, &rows, goalRecordMaxBytes) || len(rows) == 0 || len(rows) > 8 {
		return false
	}
	keys, ids := map[string]bool{}, map[string]bool{}
	for i, d := range rows {
		if !validGoalDispatchRequest(d.Request) || !runPattern.MatchString(d.RunID) || keys[d.Request.Key] || ids[d.RunID] || !validNativeAuditSource(d.Source) || !goalArtifactPath(d.Root) || !validGoalBlobRef(d.SealRef, "goal-seal-") || d.ApprovalRevision == 0 || d.ApprovalRevision > d.Request.ExpectedRevision {
			return false
		}
		keys[d.Request.Key], ids[d.RunID] = true, true
		if !goalContinuationRowValid(rows, i) {
			return false
		}
		switch d.Phase {
		case "reserved", "starting", "failed":
			if d.PID != 0 || d.ProcessStart != "" || d.Job != "" {
				return false
			}
		case "running":
			if d.PID <= 1 || !goalBriefLine(d.ProcessStart) || d.ProcessStart == "" || d.Job != "" && !goalBriefLine(d.Job) {
				return false
			}
		default:
			return false
		}
		// This predicate includes strict approved-spec decoding. Do not parse
		// the same sealed spec again before evaluating its ordered binding.
		if !goalDispatchScopeValid(d) {
			return false
		}
		for _, path := range d.Writes {
			if !goalArtifactPath(path) {
				return false
			}
		}
	}
	return true
}
func goalDispatchScopeChanged(a, b string) bool {
	x, e1 := decodeNativeGoalSpec([]byte(a))
	y, e2 := decodeNativeGoalSpec([]byte(b))
	return e1 != nil || e2 != nil || goalScopeChanged(x, y)
}

// One predicate for the writer, low-level candidate commit and saved replay.
// Retained approval cannot invent, release or rebind a held launch.
func goalDispatchHistoryValid(old, next goalReplayRecord, a, b goalRecordData, seals [][]byte) bool {
	if goalContinuationChanged(a, b) {
		return goalContinuationHistoryValid(old, next, a, b, seals)
	}
	if !goalDispatchScopesValid(a) || !goalDispatchScopesValid(b) {
		return false
	}
	if len(a.Dispatches) == 0 && len(b.Dispatches) == 0 {
		return true
	}
	if len(b.Dispatches) < len(a.Dispatches) {
		return false
	}
	if reflect.DeepEqual(a.Dispatches, b.Dispatches) {
		if !reflect.DeepEqual(a.Attempt, b.Attempt) || a.Outcome != b.Outcome || a.State != b.State && len(a.Decisions) == len(b.Decisions) && reflect.DeepEqual(a.Closure, b.Closure) {
			return false
		}
		for _, d := range a.Dispatches {
			if d.Phase != "failed" && !goalAttemptReleased(a, d.RunID) && (old.sealRef != next.sealRef || goalDispatchScopeChanged(old.spec, next.spec) || a.ApprovalRevision != b.ApprovalRevision || a.GrantBinding != b.GrantBinding || a.State != b.State) {
				return false
			}
		}
		return true
	}
	if old.spec != next.spec || old.sealRef != next.sealRef || len(seals) != 1 || !reflect.DeepEqual(a.Notes, b.Notes) || !reflect.DeepEqual(a.Decisions, b.Decisions) || a.ApprovalRevision != b.ApprovalRevision || a.GrantBinding != b.GrantBinding || a.Outcome != b.Outcome || !reflect.DeepEqual(a.Closure, b.Closure) {
		return false
	}
	if len(b.Dispatches) == len(a.Dispatches)+1 {
		if len(a.Dispatches) > 0 && a.Dispatches[len(a.Dispatches)-1].Phase != "failed" {
			return false
		}
		if len(a.Dispatches) > 0 && !reflect.DeepEqual(a.Dispatches, b.Dispatches[:len(a.Dispatches)]) || a.State != nativeGoalApproved || b.State != a.State || a.Attempt != nil || b.Attempt != nil {
			return false
		}
		d := b.Dispatches[len(b.Dispatches)-1]
		if d.Phase != "reserved" || d.Request.ExpectedRevision != old.revision || d.SealRef != old.sealRef || d.ApprovalRevision != a.ApprovalRevision || len(a.Decisions) == 0 {
			return false
		}
		s, err := decodeNativeGoalSpec([]byte(d.Spec))
		if err != nil || s.ID != d.Request.GoalID || goalDispatchScopeChanged(old.spec, d.Spec) {
			return false
		}
		if s.OwnerHold != nil || s.Coverage != nil || s.LaneBinding == "required" && d.Request.Lane != s.PreferredLane {
			return false
		}
		if len(d.Writes) != len(s.Writes) {
			return false
		}
		decision := a.Decisions[len(a.Decisions)-1]
		checked := validateGoalBrief(goalReplayRecord{revision: d.ApprovalRevision, spec: d.Spec, sealRef: d.SealRef}, decision.Authority, seals[0])
		if decision.Action != "approve" || decision.Revision != d.ApprovalRevision || checked == nil || checked.SourceState != decision.SourceState {
			return false
		}
		seal, err := decodeGoalBrief(s, seals[0])
		if err != nil {
			return false
		}
		expected, _, err := expectedGoalDelivery(s, seal, goalDeliveryProfile{Engine: d.Delivery.Engine, CodePointCap: d.Delivery.CodePointCap, Root: d.Root})
		if err != nil {
			return false
		}
		// Replay retains holds even when artifact files are absent/damaged. Launch
		// separately rechecks exact retained inode identity before any OS effect.
		if expected.Mode == "pointer" {
			expected.Device, expected.Inode = d.Delivery.Device, d.Delivery.Inode
			if expected.Device == 0 || expected.Inode == 0 {
				return false
			}
		}
		return reflect.DeepEqual(expected, d.Delivery)
	}
	if len(b.Dispatches) != len(a.Dispatches) || len(a.Dispatches) == 0 {
		return false
	}
	i := len(a.Dispatches) - 1
	if !reflect.DeepEqual(a.Dispatches[:i], b.Dispatches[:i]) {
		return false
	}
	x, y := a.Dispatches[i], b.Dispatches[i]
	from, to := x.Phase, y.Phase
	x.Phase, y.Phase = "", ""
	x.PID, y.PID = 0, 0
	x.ProcessStart, y.ProcessStart = "", ""
	x.Job, y.Job = "", ""
	if !reflect.DeepEqual(x, y) {
		return false
	}
	if from == "reserved" && (to == "starting" || to == "failed") {
		return a.State == nativeGoalApproved && b.State == a.State && reflect.DeepEqual(a.Attempt, b.Attempt)
	}
	if from == "starting" && to == "running" {
		return a.State == nativeGoalApproved && b.State == nativeGoalRunning && a.Attempt == nil && reflect.DeepEqual(b.Attempt, &goalDecisionAttempt{ID: y.RunID})
	}
	return false
}
func goalDispatchOperation(key, phase string) string {
	return "dispatch-" + goalBriefDigest([]byte(key + "\x00" + phase))[:48]
}
func saveGoalDispatchLocked(root *os.File, old goalReplayRecord, data goalRecordData, fault goalCommitIO) (goalReplayRecord, error) {
	b, err := encodeGoalRecord(data)
	if err != nil {
		return old, err
	}
	d := data.Dispatches[len(data.Dispatches)-1]
	var total int64
	seal, err := readGoalBlob(root, old.sealRef, "goal-seal-", goalSealMaxBytes, &total)
	if err != nil {
		return old, err
	}
	req := goalCommitRequest{OperationID: goalDispatchOperation(d.Request.Key, d.Phase), ExpectedRevision: old.revision, Spec: []byte(old.spec), Seal: seal, Record: b, MutationSHA256: goalBriefDigest(b)}
	specRef := goalBlobName("goal-spec-", req.Spec)
	input := goalRecordFingerprint(goalInputFingerprint(d.Request.GoalID, old.revision, specRef, old.sealRef), goalBlobName("goal-record-", b), req.MutationSHA256)
	_, p, err := commitGoalLocked(root, req, d.Request.GoalID, specRef, old.sealRef, input, fault)
	if err != nil {
		return old, err
	}
	return p.records[d.Request.GoalID], nil
}
func (m *Manager) dispatchGoal(req goalDispatchRequest, ctx goalDispatchContext, fault goalCommitIO) (goalDispatchResult, error) {
	if !validGoalDispatchRequest(req) || !validNativeAuditSource(ctx.Source) || !goalArtifactPath(ctx.Profile.Root) {
		return goalDispatchResult{}, errGoalDispatch
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	guard, err := m.nativeDispatchTransactionLock()
	if err != nil {
		return goalDispatchResult{}, err
	}
	defer guard.Close()
	if m.closed || m.readOnly {
		return goalDispatchResult{}, errGoalDispatch
	}
	if err = m.reload(); err != nil {
		return goalDispatchResult{}, errGoalDispatch
	}
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		return goalDispatchResult{}, err
	}
	defer root.Close()
	p, err := replayGoalJournalRoot(root)
	if err != nil || p.tornTail {
		return goalDispatchResult{}, errGoalLaunchHeld
	}
	// Exact retry resolves before current revision CAS, across all goal IDs.
	for _, record := range p.records {
		data, e := goalRecordMetadata(record)
		if e != nil {
			return goalDispatchResult{}, errGoalLaunchHeld
		}
		for i, d := range data.Dispatches {
			if d.Request.Key == req.Key {
				if d.Request != req || d.Source != ctx.Source || d.Root != ctx.Profile.Root || d.Delivery.Engine != ctx.Profile.Engine || d.Delivery.CodePointCap != ctx.Profile.CodePointCap {
					return goalDispatchResult{}, errGoalDispatch
				}
				if _, err = mirrorNativeAuditLocked(root, ctx.Source, fault); err != nil {
					return goalDispatchResult{RunID: d.RunID, Phase: d.Phase}, errGoalLaunchHeld
				}
				if d.Phase == "failed" {
					return goalDispatchResult{RunID: d.RunID, Phase: d.Phase}, errGoalNotLaunched
				}
				return m.reconcileGoalDispatchLocked(root, record, data, i, ctx, fault)
			}
		}
	}
	old, exists := p.records[req.GoalID]
	if !exists || old.revision != req.ExpectedRevision {
		return goalDispatchResult{}, errGoalConflict
	}
	// Account for reservation, start intent and running receipt before effects.
	if p.sequence+3 > goalJournalMaxEvents || p.committedBytes+3*(goalJournalHeaderBytes+goalJournalMaxFrameBytes) > goalJournalMaxBytes {
		return goalDispatchResult{}, errGoalDispatch
	}
	selectedEnvironment := ctx.Environment
	selectedEnvironment.Capabilities = map[string]goalAdmissionCapability{}
	if capability, ok := ctx.Environment.Capabilities[req.Lane]; ok {
		selectedEnvironment.Capabilities[req.Lane] = capability
	}
	selection, err := m.admitGoalsLocked(selectedEnvironment)
	if err != nil {
		return goalDispatchResult{}, err
	}
	var admission goalAdmissionResult
	for _, candidate := range selection.Goals {
		if candidate.GoalID == req.GoalID {
			admission = candidate
		}
	}
	if !admission.Startable || admission.Lane != req.Lane {
		if admission.LaneFault != nil {
			fault := *admission.LaneFault
			fault.Blockers = make([]string, 0, len(admission.Blockers))
			for _, blocker := range admission.Blockers {
				fault.Blockers = append(fault.Blockers, blocker.Code)
			}
			return goalDispatchResult{}, &fault
		}
		reasons := []string{}
		for _, blocker := range admission.Blockers {
			reasons = append(reasons, blocker.Explanation)
		}
		if len(reasons) == 0 {
			reasons = append(reasons, "The requested lane is not eligible for this approved goal.")
		}
		return goalDispatchResult{}, errors.New("native goal cannot start: " + strings.Join(reasons, " "))
	}
	lane, err := m.lane(req.Lane)
	if err != nil || lane.Kind != string(ctx.Profile.Engine) {
		return goalDispatchResult{}, errGoalDispatch
	}
	lock, err := m.lock(req.Lane)
	if err != nil {
		return goalDispatchResult{}, errGoalDispatch
	}
	keep := false
	defer func() {
		if !keep {
			lock.Close()
		}
	}()
	data, err := goalRecordMetadata(old)
	if err != nil || len(data.Dispatches) >= 8 {
		return goalDispatchResult{}, errGoalDispatch
	}
	spec, _ := p.spec(req.GoalID)
	seal, ok := goalAdmissionApprovedSeal(root, old, spec, data)
	if !ok {
		return goalDispatchResult{}, errGoalDispatch
	}
	var total int64
	approvedBytes, err := readGoalBlob(root, seal.SpecRef, "goal-spec-", nativeGoalMaxBytes, &total)
	if err != nil {
		return goalDispatchResult{}, err
	}
	approved, err := decodeNativeGoalSpec(approvedBytes)
	if err != nil {
		return goalDispatchResult{}, err
	}
	delivery, err := deliverGoalBrief(approved, seal, ctx.Profile)
	if err != nil {
		return goalDispatchResult{}, err
	}
	random := make([]byte, 16)
	if _, err = rand.Read(random); err != nil {
		return goalDispatchResult{}, errGoalDispatch
	}
	d := goalDispatchRecord{Request: req, RunID: hex.EncodeToString(random), Phase: "reserved", Source: ctx.Source, Root: ctx.Profile.Root, Spec: string(approvedBytes), SealRef: old.sealRef, ApprovalRevision: data.ApprovalRevision, Delivery: delivery}
	d.Writes, _, d.Scope, ok = goalCanonicalScope(spec.Writes, ctx.Environment.Roots)
	if !ok {
		return goalDispatchResult{}, errGoalDispatch
	}
	data.Dispatches = append(data.Dispatches, d)
	// Bound the largest phase payload now; a child must not discover that its
	// required identity/publication no longer fits the record limit.
	capacity := data
	capacity.Dispatches = append([]goalDispatchRecord(nil), data.Dispatches...)
	capacity.Dispatches[len(capacity.Dispatches)-1].Phase = "running"
	capacity.Dispatches[len(capacity.Dispatches)-1].PID = int(^uint(0) >> 1)
	capacity.Dispatches[len(capacity.Dispatches)-1].ProcessStart = strings.Repeat("x", 128)
	capacity.Dispatches[len(capacity.Dispatches)-1].Job = strings.Repeat("x", 128)
	capacity.State, capacity.Attempt = nativeGoalRunning, &goalDecisionAttempt{ID: d.RunID}
	if _, err := encodeGoalRecord(capacity); err != nil {
		return goalDispatchResult{}, err
	}
	old, err = saveGoalDispatchLocked(root, old, data, fault)
	if err != nil {
		return goalDispatchResult{RunID: d.RunID, Phase: "reserved"}, err
	}
	if _, err = mirrorNativeAuditLocked(root, ctx.Source, fault); err != nil {
		return goalDispatchResult{RunID: d.RunID, Phase: "reserved"}, errGoalLaunchHeld
	}
	launch := &goalLaunchContext{id: d.RunID, fault: fault, beforeStart: func() error {
		if m.reload() != nil {
			return errGoalDispatch
		}
		currentLane, err := m.lane(req.Lane)
		if err != nil || !reflect.DeepEqual(currentLane, lane) {
			return errGoalDispatch
		}
		if err := m.admitLaneLocked(req.Lane); err != nil {
			return err
		}
		if code := goalAdmissionSourceState(spec, seal, ctx.Environment.SourceState); code != "" {
			return errGoalDispatch
		}
		writes, repos, scope, ok := goalCanonicalScope(spec.Writes, ctx.Environment.Roots)
		if !ok || !reflect.DeepEqual(writes, d.Writes) || !reflect.DeepEqual(scope, d.Scope) {
			return errGoalDispatch
		}
		if len(repos) > 0 {
			claims := goalAdmissionResult{}
			goalAdmissionClaims(&claims, repos, ctx.Environment.Claims)
			if len(claims.Blockers) > 0 {
				return errGoalDispatch
			}
		}
		if err := validateGoalDelivery(approved, seal, ctx.Profile, delivery); err != nil {
			return err
		}
		data.Dispatches[len(data.Dispatches)-1].Phase = "starting"
		var e error
		old, e = saveGoalDispatchLocked(root, old, data, fault)
		if e != nil {
			return e
		}
		_, e = mirrorNativeAuditLocked(root, ctx.Source, fault)
		return e
	}}
	id, launchErr := m.launchMessage(lane, req.GoalID, delivery.Prompt, "", lock, nil, nil, nil, nil, launch)
	keep = launch.supervised
	if launchErr != nil {
		// Error strings and observed PID absence never prove nonlaunch.
		fresh, e := replayGoalJournalRoot(root)
		if e != nil || fresh.tornTail {
			return goalDispatchResult{RunID: d.RunID, Phase: "starting"}, errGoalLaunchHeld
		}
		old = fresh.records[req.GoalID]
		data, e = goalRecordMetadata(old)
		if e != nil {
			return goalDispatchResult{RunID: d.RunID}, errGoalLaunchHeld
		}
		last := len(data.Dispatches) - 1
		if id != "" && !keep {
			if child := m.goalDispatchChild(data.Dispatches[last]); child != nil {
				keep = m.superviseGoalDispatch(*child, lock) == nil
			}
		}
		if data.Dispatches[last].Phase == "reserved" {
			// A published preparation has no child: the durable phase is still
			// reserved, and the launcher cannot invoke the OS in that phase.
			dir := filepath.Join(m.store, "runs", d.RunID)
			var prepared *Run
			var readErr error
			if _, statErr := os.Lstat(dir); statErr == nil {
				prepared, readErr = loadRun(dir, d.RunID)
			} else if !os.IsNotExist(statErr) {
				readErr = statErr
			}
			if readErr != nil {
				return goalDispatchResult{RunID: d.RunID, Phase: "reserved"}, errGoalLaunchHeld
			} else if prepared != nil {
				if prepared.PID != 0 || prepared.ProcessStart != "" {
					return goalDispatchResult{RunID: d.RunID, Phase: "reserved"}, errGoalLaunchHeld
				}
				now := time.Now().UTC()
				prepared.State, prepared.StopReason, prepared.Ended = "ended", "launch_not_started", &now
				if m.save(prepared) != nil {
					return goalDispatchResult{RunID: d.RunID, Phase: "reserved"}, errGoalLaunchHeld
				}
			}
			data.Dispatches[last].Phase = "failed"
			if _, e = saveGoalDispatchLocked(root, old, data, goalCommitIO{}); e == nil {
				_, e = mirrorNativeAuditLocked(root, ctx.Source, fault)
			}
			if e == nil {
				return goalDispatchResult{RunID: d.RunID, Phase: "failed"}, errGoalNotLaunched
			}
		}
		return goalDispatchResult{RunID: d.RunID, Phase: "starting"}, errGoalLaunchHeld
	}
	return m.reconcileGoalDispatchLocked(root, old, data, len(data.Dispatches)-1, ctx, fault)
}

// Native operations serialize exact receipts. Reuse the existing validated
// control inode without creating or replacing it. Public controls retain their
// existing nonblocking policy.
// A local publisher can outlive one short acquisition while finishing a child
// receipt. Wait for its bounded handoff, then replay under the real guard; never
// infer a missing receipt or allocate another child from an unlocked snapshot.
// External holders and unavailable stores retain the short, fail-closed refusal.
func (m *Manager) nativeDispatchTransactionLock() (*controlFileLock, error) {
	ctx, cancel := context.WithTimeout(m.owningContext(), 2*time.Second)
	defer cancel()
	deadline := time.Now().Add(2 * time.Second)
	for {
		wait := min(controlLockWait, max(0, time.Until(deadline)))
		guard, err := m.nativeDispatchControlLockWithin(wait, ctx)
		if err == nil {
			return guard, nil
		}
		var busy *laneControlFault
		if !errors.As(err, &busy) || busy.Code != "locked" || !busy.local || ctx.Err() != nil || time.Until(deadline) <= 0 {
			return nil, err
		}
	}
}

func (m *Manager) nativeDispatchControlLock() (*controlFileLock, error) {
	return m.nativeDispatchControlLockWithin(controlLockWait)
}

func (m *Manager) nativeDispatchControlLockWithin(wait time.Duration, contexts ...context.Context) (*controlFileLock, error) {
	ctx := m.owningContext()
	if len(contexts) != 0 {
		ctx = contexts[0]
	}
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		return nil, errLaneControlUnavailable
	}
	defer root.Close()
	return acquireControlLock(root, unix.LOCK_EX, false, wait, ctx)
}

type goalLaunchContext struct {
	id           string
	fault        goalCommitIO
	beforeStart  func() error
	supervised   bool
	continuation *goalContinuationBinding
}

func (m *Manager) reconcileGoalDispatchLocked(root *os.File, old goalReplayRecord, data goalRecordData, index int, ctx goalDispatchContext, fault goalCommitIO) (goalDispatchResult, error) {
	d := data.Dispatches[index]
	result := goalDispatchResult{RunID: d.RunID, Phase: d.Phase}
	spec, err := decodeNativeGoalSpec([]byte(d.Spec))
	if err != nil {
		return result, errGoalLaunchHeld
	}
	for _, end := range data.Ends {
		if end.RunID == d.RunID {
			result.Phase = "ended"
			return result, nil
		}
	}
	var total int64
	sealBytes, err := readGoalBlob(root, d.SealRef, "goal-seal-", goalSealMaxBytes, &total)
	if err != nil {
		return result, errGoalLaunchHeld
	}
	seal, err := decodeGoalBrief(spec, sealBytes)
	if err != nil || validateGoalDelivery(spec, seal, ctx.Profile, d.Delivery) != nil {
		return result, errGoalLaunchHeld
	}
	if d.Phase == "reserved" {
		return result, errGoalLaunchHeld
	}
	if d.Phase == "running" {
		if fault.check("before_success") != nil {
			return result, errGoalLaunchHeld
		}
		return result, nil
	}
	r := m.goalDispatchChild(d)
	if r == nil {
		if ended := m.goalAttemptRun(d); ended != nil && goalAttemptDefinitelyGone(*ended) {
			var err error
			if ended.Ended == nil {
				err = m.recoverEnded(ended)
			} else {
				err = m.publishGoalAttemptEndLocked(ended, fault)
			}
			if err == nil {
				result.Phase = "ended"
				return result, nil
			}
		}
		return result, errGoalLaunchHeld
	}
	if m.superviseGoalDispatch(*r, nil) != nil {
		return result, errGoalLaunchHeld
	}
	data.Dispatches[index].Phase = "running"
	data.Dispatches[index].PID = r.PID
	data.Dispatches[index].ProcessStart = r.ProcessStart
	data.Dispatches[index].Job = r.LaunchdLabel
	data.State = nativeGoalRunning
	if d.Continuation != nil {
		data.Outcome = ""
	}
	data.Attempt = &goalDecisionAttempt{ID: r.ID}
	if fault.check("running_publication") != nil {
		return result, errGoalLaunchHeld
	}
	if _, err = saveGoalDispatchLocked(root, old, data, fault); err != nil {
		return result, errGoalLaunchHeld
	}
	if _, err = mirrorNativeAuditLocked(root, ctx.Source, fault); err != nil {
		return result, errGoalLaunchHeld
	}
	result.Phase = "running"
	if fault.check("before_success") != nil {
		return result, errGoalLaunchHeld
	}
	return result, nil
}

func (m *Manager) goalDispatchChild(d goalDispatchRecord) *Run {
	r := m.goalAttemptRun(d)
	if r == nil || !sameProcess(*r) {
		return nil
	}
	if r.LaunchdLabel != "" && !r.LaunchFallback {
		if r.LaunchdLabel != "nuntius.run."+r.ID || m.launcher == nil {
			return nil
		}
		output, err := m.launcher.call("print", m.launcher.domain+"/"+r.LaunchdLabel)
		if err != nil {
			return nil
		}
		identified := false
		for _, line := range strings.Split(string(output), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "pid = ") {
				pid, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "pid = ")))
				if err != nil || pid != r.PID {
					return nil
				}
				identified = true
			}
		}
		if !identified {
			return nil
		}
	}
	return r
}

func (m *Manager) superviseGoalDispatch(r Run, laneLock *os.File) error {
	if m.active[r.Lane] != nil {
		return nil
	}
	supervisor, err := m.lock("supervisor-" + r.ID)
	if errors.Is(err, errBusy) {
		return nil
	}
	if err != nil {
		return err
	}
	a := &activeRun{done: make(chan struct{}), stop: make(chan string, 1)}
	m.active[r.Lane] = a
	go m.follow(r, laneLock, supervisor, a, m.Grace)
	return nil
}

func goalDispatchPopulationValid(p *goalProjection) bool {
	keys, ids, lanes := map[string]bool{}, map[string]bool{}, map[string]bool{}
	var held []goalDispatchRecord
	for _, r := range p.records {
		data, err := goalRecordMetadata(r)
		if err != nil {
			return false
		}
		for _, d := range data.Dispatches {
			if !goalDispatchScopeValid(d) {
				return false
			}
			if keys[d.Request.Key] || ids[d.RunID] {
				return false
			}
			keys[d.Request.Key], ids[d.RunID] = true, true
			if d.Phase != "failed" && !goalAttemptReleased(data, d.RunID) {
				if lanes[d.Request.Lane] {
					return false
				}
				lanes[d.Request.Lane] = true
				spec, _ := decodeNativeGoalSpec([]byte(d.Spec))
				for _, other := range held {
					if goalAdmissionOverlaps(d.Writes, other.Writes) {
						return false
					}
					previous, _ := decodeNativeGoalSpec([]byte(other.Spec))
					for _, resource := range spec.Resources {
						for _, occupied := range previous.Resources {
							if resource == occupied {
								return false
							}
						}
					}
				}
				held = append(held, d)
			}
		}
	}
	return true
}
func (m *Manager) nativeGoalLocked(id string) (bool, error) {
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		return false, errGoalLaunchHeld
	}
	defer root.Close()
	p, err := replayGoalJournalRoot(root)
	if err != nil || p.tornTail {
		return false, errGoalLaunchHeld
	}
	_, exists := p.records[id]
	return exists, nil
}

// Status may observe native partial runs, but legacy adoption cannot guess
// identity or supervise a child outside its journal-bound launch.
func (m *Manager) nativeSupervisionAllowed(r Run) bool {
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		return false
	}
	defer root.Close()
	p, err := replayGoalJournalRoot(root)
	if err != nil {
		return false
	}
	record, exists := p.records[r.GoalID]
	if !exists {
		return true
	}
	data, err := goalRecordMetadata(record)
	if err != nil {
		return false
	}
	for _, d := range data.Dispatches {
		if d.RunID != r.ID || d.Phase != "starting" && d.Phase != "running" {
			continue
		}
		if m.goalAttemptRun(d) == nil {
			return false
		}
		if d.Phase == "running" && (r.PID != d.PID || r.ProcessStart != d.ProcessStart || r.LaunchdLabel != d.Job) {
			return false
		}
		if d.Phase == "running" && !sameProcess(r) {
			return goalAttemptDefinitelyGone(r)
		}
		return m.goalDispatchChild(d) != nil
	}
	return false
}
