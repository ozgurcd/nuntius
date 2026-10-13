package nuntius

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

// Execution input is private run storage. The durable intent retains only
// safe bindings; Delivery remains the original approved brief throughout.
type goalContinuationBinding struct {
	Parent          string `json:"parent"`
	Branch          string `json:"branch"`
	InputSHA256     string `json:"input_sha256"`
	SessionSHA256   string `json:"session_sha256"`
	RecoveryCount   int    `json:"recovery_count"`
	MessageSHA256   string `json:"message_sha256,omitempty"`
	MessageRevision uint64 `json:"message_revision,omitempty"`
}

type goalContinuationSettlement struct {
	Parent          string `json:"parent"`
	MessageSHA256   string `json:"message_sha256"`
	MessageRevision uint64 `json:"message_revision"`
}

type goalPendingMessageBinding struct {
	Digest        string `json:"digest"`
	SessionSHA256 string `json:"session_sha256"`
	Revision      uint64 `json:"revision"`
}

func (m *Manager) goalPendingMessageLocked(root *os.File, r Run, text string) (*goalPendingMessageBinding, error) {
	binding := &goalPendingMessageBinding{Digest: goalBriefDigest([]byte(text)), SessionSHA256: goalBriefDigest([]byte(r.SessionID))}
	err := visitLaneMessages(root, r.Lane, func(dir *os.File, original Run, b []byte) error {
		if original.ID != r.ID {
			return nil
		}
		holdBytes, e := readLaneControlPrivate(dir, "message-hold.json", 4096)
		if os.IsNotExist(e) {
			return nil
		}
		var h heldMessage
		if e != nil || decodeLaneControlObject(holdBytes, &h) != nil || !validMessageBinding(h, original) {
			return errGoalEndPending
		}
		pending, e := pendingText(b)
		if e != nil || laneControlDigest([]byte(pending)) != h.ContentSHA256 {
			return errGoalEndPending
		}
		binding.Digest, binding.SessionSHA256, binding.Revision = h.ContentSHA256, goalBriefDigest([]byte(h.SessionID)), h.Revision
		return nil
	})
	if err != nil {
		return nil, err
	}
	return binding, nil
}

func continuationDigest(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 32 && hex.EncodeToString(b) == s
}

func goalContinuationRowValid(rows []goalDispatchRecord, i int) bool {
	d := rows[i]
	if i+1 < len(rows) && d.Phase != "failed" && (d.Phase != "running" && d.Phase != "starting" || rows[i+1].Continuation == nil || rows[i+1].Continuation.Parent != d.RunID) {
		return false
	}
	c := d.Continuation
	if c == nil {
		return true
	}
	if i == 0 || rows[i-1].RunID != c.Parent || rows[i-1].Phase != "running" && rows[i-1].Phase != "starting" || !runPattern.MatchString(c.Parent) || c.Parent == d.RunID || !continuationDigest(c.InputSHA256) || !continuationDigest(c.SessionSHA256) || c.RecoveryCount < 0 || c.RecoveryCount > 2 {
		return false
	}
	count := 0
	if rows[i-1].Continuation != nil {
		count = rows[i-1].Continuation.RecoveryCount
	}
	switch c.Branch {
	case "message":
		if !continuationDigest(c.MessageSHA256) || c.MessageSHA256 != c.InputSHA256 || c.MessageRevision == 0 || c.RecoveryCount != count {
			return false
		}
	case "acknowledgement", "reply":
		if c.MessageSHA256 != "" || c.MessageRevision != 0 || c.RecoveryCount != count {
			return false
		}
	case "recovery":
		if c.MessageSHA256 != "" || c.MessageRevision != 0 || c.RecoveryCount != count+1 {
			return false
		}
	default:
		return false
	}
	x, y := rows[i-1], d
	x.Request.Key, y.Request.Key = "", ""
	x.Request.ExpectedRevision, y.Request.ExpectedRevision = 0, 0
	x.RunID, y.RunID, x.Phase, y.Phase = "", "", "", ""
	x.PID, y.PID, x.ProcessStart, y.ProcessStart, x.Job, y.Job = 0, 0, "", "", "", ""
	x.Continuation, y.Continuation = nil, nil
	return reflect.DeepEqual(x, y)
}

