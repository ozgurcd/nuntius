package nuntius

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"sort"
	"unicode/utf8"
)

var errGoalDependencies = errors.New("native goal dependencies or coverage are invalid; review the combined graph, active work and target acceptance before saving")

const goalChangesMaxBytes = 2 << 20

// A closure records acceptance of the immediate target; it is not a run or report.
type goalCoverageEvidence struct {
	Target         string `json:"target"`
	TargetRevision uint64 `json:"target_revision"`
	Authority      string `json:"authority"`
	Evidence       string `json:"evidence"`
}

type goalJournalChange struct {
	GoalID    string `json:"goal_id"`
	Revision  uint64 `json:"revision"`
	SpecRef   string `json:"spec_ref"`
	SealRef   string `json:"seal_ref,omitempty"`
	RecordRef string `json:"record_ref"`
}

// goalDependenciesVerified reports prerequisite truth, not launch authority.
func goalDependenciesVerified(p *goalProjection, spec nativeGoalSpec) bool {
	for _, id := range spec.After {
		record, exists := p.records[id]
		data, err := goalRecordMetadata(record)
		if !exists || err != nil || data.State != nativeGoalVerified {
			return false
		}
	}
	return true
}

func goalGraphAcyclic(specs map[string]nativeGoalSpec) bool {
	colors := map[string]uint8{}
	var visit func(string) bool
	visit = func(id string) bool {
		if colors[id] == 1 {
			return false
		}
		if colors[id] == 2 {
			return true
		}
		spec, exists := specs[id]
		if !exists {
			return true
		} // Forward references stay unresolved.
		colors[id] = 1
		for _, target := range spec.After {
			if !visit(target) {
				return false
			}
		}
		if spec.Coverage != nil && !visit(spec.Coverage.CoveredBy) {
			return false
		}
		colors[id] = 2
		return true
	}
	for id := range specs {
		if !visit(id) {
			return false
		}
	}
	return true
}

