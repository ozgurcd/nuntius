package nuntius

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func integrationTestDigest(s string) string {
	d := sha256.Sum256([]byte(s))
	return hex.EncodeToString(d[:])
}

func integrationFixture(t *testing.T) (*Manager, string, integrationEntry) {
	t.Helper()
	m, req := goalRecordsFixture(t)
	req.Spec.After = []string{}
	if _, err := m.createGoal(req); err != nil {
		var p *goalPersistenceFailure
		if errors.As(err, &p) {
			t.Fatalf("source goal fixture stage %s errno %d", p.stage, p.errno)
		}
		t.Fatalf("source goal fixture: %v", err)
	}
	goal := req.Spec.ID
	req.Spec.ID, req.Spec.WorkKind, req.OperationID = "INTEGRATION-REVIEW", "review", "review-create"
	// An existing verified native review is referenced, never manufactured by
	// an integration frame. This is the same offline record seam as decisions.
	spec, _ := json.Marshal(req.Spec)
	record, err := encodeGoalRecord(goalRecordData{Version: 1, State: nativeGoalVerified, Outcome: nativeOutcomeCompleted, Notes: []goalRecordNote{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.commitGoal(goalCommitRequest{OperationID: req.OperationID, Spec: spec, Record: record, MutationSHA256: integrationTestDigest("review-mutation")}, goalCommitIO{}); err != nil {
		t.Fatal("review goal fixture failed")
	}
	source, err := captureIntegrationSource(m.store, goal, req.Spec.ID)
	if err != nil {
		t.Fatal("source snapshot failed")
	}
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	d := integrationTestDigest("fixture")
	return m, path, integrationEntry{ID: "ENTRY-ONE", Source: source, SourceCommit: d[:40], BaseCommit: d[:40], Tree: d[:40], OperationDigest: d, FragmentDigest: d, ReviewerDigest: d, RepoID: "repo", GroupDigest: d, Scope: []string{"docs"}, SemanticKeys: []string{"docs-contract"}, PolicyDigest: d, PolicyEpoch: 1, ProfileDigest: d, AuthorityDigest: d, Attempt: 1, Dependencies: []string{}}
}

func integrationSubmit(e integrationEntry) integrationRequest {
	return integrationRequest{Version: 1, OperationID: "submit-one", EntryID: e.ID, Kind: "submit", Entry: &e}
}

func TestIntegrationSubmissionCAS(t *testing.T) {
	m, path, e := integrationFixture(t)
	r := integrationSubmit(e)
	op, err := commitIntegration(path, m.store, r, integrationIO{})
	if err != nil || op.Sequence != 1 || op.Revision != 1 {
		t.Fatalf("submit failed: %v", err)
	}
	again, err := commitIntegration(path, m.store, r, integrationIO{})
	if err != nil || again != op {
		t.Fatal("idempotent receipt changed")
	}
	r.OperationID = "other-submit"
	if _, err := commitIntegration(path, m.store, r, integrationIO{}); err == nil {
		t.Fatal("duplicate entry accepted")
	}
	r.OperationID, r.Kind, r.ExpectedRevision, r.Entry, r.State = "park", "hold", 0, nil, "parked"
	if _, err := commitIntegration(path, m.store, r, integrationIO{}); !errors.Is(err, errIntegrationStale) {
		t.Fatal("stale CAS accepted")
	}
	p, err := readIntegration(path)
	if err != nil || p.Sequence != 1 || p.Entries[e.ID].State != "submitted" {
		t.Fatal("submission became acceptance")
	}
}

func TestIntegrationIndependentCaps(t *testing.T) {
	if c := integrationCaps(); c.Version != 1 || c.MaxEvents == goalJournalMaxEvents || c.Executor || c.AuthenticatedReceipts {
		t.Fatal("integration capabilities are not independent")
	}
}

func integrationSave(t *testing.T, m *Manager, path string, r integrationRequest) integrationOperation {
	t.Helper()
	op, err := commitIntegration(path, m.store, r, integrationIO{})
	if err != nil {
		t.Fatalf("integration save failed: %v", err)
	}
	return op
}

func integrationSealRequest(e integrationEntry, revision uint64) integrationRequest {
	return integrationRequest{Version: 1, OperationID: "seal-one", EntryID: e.ID, ExpectedRevision: revision, Kind: "seal", Seal: &integrationSeal{EntryDigest: integrationDigestJSON(e), ReviewDigest: e.Source.ReviewDigest, ReviewerDigest: e.ReviewerDigest}}
}

func TestIntegrationSealsAndRevisions(t *testing.T) {
	m, path, e := integrationFixture(t)
	integrationSave(t, m, path, integrationSubmit(e))
	r := integrationSealRequest(e, 1)
	r.Seal.ReviewDigest = integrationTestDigest("wrong")
	if _, err := commitIntegration(path, m.store, r, integrationIO{}); err == nil {
		t.Fatal("wrong review seal accepted")
	}
	r = integrationSealRequest(e, 1)
	integrationSave(t, m, path, r)
	p, err := readIntegration(path)
	if err != nil || !integrationEligible(p, e.ID) {
		t.Fatal("valid independent review was not eligible")
	}
	e.FragmentDigest = integrationTestDigest("revised")
	r = integrationRequest{Version: 1, OperationID: "revise", EntryID: e.ID, ExpectedRevision: 2, Kind: "revise", Entry: &e}
	integrationSave(t, m, path, r)
	p, err = readIntegration(path)
	if err != nil || p.Entries[e.ID].Seal != nil || integrationEligible(p, e.ID) {
		t.Fatal("revision retained approval")
	}
	r = integrationSealRequest(e, 3)
	r.Seal.EntryDigest = integrationTestDigest("old entry")
	if _, err := commitIntegration(path, m.store, r, integrationIO{}); err == nil {
		t.Fatal("old entry seal accepted")
	}
	for _, field := range []string{"goal", "review"} {
		t.Run(field, func(t *testing.T) {
			m, path, e := integrationFixture(t)
			id := e.Source.GoalID
			if field == "review" {
				id = e.Source.ReviewID
			}
			if _, err := m.noteGoal(goalNoteRequest{OperationID: "source-note", ExpectedRevision: 1, GoalID: id, Note: goalRecordNote{Text: "Changed source"}}); err != nil {
				t.Fatal("source update failed")
			}
			if _, err := commitIntegration(path, m.store, integrationSubmit(e), integrationIO{}); !errors.Is(err, errIntegrationStale) {
				t.Fatal("stale cross-store source accepted")
			}
			p, err := readIntegration(path)
			if err != nil || p.Sequence != 0 {
				t.Fatal("stale source wrote an entry")
			}
		})
	}
}

func TestIntegrationIntentObservation(t *testing.T) {
	m, path, e := integrationFixture(t)
	integrationSave(t, m, path, integrationSubmit(e))
	integrationSave(t, m, path, integrationSealRequest(e, 1))
	r := integrationRequest{Version: 1, OperationID: "effect-one", EntryID: e.ID, ExpectedRevision: 2, Kind: "intent", Effect: "prepare"}
	intent := integrationSave(t, m, path, r)
	// Lost acknowledgement cannot be reinterpreted as successful effect.
	p, err := readIntegration(path)
	if err != nil || p.Entries[e.ID].Pending != r.OperationID || p.Operations[r.OperationID].Observation != nil {
		t.Fatal("intent became an observation")
	}
	if got := integrationSave(t, m, path, r); got != intent {
		t.Fatal("intent retry duplicated identity")
	}
	r.OperationID, r.ExpectedRevision = "second-effect", 3
	if _, err := commitIntegration(path, m.store, r, integrationIO{}); err == nil {
		t.Fatal("unresolved effect duplicated")
	}
	observe := integrationRequest{Version: 1, OperationID: "observe-unknown", EntryID: e.ID, ExpectedRevision: 3, Kind: "observe", Observation: &integrationObservation{OperationID: "effect-one", IntentDigest: intent.IntentDigest, ResultDigest: integrationTestDigest("result"), Outcome: "uncertain"}}
	bad := observe
	copyObs := *observe.Observation
	copyObs.IntentDigest = integrationTestDigest("other intent")
	bad.Observation = &copyObs
	if _, err := commitIntegration(path, m.store, bad, integrationIO{}); err == nil {
		t.Fatal("foreign intent observation accepted")
	}
	integrationSave(t, m, path, observe)
	p, err = readIntegration(path)
	if err != nil || p.Entries[e.ID].State != "uncertain" || p.Entries[e.ID].Pending != "effect-one" {
		t.Fatal("uncertain ownership lost")
	}
	observe.OperationID, observe.ExpectedRevision, observe.Observation.Outcome = "observe-settled", 4, "settled"
	integrationSave(t, m, path, observe)
	p, err = readIntegration(path)
	if err != nil || p.Entries[e.ID].Pending != "" || p.Operations["effect-one"].Observation == nil || p.Entries[e.ID].State == "accepted" {
		t.Fatal("settlement lost or became acceptance")
	}
	if _, err := commitIntegration(path, m.store, observe, integrationIO{}); err != nil {
		t.Fatal("observation retry failed")
	}
	observe.OperationID, observe.ExpectedRevision = "duplicate-observe", 5
	if _, err := commitIntegration(path, m.store, observe, integrationIO{}); err == nil {
		t.Fatal("duplicate observation accepted")
	}
}

func TestIntegrationPendingSourceChange(t *testing.T) {
	for _, source := range []string{"goal", "review"} {
		for _, outcome := range []string{"settled", "failed", "uncertain", "hold-uncertain"} {
			t.Run(source+"/"+outcome, func(t *testing.T) {
				m, path, e := integrationFixture(t)
				integrationSave(t, m, path, integrationSubmit(e))
				integrationSave(t, m, path, integrationSealRequest(e, 1))
				intent := integrationSave(t, m, path, integrationRequest{Version: 1, OperationID: "pending-effect", EntryID: e.ID, ExpectedRevision: 2, Kind: "intent", Effect: "prepare"})
				id := e.Source.GoalID
				if source == "review" {
					id = e.Source.ReviewID
				}
				if _, err := m.noteGoal(goalNoteRequest{OperationID: "source-change", ExpectedRevision: 1, GoalID: id, Note: goalRecordNote{Text: "Changed while effect pending"}}); err != nil {
					t.Fatal("source update failed")
				}
				r := integrationRequest{Version: 1, OperationID: "pending-result", EntryID: e.ID, ExpectedRevision: 3, Kind: "observe", Observation: &integrationObservation{OperationID: "pending-effect", IntentDigest: intent.IntentDigest, ResultDigest: integrationTestDigest("result"), Outcome: outcome}}
				if outcome == "hold-uncertain" {
					r.Kind, r.State, r.Observation = "hold", "uncertain", nil
				}
				// Source independence must not bypass exact intent binding or CAS.
				bad := r
				bad.ExpectedRevision = 2
				if _, err := commitIntegration(path, m.store, bad, integrationIO{}); !errors.Is(err, errIntegrationStale) {
					t.Fatal("stale result CAS accepted")
				}
				if r.Observation != nil {
					foreign := *r.Observation
					foreign.IntentDigest = integrationTestDigest("foreign intent")
					bad = r
					bad.Observation = &foreign
					if _, err := commitIntegration(path, m.store, bad, integrationIO{}); err == nil {
						t.Fatal("foreign result accepted")
					}
				}
				result, err := commitIntegration(path, m.store, r, integrationIO{})
				if err != nil {
					t.Fatalf("pending result refused after source change: %v", err)
				}
				p, err := readIntegration(path)
				if err != nil || p.Sequence != 4 || p.Entries[e.ID].Entry.Source != e.Source {
					t.Fatal("result lost history or changed source binding")
				}
				if outcome == "uncertain" || outcome == "hold-uncertain" {
					if p.Entries[e.ID].State != "uncertain" || p.Entries[e.ID].Pending != "pending-effect" || p.Operations["pending-effect"].Observation != nil {
						t.Fatal("uncertain result lost pending ownership")
					}
				} else {
					state := "reviewed"
					if outcome == "failed" {
						state = "parked"
					}
					if p.Entries[e.ID].State != state || p.Entries[e.ID].Pending != "" || p.Operations["pending-effect"].Observation == nil || p.Operations["pending-effect"].Observation.Outcome != outcome {
						t.Fatal("terminal result not retained")
					}
				}
				if retry := integrationSave(t, m, path, r); retry != result {
					t.Fatal("result retry changed journal identity")
				}
				if outcome == "uncertain" || outcome == "hold-uncertain" {
					integrationSave(t, m, path, integrationRequest{Version: 1, OperationID: "later-result", EntryID: e.ID, ExpectedRevision: 4, Kind: "observe", Observation: &integrationObservation{OperationID: "pending-effect", IntentDigest: intent.IntentDigest, ResultDigest: integrationTestDigest("later result"), Outcome: "settled"}})
					p, err = readIntegration(path)
					if err != nil || p.Entries[e.ID].Pending != "" || p.Entries[e.ID].State != "reviewed" {
						t.Fatal("later exact observation did not settle")
					}
				}
				revision := p.Entries[e.ID].Revision
				for _, next := range []integrationRequest{
					{Version: 1, OperationID: "next-effect", EntryID: e.ID, ExpectedRevision: revision, Kind: "intent", Effect: "gate"},
					integrationSealRequest(e, revision),
					{Version: 1, OperationID: "hold-without-pending", EntryID: e.ID, ExpectedRevision: revision, Kind: "hold", State: "uncertain"},
				} {
					if _, err := commitIntegration(path, m.store, next, integrationIO{}); !errors.Is(err, errIntegrationStale) {
						t.Fatal("fresh-source guard bypassed without pending result")
					}
				}
			})
		}
	}
}

func TestIntegrationLineageAndDependencies(t *testing.T) {
	m, path, e := integrationFixture(t)
	bad := e
	bad.Predecessor, bad.Attempt = "MISSING", 2
	if _, err := commitIntegration(path, m.store, integrationSubmit(bad), integrationIO{}); err == nil {
		t.Fatal("missing predecessor accepted")
	}
	bad = e
	bad.Dependencies = []string{"MISSING"}
	if _, err := commitIntegration(path, m.store, integrationSubmit(bad), integrationIO{}); err == nil {
		t.Fatal("missing dependency accepted")
	}
	integrationSave(t, m, path, integrationSubmit(e))
	integrationSave(t, m, path, integrationRequest{Version: 1, OperationID: "reject", EntryID: e.ID, ExpectedRevision: 1, Kind: "hold", State: "rejected"})
	e2 := e
	e2.ID, e2.Predecessor, e2.Attempt, e2.Dependencies = "ENTRY-TWO", e.ID, 2, []string{e.ID}
	r := integrationSubmit(e2)
	r.OperationID = "submit-two"
	integrationSave(t, m, path, r)
	r = integrationSealRequest(e2, 1)
	r.OperationID = "seal-two"
	integrationSave(t, m, path, r)
	p, err := readIntegration(path)
	if err != nil || integrationEligible(p, e2.ID) || p.Entries[e.ID].State != "rejected" || p.Entries[e2.ID].Entry.Attempt != 2 {
		t.Fatal("lineage or dependency hold lost")
	}
	r = integrationRequest{Version: 1, OperationID: "forged-acceptance", EntryID: e2.ID, ExpectedRevision: 2, Kind: "hold", State: "accepted"}
	if _, err := commitIntegration(path, m.store, r, integrationIO{}); err == nil {
		t.Fatal("untrusted receipt became acceptance")
	}
}

func TestIntegrationStorageFailures(t *testing.T) {
	for _, stage := range []string{"before_append", "journal_sync", "directory_sync", "ack"} {
		t.Run(stage, func(t *testing.T) {
			m, path, e := integrationFixture(t)
			r := integrationSubmit(e)
			_, err := commitIntegration(path, m.store, r, integrationIO{checkpoint: func(s string) error {
				if s == stage {
					return errors.New("fixture fault")
				}
				return nil
			}})
			if err == nil {
				t.Fatal("storage/ack fault reported success")
			}
			p, readErr := readIntegration(path)
			if readErr != nil {
				t.Fatal("complete faulted append unreadable")
			}
			want := uint64(1)
			if stage == "before_append" {
				want = 0
			} else if !errors.Is(err, errIntegrationUnconfirmed) {
				t.Fatal("complete write lost uncertainty")
			}
			if p.Sequence != want {
				t.Fatal("fault changed committed boundary")
			}
			integrationSave(t, m, path, r)
			p, _ = readIntegration(path)
			if p.Sequence != 1 || p.Entries[e.ID].State != "submitted" {
				t.Fatal("retry duplicated or accepted work")
			}
		})
	}
}

func TestIntegrationCrossStoreAndLocks(t *testing.T) {
	m, path, e := integrationFixture(t)
	root, err := openLaneControlRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	_, err = commitIntegration(path, m.store, integrationSubmit(e), integrationIO{checkpoint: func(stage string) error {
		if stage == "snapshot" {
			// Neither lock is held here. Change the source after the first snapshot.
			lock, err := openIntegrationLock(root)
			if err != nil {
				t.Fatal("snapshot held integration lock")
			}
			lock.Close()
			_, err = m.noteGoal(goalNoteRequest{OperationID: "changed-after-snapshot", ExpectedRevision: 1, GoalID: e.Source.GoalID, Note: goalRecordNote{Text: "new revision"}})
			if err != nil {
				t.Fatal("snapshot held control lock")
			}
		}
		return nil
	}})
	if !errors.Is(err, errIntegrationUnconfirmed) {
		t.Fatal("changed cross-store snapshot acknowledged")
	}
	p, err := readIntegration(path)
	if err != nil || p.Sequence != 1 || p.Entries[e.ID].State != "submitted" {
		t.Fatal("cross-store conflict lost durable state")
	}
	if _, err := commitIntegration(path, m.store, integrationSealRequest(e, 1), integrationIO{}); !errors.Is(err, errIntegrationStale) {
		t.Fatal("stale entry sealed")
	}
	// A held integration lock cannot block ordinary control operations.
	lock, err := openIntegrationLock(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := captureIntegrationSource(m.store, e.Source.GoalID, e.Source.ReviewID); err != nil {
		t.Fatal("nested locks or coupled failure domain")
	}
	if _, err := openIntegrationLock(root); !errors.Is(err, errIntegrationLocked) {
		t.Fatal("second controller did not refuse promptly")
	}
	lock.Close()
	// Conversely, holding control.lock does not prevent standalone replay/CAS.
	sourceRoot, err := openLaneControlRoot(m.store)
	if err != nil {
		t.Fatal(err)
	}
	defer sourceRoot.Close()
	control, err := laneControlLock(sourceRoot, unix.LOCK_EX)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readIntegration(path); err != nil {
		t.Fatal("replay needed control lock")
	}
	lock, err = openIntegrationLock(root)
	if err != nil {
		t.Fatal("integration lock needed control lock")
	}
	lock.Close()
	control.Close()
}

func TestIntegrationTwoControllers(t *testing.T) {
	m, path, e := integrationFixture(t)
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, id := range []string{"controller-one", "controller-two"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			<-start
			r := integrationSubmit(e)
			r.OperationID = id
			_, err := commitIntegration(path, m.store, r, integrationIO{})
			results <- err
		}(id)
	}
	close(start)
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	p, err := readIntegration(path)
	if err != nil || success != 1 || p.Sequence != 1 {
		t.Fatal("controllers both won entry CAS")
	}
}

func TestIntegrationControllerProcessHelper(t *testing.T) {
	path := os.Getenv("NUNTIUS_INTEGRATION_LOCK_FIXTURE")
	if path == "" {
		return
	}
	root, err := openLaneControlRoot(path)
	if err != nil {
		t.Fatal("helper root failed")
	}
	defer root.Close()
	if lock, err := openIntegrationLock(root); !errors.Is(err, errIntegrationLocked) {
		if lock != nil {
			lock.Close()
		}
		t.Fatal("independent controller did not see kernel lock")
	}
}

func TestIntegrationProcessContention(t *testing.T) {
	_, path, _ := integrationFixture(t)
	root, err := openLaneControlRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	lock, err := openIntegrationLock(root)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestIntegrationControllerProcessHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(), "NUNTIUS_INTEGRATION_LOCK_FIXTURE="+path)
	if err := cmd.Run(); err != nil {
		t.Fatal("independent process contention proof failed")
	}
}

func TestIntegrationPrivateFiles(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "mode", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			_, path, _ := integrationFixture(t)
			target := filepath.Join(path, integrationLogName)
			other := filepath.Join(path, "other")
			if err := os.WriteFile(other, []byte{}, 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "symlink":
				if err := os.Symlink(other, target); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(other, target); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err := os.WriteFile(target, []byte{}, 0644); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := unix.Mkfifo(target, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := readIntegration(path); err == nil {
				t.Fatal("unsafe integration file accepted")
			}
		})
	}
}

func TestIntegrationClosedReplay(t *testing.T) {
	m, path, e := integrationFixture(t)
	integrationSave(t, m, path, integrationSubmit(e))
	data, err := os.ReadFile(filepath.Join(path, integrationLogName))
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"header", "body", "truncated-header", "truncated-body", "duplicate-frame", "unknown-version", "unknown-key", "null", "alias", "missing-field", "duplicate-key", "sequence-gap"} {
		t.Run(scenario, func(t *testing.T) {
			bad := append([]byte(nil), data...)
			switch scenario {
			case "header":
				bad[19] ^= 1
			case "body":
				bad[len(bad)-1] ^= 1
			case "truncated-header":
				bad = bad[:20]
			case "truncated-body":
				bad = bad[:len(bad)-1]
			case "duplicate-frame":
				bad = append(bad, data...)
			case "sequence-gap":
				bad = encodeIntegrationFrame(2, data[84:])
			default:
				body := string(data[84:])
				switch scenario {
				case "unknown-version":
					body = strings.Replace(body, `"version":1`, `"version":2`, 1)
				case "unknown-key":
					body = strings.Replace(body, `"version":1`, `"extra":true,"version":1`, 1)
				case "null":
					body = strings.Replace(body, `"dependencies":[]`, `"dependencies":null`, 1)
				case "alias":
					body = strings.Replace(body, `"version":1`, `"Version":1`, 1)
				case "missing-field":
					body = strings.Replace(body, `,"review_verified":true`, ``, 1)
				case "duplicate-key":
					body = strings.Replace(body, `"version":1`, `"version":1,"version":1`, 1)
				}
				bad = encodeIntegrationFrame(1, []byte(body))
			}
			if p, err := replayIntegration(bad); err == nil || p != nil {
				t.Fatal("invalid history returned a prefix")
			}
		})
	}
	// Reads do not create a lock or repair torn/corrupt bytes.
	if err := os.WriteFile(filepath.Join(path, integrationLogName), data[:len(data)-1], 0600); err != nil {
		t.Fatal(err)
	}
	before := goalCommitTree(t, path)
	if _, err := readIntegration(path); err == nil {
		t.Fatal("torn integration tail accepted")
	}
	if _, err := commitIntegration(path, m.store, integrationSubmit(e), integrationIO{}); err == nil {
		t.Fatal("writer repaired torn tail")
	}
	if !reflect.DeepEqual(before, goalCommitTree(t, path)) {
		t.Fatal("failed replay/CAS changed bytes")
	}
}

