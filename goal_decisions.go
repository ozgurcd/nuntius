package nuntius

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"unicode"

	"golang.org/x/sys/unix"
)

var (
	errGoalRecordCapacity      = errors.New("native goal record has reached its encoded metadata capacity; review capacity before adding more history; earlier entries remain saved")
	errGoalDecision            = errors.New("native goal decision is invalid; supply one-line reason, authority and reviewed evidence references")
	errGoalTransition          = errors.New("native goal cannot make this transition; review its current state, or use a new ID for a permanent result")
	errGoalDecisionBusy        = errors.New("native goal still has active or uncertain work; account for its end before recording a decision; this operation stops no process")
	errGoalApprovalUnavailable = errors.New("native goal approval is unavailable until the brief sealing and delivery components can validate its exact revision and authority")
	errGoalReview              = errors.New("native goal needs reviewed evidence for its current ended attempt, or explicit alternate reviewed evidence; a report alone does not verify work")
	errGoalOwnerHold           = errors.New("native goal needs its owner decision; an adapter source or judge authority reference cannot clear the owner hold")
)

type goalDecisionRequest struct {
	OperationID       string
	ExpectedRevision  uint64
	GoalID            string
	Action            string
	Reason            string
	Authority         string
	Evidence          string
	AttemptID         string
	ReportDigest      string
	AlternateEvidence bool
}

// These observations are committed bookkeeping, never caller-supplied decision
// fields. B06 fixtures supply them; later launch/end components own publication.
type goalDecisionAttempt struct {
	ID                string `json:"id"`
	Ended             bool   `json:"ended"`
	ActiveSuccessor   bool   `json:"active_successor"`
	LaunchUnconfirmed bool   `json:"launch_unconfirmed"`
	ReportDigest      string `json:"report_digest,omitempty"`
}

type goalDecisionEvidence struct {
	Action            string `json:"action"`
	Revision          uint64 `json:"revision"`
	Reason            string `json:"reason"`
	Authority         string `json:"authority"`
	Evidence          string `json:"evidence"`
	AttemptID         string `json:"attempt_id,omitempty"`
	ReportDigest      string `json:"report_digest,omitempty"`
	AlternateEvidence bool   `json:"alternate_evidence,omitempty"`
}

// A sealed fixture is not a production compiler or an authority credential.
// No production caller supplies this until the separate sealing steps land.
type goalDecisionSeal struct {
	Revision  uint64
	SpecRef   string
	SealRef   string
	Authority string
}

// The complete D2 state table includes lifecycle edges for later components.
// The decision operation below exposes only approve, verify, park and cancel.
func goalTransitionAllowed(from, to nativeGoalState) bool {
	switch from {
	case nativeGoalDrafted:
		return to == nativeGoalApproved || to == nativeGoalParked || to == nativeGoalCancelled
	case nativeGoalApproved:
		return to == nativeGoalRunning || to == nativeGoalParked || to == nativeGoalCancelled
	case nativeGoalRunning:
		return to == nativeGoalEnded
	case nativeGoalEnded:
		return to == nativeGoalVerified || to == nativeGoalParked || to == nativeGoalCancelled || to == nativeGoalRunning
	case nativeGoalParked:
		return to == nativeGoalApproved || to == nativeGoalCancelled
	default:
		return false
	}
}

func validGoalDecision(req goalDecisionRequest) bool {
	if !validGoalOperationID(req.OperationID) || !goalPattern.MatchString(req.GoalID) || req.ExpectedRevision == 0 || req.ExpectedRevision >= goalJournalMaxEvents ||
		!goalDecisionReason(req.Reason) || !queueText(req.Authority, 128, false) || !queueText(req.Evidence, 128, false) {
		return false
	}
	if req.Action != "approve" && req.Action != "verify" && req.Action != "park" && req.Action != "cancel" {
		return false
	}
	if req.Action != "verify" {
		return req.AttemptID == "" && req.ReportDigest == "" && !req.AlternateEvidence
	}
	if req.AlternateEvidence {
		return req.ReportDigest == "" && (req.AttemptID == "" || validGoalOperationID(req.AttemptID))
	}
	return validGoalOperationID(req.AttemptID) && validGoalDigest(req.ReportDigest)
}

