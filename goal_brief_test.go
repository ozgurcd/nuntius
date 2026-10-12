package nuntius

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func briefFixture(t *testing.T) (nativeGoalSpec, goalBriefInput) {
	t.Helper()
	spec, err := decodeNativeGoalSpec(nativeSchemaBytes(t, nativeSchemaFixture()))
	if err != nil {
		t.Fatal(err)
	}
	return spec, goalBriefInput{
		Body:      []byte(fmt.Sprintf("GOAL: %s (size %s)\nKeep exact bytes: café.\n", spec.ID, spec.Size)),
		Standards: []byte("SHARED STANDARDS\nRead the approved rules.\n"),
		Profile:   goalBriefProfile{Name: "owner-profile", Revision: "standards-1", Separator: "\n---\n", Marker: "SHARED STANDARDS", Wrapper: "/goal\n"},
		Authority: "approved-plan", SourceState: "observed-head-1", ReviewedSourceState: "observed-head-1",
	}
}

func briefDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestBriefAssembly(t *testing.T) {
	spec, in := briefFixture(t)
	s, err := assembleGoalBrief(spec, 2, in)
	if err != nil {
		t.Fatal(err)
	}
	final := string(in.Body) + in.Profile.Separator + string(in.Standards)
	dispatch := in.Profile.Wrapper + final
	if s.Body != string(in.Body) || s.Standards != string(in.Standards) || s.Profile != in.Profile || s.BodySHA256 != briefDigest(in.Body) || s.StandardsSHA256 != briefDigest(in.Standards) || s.FinalSHA256 != briefDigest([]byte(final)) || s.DispatchSHA256 != briefDigest([]byte(dispatch)) || s.Bytes != len(final) || s.CodePoints != utf8.RuneCountInString(final) || s.DispatchBytes != len(dispatch) || s.DispatchCodePoints != utf8.RuneCountInString(dispatch) || strings.Count(final, in.Profile.Marker) != 1 || s.Authority != in.Authority || s.Revision != 2 {
		t.Fatal("assembly changed exact bytes, digests, authority, standards count or wrapper measurements")
	}
	if s.Bytes == s.CodePoints || s.DispatchBytes-s.Bytes != len(in.Profile.Wrapper) {
		t.Fatal("byte and code-point measurements do not include wrapper separately")
	}
}

func TestBriefAssemblyRefusals(t *testing.T) {
	for _, name := range []string{"utf8-body", "utf8-standards", "bom-body", "bom-standards", "placeholder", "header-id", "header-size", "missing-header", "header-line", "duplicate-marker", "missing-marker", "already-final", "oversize", "authority-line", "profile-line", "state-line", "unreviewed-state"} {
		t.Run(name, func(t *testing.T) {
			spec, in := briefFixture(t)
			switch name {
			case "utf8-body":
				in.Body = append(in.Body, 0xff)
			case "utf8-standards":
				in.Standards = append(in.Standards, 0xff)
			case "bom-body":
				in.Body = append([]byte{0xef, 0xbb, 0xbf}, in.Body...)
			case "bom-standards":
				in.Standards = append([]byte{0xef, 0xbb, 0xbf}, in.Standards...)
			case "placeholder":
				in.Body = append([]byte("PLACEHOLDER\n"), in.Body...)
			case "header-id":
				in.Body = bytes.Replace(in.Body, []byte(spec.ID), []byte("WRONG-ID"), 1)
			case "header-size":
				in.Body = bytes.Replace(in.Body, []byte("size "+string(spec.Size)), []byte("size L"), 1)
				if spec.Size == "L" {
					in.Body = bytes.Replace(in.Body, []byte("size L"), []byte("size S"), 1)
				}
			case "missing-header":
				in.Body = []byte("Do work.\n")
			case "header-line":
				in.Body = bytes.Replace(in.Body, []byte(")\n"), []byte(")\u2028\n"), 1)
			case "duplicate-marker":
				in.Standards = append(in.Standards, []byte(in.Profile.Marker)...)
			case "missing-marker":
				in.Standards = []byte("Rules only")
			case "already-final":
				in.Body = append(in.Body, in.Standards...)
			case "oversize":
				in.Body = append(in.Body, bytes.Repeat([]byte("x"), goalBriefMaxBytes)...)
			case "authority-line":
				in.Authority += "\u2029bad"
			case "profile-line":
				in.Profile.Name += "\u0085bad"
			case "state-line":
				in.SourceState += "\u2028bad"
				in.ReviewedSourceState = in.SourceState
			case "unreviewed-state":
				in.SourceState = "new-head"
			}
			if _, err := assembleGoalBrief(spec, 2, in); err == nil {
				t.Fatal("accepted invalid brief", name)
			}
		})
	}
	for _, sep := range []string{"\u2028", "\u2029", "\u0085"} {
		for _, field := range []string{"authority", "name", "revision", "marker", "state"} {
			spec, in := briefFixture(t)
			switch field {
			case "authority":
				in.Authority += sep
			case "name":
				in.Profile.Name += sep
			case "revision":
				in.Profile.Revision += sep
			case "marker":
				in.Profile.Marker += sep
			case "state":
				in.SourceState += sep
				in.ReviewedSourceState = in.SourceState
			}
			if _, err := assembleGoalBrief(spec, 2, in); err == nil {
				t.Fatal("accepted multi-line metadata", field)
			}
		}
	}
}

