package nuntius

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

func goalRecordsFixture(t *testing.T) (*Manager, goalRecordRequest) {
	t.Helper()
	m, raw := goalCommitFixture(t)
	spec, err := decodeNativeGoalSpec(raw.Spec)
	if err != nil {
		t.Fatal("invalid record fixture")
	}
	return m, goalRecordRequest{OperationID: "record-create", Spec: spec}
}

func TestGoalRecords(t *testing.T) {
	t.Run("draft-and-cas", func(t *testing.T) {
		m, req := goalRecordsFixture(t)
		first, err := m.createGoal(req)
		if err != nil || first.Revision != 1 || first.Sequence != 1 {
			t.Fatal("creation did not save a drafted record")
		}
		v, found, err := m.readGoal(req.Spec.ID)
		if err != nil || !found || v.State != nativeGoalDrafted || v.ApprovalRevision != 0 || v.GrantBound {
			t.Fatal("creation approved or lost record")
		}
		for _, name := range []string{"runs", "queue.json", "lane-control.json"} {
			if _, err := os.Lstat(filepath.Join(m.store, name)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("creation had launch or control effects")
			}
		}
		before := goalCommitTree(t, m.store)
		req.Spec.Title = "Updated record"
		req.OperationID = "record-update"
		if _, err := m.updateGoal(req); !errors.Is(err, errGoalConflict) {
			t.Fatal("stale record edit succeeded")
		}
		if !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
			t.Fatal("stale edit wrote files")
		}
		req.ExpectedRevision = 1
		second, err := m.updateGoal(req)
		if err != nil || second.Revision != 2 {
			t.Fatal("typed CAS update failed")
		}
		before = goalCommitTree(t, m.store)
		again, err := (&Manager{store: m.store}).updateGoal(req)
		if err != nil || again != second || !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
			t.Fatal("record operation was not idempotent after restart")
		}
		req.Spec.Title = "Different request"
		if _, err := m.updateGoal(req); !errors.Is(err, errGoalOperation) {
			t.Fatal("operation reuse accepted different input")
		}
		req.OperationID = "different-create"
		req.ExpectedRevision = 0
		if _, err := m.createGoal(req); !errors.Is(err, errGoalConflict) {
			t.Fatal("create overwrote an existing goal")
		}
	})
	t.Run("notes-restart", func(t *testing.T) {
		m, req := goalRecordsFixture(t)
		if _, err := m.createGoal(req); err != nil {
			t.Fatal("create failed")
		}
		note := goalNoteRequest{OperationID: "note-1", ExpectedRevision: 1, GoalID: req.Spec.ID, Note: goalRecordNote{Text: "First review", Evidence: "review-1"}}
		first, err := m.noteGoal(note)
		if err != nil || first.Revision != 2 {
			t.Fatal("note append failed")
		}
		note2 := note
		note2.OperationID, note2.ExpectedRevision, note2.Note.Text = "note-2", 2, "Correction evidence"
		if _, err := m.noteGoal(note2); err != nil {
			t.Fatal("second note append failed")
		}
		restarted := &Manager{store: m.store}
		before := goalCommitTree(t, m.store)
		again, err := restarted.noteGoal(note)
		if err != nil || again != first || !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
			t.Fatal("old note retry appended or lost original receipt")
		}
		v, found, err := restarted.readGoal(req.Spec.ID)
		if err != nil || !found || v.Revision != 3 || len(v.Notes) != 2 || v.Notes[0] != note.Note || v.Notes[1] != note2.Note {
			t.Fatal("append-only note history did not survive restart")
		}
		v.Notes[0].Text = "caller mutation"
		fresh, _, _ := restarted.readGoal(req.Spec.ID)
		if fresh.Notes[0] != note.Note {
			t.Fatal("caller changed stored notes")
		}
		if !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
			t.Fatal("record reads changed files")
		}
	})
	t.Run("missing-and-corrupt", func(t *testing.T) {
		m, _ := goalRecordsFixture(t)
		before := goalCommitTree(t, m.store)
		list, err := m.listGoals(0, 10)
		if err != nil || list.StorePresent || len(list.Open) != 0 || !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
			t.Fatal("initial absent journal was not an empty read-only list")
		}
		if _, err := (&Manager{store: filepath.Join(m.store, "absent")}).listGoals(0, 10); !errors.Is(err, errGoalStoreMissing) {
			t.Fatal("missing root did not give a distinct error")
		}
		goalReplayWrite(t, m.store, goalJournalName, []byte("corrupt"))
		before = goalCommitTree(t, m.store)
		if list, err := m.listGoals(0, 10); list != nil || !errors.Is(err, errGoalReplay) {
			t.Fatal("corrupt history appeared empty")
		}
		if !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
			t.Fatal("failed read repaired history")
		}
	})
	t.Run("seal-create-refused", func(t *testing.T) {
		m, req := goalRecordsFixture(t)
		req.Seal = []byte("sealed fixture")
		before := goalCommitTree(t, m.store)
		if _, err := m.createGoal(req); !errors.Is(err, errGoalRecord) || !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
			t.Fatal("creation bound a seal or wrote artifacts")
		}
	})
}