// goalDependencyChanges is the single writer/replay predicate. The primary
// input remains in the frame; these deterministic replacements publish with it.
func goalDependencyChanges(p *goalProjection, id string, primary goalReplayRecord) (map[string]goalReplayRecord, error) {
	records := make(map[string]goalReplayRecord, len(p.records)+1)
	for key, value := range p.records {
		records[key] = value
	}
	records[id] = primary
	if len(records) > goalJournalMaxGoals {
		return nil, errGoalDependencies
	}
	specs := make(map[string]nativeGoalSpec, len(records))
	for key, record := range records {
		spec, cached := p.specs[key]
		if !cached || key == id {
			var err error
			spec, err = decodeNativeGoalSpec([]byte(record.spec))
			if err != nil {
				return nil, errGoalDependencies
			}
		}
		specs[key] = spec
	}
	if !goalGraphAcyclic(specs) {
		return nil, errGoalDependencies
	}
	oldData, err := goalRecordMetadata(p.records[id])
	if err != nil {
		return nil, errGoalDependencies
	}
	inputData, err := goalRecordMetadata(primary)
	if err != nil || !reflect.DeepEqual(oldData.Closure, inputData.Closure) {
		return nil, errGoalDependencies
	}
	changes := map[string]goalReplayRecord{}
	visited := map[string]bool{}
	var closeSource func(string) error
	closeSource = func(key string) error {
		if visited[key] {
			return nil
		}
		visited[key] = true
		record, exists := records[key]
		if !exists {
			return nil
		}
		spec := specs[key]
		if spec.Coverage == nil {
			return nil
		}
		data, err := goalRecordMetadata(record)
		if err != nil {
			return errGoalDependencies
		}
		if goalRecordTerminal(data.State) {
			return nil
		}
		if data.State == nativeGoalRunning || data.Attempt != nil && (!data.Attempt.Ended || data.Attempt.ActiveSuccessor || data.Attempt.LaunchUnconfirmed) {
			return errGoalDecisionBusy
		}
		data.State, data.GrantBinding = nativeGoalParked, ""
		targetID := spec.Coverage.CoveredBy
		if err := closeSource(targetID); err != nil {
			return err
		}
		target, exists := records[targetID]
		targetData, err := goalRecordMetadata(target)
		if err != nil {
			return errGoalDependencies
		}
		if exists && targetData.State == nativeGoalVerified {
			evidence := goalCoverageEvidence{Target: targetID, TargetRevision: target.revision}
			if targetData.Closure != nil {
				evidence.Authority, evidence.Evidence = targetData.Closure.Authority, targetData.Closure.Evidence
			} else {
				for _, decision := range targetData.Decisions {
					if decision.Action == "verify" {
						evidence.Authority, evidence.Evidence = decision.Authority, decision.Evidence
					}
				}
			}
			if evidence.Authority != "" && evidence.Evidence != "" {
				data.State, data.Closure = nativeGoalVerified, &evidence
			}
		}
		encoded, err := encodeGoalRecord(data)
		if err != nil {
			return err
		}
		if string(encoded) != record.record {
			if key != id {
				record.revision++
			}
			record.record = string(encoded)
			records[key], changes[key] = record, record
		}
		return nil
	}
	spec := specs[id]
	if spec.Coverage != nil && !goalRecordTerminal(oldData.State) && inputData.State == nativeGoalVerified {
		return nil, errGoalDependencies
	}
	keys := make([]string, 0, len(records))
	for key := range records {
		if specs[key].Coverage != nil {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := closeSource(key); err != nil {
			return nil, err
		}
	}
	return changes, nil
}

func encodeGoalChanges(root *os.File, changes map[string]goalReplayRecord, fault goalCommitIO) ([]byte, error) {
	keys := make([]string, 0, len(changes))
	for key := range changes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	rows := make([]goalJournalChange, 0, len(keys))
	for _, id := range keys {
		record := changes[id]
		ref := goalBlobName("goal-record-", []byte(record.record))
		if err := stageGoalBlob(root, ref, []byte(record.record), fault); err != nil {
			return nil, err
		}
		rows = append(rows, goalJournalChange{GoalID: id, Revision: record.revision, SpecRef: goalBlobName("goal-spec-", []byte(record.spec)), SealRef: record.sealRef, RecordRef: ref})
	}
	b, err := json.Marshal(rows)
	if err != nil || len(b) > goalChangesMaxBytes {
		return nil, errGoalCommit
	}
	return b, nil
}

func decodeGoalChanges(b []byte) ([]goalJournalChange, error) {
	if len(b) > goalChangesMaxBytes || !utf8.Valid(b) || !nativeJSONScalars(b) {
		return nil, errGoalReplay
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	raw, err := queueJSON(d, 1)
	if err != nil {
		return nil, errGoalReplay
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errGoalReplay
	}
	rows, ok := raw.([]any)
	if !ok || len(rows) == 0 || len(rows) > goalJournalMaxGoals {
		return nil, errGoalReplay
	}
	for _, row := range rows {
		m, ok := row.(map[string]any)
		if !ok || !nativeClosedFields(m, []string{"goal_id", "revision", "spec_ref", "record_ref"}, []string{"seal_ref"}) {
			return nil, errGoalReplay
		}
		n, ok := m["revision"].(json.Number)
		if !ok {
			return nil, errGoalReplay
		}
		v, err := n.Int64()
		if err != nil || v < 1 || v > goalJournalMaxEvents {
			return nil, errGoalReplay
		}
	}
	var changes []goalJournalChange
	if json.Unmarshal(b, &changes) != nil {
		return nil, errGoalReplay
	}
	for i, row := range changes {
		if !goalPattern.MatchString(row.GoalID) || i > 0 && changes[i-1].GoalID >= row.GoalID || !validGoalBlobRef(row.SpecRef, "goal-spec-") || !validGoalBlobRef(row.RecordRef, "goal-record-") || row.SealRef != "" && !validGoalBlobRef(row.SealRef, "goal-seal-") {
			return nil, errGoalReplay
		}
	}
	return changes, nil
}

func replayGoalChanges(root *os.File, p *goalProjection, e goalJournalEvent, primary goalReplayRecord, total *int64) (map[string]goalReplayRecord, error) {
	expected, err := goalDependencyChanges(p, e.GoalID, primary)
	if err != nil {
		return nil, errGoalReplay
	}
	actual := map[string]goalReplayRecord{}
	if e.ChangesRef != "" {
		b, err := readGoalBlob(root, e.ChangesRef, "goal-changes-", goalChangesMaxBytes, total)
		if err != nil {
			return nil, errGoalReplay
		}
		rows, err := decodeGoalChanges(b)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			record, err := readGoalBlob(root, row.RecordRef, "goal-record-", goalRecordMaxBytes, total)
			if err != nil {
				return nil, errGoalReplay
			}
			spec, err := readGoalBlob(root, row.SpecRef, "goal-spec-", nativeGoalMaxBytes, total)
			if err != nil {
				return nil, errGoalReplay
			}
			actual[row.GoalID] = goalReplayRecord{revision: row.Revision, spec: string(spec), sealRef: row.SealRef, record: string(record)}
		}
	}
	if !reflect.DeepEqual(expected, actual) {
		return nil, errGoalReplay
	}
	return actual, nil
}

func validGoalCoverageJSON(m map[string]any) bool {
	value, exists := m["closure"]
	if !exists {
		return true
	}
	row, ok := value.(map[string]any)
	if !ok || !nativeClosedFields(row, []string{"target", "target_revision", "authority", "evidence"}, nil) {
		return false
	}
	n, ok := row["target_revision"].(json.Number)
	if !ok {
		return false
	}
	v, err := n.Int64()
	return err == nil && v > 0 && v <= goalJournalMaxEvents
}
