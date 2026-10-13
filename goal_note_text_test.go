package nuntius

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode"
)

func TestGoalNoteOneLinePredicate(t *testing.T) {
	for _, r := range []rune{'\r', '\n', '\u0085', '\u2028', '\u2029'} {
		for _, field := range []string{"text", "evidence"} {
			t.Run(fmt.Sprintf("%s-U%04X", field, r), func(t *testing.T) {
				note := goalRecordNote{Text: "Ordinary prose", Evidence: "review-reference"}
				if field == "text" {
					note.Text = "Before" + string(r) + "after"
				} else {
					note.Evidence = "Before" + string(r) + "after"
				}
				if validGoalNote(note) {
					t.Fatal("embedded line separator accepted")
				}
			})
		}
	}
}

// These instruments deliberately marshal raw metadata and frame journal bytes
// without encodeGoalRecord/encodeGoalFrame, including for negative fixtures.
func noteTextJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal("synthetic JSON failed")
	}
	return b
}

func noteTextRecord(t *testing.T, n goalRecordNote) []byte {
	return noteTextJSON(t, goalRecordData{Version: 1, State: nativeGoalDrafted, Notes: []goalRecordNote{n}})
}

func noteTextCases() []goalRecordNote {
	var notes []goalRecordNote
	for _, r := range []rune{'\r', '\n', '\u0085', '\u2028', '\u2029'} {
		s := "Before" + string(r) + "after"
		notes = append(notes, goalRecordNote{Text: s}, goalRecordNote{Text: "Ordinary prose", Evidence: s})
	}
	return notes
}

func noteTextTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		var b []byte
		if !e.IsDir() {
			b, err = os.ReadFile(path)
			if err != nil {
				return err
			}
		}
		tree[path] = fmt.Sprintf("%o:", info.Mode()) + string(b)
		return nil
	})
	if err != nil {
		t.Fatal("fixture inventory failed")
	}
	return tree
}

func TestGoalNoteOneLineCodec(t *testing.T) {
	for i, note := range noteTextCases() {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			raw := noteTextRecord(t, note)
			literal := bytes.ReplaceAll(bytes.ReplaceAll(raw, []byte(`\u2028`), []byte("\u2028")), []byte(`\u2029`), []byte("\u2029"))
			for _, b := range [][]byte{raw, literal} {
				if _, err := decodeGoalRecord(b); !errors.Is(err, errGoalRecord) {
					t.Fatal("codec accepted separator")
				}
			}
			if _, err := encodeGoalRecord(goalRecordData{Version: 1, State: nativeGoalDrafted, Notes: []goalRecordNote{note}}); !errors.Is(err, errGoalRecord) {
				t.Fatal("encoder accepted separator")
			}
		})
	}
	for _, note := range []goalRecordNote{{Text: "Café 日本語", Evidence: ""}, {Text: `literal \u2028`, Evidence: `ref-\u2029`}} {
		b, err := encodeGoalRecord(goalRecordData{Version: 1, State: nativeGoalDrafted, Notes: []goalRecordNote{note}})
		r, decodeErr := decodeGoalRecord(b)
		if err != nil || decodeErr != nil || !reflect.DeepEqual(r.Notes, []goalRecordNote{note}) {
			t.Fatal("one-line Unicode or literal escape changed")
		}
	}
}