// Kept as a baseline sentinel: reads must not create a journal even when empty.
func TestGoalRecordsNoReadWrites(t *testing.T) {
	m, _ := goalRecordsFixture(t)
	before := goalCommitTree(t, m.store)
	_, _, _ = m.readGoal("MISSING-GOAL")
	_, _ = m.listGoals(0, 1)
	if !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
		t.Fatal("read created storage")
	}
	if bytes.Contains([]byte(errGoalSnapshot.Error()), []byte(m.store)) {
		t.Fatal("error disclosed a private path")
	}
}

// Only a storage fixture may introduce lifecycle states here. There is no
// production approve/verify/dispatch operation in B05.
func goalRecordsState(t *testing.T, m *Manager, req goalRecordRequest, state nativeGoalState) goalCommitRequest {
	t.Helper()
	metadata := goalRecordData{Version: 1, State: state, Notes: []goalRecordNote{}}
	if state == nativeGoalApproved {
		metadata.ApprovalRevision, metadata.GrantBinding = 1, "batch-fixture"
		req.Seal = []byte("sealed fixture")
	}
	if state == nativeGoalVerified {
		metadata.Outcome = nativeOutcomeCompleted
	}
	record, _ := json.Marshal(metadata)
	spec, _ := json.Marshal(req.Spec)
	commit := goalCommitRequest{OperationID: req.OperationID, Spec: spec, Seal: req.Seal, Record: record, MutationSHA256: strings.Repeat("a", 64)}
	if _, _, err := m.commitGoal(commit, goalCommitIO{}); err != nil {
		t.Fatal("lifecycle storage fixture failed")
	}
	return commit
}