func TestBriefAssemblyBoundary(t *testing.T) {
	spec, in := briefFixture(t)
	extra := goalBriefMaxBytes - len(in.Body) - len(in.Profile.Separator) - len(in.Standards)
	in.Body = append(in.Body, bytes.Repeat([]byte("x"), extra)...)
	s, err := assembleGoalBrief(spec, 2, in)
	if err != nil || s.Bytes != goalBriefMaxBytes {
		t.Fatal("refused full brief boundary", err)
	}
	in.Body = append(in.Body, 'x')
	if _, err := assembleGoalBrief(spec, 2, in); err == nil {
		t.Fatal("accepted oversized full brief")
	}
}

func TestBriefSealLifecycle(t *testing.T) {
	m, create := goalRecordsFixture(t)
	_, in := briefFixture(t)
	if _, err := m.createGoal(create); err != nil {
		t.Fatal(err)
	}
	approve := goalDecisionRequest{OperationID: "approve-unsealed", GoalID: create.Spec.ID, ExpectedRevision: 1, Action: "approve", Reason: "Review accepted", Authority: in.Authority, Evidence: "review-1"}
	if _, err := m.decideGoal(approve); !errors.Is(err, errGoalApprovalUnavailable) {
		t.Fatal("unapproved body gained authority", err)
	}
	req := create
	req.OperationID, req.ExpectedRevision = "seal-1", 1
	sealed, err := m.sealGoalBrief(req, in)
	if err != nil || sealed.Revision != 2 {
		t.Fatal("could not seal drafted revision", err)
	}
	if retry, err := m.sealGoalBrief(req, in); err != nil || retry != sealed {
		t.Fatal("seal retry lost original receipt", err)
	}
	approve.OperationID, approve.ExpectedRevision = "approve-seal", 2
	approve.SourceState = "changed-source"
	if _, err := m.decideGoal(approve); !errors.Is(err, errGoalPreflight) {
		t.Fatal("approval accepted changed source observation", err)
	}
	approve.SourceState = in.SourceState
	if _, err := m.decideGoal(approve); err != nil {
		t.Fatal("validated seal refused", err)
	}
	p, err := replayGoalJournal(m.store)
	if err != nil {
		t.Fatal("approval does not replay", err)
	}
	old := p.records[create.Spec.ID]
	data, _ := goalRecordMetadata(old)
	if data.State != nativeGoalApproved || data.ApprovalRevision != 2 || data.GrantBinding != "" {
		t.Fatal("approval changed grant or binding")
	}
	before := goalCommitTree(t, m.store)
	sealBytes, err := os.ReadFile(filepath.Join(m.store, old.sealRef))
	if err != nil {
		t.Fatal(err)
	}
	var seal goalBriefSeal
	if json.Unmarshal(sealBytes, &seal) != nil {
		t.Fatal("seal codec")
	}
	newInput := in
	newInput.Body = append(append([]byte(nil), in.Body...), []byte("New request.\n")...)
	if newer, err := goalBriefPreflight(seal, newInput); !newer || err != nil {
		t.Fatal("changed source not offered", err)
	}
	newInput = in
	newInput.Standards = append(append([]byte(nil), in.Standards...), []byte("New rule.\n")...)
	if newer, err := goalBriefPreflight(seal, newInput); !newer || err != nil {
		t.Fatal("changed standards not offered", err)
	}
	newInput = in
	newInput.SourceState = "observed-head-2"
	if _, err := goalBriefPreflight(seal, newInput); !errors.Is(err, errGoalPreflight) {
		t.Fatal("stale preflight accepted", err)
	}
	if !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
		t.Fatal("source observations mutated approved seal")
	}
	req.OperationID, req.ExpectedRevision = "seal-2", old.revision
	in.Body = append(in.Body, []byte("Updated request.\n")...)
	if _, err := m.sealGoalBrief(req, in); err != nil {
		t.Fatal(err)
	}
	p, err = replayGoalJournal(m.store)
	if err != nil {
		t.Fatal(err)
	}
	data, _ = goalRecordMetadata(p.records[create.Spec.ID])
	if data.State != nativeGoalDrafted || data.ApprovalRevision != 0 || data.GrantBinding != "" || p.records[create.Spec.ID].sealRef == old.sealRef {
		t.Fatal("refresh retained obsolete authority")
	}
	if b, _ := os.ReadFile(filepath.Join(m.store, old.sealRef)); !bytes.Equal(b, sealBytes) {
		t.Fatal("refresh rewrote old seal")
	}
}