// Decision reasons are one line in both requests and retained evidence. Keep
// Queue's byte/control rules, with the additional Unicode separator rule local
// to this field rather than changing unrelated Queue text.
func goalDecisionReason(reason string) bool {
	if !queueText(reason, 512, false) {
		return false
	}
	for _, r := range reason {
		if unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
			return false
		}
	}
	return true
}

func applyGoalDecision(old goalReplayRecord, spec nativeGoalSpec, data goalRecordData, req goalDecisionRequest, seal *goalDecisionSeal) (goalRecordData, error) {
	if !validGoalDecision(req) {
		return data, errGoalDecision
	}
	if old.revision != req.ExpectedRevision {
		return data, errGoalConflict
	}
	to := map[string]nativeGoalState{"approve": nativeGoalApproved, "verify": nativeGoalVerified, "park": nativeGoalParked, "cancel": nativeGoalCancelled}[req.Action]
	if data.State == nativeGoalRunning || data.Attempt != nil && (!data.Attempt.Ended || data.Attempt.ActiveSuccessor || data.Attempt.LaunchUnconfirmed) {
		return data, errGoalDecisionBusy
	}
	if !goalTransitionAllowed(data.State, to) {
		return data, errGoalTransition
	}
	if req.Action == "approve" {
		if spec.OwnerHold != nil {
			return data, errGoalOwnerHold
		}
		if seal == nil || seal.Revision != old.revision || seal.SpecRef != goalBlobName("goal-spec-", []byte(old.spec)) || seal.SealRef == "" || seal.SealRef != old.sealRef || seal.Authority != req.Authority {
			return data, errGoalApprovalUnavailable
		}
		data.ApprovalRevision = old.revision
	}
	if req.Action == "verify" {
		a := data.Attempt
		if a == nil || !a.Ended || req.AttemptID != a.ID || (!req.AlternateEvidence && (a.ReportDigest == "" || req.ReportDigest != a.ReportDigest)) {
			return data, errGoalReview
		}
	}
	data.State, data.GrantBinding = to, ""
	data.Decisions = append(append([]goalDecisionEvidence(nil), data.Decisions...), goalDecisionEvidence{
		Action: req.Action, Revision: old.revision, Reason: req.Reason, Authority: req.Authority, Evidence: req.Evidence,
		AttemptID: req.AttemptID, ReportDigest: req.ReportDigest, AlternateEvidence: req.AlternateEvidence,
	})
	return data, nil
}