func TestGoalRecordsTerminal(t *testing.T) {
	for _, state := range []nativeGoalState{nativeGoalVerified, nativeGoalCancelled} {
		t.Run(string(state), func(t *testing.T) {
			m, req := goalRecordsFixture(t)
			commit := goalRecordsState(t, m, req, state)
			before := goalCommitTree(t, m.store)
			req.OperationID, req.ExpectedRevision, req.Spec.Title = "terminal-edit", 1, "Rewritten result"
			if _, err := m.updateGoal(req); !errors.Is(err, errGoalTerminal) {
				t.Fatal("terminal edit succeeded")
			}
			if !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
				t.Fatal("terminal edit staged artifacts")
			}
			// The storage writer and replay independently enforce the same immutable result.
			commit.OperationID, commit.ExpectedRevision = "storage-rewrite", 1
			commit.Spec, _ = json.Marshal(req.Spec)
			if _, _, err := m.commitGoal(commit, goalCommitIO{}); err == nil || !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
				t.Fatal("storage rewrote terminal spec")
			}
			note := goalNoteRequest{OperationID: "terminal-note", ExpectedRevision: 1, GoalID: req.Spec.ID, Note: goalRecordNote{Text: "Correct the interpretation", Evidence: "review-2"}}
			if _, err := m.noteGoal(note); err != nil {
				t.Fatal("terminal correction evidence refused")
			}
			v, found, err := (&Manager{store: m.store}).readGoal(req.Spec.ID)
			if err != nil || !found || v.State != state || v.Title == req.Spec.Title || len(v.Notes) != 1 || v.Notes[0] != note.Note {
				t.Fatal("correction changed terminal result or disappeared")
			}
			// A corrupt yet checksummed new frame must not bypass immutability.
			p, _ := replayGoalJournal(m.store)
			changed := p.records[req.Spec.ID]
			e := goalJournalEvent{Kind: "goal.spec", GoalID: req.Spec.ID, Revision: 3, SpecRef: goalReplayBlob(t, m.store, "goal-spec-", commit.Spec), RecordRef: goalReplayBlob(t, m.store, "goal-record-", []byte(changed.record)), OperationID: "corrupt-terminal", MutationSHA256: strings.Repeat("b", 64)}
			e.InputSHA256 = goalRecordFingerprint(goalInputFingerprint(e.GoalID, 2, e.SpecRef, ""), e.RecordRef, e.MutationSHA256)
			payload, _ := json.Marshal(e)
			journal, _ := os.ReadFile(filepath.Join(m.store, goalJournalName))
			goalReplayWrite(t, m.store, goalJournalName, append(journal, encodeGoalFrame(3, payload)...))
			if _, err := replayGoalJournal(m.store); !errors.Is(err, errGoalReplay) {
				t.Fatal("replay accepted terminal rewrite")
			}
		})
	}
}

func TestGoalRecordsRevoke(t *testing.T) {
	for _, change := range []string{"scope", "seal", "remove-seal", "lane", "title"} {
		t.Run(change, func(t *testing.T) {
			m, req := goalRecordsFixture(t)
			goalRecordsState(t, m, req, nativeGoalApproved)
			req.OperationID, req.ExpectedRevision, req.Seal = "approved-update", 1, []byte("sealed fixture")
			switch change {
			case "scope":
				req.Spec.Writes = []string{"repo/docs"}
			case "seal":
				req.Seal = []byte("another sealed fixture")
			case "remove-seal":
				req.Seal = nil
			case "lane":
				req.Spec.PreferredLane = "codex-other"
			case "title":
				req.Spec.Title = "Clearer title"
			}
			if _, err := m.updateGoal(req); err != nil {
				t.Fatal("approved record update failed")
			}
			v, _, err := (&Manager{store: m.store}).readGoal(req.Spec.ID)
			if err != nil {
				t.Fatal("changed record unreadable")
			}
			if change == "title" {
				if v.State != nativeGoalApproved || !v.GrantBound || v.ApprovalRevision != 1 {
					t.Fatal("presentation edit changed scope binding")
				}
			} else if v.State != nativeGoalDrafted || v.GrantBound || v.ApprovalRevision != 0 {
				t.Fatal("scope or seal change retained approval or grant binding")
			}
		})
	}
}