func validGoalSettlementsJSON(m map[string]any) bool {
	v, ok := m["settlements"]
	if !ok {
		return true
	}
	b, e := json.Marshal(v)
	var rows []goalContinuationSettlement
	if e != nil || !goalDispatchCanonical(b, &rows, goalRecordMaxBytes) || len(rows) == 0 || len(rows) > 8 {
		return false
	}
	seen := map[string]bool{}
	for _, s := range rows {
		if !runPattern.MatchString(s.Parent) || seen[s.Parent] || !continuationDigest(s.MessageSHA256) || s.MessageRevision == 0 {
			return false
		}
		seen[s.Parent] = true
	}
	return true
}

func goalContinuationChanged(a, b goalRecordData) bool {
	if reflect.DeepEqual(a.Dispatches, b.Dispatches) {
		return false
	}
	return len(b.Dispatches) > 0 && b.Dispatches[len(b.Dispatches)-1].Continuation != nil
}

// The writer commits through this exact complete-result predicate, as does
// independent journal replay. No history, approval or unrelated field changes.
func goalContinuationHistoryValid(old, next goalReplayRecord, a, b goalRecordData, seals [][]byte) bool {
	if old.spec != next.spec || old.sealRef != next.sealRef || !goalDispatchScopesValid(a) || !goalDispatchScopesValid(b) || len(seals) != 1 || len(a.Dispatches) == 0 || goalRecordTerminal(a.State) {
		return false
	}
	parent := a.Dispatches[len(a.Dispatches)-1]
	spec, err := decodeNativeGoalSpec([]byte(parent.Spec))
	if err != nil || len(a.Decisions) == 0 {
		return false
	}
	decision := a.Decisions[len(a.Decisions)-1]
	checked := validateGoalBrief(goalReplayRecord{revision: a.ApprovalRevision, spec: parent.Spec, sealRef: parent.SealRef}, decision.Authority, seals[0])
	if decision.Action != "approve" || decision.Revision != a.ApprovalRevision || checked == nil || checked.SourceState != decision.SourceState || spec.ID != parent.Request.GoalID || parent.SealRef != old.sealRef {
		return false
	}
	expected := a
	if len(b.Settlements) == len(a.Settlements)+1 {
		s := b.Settlements[len(a.Settlements)]
		if len(a.Settlements) > 0 && !reflect.DeepEqual(a.Settlements, b.Settlements[:len(a.Settlements)]) || a.State != nativeGoalEnded || a.Attempt == nil || a.Attempt.ID != parent.RunID || !a.Attempt.Ended || !a.Attempt.ActiveSuccessor || s.Parent != parent.RunID {
			return false
		}
		found := false
		for _, e := range a.Ends {
			if e.RunID == s.Parent && e.Held && e.Pending == "message_pending" && e.Message != nil && e.Message.Digest == s.MessageSHA256 && (e.Message.Revision == 0 || e.Message.Revision == s.MessageRevision) {
				found = true
			}
		}
		if !found {
			return false
		}
		expected.Settlements = b.Settlements
		attempt := *a.Attempt
		attempt.ActiveSuccessor = false
		expected.Attempt = &attempt
		return reflect.DeepEqual(expected, b)
	}
	if len(b.Dispatches) == len(a.Dispatches)+1 {
		d := b.Dispatches[len(a.Dispatches)]
		if !reflect.DeepEqual(a.Dispatches, b.Dispatches[:len(a.Dispatches)]) || !goalContinuationRowValid(b.Dispatches, len(a.Dispatches)) || d.Phase != "reserved" || d.Request.ExpectedRevision != old.revision || a.State != nativeGoalEnded || a.Attempt == nil || a.Attempt.ID != parent.RunID || !a.Attempt.Ended {
			return false
		}
		pending := ""
		held := false
		var accepted *goalPendingMessageBinding
		for _, e := range a.Ends {
			if e.RunID == parent.RunID {
				pending, held = e.Pending, e.Held
				accepted = e.Message
			}
		}
		c := d.Continuation
		if c == nil || c.Parent != parent.RunID {
			return false
		}
		if c.Branch == "reply" {
			if !goalAttemptReleased(a, parent.RunID) || a.Attempt.ActiveSuccessor {
				return false
			}
		} else if !held || !a.Attempt.ActiveSuccessor || pending != c.Branch+"_pending" {
			return false
		}
		if c.Branch == "message" && (accepted == nil || accepted.Digest != c.MessageSHA256 || accepted.SessionSHA256 != c.SessionSHA256 || accepted.Revision != 0 && accepted.Revision != c.MessageRevision) {
			return false
		}
		expected.Dispatches = b.Dispatches
		attempt := *a.Attempt
		attempt.ActiveSuccessor = true
		expected.Attempt = &attempt
		return reflect.DeepEqual(expected, b)
	}
	if len(b.Dispatches) != len(a.Dispatches) || parent.Continuation == nil {
		return false
	}
	i := len(a.Dispatches) - 1
	if !reflect.DeepEqual(a.Dispatches[:i], b.Dispatches[:i]) {
		return false
	}
	x, y := parent, b.Dispatches[i]
	from, to := x.Phase, y.Phase
	x.Phase, y.Phase, x.PID, y.PID, x.ProcessStart, y.ProcessStart, x.Job, y.Job = "", "", 0, 0, "", "", "", ""
	if !reflect.DeepEqual(x, y) {
		return false
	}
	expected.Dispatches = b.Dispatches
	if from == "reserved" && to == "starting" {
		return reflect.DeepEqual(expected, b)
	}
	if from == "starting" && to == "running" {
		expected.State, expected.Outcome = nativeGoalRunning, ""
		expected.Attempt = &goalDecisionAttempt{ID: parent.RunID}
		return reflect.DeepEqual(expected, b)
	}
	return false // uncertain child is never declared failed from PID absence
}

