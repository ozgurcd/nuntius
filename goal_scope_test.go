package nuntius

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

func canonicalBindingFixture(t *testing.T, writes []string, roots map[string]string) (*Manager, goalDispatchRequest, goalDispatchContext) {
	t.Helper()
	m, _, _ := manager(t)
	spec, in := briefFixture(t)
	spec.ID, spec.PreferredLane, spec.LaneBinding = "DISPATCH", "codex", "required"
	spec.After, spec.Writes, spec.Resources = []string{}, writes, []string{}
	in.Body = []byte("GOAL: DISPATCH (size S)\nwait")
	if _, err := m.createGoal(goalRecordRequest{OperationID: "create", Spec: spec}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.sealGoalBrief(goalRecordRequest{OperationID: "seal", ExpectedRevision: 1, Spec: spec}, in); err != nil {
		t.Fatal(err)
	}
	if _, err := m.decideGoal(goalDecisionRequest{OperationID: "approve", GoalID: spec.ID, ExpectedRevision: 2, Action: "approve", Reason: "Reviewed", Authority: in.Authority, Evidence: "review", SourceState: in.SourceState}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.mirrorNativeAudit("cli", goalCommitIO{}); err != nil {
		t.Fatal(err)
	}
	ctx := goalDispatchContext{Source: "cli", Profile: goalDeliveryProfile{Engine: "codex", CodePointCap: 4000, Root: deliveryRoot(t)}, Environment: goalAdmissionEnvironment{
		Roots:        roots,
		Capabilities: map[string]goalAdmissionCapability{"codex": {Kind: "codex", WorkKinds: []nativeWorkKind{"review"}}},
		SourceState:  func(nativeGoalSpec) (string, error) { return in.SourceState, nil },
		Claims: func(rs []string) ([]goalAdmissionClaim, error) {
			out := []goalAdmissionClaim{}
			for _, r := range rs {
				out = append(out, goalAdmissionClaim{Root: r, ObservedAt: time.Now().UTC()})
			}
			return out, nil
		},
	}}
	return m, goalDispatchRequest{GoalID: spec.ID, ExpectedRevision: 3, Key: "mapping-key", Lane: "codex"}, ctx
}

func canonicalBindingTree(t *testing.T, store string) map[string]string {
	t.Helper()
	tree := goalCommitTree(t, store)
	if err := filepath.WalkDir(store, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			tree[path] = "directory:" + info.Mode().String()
		}
		return nil
	}); err != nil {
		t.Fatal("directory inventory failed", err)
	}
	return tree
}

// Stage and checksum a frame independently of candidate save, then inventory
// files AND directories before the first authoritative read of forged history.
func canonicalBindingReplay(t *testing.T, m *Manager, id string, old goalReplayRecord, record []byte) {
	t.Helper()
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		t.Fatal(err)
	}
	spec := []byte(old.spec)
	specRef, recordRef := goalBlobName("goal-spec-", spec), goalBlobName("goal-record-", record)
	if err := stageGoalBlob(root, specRef, spec, goalCommitIO{}); err != nil {
		t.Fatal(err)
	}
	if err := stageGoalBlob(root, recordRef, record, goalCommitIO{}); err != nil {
		t.Fatal(err)
	}
	prefix, err := readGoalPrivate(root, goalJournalName, goalJournalMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	p, err := replayGoalJournalData(root, prefix)
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	mutation := strings.Repeat("c", 64)
	event := goalJournalEvent{Kind: "goal.spec", GoalID: id, Revision: old.revision + 1, SpecRef: specRef, SealRef: old.sealRef, RecordRef: recordRef, OperationID: "scope-forged-frame", MutationSHA256: mutation}
	event.InputSHA256 = goalRecordFingerprint(goalInputFingerprint(id, old.revision, specRef, old.sealRef), recordRef, mutation)
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	goalReplayWrite(t, m.store, goalJournalName, append(prefix[:len(prefix):len(prefix)], goalReplayFrame(p.sequence+1, payload)...))
	before := canonicalBindingTree(t, m.store)
	got, err := replayGoalJournal(m.store)
	if !errors.Is(err, errGoalReplay) || got != nil {
		t.Error("fresh replay accepted mismatched approved write", err)
	}
	if !reflect.DeepEqual(before, canonicalBindingTree(t, m.store)) {
		t.Fatal("refused replay changed durable tree")
	}
}