func TestGoalRecordsListings(t *testing.T) {
	m, req := goalRecordsFixture(t)
	states := []nativeGoalState{nativeGoalDrafted, nativeGoalApproved, nativeGoalRunning, nativeGoalEnded, nativeGoalParked, nativeGoalVerified, nativeGoalCancelled, nativeGoalVerified}
	for i, state := range states {
		req.Spec.ID, req.OperationID = fmt.Sprintf("RECORD-%02d", i), fmt.Sprintf("fixture-%d", i)
		goalRecordsState(t, m, req, state)
	}
	before := goalCommitTree(t, m.store)
	first, err := m.listGoals(0, 2)
	if err != nil || !first.StorePresent || first.Sequence != 8 || len(first.Open) != 5 || first.TerminalTotal != 3 || len(first.Terminal) != 2 || first.NextOffset != 2 {
		t.Fatal("listing mixed counts, omitted open records, or lost terminal pagination")
	}
	second, err := m.listGoals(2, 2)
	if err != nil || len(second.Open) != 5 || len(second.Terminal) != 1 || second.TerminalTotal != 3 || second.NextOffset != 0 || second.Sequence != first.Sequence {
		t.Fatal("terminal page changed full open parent")
	}
	for i, v := range first.Open {
		if v.ID != fmt.Sprintf("RECORD-%02d", i) || v.State != states[i] {
			t.Fatal("listing did not preserve complete open ID/state set")
		}
	}
	if first.Terminal[0].ID != "RECORD-05" || first.Terminal[1].ID != "RECORD-06" || second.Terminal[0].ID != "RECORD-07" {
		t.Fatal("terminal pages overlapped or skipped IDs")
	}
	for _, page := range [][2]int{{-1, 1}, {0, 0}, {0, 101}, {4, 1}} {
		if _, err := m.listGoals(page[0], page[1]); err == nil {
			t.Fatal("invalid terminal page accepted")
		}
	}
	if !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
		t.Fatal("listing scheduled or wrote files")
	}
	// Independent fixture population exceeds the encoded response budget.
	p := &goalProjection{records: map[string]goalReplayRecord{}, sequence: 4096}
	metadata := goalRecordData{Version: 1, State: nativeGoalDrafted, Notes: []goalRecordNote{}}
	for range goalRecordMaxNotes {
		metadata.Notes = append(metadata.Notes, goalRecordNote{Text: strings.Repeat("x", goalRecordNoteMaxBytes)})
	}
	record, _ := json.Marshal(metadata)
	for i := range goalJournalMaxGoals {
		req.Spec.ID = fmt.Sprintf("OPEN-%04d", i)
		spec, _ := json.Marshal(req.Spec)
		p.records[req.Spec.ID] = goalReplayRecord{revision: 1, spec: string(spec), record: string(record)}
	}
	if list, err := goalRecordList(p, true, 0, 1); list != nil || !errors.Is(err, errGoalSnapshot) || len(p.records) != goalJournalMaxGoals {
		t.Fatal("oversize snapshot succeeded with truncated open records or eviction")
	}
}

func TestGoalRecordsBoundsAndSafety(t *testing.T) {
	m, req := goalRecordsFixture(t)
	if _, err := m.createGoal(req); err != nil {
		t.Fatal("create failed")
	}
	for i := range goalRecordMaxNotes {
		n := goalNoteRequest{OperationID: fmt.Sprintf("note-%d", i), ExpectedRevision: uint64(i + 1), GoalID: req.Spec.ID, Note: goalRecordNote{Text: strings.Repeat("x", goalRecordNoteMaxBytes)}}
		if _, err := m.noteGoal(n); err != nil {
			t.Fatal("exact note boundary refused")
		}
	}
	before := goalCommitTree(t, m.store)
	n := goalNoteRequest{OperationID: "overflow", ExpectedRevision: 33, GoalID: req.Spec.ID, Note: goalRecordNote{Text: "Overflow"}}
	if _, err := m.noteGoal(n); !errors.Is(err, errGoalNotesFull) || !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
		t.Fatal("note overflow evicted history or wrote files")
	}
	for _, text := range []string{"", strings.Repeat("x", goalRecordNoteMaxBytes+1), "line\nbreak", " leading"} {
		n.Note.Text = text
		if _, err := m.noteGoal(n); !errors.Is(err, errGoalRecord) {
			t.Fatal("invalid note accepted")
		}
	}
	base := `{"version":1,"state":"drafted","notes":[]}`
	for _, bad := range []string{strings.Replace(base, `"notes":[]`, `"notes":null`, 1), strings.Replace(base, `"version":1`, `"version":1.0`, 1), strings.Replace(base, `"state":"drafted"`, `"state":"unknown"`, 1), strings.Replace(base, `"notes":[]`, `"notes":[{"text":"ok","extra":true}]`, 1), strings.Replace(base, `"notes":[]`, `"notes":[],"grant_binding":"batch"`, 1), strings.Replace(base, `"version":1`, `"version":1,"version":1`, 1), base + base} {
		if _, err := decodeGoalRecord([]byte(bad)); !errors.Is(err, errGoalRecord) {
			t.Fatal("invalid closed record metadata accepted")
		}
	}
	readonly := &Manager{store: m.store, readOnly: true}
	n.Note.Text = "Valid"
	if _, err := readonly.noteGoal(n); err == nil {
		t.Fatal("read-only manager changed record")
	}
	parent := goalReplayDir(t)
	if err := os.Symlink(m.store, filepath.Join(parent, "linked")); err != nil {
		t.Fatal("link fixture failed")
	}
	if _, err := (&Manager{store: filepath.Join(parent, "linked")}).listGoals(0, 1); !errors.Is(err, errGoalReplay) {
		t.Fatal("unsafe root appeared missing or readable")
	}
	if !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
		t.Fatal("refused operation changed private files")
	}
}