func TestBriefSealCodecAndReplay(t *testing.T) {
	spec, in := briefFixture(t)
	s, err := assembleGoalBrief(spec, 2, in)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(s)
	if got, err := decodeGoalBrief(spec, b); err != nil || !reflect.DeepEqual(got, s) {
		t.Fatal("seal does not round trip", err)
	}
	escaped := bytes.Replace(b, []byte(`"authority"`), []byte(`"\u0061uthority"`), 1)
	if _, err := decodeGoalBrief(spec, escaped); err != nil {
		t.Fatal("recognizer and decoder disagree on escaped spelling", err)
	}
	for _, name := range []string{"case", "case-duplicate", "escaped-duplicate", "profile-case", "profile-duplicate", "digest", "size", "line", "unknown", "null", "version", "spec"} {
		t.Run(name, func(t *testing.T) {
			bad := append([]byte(nil), b...)
			switch name {
			case "case":
				bad = bytes.Replace(bad, []byte(`"authority"`), []byte(`"Authority"`), 1)
			case "case-duplicate":
				bad = bytes.Replace(bad, []byte(`"authority":`), []byte(`"Authority":"other","authority":`), 1)
			case "escaped-duplicate":
				bad = bytes.Replace(bad, []byte(`"authority":`), []byte(`"\u0061uthority":"other","authority":`), 1)
			case "profile-case":
				bad = bytes.Replace(bad, []byte(`"marker"`), []byte(`"Marker"`), 1)
			case "profile-duplicate":
				bad = bytes.Replace(bad, []byte(`"marker":`), []byte(`"MARKER":"other","marker":`), 1)
			case "digest":
				bad = bytes.Replace(bad, []byte(s.FinalSHA256), []byte(strings.Repeat("0", 64)), 1)
			case "size":
				bad = bytes.Replace(bad, []byte(fmt.Sprintf(`"bytes":%d`, s.Bytes)), []byte(`"bytes":1`), 1)
			case "line":
				bad = bytes.Replace(bad, []byte(in.Authority), []byte(in.Authority+`\u2028`), 1)
			case "unknown":
				bad = append([]byte(`{"other":1,`), bad[1:]...)
			case "null":
				bad = bytes.Replace(bad, []byte(`"wrapper":"/goal\n"`), []byte(`"wrapper":null`), 1)
			case "version":
				bad = bytes.Replace(bad, []byte(`"version":1`), []byte(`"version":2`), 1)
			case "spec":
				bad = bytes.Replace(bad, []byte(s.SpecRef), []byte("goal-spec-"+strings.Repeat("0", 64)), 1)
			}
			if _, err := decodeGoalBrief(spec, bad); err == nil {
				t.Fatal("accepted contradictory seal", name)
			}
			m, create := goalRecordsFixture(t)
			if _, err := m.createGoal(create); err != nil {
				t.Fatal(err)
			}
			create.OperationID, create.ExpectedRevision, create.Seal = "opaque-seal", 1, bad
			if _, err := m.updateGoal(create); err != nil {
				t.Fatal(err)
			}
			p, err := replayGoalJournal(m.store)
			if err != nil {
				t.Fatal(err)
			}
			old := p.records[spec.ID]
			data, _ := goalRecordMetadata(old)
			data.State, data.ApprovalRevision = nativeGoalApproved, 2
			data.Decisions = append(data.Decisions, goalDecisionEvidence{Action: "approve", Revision: 2, Reason: "Review accepted", Authority: in.Authority, Evidence: "review-1", SourceState: in.SourceState})
			record, _ := encodeGoalRecord(data)
			before := goalCommitTree(t, m.store)
			if _, _, err := m.commitGoal(goalCommitRequest{OperationID: "forged-approval", ExpectedRevision: 2, Spec: []byte(old.spec), Seal: bad, Record: record, MutationSHA256: strings.Repeat("a", 64)}, goalCommitIO{}); err == nil {
				t.Fatal("writer accepted malformed seal approval")
			}
			if !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
				t.Fatal("refused writer changed files")
			}
			goalDecisionReplayCandidate(t, m, old, record)
		})
	}
}