func TestIntegrationReplayCapacity(t *testing.T) {
	m, path, e := integrationFixture(t)
	caps := integrationCaps()
	p := &integrationProjection{Entries: map[string]integrationRecord{}, Operations: map[string]integrationOperation{}}
	var data []byte
	previous := ""
	for i := 0; i < caps.MaxEvents; i++ {
		r := integrationSubmit(e)
		if i != 0 {
			r = integrationRequest{Version: 1, OperationID: "hold-" + strconv.Itoa(i), EntryID: e.ID, ExpectedRevision: uint64(i), Kind: "hold", State: "parked"}
		}
		input := integrationDigestJSON(r)
		op, err := applyIntegration(p, r, input)
		if err != nil {
			t.Fatal("capacity control failed")
		}
		body, _ := json.Marshal(integrationEvent{Version: 1, Previous: previous, Input: input, Request: r})
		data = append(data, encodeIntegrationFrame(op.Sequence, body)...)
		previous = integrationDigest(body)
	}
	if p, err := replayIntegration(data); err != nil || p.Sequence != uint64(caps.MaxEvents) {
		t.Fatal("exact event capacity refused")
	}
	if err := os.WriteFile(filepath.Join(path, integrationLogName), data, 0600); err != nil {
		t.Fatal(err)
	}
	r := integrationRequest{Version: 1, OperationID: "overflow", EntryID: e.ID, ExpectedRevision: uint64(caps.MaxEvents), Kind: "hold", State: "parked"}
	if _, err := commitIntegration(path, m.store, r, integrationIO{}); err == nil {
		t.Fatal("full integration store accepted new event")
	}
	if got, err := os.ReadFile(filepath.Join(path, integrationLogName)); err != nil || !bytes.Equal(got, data) {
		t.Fatal("full store changed")
	}
	if _, err := replayIntegration(make([]byte, caps.MaxBytes+1)); err == nil {
		t.Fatal("oversized integration store accepted")
	}
}