func TestGoalNoteOneLineWriterUnchanged(t *testing.T) {
	for i, note := range noteTextCases() {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			m, req := goalRecordsFixture(t)
			if _, err := m.createGoal(req); err != nil {
				t.Fatal(err)
			}
			before := noteTextTree(t, m.store)
			if _, err := m.noteGoal(goalNoteRequest{OperationID: "refused-note", ExpectedRevision: 1, GoalID: req.Spec.ID, Note: note}); !errors.Is(err, errGoalRecord) {
				t.Fatal("writer accepted separator")
			}
			if _, _, err := m.commitGoal(goalCommitRequest{OperationID: "refused-candidate", ExpectedRevision: 1, Spec: noteTextJSON(t, req.Spec), Record: noteTextRecord(t, note), MutationSHA256: strings.Repeat("a", 64)}, goalCommitIO{}); !errors.Is(err, errGoalRecord) {
				t.Fatal("raw candidate accepted separator")
			}
			root, err := openLaneControlRoot(m.store)
			if err != nil {
				t.Fatal(err)
			}
			candidate := goalCommitRequest{OperationID: "refused-locked", ExpectedRevision: 1, Spec: noteTextJSON(t, req.Spec), Record: noteTextRecord(t, note), MutationSHA256: strings.Repeat("a", 64)}
			if _, _, err := commitGoalLocked(root, candidate, req.Spec.ID, goalBlobName("goal-spec-", candidate.Spec), "", "unused-before-refusal", goalCommitIO{}); !errors.Is(err, errGoalRecord) {
				t.Fatal("locked candidate accepted separator")
			}
			root.Close()
			if !reflect.DeepEqual(before, noteTextTree(t, m.store)) {
				t.Fatal("refusal changed inventory or bytes")
			}
			valid := goalNoteRequest{OperationID: "valid-note", ExpectedRevision: 1, GoalID: req.Spec.ID, Note: goalRecordNote{Text: "Valid café", Evidence: "review"}}
			result, err := m.noteGoal(valid)
			if err != nil || result.Revision != 2 {
				t.Fatal("valid writer control failed")
			}
			before = noteTextTree(t, m.store)
			if again, err := m.noteGoal(valid); err != nil || again != result {
				t.Fatal("idempotent note receipt changed")
			}
			valid.OperationID = "stale-note"
			if _, err := m.noteGoal(valid); !errors.Is(err, errGoalConflict) || !reflect.DeepEqual(before, noteTextTree(t, m.store)) {
				t.Fatal("stale revision changed history")
			}
			data := goalRecordData{Version: 1, State: nativeGoalDrafted, Notes: []goalRecordNote{valid.Note, {Text: "Second valid note"}}}
			candidate = goalCommitRequest{OperationID: "valid-candidate", ExpectedRevision: 2, Spec: noteTextJSON(t, req.Spec), Record: noteTextJSON(t, data), MutationSHA256: strings.Repeat("b", 64)}
			if result, _, err := m.commitGoal(candidate, goalCommitIO{}); err != nil || result.Revision != 3 {
				t.Fatal("raw candidate positive control failed", err)
			}
			data.Notes = append(data.Notes, goalRecordNote{Text: "Third valid note"})
			candidate.OperationID, candidate.ExpectedRevision, candidate.Record = "valid-locked", 3, noteTextJSON(t, data)
			root, err = openLaneControlRoot(m.store)
			if err != nil {
				t.Fatal(err)
			}
			e := goalJournalEvent{GoalID: req.Spec.ID, Revision: 4, SpecRef: goalBlobName("goal-spec-", candidate.Spec), RecordRef: goalBlobName("goal-record-", candidate.Record), MutationSHA256: candidate.MutationSHA256}
			result, _, err = commitGoalLocked(root, candidate, req.Spec.ID, e.SpecRef, "", noteTextInput(t, e), goalCommitIO{})
			root.Close()
			if err != nil || result.Revision != 4 {
				t.Fatal("locked candidate positive control failed", err)
			}
		})
	}
}