func canonicalBindingCheck(t *testing.T, m *Manager, req goalDispatchRequest, ctx goalDispatchContext, mutate func(*goalRecordData), valid bool) {
	t.Helper()
	old, _ := dispatchRecord(t, m, req.GoalID)
	prefix, err := os.ReadFile(filepath.Join(m.store, goalJournalName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.dispatchGoal(req, ctx, dispatchOneFault("publication")); err == nil {
		t.Fatal("reservation barrier failed")
	}
	_, data := dispatchRecord(t, m, req.GoalID)
	if len(data.Dispatches) != 1 || data.Dispatches[0].Phase != "reserved" {
		t.Fatal("no genuine prelaunch reservation")
	}
	if mutate != nil {
		mutate(&data)
	}
	record, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	seal, err := os.ReadFile(filepath.Join(m.store, old.sealRef))
	if err != nil {
		t.Fatal(err)
	}
	goalReplayWrite(t, m.store, goalJournalName, prefix)
	before := canonicalBindingTree(t, m.store)
	_, p, err := m.commitGoal(goalCommitRequest{OperationID: "prep-mapping", ExpectedRevision: old.revision, Spec: []byte(old.spec), Seal: seal, Record: record, MutationSHA256: strings.Repeat("a", 64)}, goalCommitIO{})
	if valid {
		if err != nil || p == nil {
			t.Fatal("genuine mapping rejected", err)
		}
		// Replay does not consume a later environment-root map.
		for name := range ctx.Environment.Roots {
			ctx.Environment.Roots[name] = filepath.Join(t.TempDir(), "absent")
		}
		got, e := replayGoalJournal(m.store)
		if e != nil || got == nil {
			t.Fatal("genuine fresh replay rejected", e)
		}
	} else {
		if err == nil || p != nil {
			t.Error("candidate writer accepted mismatched approved-write mapping")
		}
		if err != nil && !reflect.DeepEqual(before, canonicalBindingTree(t, m.store)) {
			t.Error("candidate refusal changed durable tree")
		}
		goalReplayWrite(t, m.store, goalJournalName, prefix)
		canonicalBindingReplay(t, m, req.GoalID, old, record)
	}
	dispatchStarts(t, m, 0)
}

func TestGoalDispatchCanonicalBinding(t *testing.T) {
	goalEndParallel(t)
	for _, mode := range []string{"control", "unrelated-root", "wrong-descendant", "reordered", "mismatched", "duplicate", "extra-path"} {
		t.Run(mode, func(t *testing.T) {
			a, b := t.TempDir(), t.TempDir()
			m, req, ctx := canonicalBindingFixture(t, []string{"alpha/approved", "beta/other"}, map[string]string{"alpha": a, "beta": b})
			mutate := func(data *goalRecordData) {
				d := &data.Dispatches[0]
				switch mode {
				case "unrelated-root":
					d.Writes[0] = t.TempDir()
				case "wrong-descendant":
					d.Writes[0] = filepath.Join(a, "unapproved")
				case "reordered":
					d.Writes[0], d.Writes[1] = d.Writes[1], d.Writes[0]
				case "mismatched":
					d.Writes[0] = filepath.Join(b, "approved")
				case "duplicate":
					d.Writes[1] = d.Writes[0]
				case "extra-path":
					d.Writes = append(d.Writes, d.Writes[0])
				}
			}
			canonicalBindingCheck(t, m, req, ctx, mutate, mode == "control")
		})
	}
}

func TestGoalDispatchCanonicalAliases(t *testing.T) {
	goalEndParallel(t)
	for _, mode := range []string{"symlink-root", "nested-symlink", "empty", "same-physical-root"} {
		t.Run(mode, func(t *testing.T) {
			parent := t.TempDir()
			real := filepath.Join(parent, "real")
			if err := os.Mkdir(real, 0700); err != nil {
				t.Fatal(err)
			}
			root, writes := real, []string{"sample/approved"}
			if mode == "symlink-root" {
				root = filepath.Join(parent, "alias")
				if err := os.Symlink(real, root); err != nil {
					t.Fatal(err)
				}
			} else if mode == "nested-symlink" {
				if err := os.Mkdir(filepath.Join(real, "target"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("target", filepath.Join(real, "alias")); err != nil {
					t.Fatal(err)
				}
				writes = []string{"sample/alias/approved"}
			}
			roots := map[string]string{"sample": root}
			if mode == "empty" {
				writes = []string{}
			}
			if mode == "same-physical-root" {
				roots["other"] = real
				writes = []string{"sample/approved", "sample/other", "other/approved"}
			}
			m, req, ctx := canonicalBindingFixture(t, writes, roots)
			canonicalBindingCheck(t, m, req, ctx, nil, true)
		})
	}
}

func TestGoalDispatchCanonicalRootSpellings(t *testing.T) {
	goalEndParallel(t)
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "child"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, spelling := range []string{root, root + "/", root + "/.", root + "/child/.."} {
		m, req, ctx := canonicalBindingFixture(t, []string{"sample", "sample/absent/leaf"}, map[string]string{"sample": spelling})
		canonicalBindingCheck(t, m, req, ctx, nil, true)
	}
	upper := filepath.Join(root, "CaseRoot")
	lower := filepath.Join(root, "caseroot")
	if err := os.Mkdir(upper, 0700); err != nil {
		t.Fatal(err)
	}
	a, err := os.Stat(upper)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Stat(lower)
	if err == nil && os.SameFile(a, b) {
		x, _, ok := goalAdmissionPaths([]string{"sample/approved"}, map[string]string{"sample": upper})
		y, _, yes := goalAdmissionPaths([]string{"sample/approved"}, map[string]string{"sample": lower})
		if !ok || !yes || !reflect.DeepEqual(x, y) || !goalAdmissionOverlaps(x, y) {
			t.Fatal("physical case aliases have different conflict paths")
		}
		t.Log("case-insensitive alias exercised")
	} else {
		if err := os.Mkdir(lower, 0700); err != nil {
			t.Fatal(err)
		}
		x, _, ok := goalAdmissionPaths([]string{"sample/approved"}, map[string]string{"sample": upper})
		y, _, yes := goalAdmissionPaths([]string{"sample/approved"}, map[string]string{"sample": lower})
		if !ok || !yes || goalAdmissionOverlaps(x, y) {
			t.Fatal("distinct case-sensitive paths collapsed")
		}
		t.Log("case-sensitive distinct roots exercised")
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := goalAdmissionPaths([]string{"sample/escape/leaf"}, map[string]string{"sample": root}); ok {
		t.Fatal("link escape accepted")
	}
	// Resolve symlink/.. by physical filesystem semantics before normalization.
	if err := os.Mkdir(filepath.Join(outside, "child"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "child"), filepath.Join(root, "parent-alias")); err != nil {
		t.Fatal(err)
	}
	x, _, ok := goalAdmissionPaths([]string{"sample/approved"}, map[string]string{"sample": filepath.Join(root, "parent-alias") + "/.."})
	if !ok || !reflect.DeepEqual(x, []string{filepath.Join(outside, "approved")}) {
		t.Fatal("symlink parent normalized before physical resolution")
	}
	if _, _, ok := goalAdmissionPaths([]string{"sample/../other"}, map[string]string{"sample": root}); ok {
		t.Fatal("approved traversal accepted")
	}
}

func TestGoalDispatchCanonicalRootChanges(t *testing.T) {
	goalEndParallel(t)
	for _, mode := range []string{"gone", "replaced", "link-replaced"} {
		t.Run(mode, func(t *testing.T) {
			parent := t.TempDir()
			root := filepath.Join(parent, "repo")
			if err := os.Mkdir(root, 0700); err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(parent, "alias")
			if err := os.Symlink(root, alias); err != nil {
				t.Fatal(err)
			}
			m, req, ctx := canonicalBindingFixture(t, []string{"sample/approved"}, map[string]string{"sample": alias})
			var snapshot []goalWriteEvidence
			fault := goalCommitIO{checkpoint: func(stage string) error {
				if stage != "prepare_sync" {
					return nil
				}
				_, data := dispatchRecord(t, m, req.GoalID)
				snapshot = append([]goalWriteEvidence(nil), data.Dispatches[0].Scope...)
				if mode == "link-replaced" {
					if err := os.Remove(alias); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(t.TempDir(), alias); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.Rename(root, root+"-old"); err != nil {
						t.Fatal(err)
					}
					if mode == "replaced" {
						if err := os.Mkdir(root, 0700); err != nil {
							t.Fatal(err)
						}
					}
				}
				return nil
			}}
			result, err := m.dispatchGoal(req, ctx, fault)
			if !errors.Is(err, errGoalNotLaunched) || result.Phase != "failed" {
				t.Fatal("changed root launched", err)
			}
			_, data := dispatchRecord(t, m, req.GoalID)
			if snapshot == nil || !reflect.DeepEqual(snapshot, data.Dispatches[0].Scope) {
				t.Fatal("historical scope rewritten")
			}
			before := goalCommitTree(t, m.store)
			if p, err := replayGoalJournal(m.store); err != nil || p == nil {
				t.Fatal("historical replay consulted current roots", err)
			}
			if !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
				t.Fatal("read changed bytes")
			}
			dispatchStarts(t, m, 0)
		})
	}
}

func canonicalBindingRefuse(t *testing.T, m *Manager, old goalReplayRecord, data goalRecordData) {
	t.Helper()
	record, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	seal, err := os.ReadFile(filepath.Join(m.store, old.sealRef))
	if err != nil {
		t.Fatal(err)
	}
	before := canonicalBindingTree(t, m.store)
	_, p, err := m.commitGoal(goalCommitRequest{OperationID: "scope-counterfeit", ExpectedRevision: old.revision, Spec: []byte(old.spec), Seal: seal, Record: record, MutationSHA256: strings.Repeat("a", 64)}, goalCommitIO{})
	if err == nil || p != nil {
		t.Fatal("retained scope substitution accepted")
	}
	if !reflect.DeepEqual(before, canonicalBindingTree(t, m.store)) {
		t.Fatal("binding refusal staged durable data")
	}
	canonicalBindingReplay(t, m, "DISPATCH", old, record)
}

func canonicalEndedFixture(t *testing.T) (*Manager, Run) {
	t.Helper()
	m, req, ctx := canonicalBindingFixture(t, []string{"sample/approved"}, map[string]string{"sample": t.TempDir()})
	result, err := m.dispatchGoal(req, ctx, goalCommitIO{})
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, m, req.Lane)
	r, err := loadRun(filepath.Join(m.store, "runs", result.RunID), result.RunID)
	if err != nil || r == nil {
		t.Fatal("fixture identity")
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := signalRun(*r, syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(4 * time.Second); !goalAttemptDefinitelyGone(*r); {
		if time.Now().After(deadline) {
			t.Fatal("offline child did not end")
		}
		time.Sleep(time.Millisecond)
	}
	now := time.Now().UTC()
	r.Ended, r.State, r.StopReason = &now, "ended", "ended_no_report"
	if err := m.save(r); err != nil {
		t.Fatal(err)
	}
	t.Log("offline child ended; identity absence verified before scope/end repair")
	return m, *r
}

func TestGoalDispatchCanonicalEndBinding(t *testing.T) {
	goalEndParallel(t)
	base, original := canonicalEndedFixture(t)
	for _, mode := range []string{"end-path", "end-evidence", "coherent-replacement", "released", "note", "report"} {
		t.Run(mode, func(t *testing.T) {
			m, r := goalEndClone(t, base, original)
			old, data := dispatchRecord(t, m, r.GoalID)
			end := goalAttemptEnd{RunID: r.ID, Lane: r.Lane, PID: r.PID, ProcessStart: r.ProcessStart, Job: r.LaunchdLabel, Started: r.Started, Ended: *r.Ended, Outcome: nativeOutcomeEndedNoReport}
			snapshot := append([]goalWriteEvidence(nil), data.Dispatches[0].Scope...)
			next, err := applyGoalAttemptEnd(data, end)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "released" || mode == "note" || mode == "report" {
				if err := goalEndPublish(t, m, &r, goalCommitIO{}); err != nil {
					t.Fatal(err)
				}
				old, next = dispatchRecord(t, m, r.GoalID)
				if !goalAttemptReleased(next, r.ID) || !reflect.DeepEqual(snapshot, next.Dispatches[0].Scope) {
					t.Fatal("genuine end failed to retain and release scope")
				}
			}
			next.Dispatches = append([]goalDispatchRecord(nil), next.Dispatches...)
			d := &next.Dispatches[0]
			d.Writes = append([]string(nil), d.Writes...)
			d.Scope = append([]goalWriteEvidence(nil), d.Scope...)
			if len(d.Writes) == 0 {
				t.Fatal("fixture has no approved write")
			}
			switch mode {
			case "end-evidence":
				d.Scope[0].Write = "sample/different"
			case "coherent-replacement":
				root := t.TempDir()
				s, _ := decodeNativeGoalSpec([]byte(d.Spec))
				d.Writes, _, d.Scope, _ = goalCanonicalScope(s.Writes, map[string]string{"sample": root})
				if !goalDispatchScopeValid(*d) {
					t.Fatal("coherent mutation did not isolate history immutability")
				}
			default:
				d.Writes[0] = filepath.Join(t.TempDir(), "unapproved")
			}
			if mode == "note" {
				next.Notes = append(next.Notes, goalRecordNote{Text: "note with changed mapping"})
			}
			if mode == "report" {
				report := goalAttemptReport{RunID: r.ID, Digest: strings.Repeat("a", 64), Bytes: 1, Modified: r.Ended.Add(time.Second)}
				if _, err := applyGoalAttemptReport(next, report); err == nil {
					t.Fatal("invalid scope acquired report authority")
				}
			}
			if mode != "coherent-replacement" && goalAttemptReleased(next, r.ID) {
				t.Fatal("invalid scope became released ownership")
			}
			canonicalBindingRefuse(t, m, old, next)
		})
	}
}

func TestGoalDispatchCanonicalCodec(t *testing.T) {
	goalEndParallel(t)
	m, req, ctx := canonicalBindingFixture(t, []string{"sample/approved"}, map[string]string{"sample": t.TempDir()})
	if _, err := m.dispatchGoal(req, ctx, dispatchOneFault("publication")); err == nil {
		t.Fatal("checkpoint")
	}
	_, data := dispatchRecord(t, m, req.GoalID)
	b, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeGoalRecord(b); err != nil {
		t.Fatal("genuine codec", err)
	}
	for _, key := range []string{"write", "root", "ancestor", "resolved", "root_device", "root_inode", "device", "inode"} {
		for _, mode := range []string{"case", "null", "type", "unknown", "missing"} {
			t.Run(key+"/"+mode, func(t *testing.T) {
				var raw map[string]any
				if err := json.Unmarshal(b, &raw); err != nil {
					t.Fatal(err)
				}
				row := raw["dispatches"].([]any)[0].(map[string]any)["scope_evidence"].([]any)[0].(map[string]any)
				switch mode {
				case "case":
					row[strings.ToUpper(key)] = row[key]
					delete(row, key)
				case "null":
					row[key] = nil
				case "type":
					row[key] = []any{}
				case "unknown":
					row["unexpected"] = true
				case "missing":
					delete(row, key)
				}
				mutated, _ := json.Marshal(raw)
				if _, err := decodeGoalRecord(mutated); err == nil {
					t.Fatal("bad scope evidence accepted")
				}
			})
		}
	}
	for _, separator := range []string{"\u2028", "\u2029", "\u0085"} {
		for _, key := range []string{"write", "root", "ancestor", "resolved"} {
			var raw map[string]any
			_ = json.Unmarshal(b, &raw)
			row := raw["dispatches"].([]any)[0].(map[string]any)["scope_evidence"].([]any)[0].(map[string]any)
			row[key] = row[key].(string) + separator
			mutated, _ := json.Marshal(raw)
			if _, err := decodeGoalRecord(mutated); err == nil {
				t.Fatal("multiline scope accepted", key)
			}
		}
	}
	escaped := strings.Replace(string(b), `"scope_evidence"`, `"scope_\u0065vidence"`, 1)
	if _, err := decodeGoalRecord([]byte(escaped)); err != nil {
		t.Fatal("canonical exact escape refused", err)
	}
	duplicate := strings.Replace(string(b), `"scope_evidence":`, `"scope_\u0065vidence":[],"scope_evidence":`, 1)
	if _, err := decodeGoalRecord([]byte(duplicate)); err == nil {
		t.Fatal("decoded duplicate accepted")
	}
	var raw map[string]any
	_ = json.Unmarshal(b, &raw)
	delete(raw["dispatches"].([]any)[0].(map[string]any), "scope_evidence")
	missing, _ := json.Marshal(raw)
	if _, err := decodeGoalRecord(missing); err == nil {
		t.Fatal("old unbound reservation accepted")
	}
}

func TestGoalDispatchCanonicalEndRepair(t *testing.T) {
	goalEndParallel(t)
	base, original := canonicalEndedFixture(t)
	_, initial := dispatchRecord(t, base, original.GoalID)
	snapshot := initial.Dispatches[0]
	for _, stage := range []string{"end_meta_saved", "blob_write", "partial_write", "journal_sync", "journal_directory_sync", "publication", "native_receipt.write"} {
		t.Run(stage, func(t *testing.T) {
			m, r := goalEndClone(t, base, original)
			fault := goalCommitIO{checkpoint: func(at string) error {
				if at == stage {
					return errors.New("injected delivery fault")
				}
				return nil
			}}
			if err := goalEndPublish(t, m, &r, fault); err == nil {
				t.Fatal("fault did not hold delivery")
			}
			before := goalCommitTree(t, m.store)
			_, _ = replayGoalJournal(m.store)
			if !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
				t.Fatal("reader repaired uncertain end")
			}
			n := dispatchReopen(t, m)
			_, data := dispatchRecord(t, n, r.GoalID)
			if data.State != nativeGoalEnded || len(data.Ends) != 1 || data.Ends[0].RunID != r.ID || !data.Ends[0].Ended.Equal(*r.Ended) || !reflect.DeepEqual(snapshot, data.Dispatches[0]) || !goalAttemptReleased(data, r.ID) {
				t.Fatal("repair lost original identity or canonical evidence")
			}
			before = goalCommitTree(t, n.store)
			if err := n.Recover(); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, goalCommitTree(t, n.store)) {
				t.Fatal("confirmed retry rewrote history")
			}
			dispatchStarts(t, m, 1)
		})
	}
}