func goalDispatchRunMatches(d goalDispatchRecord, r Run) bool {
	if r.Integrity == "" || r.ID != d.RunID || r.GoalID != d.Request.GoalID || r.Lane != d.Request.Lane || r.Kind != string(d.Delivery.Engine) || r.Brief != d.Delivery.Prompt {
		return false
	}
	if d.Continuation == nil {
		return r.ParentRunID == "" && r.SHA256 == d.Delivery.PromptSHA256
	}
	c := d.Continuation
	return r.ParentRunID == c.Parent && r.SHA256 == c.InputSHA256 && r.NativeSessionSHA256 == c.SessionSHA256 && r.ResumeCount == c.RecoveryCount && r.RecoveryRun == (c.Branch == "recovery") && (r.Resumed || c.Branch == "recovery")
}

// Selection and the final guard both invoke the adapter. Its observation time
// must belong to this invocation; a cached context cannot satisfy freshness.
func (m *Manager) freshGoalContinuationLocked(root *os.File, old goalReplayRecord, data goalRecordData, parent goalDispatchRecord, branch string, lane Lane, reviewed string) (goalDispatchContext, error) {
	var ctx goalDispatchContext
	if m.continuationResolver == nil || m.closed || m.readOnly || m.reload() != nil || m.checkFrozen() != nil || m.admitLaneExceptLocked(lane.Name, reviewed) != nil {
		return ctx, errGoalDispatch
	}
	current, err := m.lane(lane.Name)
	if err != nil || !reflect.DeepEqual(current, lane) {
		return ctx, errGoalDispatch
	}
	query := goalContinuationQuery{GoalID: parent.Request.GoalID, Lane: lane.Name, ParentID: parent.RunID, Branch: branch, ObservedAfter: time.Now().UTC()}
	fresh, err := m.continuationResolver(query)
	now := time.Now().UTC()
	if err != nil || fresh.ObservedAt.Before(query.ObservedAfter) || fresh.ObservedAt.After(now) || now.Sub(fresh.ObservedAt) > 30*time.Second {
		return ctx, errGoalDispatch
	}
	ctx = fresh.Dispatch
	if ctx.Source != parent.Source || ctx.Profile.Root != parent.Root || ctx.Profile.Engine != parent.Delivery.Engine || ctx.Profile.CodePointCap != parent.Delivery.CodePointCap {
		return ctx, errGoalDispatch
	}
	spec, err := decodeNativeGoalSpec([]byte(parent.Spec))
	if err != nil {
		return ctx, errGoalDispatch
	}
	approvedData := data
	approvedData.State = nativeGoalApproved
	seal, ok := goalAdmissionApprovedSeal(root, old, spec, approvedData)
	if !ok || goalAdmissionSourceState(spec, seal, ctx.Environment.SourceState) != "" {
		return ctx, errGoalDispatch
	}
	capability, ok := ctx.Environment.Capabilities[lane.Name]
	eligible := false
	for _, kind := range capability.WorkKinds {
		if kind == spec.WorkKind {
			eligible = true
		}
	}
	if !ok || capability.Kind != nativeEngineKind(lane.Kind) || !eligible {
		return ctx, errGoalDispatch
	}
	writes, repos, scope, ok := goalCanonicalScope(spec.Writes, ctx.Environment.Roots)
	if !ok || !reflect.DeepEqual(writes, parent.Writes) || !reflect.DeepEqual(scope, parent.Scope) || validateGoalDelivery(spec, seal, ctx.Profile, parent.Delivery) != nil {
		return ctx, errGoalDispatch
	}
	if len(repos) > 0 {
		claims := goalAdmissionResult{}
		observe := ctx.Environment.Claims
		if observe == nil {
			return ctx, errGoalDispatch
		}
		goalAdmissionClaims(&claims, repos, func(roots []string) ([]goalAdmissionClaim, error) {
			rows, e := observe(roots)
			for _, row := range rows {
				if row.ObservedAt.Before(query.ObservedAfter) {
					return nil, errGoalDispatch
				}
			}
			return rows, e
		})
		if len(claims.Blockers) != 0 {
			return ctx, errGoalDispatch
		}
	}
	p, err := replayGoalJournalRoot(root)
	if err != nil || p.tornTail || !goalDispatchPopulationValid(p) {
		return ctx, errGoalLaunchHeld
	}
	runs, err := m.history()
	if err != nil {
		return ctx, errGoalDispatch
	}
	reservations, unknown := goalAdmissionReservations(p, runs, root, ctx.Environment)
	if unknown {
		return ctx, errGoalDispatch
	}
	for _, occupied := range reservations {
		if occupied.id == spec.ID {
			continue
		} // replay proves this one chain
		if goalAdmissionOverlaps(writes, occupied.writes) {
			return ctx, errGoalDispatch
		}
		for _, x := range spec.Resources {
			for _, y := range occupied.resources {
				if x == y {
					return ctx, errGoalDispatch
				}
			}
		}
	}
	for _, r := range runs {
		if r.Lane == lane.Name && r.ID != parent.RunID && r.State == "running" && (sameProcess(r) || r.ProcessStart == "" && processExists(r.PID)) {
			return ctx, errGoalDispatch
		}
	}
	if _, err := mirrorNativeAuditLocked(root, ctx.Source, goalCommitIO{}); err != nil {
		return ctx, errGoalLaunchHeld
	}
	return ctx, nil
}