// Re-frame a fixture-owned authoritative journal in a fresh directory. Blob
// checksums, receipts and both frame hashes are recomputed independently. No
// writer or production record/frame encoder constructs negative fixtures.
func noteTextReframe(t *testing.T, source string, note goalRecordNote, onlyGoal string) string {
	t.Helper()
	dir := goalReplayDir(t)
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join(source, name))
		if err != nil {
			t.Fatal("fixture blob missing")
		}
		return b
	}
	copyRef := func(ref string) {
		if ref != "" {
			goalReplayWrite(t, dir, ref, read(ref))
		}
	}
	changeRecord := func(ref, id string) string {
		b := read(ref)
		if onlyGoal == "" || id == onlyGoal {
			var r goalRecordData
			if json.Unmarshal(b, &r) != nil {
				t.Fatal("fixture record JSON failed")
			}
			r.Notes = append(r.Notes, note)
			b = noteTextJSON(t, r)
		}
		return goalReplayBlob(t, dir, "goal-record-", b)
	}
	var journal []byte
	data := read(goalJournalName)
	for len(data) > 0 {
		if len(data) < 84 {
			t.Fatal("unexpected torn fixture")
		}
		size := int(binary.BigEndian.Uint32(data[16:20]))
		if len(data) < 84+size {
			t.Fatal("unexpected partial fixture")
		}
		var e goalJournalEvent
		if json.Unmarshal(data[84:84+size], &e) != nil {
			t.Fatal("fixture event JSON failed")
		}
		copyRef(e.SpecRef)
		copyRef(e.SealRef)
		if e.RecordRef != "" {
			e.RecordRef = changeRecord(e.RecordRef, e.GoalID)
		}
		if e.ChangesRef != "" {
			var rows []goalJournalChange
			if json.Unmarshal(read(e.ChangesRef), &rows) != nil {
				t.Fatal("fixture changes JSON failed")
			}
			for i := range rows {
				copyRef(rows[i].SpecRef)
				copyRef(rows[i].SealRef)
				rows[i].RecordRef = changeRecord(rows[i].RecordRef, rows[i].GoalID)
			}
			e.ChangesRef = goalReplayBlob(t, dir, "goal-changes-", noteTextJSON(t, rows))
		}
		if e.OperationID != "" {
			e.InputSHA256 = noteTextInput(t, e)
		}
		journal = append(journal, noteTextFrame(binary.BigEndian.Uint64(data[8:16]), noteTextJSON(t, e))...)
		data = data[84+size:]
	}
	goalReplayWrite(t, dir, goalJournalName, journal)
	return dir
}

func noteTextReplayCheck(t *testing.T, dir string, valid bool) *goalProjection {
	t.Helper()
	before := noteTextTree(t, dir)
	root, err := openLaneControlRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	p, err := replayGoalJournalRoot(root)
	if valid {
		if err != nil || p == nil || p.tornTail {
			t.Fatal("reframed positive control failed", err)
		}
	} else if !errors.Is(err, errGoalReplay) || p != nil {
		t.Fatal("reframed replay accepted invalid retained note")
	}
	if !reflect.DeepEqual(before, noteTextTree(t, dir)) {
		t.Fatal("reframed replay changed bytes or inventory")
	}
	return p
}