// decideGoal records a decision only. It never invokes a launcher, scheduler,
// cancellation, lane control or recovery. Source is not an authority claim.
func (m *Manager) decideGoal(req goalDecisionRequest) (goalCommitResult, error) {
	if !validGoalDecision(req) {
		return goalCommitResult{}, errGoalDecision
	}
	request, err := json.Marshal(req)
	if err != nil {
		return goalCommitResult{}, errGoalDecision
	}
	sum := sha256.Sum256(append([]byte("decision:"), request...))
	mutation := hex.EncodeToString(sum[:])
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.readOnly || m.closed {
		return goalCommitResult{}, errGoalCommit
	}
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		return goalCommitResult{}, errGoalCommit
	}
	defer root.Close()
	lock, err := laneControlLock(root, unix.LOCK_EX)
	if err != nil {
		return goalCommitResult{}, errGoalCommit
	}
	defer lock.Close()
	p, err := replayGoalJournalRoot(root)
	if err != nil {
		return goalCommitResult{}, err
	}
	if receipt, found := p.operations[req.OperationID]; found {
		if receipt.mutation != mutation {
			return goalCommitResult{}, errGoalOperation
		}
		result, _, err := confirmGoalOperation(root, p, receipt, goalCommitIO{})
		return result, err
	}
	old := p.records[req.GoalID]
	data, err := goalRecordMetadata(old)
	if err != nil {
		return goalCommitResult{}, errGoalReplay
	}
	spec, err := decodeNativeGoalSpec([]byte(old.spec))
	if err != nil {
		return goalCommitResult{}, errGoalTransition
	}
	data, err = applyGoalDecision(old, spec, data, req, nil)
	if err != nil {
		return goalCommitResult{}, err
	}
	record, err := encodeGoalRecord(data)
	if err != nil {
		return goalCommitResult{}, err
	}
	var sealBytes []byte
	if old.sealRef != "" {
		var total int64
		sealBytes, err = readGoalBlob(root, old.sealRef, "goal-seal-", goalSealMaxBytes, &total)
		if err != nil {
			return goalCommitResult{}, errGoalReplay
		}
	}
	specRef := goalBlobName("goal-spec-", []byte(old.spec))
	input := goalRecordFingerprint(goalInputFingerprint(req.GoalID, old.revision, specRef, old.sealRef), goalBlobName("goal-record-", record), mutation)
	commit := goalCommitRequest{OperationID: req.OperationID, ExpectedRevision: old.revision, Spec: []byte(old.spec), Seal: sealBytes, Record: record, MutationSHA256: mutation}
	result, _, err := commitGoalLocked(root, commit, req.GoalID, specRef, old.sealRef, input, goalCommitIO{})
	return result, err
}

func validGoalDecisionData(r goalRecordData) bool {
	if a := r.Attempt; a != nil {
		if a.ID != "" && !validGoalOperationID(a.ID) || a.ReportDigest != "" && !validGoalDigest(a.ReportDigest) || a.ID == "" && (!a.Ended || a.ReportDigest != "") {
			return false
		}
	}
	for _, d := range r.Decisions {
		req := goalDecisionRequest{OperationID: "check", GoalID: "CHECK", ExpectedRevision: d.Revision, Action: d.Action, Reason: d.Reason, Authority: d.Authority, Evidence: d.Evidence, AttemptID: d.AttemptID, ReportDigest: d.ReportDigest, AlternateEvidence: d.AlternateEvidence}
		if !validGoalDecision(req) || strings.TrimSpace(d.Reason) == "" {
			return false
		}
	}
	return true
}

func validGoalDecisionJSON(m map[string]any) bool {
	if value, exists := m["attempt"]; exists {
		a, ok := value.(map[string]any)
		if !ok || !nativeClosedFields(a, []string{"id", "ended", "active_successor", "launch_unconfirmed"}, []string{"report_digest"}) {
			return false
		}
		for _, key := range []string{"ended", "active_successor", "launch_unconfirmed"} {
			if _, ok := a[key].(bool); !ok {
				return false
			}
		}
	}
	if value, exists := m["decisions"]; exists {
		rows, ok := value.([]any)
		if !ok {
			return false
		}
		for _, row := range rows {
			d, ok := row.(map[string]any)
			if !ok || !nativeClosedFields(d, []string{"action", "revision", "reason", "authority", "evidence"}, []string{"attempt_id", "report_digest", "alternate_evidence"}) {
				return false
			}
			n, ok := d["revision"].(json.Number)
			if !ok {
				return false
			}
			v, err := n.Int64()
			if err != nil || v < 1 || v >= goalJournalMaxEvents {
				return false
			}
			if value, exists := d["alternate_evidence"]; exists {
				if _, ok := value.(bool); !ok {
					return false
				}
			}
		}
	}
	return true
}

// encodeGoalRecord distinguishes the encoded capacity limit from bad fields.
func encodeGoalRecord(r goalRecordData) ([]byte, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return nil, errGoalRecord
	}
	if len(b) > goalRecordMaxBytes {
		return nil, errGoalRecordCapacity
	}
	if _, err := decodeGoalRecord(b); err != nil {
		return nil, err
	}
	return b, nil
}