func (m *Manager) launchGoalContinuationLocked(lane Lane, text, session string, lock *os.File, parent *Run, message *heldMessage, ops *laneControlIO, started *bool, fault goalCommitIO, branch string) (string, error) {
	if parent == nil || len(text) == 0 || len(text) > 12000 || !goalAttemptDefinitelyGone(*parent) {
		return "", errGoalDispatch
	}
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		return "", err
	}
	defer root.Close()
	p, err := replayGoalJournalRoot(root)
	if err != nil || p.tornTail {
		return "", errGoalLaunchHeld
	}
	old, ok := p.records[parent.GoalID]
	if !ok {
		return "", errGoalDispatch
	}
	data, err := goalRecordMetadata(old)
	if err != nil || len(data.Dispatches) == 0 {
		return "", errGoalDispatch
	}
	for i, d := range data.Dispatches {
		if d.Continuation != nil && d.Continuation.Parent == parent.ID {
			if d.Continuation.InputSHA256 != goalBriefDigest([]byte(text)) || d.Continuation.SessionSHA256 != goalBriefDigest([]byte(session)) {
				return "", errGoalDispatch
			}
			result, e := m.reconcileGoalDispatchLocked(root, old, data, i, goalDispatchContext{Profile: goalDeliveryProfile{Engine: d.Delivery.Engine, CodePointCap: d.Delivery.CodePointCap, Root: d.Root}, Source: d.Source}, fault)
			return result.RunID, e
		}
	}
	previous := data.Dispatches[len(data.Dispatches)-1]
	if previous.RunID != parent.ID || m.goalAttemptRun(previous) == nil {
		return "", errGoalDispatch
	}
	if branch == "" {
		if message != nil {
			branch = "message"
		} else if parent.NativeEnd != nil {
			branch = strings.TrimSuffix(parent.NativeEnd.Pending, "_pending")
		}
	}
	if branch != "recovery" && !sessionPattern.MatchString(session) || branch == "recovery" && (parent.ResumeCount >= 2 || m.reportExists(*parent) || parent.StopAt != nil || m.config.AutoResume != nil && !*m.config.AutoResume) {
		return "", errGoalDispatch
	}
	reviewed := ""
	if message != nil {
		reviewed = parent.ID
	}
	ctx, err := m.freshGoalContinuationLocked(root, old, data, previous, branch, lane, reviewed)
	if err != nil {
		return "", err
	}
	// Reserve the full bounded lifecycle, including actual end and later report.
	if len(data.Dispatches) >= 8 || len(data.Ends) >= 8 || len(data.Reports) >= 8 || p.sequence+6 > goalJournalMaxEvents || p.committedBytes+6*(goalJournalHeaderBytes+goalJournalMaxFrameBytes) > goalJournalMaxBytes {
		return "", errGoalDispatch
	}
	random := make([]byte, 16)
	if _, err = rand.Read(random); err != nil {
		return "", errGoalDispatch
	}
	d := previous
	d.RunID, d.Phase, d.PID, d.ProcessStart, d.Job = hex.EncodeToString(random), "reserved", 0, "", ""
	d.Request.ExpectedRevision, d.Request.Key = old.revision, "continue-"+parent.ID
	c := &goalContinuationBinding{Parent: parent.ID, Branch: branch, InputSHA256: goalBriefDigest([]byte(text)), SessionSHA256: goalBriefDigest([]byte(session)), RecoveryCount: parent.ResumeCount}
	if branch == "recovery" {
		c.RecoveryCount++
	}
	if message != nil {
		c.MessageSHA256, c.MessageRevision = message.ContentSHA256, message.Revision
	}
	d.Continuation = c
	data.Dispatches = append(append([]goalDispatchRecord(nil), data.Dispatches...), d)
	attempt := *data.Attempt
	attempt.ActiveSuccessor = true
	data.Attempt = &attempt
	capacity := data
	capacity.Dispatches = append([]goalDispatchRecord(nil), data.Dispatches...)
	last := len(capacity.Dispatches) - 1
	capacity.Dispatches[last].Phase, capacity.Dispatches[last].PID, capacity.Dispatches[last].ProcessStart, capacity.Dispatches[last].Job = "running", int(^uint(0)>>1), strings.Repeat("x", 128), strings.Repeat("x", 128)
	capacity.State, capacity.Outcome, capacity.Attempt = nativeGoalRunning, "", &goalDecisionAttempt{ID: d.RunID}
	stamp := time.Now().UTC()
	end := goalAttemptEnd{RunID: d.RunID, Lane: lane.Name, PID: int(^uint(0) >> 1), ProcessStart: strings.Repeat("x", 128), Job: strings.Repeat("x", 128), Started: stamp, Ended: stamp, Outcome: nativeOutcomeUnknownExit, Held: true, Pending: "recovery_pending"}
	capacity.Ends = append(append([]goalAttemptEnd(nil), data.Ends...), end)
	capacity.State, capacity.Outcome, capacity.Attempt = nativeGoalEnded, end.Outcome, &goalDecisionAttempt{ID: d.RunID, Ended: true, ActiveSuccessor: true, ReportDigest: strings.Repeat("f", 64)}
	capacity.Reports = append(append([]goalAttemptReport(nil), data.Reports...), goalAttemptReport{RunID: d.RunID, Digest: strings.Repeat("f", 64), Bytes: 65536, Modified: stamp})
	if _, err = encodeGoalRecord(capacity); err != nil {
		return "", err
	}
	old, err = saveGoalDispatchLocked(root, old, data, fault)
	if err != nil {
		return "", err
	}
	if _, err = mirrorNativeAuditLocked(root, ctx.Source, fault); err != nil {
		return "", errGoalLaunchHeld
	}
	launch := &goalLaunchContext{id: d.RunID, fault: fault, continuation: c}
	launch.beforeStart = func() error {
		if _, e := m.freshGoalContinuationLocked(root, old, data, previous, branch, lane, reviewed); e != nil {
			return e
		}
		data.Dispatches[len(data.Dispatches)-1].Phase = "starting"
		var e error
		old, e = saveGoalDispatchLocked(root, old, data, fault)
		if e != nil {
			return e
		}
		_, e = mirrorNativeAuditLocked(root, ctx.Source, fault)
		return e
	}
	id, launchErr := m.launchMessage(lane, parent.GoalID, text, session, lock, parent, message, ops, started, launch)
	if launchErr != nil {
		fresh, e := replayGoalJournalRoot(root)
		if e == nil && !fresh.tornTail {
			record := fresh.records[parent.GoalID]
			retained, e := goalRecordMetadata(record)
			if e == nil && len(retained.Dispatches) > 0 {
				candidate := retained.Dispatches[len(retained.Dispatches)-1]
				if child := m.goalDispatchChild(candidate); child != nil {
					_ = m.superviseGoalDispatch(*child, lock)
				}
			}
		}
		// Preparation uncertainty retains the same allocated intent. A retry
		// adopts or holds it and never allocates a replacement ID.
		if launch.supervised || id != "" {
			return d.RunID, errGoalLaunchHeld
		}
		return "", errGoalLaunchHeld
	}
	result, err := m.reconcileGoalDispatchLocked(root, old, data, len(data.Dispatches)-1, ctx, fault)
	return result.RunID, err
}