func TestGoalNoteOneLineB13HistoryAndReplay(t *testing.T) {
	linkedParallel(t)
	m, r, h, _ := linkedHeld(t)
	if err := m.discardHeld(h, nil); err != nil {
		t.Fatal(err)
	}
	_, settled := dispatchRecord(t, m, r.GoalID)
	if len(settled.Settlements) != 1 {
		t.Fatal("positive settlement branch not reached")
	}
	linkedControl(t, m, false)
	reply := "wait\nA second legitimate line"
	if _, err := m.Reply("codex", reply); err != nil {
		t.Fatal(err)
	}
	waitReady(t, m, "codex")
	done := linkedDone(t, m, "codex")
	if err := m.Cancel("codex"); err != nil {
		t.Fatal(err)
	}
	linkedWait(t, done)
	_, final := dispatchRecord(t, m, r.GoalID)
	if len(final.Dispatches) != 2 || final.Dispatches[1].Continuation == nil {
		t.Fatal("positive successor branch not reached")
	}
	sum := sha256.Sum256([]byte(reply))
	if final.Dispatches[1].Continuation.InputSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatal("multiline reply digest changed")
	}
	if _, err := decodeGoalRecord(noteTextJSON(t, final)); err != nil {
		t.Fatal("continuation codec control failed")
	}
	for _, separator := range []string{"\r", "\n", "\u0085", "\u2028", "\u2029"} {
		for _, field := range []string{"branch", "parent", "input", "session", "run"} {
			var bad goalRecordData
			if json.Unmarshal(noteTextJSON(t, final), &bad) != nil {
				t.Fatal("metadata control failed")
			}
			c := bad.Dispatches[1].Continuation
			s := "Before" + separator + "after"
			switch field {
			case "branch":
				c.Branch = s
			case "parent":
				c.Parent = s
			case "input":
				c.InputSHA256 = s
			case "session":
				c.SessionSHA256 = s
			case "run":
				bad.Dispatches[1].RunID = s
			}
			if _, err := decodeGoalRecord(noteTextJSON(t, bad)); !errors.Is(err, errGoalRecord) {
				t.Fatal("closed continuation metadata accepted separator")
			}
		}
	}
	valid := goalRecordNote{Text: "Retained café note", Evidence: "review-reference"}
	dir := noteTextReframe(t, m.store, valid, "")
	noteTextReplayCheck(t, dir, true)
	// Every consecutive pair is an actual valid transition, not a manufactured
	// refusal with an invalid seal or unrelated continuation field.
	journal, err := os.ReadFile(filepath.Join(dir, goalJournalName))
	if err != nil {
		t.Fatal(err)
	}
	old := goalReplayRecord{}
	seenEnd, seenSettlement, seenSuccessor := false, false, false
	for len(journal) > 0 {
		size := int(binary.BigEndian.Uint32(journal[16:20]))
		var e goalJournalEvent
		if json.Unmarshal(journal[84:84+size], &e) != nil {
			t.Fatal("event failed")
		}
		spec, _ := os.ReadFile(filepath.Join(dir, e.SpecRef))
		record, _ := os.ReadFile(filepath.Join(dir, e.RecordRef))
		seal, _ := os.ReadFile(filepath.Join(dir, e.SealRef))
		next := goalReplayRecord{revision: e.Revision, spec: string(spec), sealRef: e.SealRef, record: string(record)}
		if !goalRecordHistoryValid(old, next, seal) {
			t.Fatal("positive history pair refused")
		}
		a, _ := goalRecordMetadata(old)
		b, _ := goalRecordMetadata(next)
		seenEnd = seenEnd || len(b.Ends) > len(a.Ends)
		seenSettlement = seenSettlement || len(b.Settlements) > len(a.Settlements)
		seenSuccessor = seenSuccessor || len(b.Dispatches) > 1 && len(b.Dispatches) > len(a.Dispatches)
		for _, note := range noteTextCases() {
			for _, side := range []string{"next", "old", "both"} {
				badOld, badNext := old, next
				if side != "old" {
					x := b
					x.Notes = []goalRecordNote{note}
					badNext.record = string(noteTextJSON(t, x))
				}
				if side != "next" && old.revision > 0 {
					x := a
					x.Notes = []goalRecordNote{note}
					badOld.record = string(noteTextJSON(t, x))
				} else if side == "old" {
					continue
				}
				if goalRecordHistoryValid(badOld, badNext, seal) {
					t.Fatal("history shortcut retained invalid note")
				}
			}
		}
		old = next
		journal = journal[84+size:]
	}
	if !seenEnd || !seenSettlement || !seenSuccessor {
		t.Fatal("history branch controls incomplete")
	}
	for _, note := range noteTextCases() {
		noteTextReplayCheck(t, noteTextReframe(t, m.store, note, ""), false)
	}
}

func TestGoalNoteOneLineDependentReplay(t *testing.T) {
	m, req := coverageTransactionFixture(t)
	if _, _, err := m.commitGoal(req, goalCommitIO{}); err != nil {
		t.Fatal(err)
	}
	control := noteTextReplayCheck(t, noteTextReframe(t, m.store, goalRecordNote{Text: "Dependent valid note"}, "SOURCE-00"), true)
	data, err := goalRecordMetadata(control.records["SOURCE-00"])
	if err != nil || data.Closure == nil || data.State != nativeGoalVerified {
		t.Fatal("covered population control did not close")
	}
	for _, note := range noteTextCases() {
		noteTextReplayCheck(t, noteTextReframe(t, m.store, note, "SOURCE-00"), false)
	}
}

func noteTextFrame(sequence uint64, payload []byte) []byte {
	h := make([]byte, 84)
	copy(h, "NGJF")
	binary.BigEndian.PutUint32(h[4:8], 1)
	binary.BigEndian.PutUint64(h[8:16], sequence)
	binary.BigEndian.PutUint32(h[16:20], uint32(len(payload)))
	header, body := sha256.Sum256(h[:20]), sha256.Sum256(payload)
	copy(h[20:52], header[:])
	copy(h[52:84], body[:])
	return append(h, payload...)
}

func noteTextInput(t *testing.T, e goalJournalEvent) string {
	b := noteTextJSON(t, struct {
		ID       string
		Revision uint64
		Spec     string
		Seal     string
	}{e.GoalID, e.Revision - 1, e.SpecRef, e.SealRef})
	sum := sha256.Sum256(b)
	input := hex.EncodeToString(sum[:])
	if e.RecordRef != "" {
		sum = sha256.Sum256(noteTextJSON(t, []string{input, e.RecordRef, e.MutationSHA256}))
		input = hex.EncodeToString(sum[:])
	}
	return input
}