func TestIntegrationLegacyBinary(t *testing.T) {
	m, path, e := integrationFixture(t)
	integrationSave(t, m, path, integrationSubmit(e))
	before, err := os.ReadFile(filepath.Join(m.store, goalJournalName))
	if err != nil {
		t.Fatal(err)
	}
	// The ordinary store may also contain integration files: old binaries must
	// ignore them, including unsupported integration schemas.
	data, err := os.ReadFile(filepath.Join(path, integrationLogName))
	if err != nil {
		t.Fatal(err)
	}
	c, store := fixture(t)
	if err := os.MkdirAll(store, 0700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "lanes.json")
	b, _ := json.Marshal(c)
	if err := os.WriteFile(config, b, 0600); err != nil {
		t.Fatal(err)
	}
	binaryPath := os.Getenv("NUNTIUS_INTEGRATION_BASE_BINARY")
	if binaryPath == "" {
		binaryPath = cliBinary
	}
	for _, unsupported := range []bool{false, true} {
		if unsupported {
			binary.BigEndian.PutUint32(data[4:8], 99)
		}
		if err := os.WriteFile(filepath.Join(store, integrationLogName), data, 0600); err != nil {
			t.Fatal(err)
		}
		input := "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\"}\n{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}\n"
		input += "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/call\",\"params\":{\"name\":\"nuntius_dispatch\",\"arguments\":{\"lane\":\"codex\",\"goal_id\":\"LEGACY-CONTROL\",\"text\":\"hello\"}}}\n"
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd := exec.CommandContext(ctx, binaryPath, "--config", config, "--store", store, "mcp")
		cmd.Stdin = strings.NewReader(input)
		out, err := cmd.Output()
		cancel()
		// Never expose raw machine output or identifiers on a failing assertion.
		if err != nil || bytes.Contains(out, []byte(`"isError":true`)) {
			t.Fatal("legacy binary refused ordinary launch")
		}
		runs, err := (&Manager{store: store}).history()
		if err != nil || len(runs) != 1 || runs[0].PID <= 0 || runs[0].ProcessStart == "" {
			t.Fatal("legacy binary did not launch compiled fake")
		}
		// Separate stores keep the two attempts independent, with fixture-owned cleanup.
		if unsupported == false {
			c, store = fixture(t)
			if err := os.MkdirAll(store, 0700); err != nil {
				t.Fatal(err)
			}
			b, _ = json.Marshal(c)
			if err := os.WriteFile(config, b, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	after, err := os.ReadFile(filepath.Join(m.store, goalJournalName))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("integration wrote goal.spec journal")
	}
}
