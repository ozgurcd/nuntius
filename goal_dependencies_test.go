package nuntius

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

func dependencyFixture(t *testing.T, m *Manager, id string, state nativeGoalState, after []string) nativeGoalSpec {
	t.Helper()
	_, request := goalRecordsFixture(t)
	spec := request.Spec
	spec.ID, spec.After, spec.Coverage = id, after, nil
	if spec.After == nil {
		spec.After = []string{}
	}
	b, _ := json.Marshal(spec)
	data := goalRecordData{Version: 1, State: state, Notes: []goalRecordNote{}}
	if state == nativeGoalEnded {
		data.Attempt = &goalDecisionAttempt{Ended: true}
	}
	record, err := encodeGoalRecord(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.commitGoal(goalCommitRequest{OperationID: "fixture-" + id, Spec: b, Record: record, MutationSHA256: strings.Repeat("a", 64)}, goalCommitIO{}); err != nil {
		t.Fatal(err)
	}
	return spec
}

func verifyDependency(t *testing.T, m *Manager, id string) goalDecisionRequest {
	t.Helper()
	row, ok, err := m.readGoal(id)
	if err != nil || !ok {
		t.Fatal("missing target")
	}
	req := goalDecisionRequest{OperationID: "verify-" + id, ExpectedRevision: row.Revision, GoalID: id, Action: "verify", Reason: "Reviewed the completed work", Authority: "approved-plan", Evidence: "review-record", AlternateEvidence: true}
	if _, err := m.decideGoal(req); err != nil {
		t.Fatal(err)
	}
	return req
}

func TestVerifiedDependencies(t *testing.T) {
	for _, state := range []nativeGoalState{nativeGoalVerified, nativeGoalCancelled, nativeGoalEnded, nativeGoalParked, nativeGoalRunning, nativeGoalApproved, nativeGoalDrafted, "missing"} {
		t.Run(string(state), func(t *testing.T) {
			m, _ := goalRecordsFixture(t)
			if state != "missing" {
				dependencyFixture(t, m, "PREREQUISITE", state, nil)
			}
			spec := dependencyFixture(t, m, "DEPENDENT", nativeGoalApproved, []string{"PREREQUISITE"})
			p, err := replayGoalJournal(m.store)
			if err != nil {
				t.Fatal(err)
			}
			if got := goalDependenciesVerified(p, spec); got != (state == nativeGoalVerified) {
				t.Errorf("prerequisite %s truth = %v", state, got)
			}
		})
	}
}

func TestCoveredClosure(t *testing.T) {
	m, _ := goalRecordsFixture(t)
	dependencyFixture(t, m, "TARGET", nativeGoalEnded, nil)
	source := dependencyFixture(t, m, "SOURCE", nativeGoalDrafted, []string{"UNRESOLVED"})
	chain := dependencyFixture(t, m, "CHAIN", nativeGoalDrafted, nil)
	dependent := dependencyFixture(t, m, "DEPENDENT", nativeGoalApproved, []string{"CHAIN"})
	for _, entry := range []struct {
		spec   nativeGoalSpec
		target string
	}{{source, "TARGET"}, {chain, "SOURCE"}} {
		entry.spec.Coverage = &nativeCoverageHold{CoveredBy: entry.target, Reason: "Accepted work replaces this scope"}
		if _, err := m.updateGoal(goalRecordRequest{OperationID: "cover-" + entry.spec.ID, ExpectedRevision: 1, Spec: entry.spec}); err != nil {
			t.Fatal(err)
		}
		row, _, _ := m.readGoal(entry.spec.ID)
		if row.State != nativeGoalParked {
			t.Errorf("covered source remained %s instead of parked", row.State)
		}
	}
	before, err := replayGoalJournal(m.store)
	if err != nil {
		t.Fatal(err)
	}
	req := verifyDependency(t, m, "TARGET")
	after, err := replayGoalJournal(m.store)
	if err != nil {
		t.Fatal(err)
	}
	if after.sequence != before.sequence+1 {
		t.Error("cascade was not one transaction")
	}
	for _, id := range []string{"SOURCE", "CHAIN"} {
		row, ok, err := m.readGoal(id)
		if err != nil || !ok || row.State != nativeGoalVerified || row.Revision != 3 {
			t.Errorf("closure missing for %s", id)
		}
		metadata, err := goalRecordMetadata(after.records[id])
		if err != nil || metadata.Attempt != nil || metadata.Outcome != "" || len(metadata.Decisions) != 0 {
			t.Error("covered closure fabricated attempt, outcome, or review")
		}
		wantTarget := map[string]string{"SOURCE": "TARGET", "CHAIN": "SOURCE"}[id]
		if row.Closure == nil || row.Closure.Target != wantTarget || row.Closure.TargetRevision != after.records[wantTarget].revision || row.Closure.Authority != req.Authority || row.Closure.Evidence != req.Evidence {
			t.Error("closure has no exact target review provenance")
		}
	}
	if !goalDependenciesVerified(after, dependent) {
		t.Error("transitively covered prerequisite did not release dependent")
	}
	if _, err := m.decideGoal(req); err != nil {
		t.Fatal(err)
	}
	duplicate, _ := replayGoalJournal(m.store)
	if !reflect.DeepEqual(after, duplicate) {
		t.Error("duplicate verify emitted new closures")
	}
}

func TestCoveredClosureHeldTargets(t *testing.T) {
	for _, state := range []nativeGoalState{nativeGoalCancelled, nativeGoalParked, nativeGoalEnded} {
		t.Run(string(state), func(t *testing.T) {
			m, req := goalRecordsFixture(t)
			dependencyFixture(t, m, "TARGET", state, nil)
			req.Spec.ID = "SOURCE"
			req.Spec.Coverage = &nativeCoverageHold{CoveredBy: "TARGET", Reason: "Waiting for target acceptance"}
			if _, err := m.createGoal(req); err != nil {
				t.Fatal(err)
			}
			row, _, _ := m.readGoal("SOURCE")
			if row.State != nativeGoalParked {
				t.Error("unverified target did not hold source")
			}
		})
	}
}

func TestCoveredClosureFailedTarget(t *testing.T) {
	m, req := goalRecordsFixture(t)
	spec := dependencyFixture(t, m, "TARGET", nativeGoalEnded, nil)
	p, _ := replayGoalJournal(m.store)
	data, _ := goalRecordMetadata(p.records["TARGET"])
	data.Outcome = nativeOutcomeFailed
	b, _ := json.Marshal(spec)
	record, _ := encodeGoalRecord(data)
	if _, _, err := m.commitGoal(goalCommitRequest{OperationID: "failed-target", ExpectedRevision: 1, Spec: b, Record: record, MutationSHA256: strings.Repeat("b", 64)}, goalCommitIO{}); err != nil {
		t.Fatal(err)
	}
	req.Spec.ID = "SOURCE"
	req.Spec.Coverage = &nativeCoverageHold{CoveredBy: "TARGET", Reason: "Target still needs acceptance"}
	if _, err := m.createGoal(req); err != nil {
		t.Fatal(err)
	}
	row, _, _ := m.readGoal("SOURCE")
	if row.State != nativeGoalParked || row.Closure != nil {
		t.Error("failed target closed source without acceptance")
	}
}

func coverageTransactionFixture(t *testing.T) (*Manager, goalCommitRequest) {
	t.Helper()
	m, req := goalRecordsFixture(t)
	target := dependencyFixture(t, m, "TARGET", nativeGoalEnded, nil)
	for i := 0; i < 20; i++ {
		req.OperationID = fmt.Sprintf("create-%02d", i)
		req.Spec.ID = fmt.Sprintf("SOURCE-%02d", i)
		req.Spec.Coverage = &nativeCoverageHold{CoveredBy: "TARGET", Reason: "Target acceptance covers the work"}
		if _, err := m.createGoal(req); err != nil {
			t.Fatal(err)
		}
	}
	p, err := replayGoalJournal(m.store)
	if err != nil {
		t.Fatal(err)
	}
	old := p.records["TARGET"]
	metadata, _ := goalRecordMetadata(old)
	decision := goalDecisionRequest{OperationID: "accept", GoalID: "TARGET", ExpectedRevision: 1, Action: "verify", Reason: "Reviewed completed work", Authority: "approved-plan", Evidence: "review-record", AlternateEvidence: true}
	metadata, err = applyGoalDecision(old, target, metadata, decision, nil)
	if err != nil {
		t.Fatal(err)
	}
	record, err := encodeGoalRecord(metadata)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(target)
	return m, goalCommitRequest{OperationID: "accept", ExpectedRevision: 1, Spec: b, Record: record, MutationSHA256: strings.Repeat("b", 64)}
}

func TestCoveredClosureAtomic(t *testing.T) {
	for _, stage := range []string{"cascade_blob", "before_append", "partial_write", "journal_sync", "journal_directory_sync", "publication", "none"} {
		t.Run(stage, func(t *testing.T) {
			m, req := coverageTransactionFixture(t)
			before, err := replayGoalJournal(m.store)
			if err != nil {
				t.Fatal(err)
			}
			blobWrites := 0
			fault := goalCommitIO{checkpoint: func(at string) error {
				if at == "blob_write" {
					blobWrites++
				}
				if at == stage || stage == "cascade_blob" && at == "blob_write" && blobWrites == 3 {
					return errors.New("injected failure")
				}
				return nil
			}}
			result, _, err := m.commitGoal(req, fault)
			committed := stage == "journal_sync" || stage == "journal_directory_sync" || stage == "publication" || stage == "none"
			if committed && stage != "none" && !errors.Is(err, errGoalUnconfirmed) {
				t.Fatal("durability failure was not unconfirmed")
			}
			if !committed && err == nil {
				t.Fatal("ignored precommit failure")
			}
			after, err := replayGoalJournal(m.store)
			if err != nil {
				t.Fatal(err)
			}
			want := nativeGoalParked
			if committed {
				want = nativeGoalVerified
			}
			for i := 0; i < 20; i++ {
				data, _ := goalRecordMetadata(after.records[fmt.Sprintf("SOURCE-%02d", i)])
				if data.State != want {
					t.Fatal("partial cascade became visible")
				}
			}
			if committed && after.sequence != before.sequence+1 {
				t.Fatal("cascade used several frames")
			}
			if committed {
				root, _, _, event := coverageLastFrame(t, m)
				var total int64
				changes, err := readGoalBlob(root, event.ChangesRef, "goal-changes-", goalChangesMaxBytes, &total)
				if err != nil || len(changes) <= goalJournalMaxFrameBytes {
					t.Fatal("large atomic cascade did not exceed the single-frame payload limit")
				}
			}
			retry, replayed, err := m.commitGoal(req, goalCommitIO{})
			if err != nil {
				t.Fatal(err)
			}
			if committed && result != retry {
				t.Fatal("retry changed receipt")
			}
			if replayed.sequence != before.sequence+1 {
				t.Fatal("retry repeated closure")
			}
		})
	}
}

func TestCoveragePopulationBound(t *testing.T) {
	_, req := goalRecordsFixture(t)
	spec := req.Spec
	spec.ID, spec.After, spec.Coverage = "TARGET", []string{}, nil
	b, _ := json.Marshal(spec)
	ended := goalRecordData{Version: 1, State: nativeGoalEnded, Notes: []goalRecordNote{}, Attempt: &goalDecisionAttempt{Ended: true}}
	record, _ := encodeGoalRecord(ended)
	old := goalReplayRecord{revision: 1, spec: string(b), record: string(record)}
	accepted, err := applyGoalDecision(old, spec, ended, goalDecisionRequest{OperationID: "accept", ExpectedRevision: 1, GoalID: "TARGET", Action: "verify", Reason: "Reviewed work", Authority: "approved-plan", Evidence: "review-record", AlternateEvidence: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	record, _ = encodeGoalRecord(accepted)
	primary := goalReplayRecord{revision: 2, spec: string(b), record: string(record)}
	p := &goalProjection{records: map[string]goalReplayRecord{"TARGET": old}}
	parked, _ := encodeGoalRecord(goalRecordData{Version: 1, State: nativeGoalParked, Notes: []goalRecordNote{}})
	for i := 0; i < goalJournalMaxGoals-1; i++ {
		spec.ID = fmt.Sprintf("SOURCE-%04d", i)
		spec.Coverage = &nativeCoverageHold{CoveredBy: "TARGET", Reason: "Accepted target replaces scope"}
		b, _ := json.Marshal(spec)
		p.records[spec.ID] = goalReplayRecord{revision: 1, spec: string(b), record: string(parked)}
	}
	changes, err := goalDependencyChanges(p, "TARGET", primary)
	if err != nil || len(changes) != goalJournalMaxGoals-1 {
		t.Fatal("exact population bound did not close complete set")
	}
	for _, row := range changes {
		metadata, _ := goalRecordMetadata(row)
		if row.revision != 2 || metadata.State != nativeGoalVerified || metadata.Closure == nil {
			t.Fatal("population cascade was incomplete")
		}
	}
	p.records["EXTRA"] = p.records["SOURCE-0000"]
	if _, err := goalDependencyChanges(p, "TARGET", primary); err == nil {
		t.Fatal("accepted population overflow")
	}
}

func coverageLastFrame(t *testing.T, m *Manager) (*os.File, []byte, uint64, goalJournalEvent) {
	t.Helper()
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	journal, err := readGoalPrivate(root, goalJournalName, goalJournalMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	offset := 0
	for offset < len(journal) {
		size := goalJournalHeaderBytes + int(binary.BigEndian.Uint32(journal[offset+16:offset+20]))
		if offset+size == len(journal) {
			break
		}
		offset += size
	}
	e, err := decodeGoalJournalEvent(journal[offset+goalJournalHeaderBytes:])
	if err != nil {
		t.Fatal(err)
	}
	return root, journal[:offset:offset], binary.BigEndian.Uint64(journal[offset+8 : offset+16]), e
}

func TestCoverageReplayIntegrity(t *testing.T) {
	for _, kind := range []string{"omitted", "partial", "forged", "duplicate", "revision", "cycle"} {
		t.Run(kind, func(t *testing.T) {
			m, req := coverageTransactionFixture(t)
			if _, _, err := m.commitGoal(req, goalCommitIO{}); err != nil {
				t.Fatal(err)
			}
			root, prefix, sequence, e := coverageLastFrame(t, m)
			var total int64
			b, err := readGoalBlob(root, e.ChangesRef, "goal-changes-", goalChangesMaxBytes, &total)
			if err != nil {
				t.Fatal(err)
			}
			rows, err := decodeGoalChanges(b)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "omitted":
				e.ChangesRef = ""
			case "partial":
				rows = rows[:len(rows)-1]
			case "duplicate":
				rows = append(rows, rows[len(rows)-1])
			case "revision":
				rows[0].Revision++
			case "forged":
				record, err := readGoalBlob(root, rows[0].RecordRef, "goal-record-", goalRecordMaxBytes, &total)
				if err != nil {
					t.Fatal(err)
				}
				data, _ := decodeGoalRecord(record)
				data.Closure.Evidence = "unreviewed-substitution"
				changed, _ := encodeGoalRecord(data)
				rows[0].RecordRef = goalReplayBlob(t, m.store, "goal-record-", changed)
			case "cycle":
				spec, _ := decodeNativeGoalSpec(req.Spec)
				spec.After = []string{"SOURCE-00"}
				changed, _ := json.Marshal(spec)
				e.SpecRef = goalReplayBlob(t, m.store, "goal-spec-", changed)
				e.InputSHA256 = goalRecordFingerprint(goalInputFingerprint(e.GoalID, e.Revision-1, e.SpecRef, e.SealRef), e.RecordRef, e.MutationSHA256)
			}
			if kind != "omitted" {
				changed, _ := json.Marshal(rows)
				e.ChangesRef = goalReplayBlob(t, m.store, "goal-changes-", changed)
			}
			payload, _ := json.Marshal(e)
			before := goalCommitTree(t, m.store)
			projection, err := replayGoalJournalData(root, append(prefix, encodeGoalFrame(sequence, payload)...))
			if !errors.Is(err, errGoalReplay) || projection != nil {
				t.Fatal("replay accepted incomplete or forged cascade")
			}
			if !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
				t.Fatal("replay wrote history")
			}
		})
	}
}

func TestCoverageStrictKeys(t *testing.T) {
	digest := strings.Repeat("a", 64)
	row := []byte(fmt.Sprintf(`[{"goal_id":"SOURCE","revision":1,"spec_ref":"goal-spec-%s","record_ref":"goal-record-%s","seal_ref":"goal-seal-%s"}]`, digest, digest, digest))
	record := []byte(`{"version":1,"state":"verified","notes":[],"closure":{"target":"TARGET","target_revision":2,"authority":"approved-plan","evidence":"review-record"}}`)
	items := []struct {
		b      []byte
		key    string
		decode func([]byte) error
	}{}
	for _, key := range []string{"goal_id", "revision", "spec_ref", "record_ref", "seal_ref"} {
		items = append(items, struct {
			b      []byte
			key    string
			decode func([]byte) error
		}{row, key, func(b []byte) error { _, err := decodeGoalChanges(b); return err }})
	}
	for _, key := range []string{"closure", "target", "target_revision", "authority", "evidence"} {
		items = append(items, struct {
			b      []byte
			key    string
			decode func([]byte) error
		}{record, key, func(b []byte) error { _, err := decodeGoalRecord(b); return err }})
	}
	event := []byte(fmt.Sprintf(`{"kind":"goal.spec","goal_id":"SOURCE","revision":1,"spec_ref":"goal-spec-%s","operation_id":"operation","input_sha256":"%s","changes_ref":"goal-changes-%s"}`, digest, digest, digest))
	items = append(items, struct {
		b      []byte
		key    string
		decode func([]byte) error
	}{event, "changes_ref", func(b []byte) error { _, err := decodeGoalJournalEvent(b); return err }})
	for _, item := range items {
		for _, variant := range []string{strings.ToUpper(item.key), strings.ToUpper(item.key[:1]) + item.key[1:]} {
			changed := bytes.Replace(item.b, []byte(`"`+item.key+`"`), []byte(`"`+variant+`"`), 1)
			if item.decode(changed) == nil {
				t.Fatal("accepted case variant of strict field")
			}
			duplicate := bytes.Replace(item.b, []byte(`"`+item.key+`":`), []byte(`"`+variant+`":"SOURCE","`+item.key+`":`), 1)
			if item.decode(duplicate) == nil {
				t.Fatal("accepted duplicate field by case")
			}
		}
		escape := `"\u` + fmt.Sprintf("%04x", item.key[0]) + item.key[1:] + `"`
		escaped := bytes.Replace(item.b, []byte(`"`+item.key+`"`), []byte(escape), 1)
		if item.decode(escaped) != nil {
			t.Fatal("rejected decoded canonical key")
		}
		duplicate := bytes.Replace(item.b, []byte(`"`+item.key+`":`), []byte(escape+`:"SOURCE","`+item.key+`":`), 1)
		if item.decode(duplicate) == nil {
			t.Fatal("accepted escaped duplicate")
		}
	}
}

func TestCoverageProvenanceOneLine(t *testing.T) {
	for _, key := range []string{"authority", "evidence"} {
		for _, sep := range []string{"\u2028", "\u2029", "\u0085"} {
			record := []byte(`{"version":1,"state":"verified","notes":[],"closure":{"target":"TARGET","target_revision":2,"authority":"approved-plan","evidence":"review-record"}}`)
			value, _ := json.Marshal("first" + sep + "second")
			old := map[string]string{"authority": "approved-plan", "evidence": "review-record"}[key]
			record = bytes.Replace(record, []byte(`"`+old+`"`), value, 1)
			if _, err := decodeGoalRecord(record); err == nil {
				t.Error("accepted multiline closure provenance")
			}
		}
	}
}

func TestDependencyCycles(t *testing.T) {
	for _, coverage := range []bool{false, true} {
		t.Run(map[bool]string{false: "dependency", true: "mixed"}[coverage], func(t *testing.T) {
			m, req := goalRecordsFixture(t)
			req.Spec.ID, req.Spec.After = "FIRST", []string{"FORWARD"}
			if _, err := m.createGoal(req); err != nil {
				t.Fatal(err)
			}
			req.OperationID, req.Spec.ID, req.Spec.After = "forward", "FORWARD", []string{"FIRST"}
			if coverage {
				req.Spec.After = []string{}
				req.Spec.Coverage = &nativeCoverageHold{CoveredBy: "FIRST", Reason: "Covered scope"}
			}
			if _, err := m.createGoal(req); err == nil {
				t.Error("accepted combined cycle through forward reference")
			}
		})
	}
}

func TestCoveredClosureRunning(t *testing.T) {
	m, _ := goalRecordsFixture(t)
	source := dependencyFixture(t, m, "SOURCE", nativeGoalRunning, nil)
	source.Coverage = &nativeCoverageHold{CoveredBy: "TARGET", Reason: "Covered scope"}
	b, _ := json.Marshal(source)
	p, _ := replayGoalJournal(m.store)
	record := []byte(p.records["SOURCE"].record)
	before := goalCommitTree(t, m.store)
	if _, _, err := m.commitGoal(goalCommitRequest{OperationID: "cover-running", ExpectedRevision: 1, Spec: b, Record: record, MutationSHA256: strings.Repeat("b", 64)}, goalCommitIO{}); err == nil {
		t.Error("covered running source")
	}
	if !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
		t.Error("refusal changed store")
	}
}

func TestCoveredClosureAlreadyVerified(t *testing.T) {
	m, req := goalRecordsFixture(t)
	dependencyFixture(t, m, "TARGET", nativeGoalEnded, nil)
	verifyDependency(t, m, "TARGET")
	req.Spec.ID = "SOURCE"
	req.Spec.Coverage = &nativeCoverageHold{CoveredBy: "TARGET", Reason: "Accepted target covers this work"}
	if _, err := m.createGoal(req); err != nil {
		t.Fatal(err)
	}
	row, _, _ := m.readGoal("SOURCE")
	if row.State != nativeGoalVerified {
		t.Error("already accepted target did not close new source")
	}
}

func TestCoverageOneLine(t *testing.T) {
	for _, sep := range []string{"\u2028", "\u2029", "\u0085"} {
		m, req := goalRecordsFixture(t)
		req.Spec.Coverage = &nativeCoverageHold{CoveredBy: "TARGET", Reason: "First" + sep + "second"}
		if _, err := m.createGoal(req); !errors.Is(err, errGoalRecord) {
			t.Error("accepted multiline coverage reason")
		}
		b, _ := json.Marshal(req.Spec)
		if _, err := decodeNativeGoalSpec(b); err == nil {
			t.Error("replay decoder accepted multiline reason")
		}
	}
}