func TestGoalNoteOneLineFreshReplay(t *testing.T) {
	for _, branch := range []string{"initial", "next", "retained", "reused"} {
		t.Run(branch, func(t *testing.T) {
			for _, note := range append([]goalRecordNote{{Text: "Valid café", Evidence: "review"}}, noteTextCases()...) {
				dir := goalReplayDir(t)
				spec := nativeSchemaFixture()
				ref := goalReplayBlob(t, dir, "goal-spec-", nativeSchemaBytes(t, spec))
				var journal []byte
				for revision := uint64(1); revision <= 2; revision++ {
					n := note
					if branch == "next" && revision == 1 {
						n = goalRecordNote{Text: "Earlier valid"}
					}
					data := goalRecordData{Version: 1, State: nativeGoalDrafted, Notes: []goalRecordNote{n}}
					if branch == "next" && revision == 2 {
						data.Notes = []goalRecordNote{{Text: "Earlier valid"}, note}
					}
					if branch == "retained" && revision == 2 {
						data.Notes = append(data.Notes, goalRecordNote{Text: "Later valid"})
					}
					e := goalJournalEvent{Kind: "goal.spec", GoalID: spec["id"].(string), Revision: revision, SpecRef: ref, RecordRef: goalReplayBlob(t, dir, "goal-record-", noteTextJSON(t, data)), OperationID: fmt.Sprintf("note-frame-%d", revision), MutationSHA256: strings.Repeat("a", 64)}
					e.InputSHA256 = noteTextInput(t, e)
					journal = append(journal, noteTextFrame(revision, noteTextJSON(t, e))...)
					if branch == "initial" {
						break
					}
				}
				goalReplayWrite(t, dir, "goal-journal.bin", journal)
				root, err := openLaneControlRoot(dir)
				if err != nil {
					t.Fatal(err)
				}
				before := noteTextTree(t, dir)
				p, err := replayGoalJournalRoot(root)
				root.Close()
				if note.Text == "Valid café" {
					if err != nil || p == nil || p.tornTail {
						t.Fatal("positive framed replay failed", err)
					}
				} else if !errors.Is(err, errGoalReplay) || p != nil {
					t.Fatal("fresh framed replay accepted separator")
				}
				if !reflect.DeepEqual(before, noteTextTree(t, dir)) {
					t.Fatal("fresh replay wrote fixture")
				}
			}
		})
	}
}