func TestBriefSealPreflightReplay(t *testing.T) {
	m, create := goalRecordsFixture(t)
	_, in := briefFixture(t)
	if _, err := m.createGoal(create); err != nil {
		t.Fatal(err)
	}
	create.OperationID, create.ExpectedRevision = "seal", 1
	if _, err := m.sealGoalBrief(create, in); err != nil {
		t.Fatal(err)
	}
	p, err := replayGoalJournal(m.store)
	if err != nil {
		t.Fatal(err)
	}
	old := p.records[create.Spec.ID]
	data, _ := goalRecordMetadata(old)
	data.State, data.ApprovalRevision = nativeGoalApproved, 2
	data.Decisions = []goalDecisionEvidence{{Action: "approve", Revision: 2, Reason: "Review accepted", Authority: in.Authority, Evidence: "review-1", SourceState: "wrong-source"}}
	record, err := encodeGoalRecord(data)
	if err != nil {
		t.Fatal(err)
	}
	goalDecisionReplayCandidate(t, m, old, record)
}

func TestBriefSealGrantInvalidation(t *testing.T) {
	m, create := goalRecordsFixture(t)
	_, in := briefFixture(t)
	s, err := assembleGoalBrief(create.Spec, 1, in)
	if err != nil {
		t.Fatal(err)
	}
	seal, _ := json.Marshal(s)
	data := goalRecordData{Version: 1, State: nativeGoalApproved, ApprovalRevision: 1, GrantBinding: "exact-grant", Notes: []goalRecordNote{}}
	record, err := encodeGoalRecord(data)
	if err != nil {
		t.Fatal(err)
	}
	spec := nativeSchemaBytes(t, nativeSchemaFixture())
	if _, _, err := m.commitGoal(goalCommitRequest{OperationID: "fixture", Spec: spec, Seal: seal, Record: record, MutationSHA256: strings.Repeat("a", 64)}, goalCommitIO{}); err != nil {
		t.Fatal(err)
	}
	p, err := replayGoalJournal(m.store)
	if err != nil {
		t.Fatal(err)
	}
	old := p.records[create.Spec.ID]
	in.Body = append(in.Body, []byte("A revised body.\n")...)
	s, err = assembleGoalBrief(create.Spec, 2, in)
	if err != nil {
		t.Fatal(err)
	}
	changedSeal, _ := json.Marshal(s)
	before := goalCommitTree(t, m.store)
	if _, _, err := m.commitGoal(goalCommitRequest{OperationID: "forged-refresh", ExpectedRevision: 1, Spec: spec, Seal: changedSeal, Record: record, MutationSHA256: strings.Repeat("b", 64)}, goalCommitIO{}); err == nil {
		t.Fatal("writer retained grant after seal changed")
	}
	if !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
		t.Fatal("refused refresh wrote files")
	}
	// Complete independent history, with valid checksums and new seal blob.
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	ref := goalBlobName("goal-seal-", changedSeal)
	if err := stageGoalBlob(root, ref, changedSeal, goalCommitIO{}); err != nil {
		t.Fatal(err)
	}
	forged := old
	forged.sealRef = ref
	goalDecisionReplayCandidate(t, m, forged, record)
	create.OperationID, create.ExpectedRevision = "refresh", 1
	if _, err := m.sealGoalBrief(create, in); err != nil {
		t.Fatal(err)
	}
	p, err = replayGoalJournal(m.store)
	if err != nil {
		t.Fatal(err)
	}
	data, _ = goalRecordMetadata(p.records[create.Spec.ID])
	if data.State != nativeGoalDrafted || data.GrantBinding != "" || data.ApprovalRevision != 0 {
		t.Fatal("refresh retained obsolete exact grant")
	}
}