func TestGoalRecordsConcurrentCAS(t *testing.T) {
	m, req := goalRecordsFixture(t)
	if _, err := m.createGoal(req); err != nil {
		t.Fatal("create failed")
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := range 2 {
		wg.Go(func() {
			edit := req
			edit.ExpectedRevision, edit.OperationID, edit.Spec.Title = 1, fmt.Sprintf("edit-%d", i), fmt.Sprintf("Title %d", i)
			_, err := m.updateGoal(edit)
			results <- err
		})
	}
	wg.Wait()
	close(results)
	won, conflict := 0, 0
	for err := range results {
		if err == nil {
			won++
		} else if errors.Is(err, errGoalConflict) {
			conflict++
		} else {
			t.Fatal("unexpected CAS refusal")
		}
	}
	if won != 1 || conflict != 1 {
		t.Fatal("concurrent edits did not have exactly one winner")
	}
}

func TestGoalRecordsReceiptAndLock(t *testing.T) {
	m, req := goalRecordsFixture(t)
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		t.Fatal("root fixture failed")
	}
	defer root.Close()
	lock, err := laneControlLock(root, unix.LOCK_EX)
	if err != nil {
		t.Fatal("lock fixture failed")
	}
	before := goalCommitTree(t, m.store)
	if _, err := m.createGoal(req); err == nil || !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
		t.Fatal("typed records bypassed shared control lock")
	}
	if lock.Close() != nil {
		t.Fatal("fixture lock close failed")
	}
	first, err := m.createGoal(req)
	if err != nil {
		t.Fatal("create after lock release failed")
	}
	if _, err := m.noteGoal(goalNoteRequest{OperationID: "later-note", ExpectedRevision: 1, GoalID: req.Spec.ID, Note: goalRecordNote{Text: "Later review"}}); err != nil {
		t.Fatal("note failed")
	}
	before = goalCommitTree(t, m.store)
	again, err := (&Manager{store: m.store}).createGoal(req)
	if err != nil || again != first || !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
		t.Fatal("creation retry lost original result after a later edit")
	}
	// Record metadata cannot be omitted to erase note history.
	spec, _ := json.Marshal(req.Spec)
	if _, _, err := m.commitGoal(goalCommitRequest{OperationID: "drop-notes", ExpectedRevision: 2, Spec: spec}, goalCommitIO{}); err == nil || !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
		t.Fatal("spec-only update erased notes")
	}
	// Invalid typed UTF-8 must refuse before JSON encoding can replace it.
	req.Spec.ID, req.OperationID, req.Spec.Title = "INVALID-TITLE", "invalid-title", string([]byte{0xff})
	if _, err := m.createGoal(req); !errors.Is(err, errGoalRecord) || !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
		t.Fatal("typed input normalized invalid title or wrote files")
	}
}