func nativeRecoveryText(r Run) string {
	when := time.Now().UTC()
	if r.Ended != nil {
		when = *r.Ended
	}
	return fmt.Sprintf("Previous run ended before report delivery; exit cause unknown at %s. Some of your work may already be done. Re-measure the repositories (git status, git log, your claims, any scratch containers), keep what is committed, then finish the goal.\n\n", when.Format(time.RFC3339)) + r.Brief
}

func (m *Manager) settleGoalMessageLocked(run Run, h heldMessage) error {
	known, err := m.nativeGoalLocked(run.GoalID)
	if err != nil || !known {
		return err
	}
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		return err
	}
	defer root.Close()
	p, err := replayGoalJournalRoot(root)
	if err != nil || p.tornTail {
		return errGoalLaunchHeld
	}
	old := p.records[run.GoalID]
	data, err := goalRecordMetadata(old)
	if err != nil {
		return err
	}
	if run.Ended == nil || data.State != nativeGoalEnded {
		return nil
	} // discard text, retain the actual running owner
	s := goalContinuationSettlement{Parent: run.ID, MessageSHA256: h.ContentSHA256, MessageRevision: h.Revision}
	for _, previous := range data.Settlements {
		if previous.Parent == run.ID {
			if reflect.DeepEqual(previous, s) {
				return nil
			}
			return errGoalDispatch
		}
	}
	if len(data.Dispatches) == 0 || data.Dispatches[len(data.Dispatches)-1].RunID != run.ID || run.ContinueRunID != "" || !goalAttemptDefinitelyGone(run) || data.Attempt == nil || len(data.Settlements) >= 8 {
		return errGoalLaunchHeld
	}
	data.Settlements = append(append([]goalContinuationSettlement(nil), data.Settlements...), s)
	attempt := *data.Attempt
	attempt.ActiveSuccessor = false
	data.Attempt = &attempt
	if err = saveGoalAttemptLocked(root, old, data, "discard-"+run.ID, goalCommitIO{}); err != nil {
		return err
	}
	_, err = mirrorNativeAuditLocked(root, data.Dispatches[len(data.Dispatches)-1].Source, goalCommitIO{})
	return err
}