func TestGoalNoteOneLineCapacityAndSafety(t *testing.T) {
	if !unicode.Is(unicode.Zl, '\u2028') || !unicode.Is(unicode.Zp, '\u2029') {
		t.Fatal("Unicode category control failed")
	}
	n := goalRecordNote{Text: strings.Repeat("é", 512), Evidence: strings.Repeat("é", 64)}
	if len(n.Text) != 1024 || len(n.Evidence) != 128 || !validGoalNote(n) {
		t.Fatal("UTF-8 byte boundary refused")
	}
	for _, field := range []string{"text", "evidence"} {
		bad := n
		if field == "text" {
			bad.Text += "x"
		} else {
			bad.Evidence += "x"
		}
		if validGoalNote(bad) {
			t.Fatal("one-byte overflow accepted")
		}
		for _, s := range []string{"", " ", " leading", "trailing ", "x\tvalue", "x\x00value", "x\u061cvalue", "x\u200evalue", "x\u200fvalue", "x\u202avalue", "x\u202evalue", "x\u2066value", "x\u2069value", "x\xffvalue"} {
			if field == "evidence" && s == "" {
				continue
			}
			bad = n
			if field == "text" {
				bad.Text = s
			} else {
				bad.Evidence = s
			}
			if validGoalNote(bad) {
				t.Fatal("inherited safety refusal lost")
			}
		}
	}
	if !goalDecisionReason(strings.Repeat("x", 512)) || goalDecisionReason(strings.Repeat("x", 513)) || !validGoalNote(goalRecordNote{Text: strings.Repeat("x", 1024)}) {
		t.Fatal("decision and note capacities conflated")
	}
	data := goalRecordData{Version: 1, State: nativeGoalDrafted, Notes: make([]goalRecordNote, 32)}
	for i := range data.Notes {
		data.Notes[i] = goalRecordNote{Text: fmt.Sprintf("%02d", i) + strings.Repeat("x", 1022)}
	}
	b, err := encodeGoalRecord(data)
	r, decodeErr := decodeGoalRecord(b)
	if err != nil || decodeErr != nil || !reflect.DeepEqual(data.Notes, r.Notes) {
		t.Fatal("note-count or append order changed")
	}
	data.Notes = append(data.Notes, goalRecordNote{Text: "extra"})
	if _, err := encodeGoalRecord(data); !errors.Is(err, errGoalRecord) {
		t.Fatal("note-count overflow accepted")
	}
	data.Notes = data.Notes[:32]
	for i := range data.Notes {
		data.Notes[i].Text = strings.Repeat(`"`, 1024)
	}
	if len(noteTextJSON(t, data)) <= 65536 {
		t.Fatal("escaped overflow control is undersized")
	}
	if _, err := encodeGoalRecord(data); !errors.Is(err, errGoalRecordCapacity) {
		t.Fatal("encoded capacity refusal changed")
	}
	over := len(noteTextJSON(t, data)) - 65536
	data.Notes[31].Text = strings.Repeat(`"`, 1024-(over+1)/2) + strings.Repeat("x", over%2)
	if len(noteTextJSON(t, data)) != 65536 {
		t.Fatal("exact encoded boundary instrument failed")
	}
	if _, err := encodeGoalRecord(data); err != nil {
		t.Fatal("exact encoded boundary refused")
	}
	data.Notes[31].Text += "x"
	if len(noteTextJSON(t, data)) != 65537 {
		t.Fatal("one-byte encoded overflow instrument failed")
	}
	if _, err := encodeGoalRecord(data); !errors.Is(err, errGoalRecordCapacity) {
		t.Fatal("one-byte encoded overflow accepted")
	}
}

func TestGoalNoteB13TextClassificationControls(t *testing.T) {
	goalEndParallel(t)
	m, r := goalEndSavedFixture(t)
	m.config.AutoResume = nil
	if branch := m.goalContinuationPendingLocked(&r, "", false); branch != "recovery_pending" || !validGoalNote(goalRecordNote{Text: r.ResumeBlockedReason}) {
		t.Fatal("generated pending reason is not one line")
	}
	body := "First legitimate line\n\nSecond legitimate line"
	held, err := pendingText(noteTextJSON(t, struct {
		Text string `json:"text"`
	}{body}))
	if err != nil || held != body {
		t.Fatal("held-message multiline bytes changed")
	}
	lane := Lane{ContinueText: body}
	if lane.continuation() != body {
		t.Fatal("continuation config text changed")
	}
	r.Brief = body
	r.Ended = new(time.Time)
	*r.Ended = time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	prompt := nativeRecoveryText(r)
	if !strings.HasSuffix(prompt, "\n\n"+body) || !strings.Contains(prompt, r.Ended.Format(time.RFC3339)) {
		t.Fatal("recovery prompt or brief multiline contract changed")
	}
	end := goalAttemptEnd{RunID: strings.Repeat("a", 32), Lane: "codex", PID: 2, ProcessStart: "identity", Started: *r.Ended, Ended: *r.Ended, Outcome: nativeOutcomeEndedNoReport, Held: true, Pending: "recovery_pending"}
	if !validGoalAttemptEnd(end) {
		t.Fatal("one-line metadata positive control failed")
	}
	for _, separator := range []string{"\r", "\n", "\u0085", "\u2028", "\u2029"} {
		for _, field := range []string{"pending", "id", "process", "job"} {
			bad := end
			s := "Before" + separator + "after"
			switch field {
			case "pending":
				bad.Pending = s
			case "id":
				bad.RunID = s
			case "process":
				bad.ProcessStart = s
			case "job":
				bad.Job = s
			}
			if validGoalAttemptEnd(bad) {
				t.Fatal("one-line metadata accepted separator")
			}
		}
		if continuationDigest("Before"+separator+"after") || goalBriefLine("Before"+separator+"after") {
			t.Fatal("closed digest or line reference widened")
		}
	}
	if !continuationDigest(strings.Repeat("a", 64)) {
		t.Fatal("digest positive control failed")
	}
}