func TestGoalRecordsSnapshotByteBoundary(t *testing.T) {
	// Construct an independent DTO oracle without the production listing function.
	// ASCII note lengths tune the exact encoded envelope boundary, one byte at a time.
	_, req := goalRecordsFixture(t)
	p := &goalProjection{sequence: 32, records: map[string]goalReplayRecord{}}
	oracle := goalRecordListing{Sequence: 32, StorePresent: true, OpenTotal: 32, Open: []nativeGoalRecordDTO{}, Terminal: []nativeGoalRecordDTO{}}
	for i := range 32 {
		req.Spec.ID = fmt.Sprintf("BOUNDARY-%02d", i)
		metadata := goalRecordData{Version: 1, State: nativeGoalDrafted, Notes: []goalRecordNote{}}
		for range goalRecordMaxNotes {
			metadata.Notes = append(metadata.Notes, goalRecordNote{Text: strings.Repeat("x", goalRecordNoteMaxBytes)})
		}
		spec, _ := json.Marshal(req.Spec)
		record, _ := json.Marshal(metadata)
		p.records[req.Spec.ID] = goalReplayRecord{revision: 1, spec: string(spec), record: string(record)}
		v, err := goalRecordView(p.records[req.Spec.ID])
		if err != nil {
			t.Fatal("boundary fixture invalid")
		}
		oracle.Open = append(oracle.Open, v)
	}
	encoded, _ := json.Marshal(oracle)
	excess := len(encoded) - goalRecordSnapshotMaxBytes
	if excess <= 0 {
		t.Fatal("boundary fixture did not exceed response budget")
	}
	for i := range oracle.Open {
		for j := range oracle.Open[i].Notes {
			take := min(excess, len(oracle.Open[i].Notes[j].Text)-1)
			oracle.Open[i].Notes[j].Text = oracle.Open[i].Notes[j].Text[:len(oracle.Open[i].Notes[j].Text)-take]
			excess -= take
		}
		metadata := goalRecordData{Version: 1, State: nativeGoalDrafted, Notes: oracle.Open[i].Notes}
		record, _ := json.Marshal(metadata)
		r := p.records[oracle.Open[i].ID]
		r.record = string(record)
		p.records[oracle.Open[i].ID] = r
	}
	encoded, _ = json.Marshal(oracle)
	if len(encoded) != goalRecordSnapshotMaxBytes {
		t.Fatal("oracle did not tune exact encoded boundary")
	}
	list, err := goalRecordList(p, true, 0, 1)
	if err != nil || !reflect.DeepEqual(list, &oracle) {
		t.Fatal("exact snapshot byte boundary refused or changed rows")
	}
	r := p.records[oracle.Open[0].ID]
	metadata, _ := goalRecordMetadata(r)
	metadata.Notes[0].Text += "x"
	record, _ := json.Marshal(metadata)
	r.record = string(record)
	p.records[oracle.Open[0].ID] = r
	if list, err := goalRecordList(p, true, 0, 1); list != nil || !errors.Is(err, errGoalSnapshot) {
		t.Fatal("one-byte snapshot overflow returned partial success")
	}
}

func TestGoalRecordsUnconfirmed(t *testing.T) {
	m, req := goalRecordsFixture(t)
	requestBytes, _ := json.Marshal(struct {
		Kind   string
		Record goalRecordRequest
		Note   goalNoteRequest
	}{"create", req, goalNoteRequest{}})
	// Match the canonical typed request, then inject the existing fsync fault.
	mutation := strings.TrimPrefix(goalBlobName("goal-record-", requestBytes), "goal-record-")
	spec, _ := json.Marshal(req.Spec)
	record, _ := json.Marshal(goalRecordData{Version: 1, State: nativeGoalDrafted, Notes: []goalRecordNote{}})
	commit := goalCommitRequest{OperationID: req.OperationID, Spec: spec, Record: record, MutationSHA256: mutation}
	result, p, err := m.commitGoal(commit, goalCommitIO{checkpoint: func(stage string) error {
		if stage == "journal_sync" {
			return errors.New("injected sync failure")
		}
		return nil
	}})
	if !errors.Is(err, errGoalUnconfirmed) || p != nil || result.Revision != 1 {
		t.Fatal("record fault falsely confirmed or lost recorded result")
	}
	before := goalCommitTree(t, m.store)
	lookup, found, err := (&Manager{store: m.store}).lookupGoalOperation(req.OperationID)
	if err != nil || !found || lookup != result || !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
		t.Fatal("read-only uncertain receipt lookup changed storage")
	}
	confirmed, err := (&Manager{store: m.store}).createGoal(req)
	if err != nil || confirmed != result || !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
		t.Fatal("typed retry failed to confirm existing event without duplicate append")
	}
}