func (m *Manager) repairGoalContinuationLocked(d goalDispatchRecord, r Run) error {
	if d.Continuation == nil {
		return nil
	}
	parent, err := loadRun(filepath.Join(m.store, "runs", d.Continuation.Parent), d.Continuation.Parent)
	if err != nil || parent == nil {
		return errGoalLaunchHeld
	}
	if parent.ContinueRunID == "" {
		parent.ContinueRunID = r.ID
		if err = m.save(parent); err != nil {
			return err
		}
	}
	if r.RecoveryRun && !r.ResumedSignalled {
		if err = m.emitSignal(r, "resumed"); err != nil {
			return err
		}
		r.ResumedSignalled = true
		return m.save(&r)
	}
	return nil
}

func (m *Manager) recoverGoalContinuationLocked(r Run) error {
	if r.NativeEnd == nil || r.ContinueRunID != "" || m.checkFrozen() != nil {
		return nil
	}
	lane, err := m.lane(r.Lane)
	if err != nil || lane.Kind != r.Kind || m.admitLaneLocked(r.Lane) != nil {
		return nil
	}
	text, session := "", r.SessionID
	switch r.NativeEnd.Pending {
	case "recovery_pending":
		if r.ResumeCount >= 2 || m.reportExists(r) || r.StopAt != nil || m.config.AutoResume != nil && !*m.config.AutoResume {
			return nil
		}
		if terminal, _ := m.terminalTurn(r); terminal {
			return nil
		}
		text = nativeRecoveryText(r)
		if !sessionPattern.MatchString(session) {
			session = ""
		}
	case "acknowledgement_pending":
		if !r.Resumed && m.noWork(r.ID) && lane.Kind == "claude" && lane.mode() == "goal_then_continue" {
			text = lane.continuation()
		}
	case "message_pending":
		text, err = m.pendingMessage(r.ID)
		if err != nil {
			return nil
		}
	}
	if text == "" {
		return nil
	}
	lock, err := m.lock(r.Lane)
	if err != nil {
		return nil
	}
	id, err := m.launch(lane, r.GoalID, text, session, lock, &r)
	if m.active[r.Lane] == nil {
		lock.Close()
	}
	if err != nil {
		return nil
	} // visible pending ownership, no new count
	if r.NativeEnd.Pending == "recovery_pending" {
		child, e := loadRun(filepath.Join(m.store, "runs", id), id)
		if e != nil || child == nil {
			return errGoalLaunchHeld
		}
		if e = m.emitSignal(*child, "resumed"); e != nil {
			return e
		}
		child.ResumedSignalled = true
		return m.save(child)
	}
	return nil
}
