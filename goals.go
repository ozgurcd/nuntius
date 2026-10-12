package nuntius

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const (
	goalRecordMaxBytes         = 64 << 10
	goalRecordMaxNotes         = 32
	goalRecordNoteMaxBytes     = 1024
	goalRecordSnapshotMaxBytes = 1 << 20
	goalRecordPageMax          = 100
)

var errGoalRecord = errors.New("native goal record is invalid; review its fields before saving")
var errGoalTerminal = errors.New("this goal has a permanent result; add a note or correction evidence, or create a new goal")
var errGoalSnapshot = errors.New("native goal snapshot is too large; no partial open list is available; preserve history and review capacity")
var errGoalNotesFull = errors.New("native goal note history is full; preserve existing notes and review capacity before adding more")
var errGoalStoreMissing = errors.New("native goal store is missing; select an existing private store before using native goals")

type goalRecordNote struct {
	Text     string `json:"text"`
	Evidence string `json:"evidence,omitempty"`
}

type goalRecordData struct {
	Version          int                    `json:"version"`
	State            nativeGoalState        `json:"state"`
	Outcome          nativeGoalOutcome      `json:"outcome,omitempty"`
	ApprovalRevision uint64                 `json:"approval_revision,omitempty"`
	GrantBinding     string                 `json:"grant_binding,omitempty"`
	Notes            []goalRecordNote       `json:"notes"`
	Decisions        []goalDecisionEvidence `json:"decisions,omitempty"`
	Attempt          *goalDecisionAttempt   `json:"attempt,omitempty"`
	Closure          *goalCoverageEvidence  `json:"closure,omitempty"`
	Dispatches       []goalDispatchRecord   `json:"dispatches,omitempty"`
	Ends             []goalAttemptEnd       `json:"ends,omitempty"`
	Reports          []goalAttemptReport    `json:"reports,omitempty"`
}

type goalRecordRequest struct {
	OperationID      string
	ExpectedRevision uint64
	Spec             nativeGoalSpec
	Seal             []byte
}

type goalNoteRequest struct {
	OperationID      string
	ExpectedRevision uint64
	GoalID           string
	Note             goalRecordNote
}

type nativeGoalRecordDTO struct {
	nativeGoalDTO
	Revision         uint64                 `json:"revision"`
	ApprovalRevision uint64                 `json:"approval_revision,omitempty"`
	GrantBound       bool                   `json:"grant_bound"`
	Notes            []goalRecordNote       `json:"notes"`
	Decisions        []goalDecisionEvidence `json:"decisions,omitempty"`
	Closure          *goalCoverageEvidence  `json:"closure,omitempty"`
}

type goalRecordListing struct {
	Sequence        uint64                `json:"sequence"`
	StorePresent    bool                  `json:"store_present"`
	UncommittedTail bool                  `json:"uncommitted_tail"`
	OpenTotal       int                   `json:"open_total"`
	Open            []nativeGoalRecordDTO `json:"open"`
	Terminal        []nativeGoalRecordDTO `json:"terminal"`
	TerminalTotal   int                   `json:"terminal_total"`
	NextOffset      int                   `json:"next_offset"`
}

func (m *Manager) createGoal(req goalRecordRequest) (goalCommitResult, error) {
	if req.ExpectedRevision != 0 || req.Seal != nil {
		return goalCommitResult{}, errGoalRecord
	}
	return m.changeGoalRecord("create", req, goalNoteRequest{})
}

func (m *Manager) updateGoal(req goalRecordRequest) (goalCommitResult, error) {
	if req.ExpectedRevision == 0 {
		return goalCommitResult{}, errGoalConflict
	}
	return m.changeGoalRecord("update", req, goalNoteRequest{})
}

func (m *Manager) noteGoal(req goalNoteRequest) (goalCommitResult, error) {
	if req.ExpectedRevision == 0 || !goalPattern.MatchString(req.GoalID) || !validGoalNote(req.Note) {
		return goalCommitResult{}, errGoalRecord
	}
	return m.changeGoalRecord("note", goalRecordRequest{}, req)
}