func TestBriefLegacyDecisionReceipt(t *testing.T) {
	m, req := goalDecisionFixture(t, nativeGoalDrafted, nil)
	result, err := m.decideGoal(req)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(m.store, goalJournalName)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	firstSize := goalJournalHeaderBytes + int(uint32(b[16])<<24|uint32(b[17])<<16|uint32(b[18])<<8|uint32(b[19]))
	var event goalJournalEvent
	if json.Unmarshal(b[firstSize+goalJournalHeaderBytes:], &event) != nil {
		t.Fatal("fixture event")
	}
	// Exact B07 request encoding: the new source observation field was absent.
	legacy := []byte(`{"OperationID":"decision","ExpectedRevision":1,"GoalID":"EXAMPLE-DOCS","Action":"park","Reason":"Review before continuing","Authority":"approved-plan","Evidence":"review-record","AttemptID":"","ReportDigest":"","AlternateEvidence":false}`)
	event.MutationSHA256 = briefDigest(append([]byte("decision:"), legacy...))
	event.InputSHA256 = goalRecordFingerprint(goalInputFingerprint(req.GoalID, 1, event.SpecRef, event.SealRef), event.RecordRef, event.MutationSHA256)
	payload, _ := json.Marshal(event)
	goalReplayWrite(t, m.store, goalJournalName, append(append([]byte(nil), b[:firstSize]...), goalReplayFrame(2, payload)...))
	before := goalCommitTree(t, m.store)
	if replayed, err := m.decideGoal(req); err != nil || replayed != result {
		t.Fatal("new empty source field invalidated a saved B07 decision receipt", err)
	}
	if !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
		t.Fatal("legacy retry changed history")
	}
}

func TestBriefRefusalExplanation(t *testing.T) {
	spec, in := briefFixture(t)
	in.Body = append(in.Body, bytes.Repeat([]byte("x"), goalBriefMaxBytes)...)
	_, err := assembleGoalBrief(spec, 2, in)
	if err == nil || !strings.Contains(err.Error(), "bytes") || !strings.Contains(err.Error(), "code points") || !strings.Contains(err.Error(), "128 KiB") || strings.Contains(err.Error(), "Keep exact bytes") {
		t.Fatal("oversize refusal did not explain the limit and safe measurements")
	}
}
