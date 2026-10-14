package nuntius

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func projectionRead(t *testing.T, m *Manager, env goalAdmissionEnvironment) goalQueueSnapshot {
	t.Helper()
	v, err := m.projectGoals(env)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestQueuePartition(t *testing.T) {
	m, s, env := admissionGoalFixture(t)
	admissionApproved(t, m, s)
	for _, state := range []nativeGoalState{nativeGoalDrafted, nativeGoalRunning, nativeGoalEnded, nativeGoalParked, nativeGoalVerified, nativeGoalCancelled} {
		dependencyFixture(t, m, "STATE-"+strings.ToUpper(string(state)), state, nil)
	}
	held := s
	held.ID, held.OwnerHold = "OWNER-HELD", &nativeOwnerHold{Categories: []string{"release"}, Reason: "Owner decision"}
	if _, err := m.createGoal(goalRecordRequest{OperationID: "owner-create", Spec: held}); err != nil {
		t.Fatal(err)
	}
	v := projectionRead(t, m, env)
	c := v.Counts
	if c.Open != 6 || c.Drafted != 2 || c.Approved != 1 || c.Running != 1 || c.Ended != 1 || c.Parked != 1 || c.Verified != 1 || c.Cancelled != 1 || c.NeedsOwner != 1 {
		t.Fatalf("incorrect partition: %+v", c)
	}
	if c.Drafted+c.Approved+c.Running+c.Ended+c.Parked != c.Open || c.Ready > c.Approved || c.Startable > c.Ready {
		t.Fatal("subset or partition violation")
	}
	if len(v.Goals) != c.Open || c.Ready != 1 {
		t.Fatal("open rows or readiness incorrect")
	}
	seen := map[string]bool{}
	for _, r := range v.Goals {
		if seen[r.ID] || goalRecordTerminal(r.State) || r.Ready && r.State != nativeGoalApproved || r.Startable && !r.Ready {
			t.Fatal("incorrect row membership")
		}
		seen[r.ID] = true
	}
}

func TestComputedIdle(t *testing.T) {
	m, high, env := admissionGoalFixture(t)
	high.ID, high.Order, high.After = "HIGH-BLOCKED", 0, []string{"MISSING"}
	admissionApproved(t, m, high)
	low := high
	low.ID, low.Order, low.After = "LOW-READY", 10, []string{}
	admissionApproved(t, m, low)
	projectionAudit(t, m)
	v := projectionRead(t, m, env)
	if !v.Complete || !v.Attention || v.Counts.Startable != 1 {
		t.Fatalf("manual ready must require attention: complete=%t attention=%t counts=%+v rows=%+v", v.Complete, v.Attention, v.Counts, v.Goals)
	}
	if len(v.Lanes) == 0 || v.Lanes[1].Status.Lane != "codex" || v.Lanes[1].Reason.Code != "manual_ready" {
		t.Fatal("lower startable candidate must defeat blocked priority")
	}
	for _, lane := range v.Lanes {
		if strings.Contains(lane.Reason.Explanation, "TTL") {
			t.Fatal("idle TTL is not a reason")
		}
	}
	if v.AutomationAvailable {
		t.Fatal("projection cannot grant automation")
	}
}

func TestGoalProjectionReadOnly(t *testing.T) {
	m, s, env := admissionGoalFixture(t)
	admissionApproved(t, m, s)
	projectionAudit(t, m)
	before := projectionFiles(t, m.store)
	a := projectionRead(t, m, env)
	if a.Revision == "" || a.Sequence != 3 || a.ControlRevision != 0 {
		t.Fatal("missing composite binding")
	}
	a.Goals[0].Kinds[0] = "claude"
	b := projectionRead(t, m, env)
	if b.Goals[0].Kinds[0] != "codex" {
		t.Fatal("snapshot alias leaked")
	}
	after := projectionFiles(t, m.store)
	if string(before) != string(after) {
		t.Fatal("passive read mutated store")
	}
	encoded, err := json.Marshal(b)
	if err != nil || strings.Contains(string(encoded), env.Roots["repo"]) {
		t.Fatal("physical workspace path escaped")
	}
}

func projectionAudit(t *testing.T, m *Manager) {
	t.Helper()
	if _, err := m.mirrorNativeAudit("native", goalCommitIO{}); err != nil {
		t.Fatal(err)
	}
}

func TestGoalProjectionDraining(t *testing.T) {
	m, s, env := admissionGoalFixture(t)
	s.Resources = []string{"mint"}
	admissionApproved(t, m, s)
	r := admissionSyntheticRun(t, m, "LEGACY-RUN", "codex", "running", false)
	env.LegacyScopes = map[string]goalAdmissionScope{r.ID: {Writes: []string{"repo/docs"}, Resources: []string{"mint"}}}
	if _, err := m.setLaneControl(laneControlChange{Lane: "codex", Paused: true, AfterCurrent: true}, "cli"); err != nil {
		t.Fatal(err)
	}
	projectionAudit(t, m)
	before := projectionFiles(t, m.store)
	v := projectionRead(t, m, env)
	if !v.Complete || v.Counts.Ready != 1 || v.Counts.Startable != 0 || v.ControlRevision != 1 {
		t.Fatal("draining lost counts or binding")
	}
	row := v.Goals[0]
	for _, code := range []string{"write_conflict", "resource_conflict", "lane_paused", "running"} {
		if !projectionBlocked(row.Blockers, code) {
			t.Fatalf("draining lost %s", code)
		}
	}
	if v.Lanes[1].Status.LaneControl.Phase != "draining" || v.Lanes[1].Status.State != "running" || v.Lanes[1].Reason.Code != "running" {
		t.Fatal("draining overwrote run state")
	}
	if !bytes.Equal(before, projectionFiles(t, m.store)) {
		t.Fatal("draining read changed storage")
	}
}

func projectionBlocked(blockers []goalAdmissionBlocker, code string) bool {
	for _, b := range blockers {
		if b.Code == code {
			return true
		}
	}
	return false
}

func TestGoalProjectionObservations(t *testing.T) {
	for _, scenario := range []string{"source_missing", "source_changed", "source_error", "claims_missing", "claims_stale", "claims_partial", "claims_duplicate", "claims_future", "claims_held"} {
		t.Run(scenario, func(t *testing.T) {
			m, s, env := admissionGoalFixture(t)
			admissionApproved(t, m, s)
			projectionAudit(t, m)
			code, complete := "", false
			switch scenario {
			case "source_missing":
				env.SourceState, code = nil, "source_unavailable"
			case "source_changed":
				env.SourceState, code, complete = func(nativeGoalSpec) (string, error) { return "observed-head-2", nil }, "source_changed", true
			case "source_error":
				env.SourceState, code = func(nativeGoalSpec) (string, error) { return "", errors.New("private error") }, "source_unavailable"
			default:
				code = "claims_unavailable"
				if scenario == "claims_missing" {
					env.Claims = nil
					break
				}
				env.Claims = func(roots []string) ([]goalAdmissionClaim, error) {
					row := goalAdmissionClaim{Root: roots[0], ObservedAt: time.Now()}
					switch scenario {
					case "claims_stale":
						row.ObservedAt = time.Now().Add(-31 * time.Second)
					case "claims_partial":
						return []goalAdmissionClaim{}, nil
					case "claims_duplicate":
						return []goalAdmissionClaim{row, row}, nil
					case "claims_future":
						row.ObservedAt = time.Now().Add(time.Hour)
					case "claims_held":
						row.Holder = "another-goal"
					}
					return []goalAdmissionClaim{row}, nil
				}
				if scenario == "claims_held" {
					code, complete = "repository_claimed", true
				}
			}
			before := projectionFiles(t, m.store)
			v := projectionRead(t, m, env)
			if v.Complete != complete || v.Counts.Ready != 1 || v.Counts.Startable != 0 || !projectionBlocked(v.Goals[0].Blockers, code) {
				t.Fatalf("wrong observation classification for %s", scenario)
			}
			if !complete && (!v.Attention || v.Lanes[1].Reason.Code != "observation_unavailable") {
				t.Fatal("unknown evidence permitted quiet stop")
			}
			if !bytes.Equal(before, projectionFiles(t, m.store)) {
				t.Fatal("observation repaired storage")
			}
			for _, observation := range v.Observations {
				if observation.Provenance == "" || strings.Contains(observation.Key, "/") {
					t.Fatal("missing provenance or raw path")
				}
			}
		})
	}
}

func TestGoalProjectionOneClaimSnapshot(t *testing.T) {
	m, first, env := admissionGoalFixture(t)
	admissionApproved(t, m, first)
	second := first
	second.ID = "SECOND-READY"
	admissionApproved(t, m, second)
	projectionAudit(t, m)
	observe := env.Claims
	calls := 0
	env.Claims = func(roots []string) ([]goalAdmissionClaim, error) {
		calls++
		if calls != 1 {
			t.Fatal("claims reread within a snapshot")
		}
		return observe(roots)
	}
	v := projectionRead(t, m, env)
	if calls != 1 || v.Counts.Startable != 2 || len(v.Goals) != 2 {
		t.Fatal("shared claims or candidate completeness failed")
	}
	claimCount := 0
	for _, observation := range v.Observations {
		if observation.Kind == "claim" {
			claimCount++
			if !observation.Available || observation.ObservedAt.IsZero() {
				t.Fatal("invalid available observation")
			}
		}
	}
	if claimCount != 1 {
		t.Fatal("claim observation was duplicated")
	}
}

func TestGoalProjectionConsistency(t *testing.T) {
	m, s, env := admissionGoalFixture(t)
	admissionApproved(t, m, s)
	dependencyFixture(t, m, "UNBOUND-RUNNING", nativeGoalRunning, nil)
	projectionAudit(t, m)
	v := projectionRead(t, m, env)
	if v.Complete || !v.Attention || v.Counts.Startable != 0 || !projectionBlocked(v.Consistency, "attempt_observation_mismatch") {
		t.Fatal("unbound native running state was considered healthy")
	}
}

func TestGoalProjectionControlReasons(t *testing.T) {
	for _, code := range []string{"lane_paused", "frozen", "lane_control_unavailable", "waiting_to_resume", "held_message"} {
		t.Run(code, func(t *testing.T) {
			in := projectionPureInput()
			s := in.statuses["codex"]
			s.LaneControl.Blockers = []string{code}
			in.statuses["codex"] = s
			v, err := computeGoalQueue(in)
			if err != nil || v.Counts.Ready != 1 || v.Counts.Startable != 0 || v.Lanes[0].Reason.Code != code {
				t.Fatal("control reason or subsets incorrect")
			}
		})
	}
	in := projectionPureInput()
	in.rows = nil
	v, err := computeGoalQueue(in)
	if err != nil || v.Lanes[0].Reason.Code != "no_matching_kind" || v.Attention {
		t.Fatal("empty queue misclassified")
	}
	in = projectionPureInput()
	in.lanes = append(in.lanes, Lane{Name: "codex-z", Kind: "codex"})
	in.statuses["codex-z"] = in.statuses["codex"]
	in.capabilities["codex-z"] = in.capabilities["codex"]
	v, err = computeGoalQueue(in)
	if err != nil || v.Goals[0].Lane != "codex" {
		t.Fatal("lane name tie-break is not deterministic")
	}
}

func projectionPureInput() goalQueueInput {
	return goalQueueInput{
		snapshot:     goalQueueSnapshot{Version: 1, Complete: true, StorePresent: true, Goals: []goalQueueRow{}, Lanes: []goalQueueLane{}, Observations: []goalQueueObservation{}, Consistency: []goalAdmissionBlocker{}},
		rows:         []goalQueueRow{{nativeGoalDTO: nativeGoalDTO{ID: "PURE-GOAL", State: nativeGoalApproved, Kinds: []nativeEngineKind{"codex"}, WorkKind: "code", LaneKind: "codex"}, Ready: true, Blockers: []goalAdmissionBlocker{}}},
		lanes:        []Lane{{Name: "codex", Kind: "codex"}},
		statuses:     map[string]Status{"codex": {Lane: "codex", State: "idle", LaneControl: laneControlView{Name: "codex", Configured: true, DispatchEligible: true, Blockers: []string{}}}},
		capabilities: map[string]goalAdmissionCapability{"codex": {Kind: "codex", WorkKinds: []nativeWorkKind{"code"}}},
		busy:         map[string]bool{},
	}
}

func TestGoalProjectionBoundAndIdentity(t *testing.T) {
	in := projectionPureInput()
	a, err := computeGoalQueue(in)
	if err != nil {
		t.Fatal(err)
	}
	in = projectionPureInput()
	in.snapshot.ControlRevision++
	b, err := computeGoalQueue(in)
	if err != nil || a.Revision == b.Revision {
		t.Fatal("control revision did not bind snapshot")
	}
	in = projectionPureInput()
	in.snapshot.Sequence++
	c, err := computeGoalQueue(in)
	if err != nil || a.Revision == c.Revision {
		t.Fatal("journal sequence did not bind snapshot")
	}
	in = projectionPureInput()
	in.rows[0].Title = strings.Repeat("x", goalRecordSnapshotMaxBytes)
	if _, err := computeGoalQueue(in); err == nil {
		t.Fatal("oversized snapshot returned partial success")
	}
}

func TestGoalProjectionTextStatus(t *testing.T) {
	m, s, env := admissionGoalFixture(t)
	admissionApproved(t, m, s)
	projectionAudit(t, m)
	v := projectionRead(t, m, env)
	rows := []Status{}
	for _, lane := range v.Lanes {
		rows = append(rows, lane.Status)
		if lane.Status.GoalProjectionRevision != v.Revision {
			t.Fatal("mixed status revision")
		}
	}
	var out bytes.Buffer
	if err := m.writeStatus(&out, map[string]any{"lanes": rows, "frozen": false, "native_queue": v}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "ready=1 startable=1") || !strings.Contains(out.String(), "Reviewed work can start manually") {
		t.Fatal("text status omitted passive attention")
	}
}

func projectionFiles(t *testing.T, root string) []byte {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		files[strings.TrimPrefix(path, root)] = string(b) + info.ModTime().String() + info.Mode().String()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(files)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