func (m *Manager) readGoal(id string) (nativeGoalRecordDTO, bool, error) {
	if !goalPattern.MatchString(id) {
		return nativeGoalRecordDTO{}, false, errGoalRecord
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	p, _, err := readGoalRecords(m.store)
	if err != nil {
		return nativeGoalRecordDTO{}, false, err
	}
	r, found := p.records[id]
	if !found {
		return nativeGoalRecordDTO{}, false, nil
	}
	v, err := goalRecordView(r)
	return v, err == nil, err
}

func (m *Manager) listGoals(offset, limit int) (*goalRecordListing, error) {
	if offset < 0 || limit < 1 || limit > goalRecordPageMax {
		return nil, errGoalRecord
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	p, present, err := readGoalRecords(m.store)
	if err != nil {
		return nil, err
	}
	return goalRecordList(p, present, offset, limit)
}

func validGoalDigest(value string) bool {
	return validGoalBlobRef("goal-record-"+value, "goal-record-")
}

func goalRecordFingerprint(storage, record, mutation string) string {
	b, _ := json.Marshal([]string{storage, record, mutation})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func validGoalNote(n goalRecordNote) bool {
	return queueText(n.Text, goalRecordNoteMaxBytes, false) && strings.TrimSpace(n.Text) == n.Text && !strings.ContainsAny(n.Text, "\r\n") &&
		(n.Evidence == "" || (queueText(n.Evidence, 128, false) && strings.TrimSpace(n.Evidence) == n.Evidence && !strings.ContainsAny(n.Evidence, "\r\n")))
}

func decodeGoalRecord(b []byte) (goalRecordData, error) {
	var r goalRecordData
	if len(b) > goalRecordMaxBytes || !utf8.Valid(b) || !nativeJSONScalars(b) {
		return r, errGoalRecord
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	raw, err := queueJSON(d, 1)
	if err != nil {
		return r, errGoalRecord
	}
	if _, err := d.Token(); err != io.EOF {
		return r, errGoalRecord
	}
	m, ok := raw.(map[string]any)
	if !ok || !nativeClosedFields(m, []string{"version", "state", "notes"}, []string{"outcome", "approval_revision", "grant_binding", "decisions", "attempt", "closure", "dispatches", "ends", "reports"}) || !validGoalDecisionJSON(m) || !validGoalCoverageJSON(m) || !validGoalDispatchJSON(m) || !validGoalAttemptsJSON(m) {
		return r, errGoalRecord
	}
	version, ok := m["version"].(json.Number)
	if !ok || version.String() != "1" {
		return r, errGoalRecord
	}
	if v, exists := m["approval_revision"]; exists {
		n, ok := v.(json.Number)
		if !ok {
			return r, errGoalRecord
		}
		x, err := n.Int64()
		if err != nil || x < 1 || x > goalJournalMaxEvents {
			return r, errGoalRecord
		}
	}
	notes, ok := m["notes"].([]any)
	if !ok || len(notes) > goalRecordMaxNotes {
		return r, errGoalRecord
	}
	for _, value := range notes {
		n, ok := value.(map[string]any)
		if !ok || !nativeClosedFields(n, []string{"text"}, []string{"evidence"}) {
			return r, errGoalRecord
		}
	}
	if json.Unmarshal(b, &r) != nil || r.Version != 1 {
		return goalRecordData{}, errGoalRecord
	}
	switch r.State {
	case nativeGoalDrafted, nativeGoalApproved, nativeGoalRunning, nativeGoalEnded, nativeGoalVerified, nativeGoalCancelled, nativeGoalParked:
	default:
		return goalRecordData{}, errGoalRecord
	}
	switch r.Outcome {
	case "", nativeOutcomeCompleted, nativeOutcomeTimedOut, nativeOutcomeCancelled, nativeOutcomeInterrupted, nativeOutcomeReportDeliveryFailed, nativeOutcomeEndedNoReport, nativeOutcomeUnknownExit, nativeOutcomeFailed:
	default:
		return goalRecordData{}, errGoalRecord
	}
	if r.GrantBinding != "" && (!validGoalOperationID(r.GrantBinding) || r.ApprovalRevision == 0) {
		return goalRecordData{}, errGoalRecord
	}
	if r.State == nativeGoalDrafted && (r.ApprovalRevision != 0 || r.GrantBinding != "") {
		return goalRecordData{}, errGoalRecord
	}
	for _, note := range r.Notes {
		if !validGoalNote(note) {
			return goalRecordData{}, errGoalRecord
		}
	}
	if !validGoalDecisionData(r) {
		return goalRecordData{}, errGoalRecord
	}
	if c := r.Closure; c != nil && (r.State != nativeGoalVerified || !goalPattern.MatchString(c.Target) || c.TargetRevision == 0 || c.TargetRevision > goalJournalMaxEvents || !queueText(c.Authority, 128, false) || !queueText(c.Evidence, 128, false) || !goalDecisionReason(c.Authority) || !goalDecisionReason(c.Evidence)) {
		return goalRecordData{}, errGoalRecord
	}
	return r, nil
}

func goalRecordMetadata(r goalReplayRecord) (goalRecordData, error) {
	if r.record == "" {
		return goalRecordData{Version: 1, State: nativeGoalDrafted, Notes: []goalRecordNote{}}, nil
	}
	return decodeGoalRecord([]byte(r.record))
}

func goalRecordTerminal(state nativeGoalState) bool {
	return state == nativeGoalVerified || state == nativeGoalCancelled
}

// A replay cannot drop old notes or rewrite a permanent result, even through
// the low-level storage seam. Lifecycle decisions are a later component.
func goalRecordHistoryValid(old, next goalReplayRecord, seals ...[]byte) bool {
	if old.revision == 0 {
		data, err := goalRecordMetadata(next)
		// Initial bookkeeping fixtures may describe observations, but there is
		// no prior stored revision for a decision to have reviewed.
		return err == nil && len(data.Decisions) == 0 && data.Closure == nil && len(data.Dispatches) == 0 && len(data.Ends) == 0 && len(data.Reports) == 0
	}
	a, err := goalRecordMetadata(old)
	if err != nil {
		return false
	}
	b, err := goalRecordMetadata(next)
	// Metadata decoding applies goalDispatchScopeValid to every retained
	// receipt before these end/report or unchanged-record shortcuts. Repeating
	// strict spec parsing here adds cost without checking another observation.
	if err == nil && (!reflect.DeepEqual(a.Ends, b.Ends) || !reflect.DeepEqual(a.Reports, b.Reports)) {
		return goalAttemptsHistoryValid(old, next, a, b)
	}
	if err == nil && !goalDispatchHistoryValid(old, next, a, b, seals) {
		return false
	}
	if err != nil || len(b.Notes) < len(a.Notes) || !reflect.DeepEqual(a.Notes, b.Notes[:len(a.Notes)]) {
		return false
	}
	if old.record != "" && next.record == "" {
		return false
	}
	if len(b.Decisions) < len(a.Decisions) || len(a.Decisions) > 0 && !reflect.DeepEqual(a.Decisions, b.Decisions[:len(a.Decisions)]) {
		return false
	}
	scopeChanged := false
	if old.spec != next.spec {
		before, err := decodeNativeGoalSpec([]byte(old.spec))
		if err != nil {
			return false
		}
		after, err := decodeNativeGoalSpec([]byte(next.spec))
		if err != nil {
			return false
		}
		scopeChanged = goalScopeChanged(before, after)
	}
	if scopeChanged || old.sealRef != next.sealRef {
		if b.ApprovalRevision != 0 || b.GrantBinding != "" {
			return false
		}
		if scopeChanged && b.Outcome != "" {
			return false
		}
		if b.State != nativeGoalDrafted {
			spec, err := decodeNativeGoalSpec([]byte(next.spec))
			if err != nil || spec.Coverage == nil || b.State != nativeGoalParked {
				return false
			}
		}
	}
	// Retaining a decision prefix cannot confer a new approval or exact grant.
	// Notes and presentation edits may keep an existing binding; approval must
	// append a reviewed decision and pass the seal/source predicate below.
	if len(b.Decisions) == len(a.Decisions) &&
		(b.State == nativeGoalApproved && (a.State != nativeGoalApproved || b.ApprovalRevision != a.ApprovalRevision) ||
			b.ApprovalRevision != 0 && b.ApprovalRevision != a.ApprovalRevision ||
			b.GrantBinding != "" && b.GrantBinding != a.GrantBinding) {
		return false
	}
	if len(b.Decisions) > len(a.Decisions) {
		if len(b.Decisions) != len(a.Decisions)+1 || old.spec != next.spec || old.sealRef != next.sealRef || a.Outcome != b.Outcome || !reflect.DeepEqual(a.Attempt, b.Attempt) {
			return false
		}
		d := b.Decisions[len(a.Decisions)]
		spec, err := decodeNativeGoalSpec([]byte(old.spec))
		if err != nil {
			return false
		}
		// Reapply the writer's exact predicate to the retained observation.
		// A compiled seal must satisfy the same predicate on write and replay.
		req := goalDecisionRequest{OperationID: "check", GoalID: spec.ID, ExpectedRevision: d.Revision, Action: d.Action, Reason: d.Reason, Authority: d.Authority, Evidence: d.Evidence, AttemptID: d.AttemptID, ReportDigest: d.ReportDigest, AlternateEvidence: d.AlternateEvidence, SourceState: d.SourceState}
		var checked *goalDecisionSeal
		if len(seals) == 1 {
			checked = validateGoalBrief(old, req.Authority, seals[0])
		}
		expected, err := applyGoalDecision(old, spec, a, req, checked)
		if err != nil || !reflect.DeepEqual(expected, b) {
			return false
		}
	}
	if goalRecordTerminal(a.State) {
		if old.spec != next.spec || old.sealRef != next.sealRef {
			return false
		}
		a.Notes, b.Notes = nil, nil
		return reflect.DeepEqual(a, b)
	}
	return true
}

func goalScopeChanged(before, after nativeGoalSpec) bool {
	// Title and presentation order do not alter the execution scope.
	before.Title, after.Title, before.Order, after.Order = "", "", 0, 0
	return !reflect.DeepEqual(before, after)
}

func (m *Manager) changeGoalRecord(kind string, req goalRecordRequest, note goalNoteRequest) (goalCommitResult, error) {
	if kind != "create" && kind != "update" && kind != "note" {
		return goalCommitResult{}, errGoalRecord
	}
	if kind != "note" && (!validNativeGoalSpec(req.Spec) || len(req.Seal) > goalSealMaxBytes) {
		return goalCommitResult{}, errGoalRecord
	}
	// Canonical typed input owns every slice and supplies a semantic receipt.
	requestBytes, err := json.Marshal(struct {
		Kind   string
		Record goalRecordRequest
		Note   goalNoteRequest
	}{kind, req, note})
	if err != nil {
		return goalCommitResult{}, errGoalRecord
	}
	var owned struct {
		Kind   string
		Record goalRecordRequest
		Note   goalNoteRequest
	}
	if json.Unmarshal(requestBytes, &owned) != nil {
		return goalCommitResult{}, errGoalRecord
	}
	req, note = owned.Record, owned.Note
	operation, revision, id := req.OperationID, req.ExpectedRevision, req.Spec.ID
	if kind == "note" {
		operation, revision, id = note.OperationID, note.ExpectedRevision, note.GoalID
	}
	if !validGoalOperationID(operation) || revision >= goalJournalMaxEvents {
		return goalCommitResult{}, errGoalRecord
	}
	sum := sha256.Sum256(requestBytes)
	mutation := hex.EncodeToString(sum[:])
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.readOnly || m.closed {
		return goalCommitResult{}, errGoalCommit
	}
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		return goalCommitResult{}, goalPersistenceError(errGoalCommit, "record.root", err)
	}
	defer root.Close()
	lock, err := laneControlLock(root, unix.LOCK_EX)
	if err != nil {
		return goalCommitResult{}, goalPersistenceError(errGoalCommit, "record.lock", err)
	}
	defer lock.Close()
	p, err := replayGoalJournalRoot(root)
	if err != nil {
		return goalCommitResult{}, err
	}
	if receipt, found := p.operations[operation]; found {
		if receipt.mutation != mutation {
			return goalCommitResult{}, errGoalOperation
		}
		result, _, err := confirmGoalOperation(root, p, receipt, goalCommitIO{})
		return result, err
	}
	old := p.records[id]
	if old.revision != revision {
		return goalCommitResult{}, errGoalConflict
	}
	metadata, err := goalRecordMetadata(old)
	if err != nil {
		return goalCommitResult{}, errGoalReplay
	}
	var specBytes, sealBytes []byte
	sealRef := old.sealRef
	if kind == "note" {
		if old.revision == 0 || !validGoalNote(note.Note) {
			return goalCommitResult{}, errGoalRecord
		}
		if len(metadata.Notes) >= goalRecordMaxNotes {
			return goalCommitResult{}, errGoalNotesFull
		}
		metadata.Notes = append(metadata.Notes, note.Note)
		specBytes = []byte(old.spec)
		if sealRef != "" {
			var total int64
			sealBytes, err = readGoalBlob(root, sealRef, "goal-seal-", goalSealMaxBytes, &total)
			if err != nil {
				return goalCommitResult{}, errGoalReplay
			}
		}
	} else {
		if goalRecordTerminal(metadata.State) {
			return goalCommitResult{}, errGoalTerminal
		}
		// A record edit cannot change the scope of an in-flight attempt.
		if metadata.State == nativeGoalRunning || metadata.State == nativeGoalEnded {
			return goalCommitResult{}, errGoalRecord
		}
		specBytes, err = json.Marshal(req.Spec)
		if err != nil {
			return goalCommitResult{}, errGoalRecord
		}
		if _, err := decodeNativeGoalSpec(specBytes); err != nil {
			return goalCommitResult{}, errGoalRecord
		}
		if len(req.Seal) > goalSealMaxBytes {
			return goalCommitResult{}, errGoalRecord
		}
		sealBytes, sealRef = req.Seal, ""
		if sealBytes != nil {
			sealRef = goalBlobName("goal-seal-", sealBytes)
		}
		if old.revision != 0 {
			before, _ := p.spec(id)
			if goalScopeChanged(before, req.Spec) || old.sealRef != sealRef {
				metadata.State, metadata.Outcome, metadata.ApprovalRevision, metadata.GrantBinding = nativeGoalDrafted, "", 0, ""
			}
		}
	}
	record, err := encodeGoalRecord(metadata)
	if err != nil {
		return goalCommitResult{}, err
	}
	specRef := goalBlobName("goal-spec-", specBytes)
	input := goalRecordFingerprint(goalInputFingerprint(id, revision, specRef, sealRef), goalBlobName("goal-record-", record), mutation)
	commit := goalCommitRequest{OperationID: operation, ExpectedRevision: revision, Spec: specBytes, Seal: sealBytes, Record: record, MutationSHA256: mutation}
	result, _, err := commitGoalLocked(root, commit, id, specRef, sealRef, input, goalCommitIO{})
	return result, err
}

func readGoalRecords(path string) (*goalProjection, bool, error) {
	root, err := openLaneControlRoot(path)
	if err != nil {
		if goalRecordRootMissing(path) {
			return nil, false, errGoalStoreMissing
		}
		return nil, false, errGoalReplay
	}
	defer root.Close()
	data, err := readGoalPrivate(root, goalJournalName, goalJournalMaxBytes)
	present := !errors.Is(err, os.ErrNotExist)
	if err != nil && present {
		return nil, false, errGoalReplay
	}
	p, err := replayGoalJournalData(root, data)
	return p, present, err
}

// The shared root opener intentionally hides errno. Recheck absence using
// descriptors without following links, so unsafe roots never appear empty.
func goalRecordRootMissing(path string) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return false
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return false
	}
	for _, component := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		next, err := unix.Openat(fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if err != nil {
			return errors.Is(err, unix.ENOENT)
		}
		fd = next
	}
	unix.Close(fd)
	return false
}

func goalRecordView(r goalReplayRecord) (nativeGoalRecordDTO, error) {
	spec, err := decodeNativeGoalSpec([]byte(r.spec))
	if err != nil {
		return nativeGoalRecordDTO{}, errGoalReplay
	}
	metadata, err := goalRecordMetadata(r)
	if err != nil {
		return nativeGoalRecordDTO{}, errGoalReplay
	}
	v, err := nativeGoalView(spec, metadata.State, metadata.Outcome)
	if err != nil {
		return nativeGoalRecordDTO{}, errGoalSnapshot
	}
	for i := range metadata.Notes {
		metadata.Notes[i].Text = Redact(metadata.Notes[i].Text)
		metadata.Notes[i].Evidence = Redact(metadata.Notes[i].Evidence)
	}
	for i := range metadata.Decisions {
		metadata.Decisions[i].Reason = Redact(metadata.Decisions[i].Reason)
		metadata.Decisions[i].Authority = Redact(metadata.Decisions[i].Authority)
		metadata.Decisions[i].Evidence = Redact(metadata.Decisions[i].Evidence)
		metadata.Decisions[i].SourceState = Redact(metadata.Decisions[i].SourceState)
	}
	if metadata.Closure != nil {
		metadata.Closure.Authority = Redact(metadata.Closure.Authority)
		metadata.Closure.Evidence = Redact(metadata.Closure.Evidence)
	}
	return nativeGoalRecordDTO{nativeGoalDTO: v, Revision: r.revision, ApprovalRevision: metadata.ApprovalRevision, GrantBound: metadata.GrantBinding != "", Notes: metadata.Notes, Decisions: metadata.Decisions, Closure: metadata.Closure}, nil
}

func goalRecordList(p *goalProjection, present bool, offset, limit int) (*goalRecordListing, error) {
	list := &goalRecordListing{Sequence: p.sequence, StorePresent: present, UncommittedTail: p.tornTail, Open: []nativeGoalRecordDTO{}, Terminal: []nativeGoalRecordDTO{}}
	bytesUsed := 0
	ids := make([]string, 0, len(p.records))
	for id := range p.records {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		v, err := goalRecordView(p.records[id])
		if err != nil {
			return nil, err
		}
		if goalRecordTerminal(v.State) {
			if list.TerminalTotal >= offset && len(list.Terminal) < limit {
				list.Terminal = append(list.Terminal, v)
				encoded, _ := json.Marshal(v)
				bytesUsed += len(encoded) + 1
			}
			list.TerminalTotal++
		} else {
			list.Open = append(list.Open, v)
			list.OpenTotal++
			encoded, _ := json.Marshal(v)
			bytesUsed += len(encoded) + 1
		}
		// Refuse early without building an oversized response; never return
		// the partial list. The final encoding below also includes its envelope.
		if bytesUsed > goalRecordSnapshotMaxBytes {
			return nil, errGoalSnapshot
		}
	}
	if offset > list.TerminalTotal {
		return nil, errGoalRecord
	}
	if offset+len(list.Terminal) < list.TerminalTotal {
		list.NextOffset = offset + len(list.Terminal)
	}
	b, err := json.Marshal(list)
	if err != nil || len(b) > goalRecordSnapshotMaxBytes {
		return nil, errGoalSnapshot
	}
	return list, nil
}
