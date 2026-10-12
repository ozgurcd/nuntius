package nuntius

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func admissionGoalFixture(t *testing.T) (*Manager, nativeGoalSpec, goalAdmissionEnvironment) {
	t.Helper()
	m, req := goalRecordsFixture(t)
	m.config.Lanes = []Lane{{Name: "codex", Kind: "codex"}, {Name: "claude", Kind: "claude"}}
	if err := m.prepareControlStore(); err != nil {
		t.Fatal(err)
	}
	lock, err := m.controlLock()
	if err != nil {
		t.Fatal(err)
	}
	lock.Close()
	s := req.Spec
	s.ID, s.PreferredLane, s.LaneBinding = "ADMISSION", "codex", "required"
	s.After, s.Writes, s.Resources = []string{}, []string{"repo/docs"}, []string{}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	env := goalAdmissionEnvironment{
		Roots: map[string]string{"repo": root},
		Capabilities: map[string]goalAdmissionCapability{
			"codex":  {Kind: "codex", WorkKinds: []nativeWorkKind{"review", "code"}},
			"claude": {Kind: "claude", WorkKinds: []nativeWorkKind{"review", "code"}},
		},
		SourceState: func(nativeGoalSpec) (string, error) { return "observed-head-1", nil },
		Claims: func(roots []string) ([]goalAdmissionClaim, error) {
			rows := []goalAdmissionClaim{}
			for _, root := range roots {
				rows = append(rows, goalAdmissionClaim{Root: root, ObservedAt: time.Now()})
			}
			return rows, nil
		},
	}
	return m, s, env
}

func admissionApproved(t *testing.T, m *Manager, s nativeGoalSpec) []byte {
	t.Helper()
	_, in := briefFixture(t)
	in.Body = []byte(fmt.Sprintf("GOAL: %s (size %s)\nReview the declared scope.\n", s.ID, s.Size))
	if _, err := m.createGoal(goalRecordRequest{OperationID: s.ID + "-create", Spec: s}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.sealGoalBrief(goalRecordRequest{OperationID: s.ID + "-seal", ExpectedRevision: 1, Spec: s}, in); err != nil {
		t.Fatal(err)
	}
	if _, err := m.decideGoal(goalDecisionRequest{OperationID: s.ID + "-approve", GoalID: s.ID, ExpectedRevision: 2, Action: "approve", Reason: "Reviewed exact scope", Authority: in.Authority, Evidence: "review", SourceState: in.SourceState}); err != nil {
		t.Fatal(err)
	}
	p, err := replayGoalJournal(m.store)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(m.store, p.records[s.ID].sealRef))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func admissionObserved(t *testing.T, m *Manager, env goalAdmissionEnvironment) goalAdmissionSnapshot {
	t.Helper()
	if _, err := m.mirrorNativeAudit("native", goalCommitIO{}); err != nil {
		t.Fatal(err)
	}
	v, err := m.admitGoals(env)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func admissionResult(t *testing.T, v goalAdmissionSnapshot, id string) goalAdmissionResult {
	t.Helper()
	for _, r := range v.Goals {
		if r.GoalID == id {
			return r
		}
	}
	t.Fatal("admission omitted the goal")
	return goalAdmissionResult{}
}

func admissionHas(r goalAdmissionResult, code string) bool {
	for _, b := range r.Blockers {
		if b.Code == code && b.Explanation != "" && b.Explanation != code {
			return true
		}
	}
	return false
}

func TestGoalAdmission(t *testing.T) {
	for _, tc := range []struct {
		name      string
		change    func(*testing.T, *Manager, *nativeGoalSpec, *goalAdmissionEnvironment)
		ready     bool
		startable bool
		code      string
	}{
		{"eligible", nil, true, true, ""},
		{"unknown-dependency", func(t *testing.T, m *Manager, s *nativeGoalSpec, env *goalAdmissionEnvironment) {
			s.After = []string{"UNKNOWN"}
		}, false, false, "dependencies_unverified"},
		{"required-missing", func(t *testing.T, m *Manager, s *nativeGoalSpec, env *goalAdmissionEnvironment) {
			s.PreferredLane = "removed"
		}, true, false, "lane_not_configured"},
		{"wrong-engine", func(t *testing.T, m *Manager, s *nativeGoalSpec, env *goalAdmissionEnvironment) {
			m.config.Lanes[0].Kind = "opencode"
		}, true, false, "lane_kind"},
		{"capability-missing", func(t *testing.T, m *Manager, s *nativeGoalSpec, env *goalAdmissionEnvironment) {
			delete(env.Capabilities, "codex")
		}, true, false, "lane_capability"},
		{"capability-stale-kind", func(t *testing.T, m *Manager, s *nativeGoalSpec, env *goalAdmissionEnvironment) {
			env.Capabilities["codex"] = goalAdmissionCapability{Kind: "claude", WorkKinds: []nativeWorkKind{"review"}}
		}, true, false, "lane_capability"},
		{"work-kind", func(t *testing.T, m *Manager, s *nativeGoalSpec, env *goalAdmissionEnvironment) {
			env.Capabilities["codex"] = goalAdmissionCapability{Kind: "codex", WorkKinds: []nativeWorkKind{"code"}}
		}, true, false, "lane_capability"},
		{"unknown-root", func(t *testing.T, m *Manager, s *nativeGoalSpec, env *goalAdmissionEnvironment) {
			s.Writes = []string{"unknown/docs"}
		}, true, false, "write_scope_unavailable"},
		{"claims-unknown", func(t *testing.T, m *Manager, s *nativeGoalSpec, env *goalAdmissionEnvironment) { env.Claims = nil }, true, false, "claims_unavailable"},
		{"claims-omitted", func(t *testing.T, m *Manager, s *nativeGoalSpec, env *goalAdmissionEnvironment) {
			env.Claims = func([]string) ([]goalAdmissionClaim, error) { return nil, nil }
		}, true, false, "claims_unavailable"},
		{"claims-stale", func(t *testing.T, m *Manager, s *nativeGoalSpec, env *goalAdmissionEnvironment) {
			env.Claims = func(roots []string) ([]goalAdmissionClaim, error) {
				return []goalAdmissionClaim{{Root: roots[0], ObservedAt: time.Now().Add(-time.Minute)}}, nil
			}
		}, true, false, "claims_unavailable"},
		{"claims-held", func(t *testing.T, m *Manager, s *nativeGoalSpec, env *goalAdmissionEnvironment) {
			env.Claims = func(roots []string) ([]goalAdmissionClaim, error) {
				return []goalAdmissionClaim{{Root: roots[0], Holder: "another-goal", ObservedAt: time.Now()}}, nil
			}
		}, true, false, "repository_claimed"},
		{"source-changed", func(t *testing.T, m *Manager, s *nativeGoalSpec, env *goalAdmissionEnvironment) {
			env.SourceState = func(nativeGoalSpec) (string, error) { return "new-head", nil }
		}, true, false, "source_changed"},
		{"source-line-separator", func(t *testing.T, m *Manager, s *nativeGoalSpec, env *goalAdmissionEnvironment) {
			env.SourceState = func(nativeGoalSpec) (string, error) { return "head\u2028second", nil }
		}, true, false, "source_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, s, env := admissionGoalFixture(t)
			if tc.change != nil {
				tc.change(t, m, &s, &env)
			}
			admissionApproved(t, m, s)
			v := admissionObserved(t, m, env)
			r := admissionResult(t, v, s.ID)
			if r.Ready != tc.ready || r.Startable != tc.startable || tc.code != "" && !admissionHas(r, tc.code) {
				t.Fatal("wrong ready/startable decision or missing plain explanation", r)
			}
			if tc.startable && (v.SelectedGoal != s.ID || v.SelectedLane != "codex") {
				t.Fatal("eligible goal not selected")
			}
		})
	}
}

func TestGoalAdmissionConflicts(t *testing.T) {
	for _, tc := range []struct {
		name, held, candidate string
		conflict              bool
	}{
		{"ancestor", "repo/docs", "repo/docs/api", true},
		{"descendant", "repo/docs/api", "repo/docs", true},
		{"whole-repo", "repo", "repo/docs", true},
		{"whole-repo-candidate", "repo/docs", "repo", true},
		{"same-path", "repo/docs", "repo/docs", true},
		{"siblings", "repo/docs", "repo/src", false},
		{"component-prefix", "repo/docs", "repo/docs-other", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, s, env := admissionGoalFixture(t)
			s.Writes = []string{tc.candidate}
			admissionApproved(t, m, s)
			held := s
			held.ID, held.Writes = "HELD", []string{tc.held}
			admissionReservation(t, m, held, nativeGoalRunning, &goalDecisionAttempt{ID: "attempt", Ended: false})
			r := admissionResult(t, admissionObserved(t, m, env), s.ID)
			if r.Startable == tc.conflict || tc.conflict && !admissionHas(r, "write_conflict") {
				t.Fatal("path overlap predicate failed", r)
			}
		})
	}
	for _, state := range []nativeGoalState{nativeGoalRunning, nativeGoalEnded, nativeGoalParked} {
		t.Run("mint-"+string(state), func(t *testing.T) {
			m, s, env := admissionGoalFixture(t)
			s.Resources = []string{"mint"}
			admissionApproved(t, m, s)
			held := s
			held.ID, held.Writes = "MINT-HELD", []string{}
			admissionReservation(t, m, held, state, &goalDecisionAttempt{ID: "unconfirmed", Ended: true, LaunchUnconfirmed: true})
			r := admissionResult(t, admissionObserved(t, m, env), s.ID)
			if r.Startable || !admissionHas(r, "resource_conflict") {
				t.Fatal("unsettled attempt released exclusive mint", r)
			}
			if state != nativeGoalRunning && !admissionHas(r, "reservation_unavailable") {
				t.Fatal("unsettled historical scope treated as current")
			}
		})
	}
}

func admissionReservation(t *testing.T, m *Manager, s nativeGoalSpec, state nativeGoalState, attempt *goalDecisionAttempt) {
	t.Helper()
	spec, _ := json.Marshal(s)
	data, err := encodeGoalRecord(goalRecordData{Version: 1, State: state, Notes: []goalRecordNote{}, Attempt: attempt})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.commitGoal(goalCommitRequest{OperationID: s.ID + "-reservation", Spec: spec, Record: data, MutationSHA256: strings.Repeat("c", 64)}, goalCommitIO{}); err != nil {
		t.Fatal(err)
	}
}

func TestGoalAdmissionSelection(t *testing.T) {
	m, s, env := admissionGoalFixture(t)
	s.ID, s.Order, s.PreferredLane = "HIGH", 0, "removed"
	admissionApproved(t, m, s)
	s.ID, s.Order, s.LaneBinding, s.PreferredLane = "Z-ELIGIBLE", 1, "preferred", "absent"
	admissionApproved(t, m, s)
	s.ID = "A-ELIGIBLE"
	admissionApproved(t, m, s)
	v := admissionObserved(t, m, env)
	if v.SelectedGoal != "A-ELIGIBLE" || v.SelectedLane != "codex" || len(v.Goals) != 3 || v.Goals[0].GoalID != "HIGH" || v.Goals[1].GoalID != "A-ELIGIBLE" {
		t.Fatal("selection did not skip blocked priority or use ASCII ties/preferred engine", v)
	}
	// Preferred named lane wins over engine preference, then name ties are ASCII.
	m.config.Lanes = append(m.config.Lanes, Lane{Name: "codex-a", Kind: "codex"})
	env.Capabilities["codex-a"] = env.Capabilities["codex"]
	admissionPause(t, m, false)
	v = admissionObserved(t, m, env)
	if v.SelectedLane != "codex-a" {
		t.Fatal("fallback did not use eligible engine/name ordering", v)
	}
}

func TestGoalAdmissionPauseAndHolds(t *testing.T) {
	for _, state := range []string{"paused", "draining", "frozen", "recovery", "held", "lane-lock"} {
		t.Run(state, func(t *testing.T) {
			m, s, env := admissionGoalFixture(t)
			admissionApproved(t, m, s)
			code := "lane_paused"
			switch state {
			case "paused", "draining":
				admissionPause(t, m, state == "draining")
				if state == "draining" {
					admissionSyntheticRun(t, m, s.ID, "codex", "running", false)
				}
				if err := m.Freeze("Owner review"); err != nil {
					t.Fatal(err)
				}
			case "frozen":
				if err := m.Freeze("Owner review"); err != nil {
					t.Fatal(err)
				}
				code = "frozen"
			case "recovery", "held":
				r := Run{ID: strings.Repeat("a", 32), Lane: "codex", GoalID: s.ID, Started: time.Now(), State: "ended", WaitingToResume: true}
				if err := os.Mkdir(filepath.Join(m.store, "runs", r.ID), 0700); err != nil {
					t.Fatal(err)
				}
				if err := m.save(&r); err != nil {
					t.Fatal(err)
				}
				code = "waiting_to_resume"
				if state == "held" {
					if err := os.WriteFile(filepath.Join(m.store, "runs", r.ID, "message-hold.json"), []byte("{}"), 0600); err != nil {
						t.Fatal(err)
					}
					code = "held_message"
				}
			case "lane-lock":
				lock, err := m.lock("codex")
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
				code = "lane_busy"
			}
			v := admissionObserved(t, m, env)
			r := admissionResult(t, v, s.ID)
			if !r.Ready || r.Startable || !admissionHas(r, code) {
				t.Fatal("operational hold was not retained", r)
			}
			if state == "paused" || state == "draining" {
				if r.LaneFault == nil || r.LaneFault.Code != "lane_paused" || !r.LaneFault.Paused || r.LaneFault.Retryable || !reflect.DeepEqual(r.LaneFault.Blockers, []string{"lane_paused", "frozen"}) {
					t.Fatal("primary A refusal lost safe blockers", r)
				}
			}
		})
	}
}

func TestGoalAdmissionRetainedApproval(t *testing.T) {
	m, s, env := admissionGoalFixture(t)
	seal := admissionApproved(t, m, s)
	if _, err := m.noteGoal(goalNoteRequest{OperationID: "note", ExpectedRevision: 3, GoalID: s.ID, Note: goalRecordNote{Text: "Retain approval"}}); err != nil {
		t.Fatal(err)
	}
	s.Title, s.Order = "Presentation update", 9
	if _, err := m.updateGoal(goalRecordRequest{OperationID: "presentation", ExpectedRevision: 4, Spec: s, Seal: seal}); err != nil {
		t.Fatal(err)
	}
	before := admissionObserved(t, m, env)
	r := admissionResult(t, before, s.ID)
	if !r.Ready || !r.Startable || r.Revision != 5 {
		t.Fatal("valid retained approval lost after note/presentation edit", r)
	}
	// A new reader recomputes the exact predicate from saved history.
	reopened := &Manager{store: m.store, config: m.config}
	after, err := reopened.admitGoals(env)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("replay reader disagrees with writer", err)
	}
	s.Writes = []string{"repo/src"}
	if _, err := m.updateGoal(goalRecordRequest{OperationID: "scope", ExpectedRevision: 5, Spec: s, Seal: seal}); err != nil {
		t.Fatal(err)
	}
	r = admissionResult(t, admissionObserved(t, m, env), s.ID)
	if r.Ready || r.Startable || !admissionHas(r, "approval_required") {
		t.Fatal("same seal retained approval after scope change", r)
	}
}

func TestGoalAdmissionHealth(t *testing.T) {
	for _, name := range []string{"pending-audit", "corrupt-journal", "corrupt-controls", "unapproved", "owner-hold", "coverage", "opaque-seal"} {
		t.Run(name, func(t *testing.T) {
			m, s, env := admissionGoalFixture(t)
			if name == "unapproved" || name == "owner-hold" || name == "coverage" || name == "opaque-seal" {
				if name == "owner-hold" {
					s.OwnerHold = &nativeOwnerHold{Categories: []string{"owner"}, Reason: "Owner decision needed"}
				}
				if name == "coverage" {
					s.Coverage = &nativeCoverageHold{CoveredBy: "TARGET", Reason: "Covered by another goal"}
				}
				if _, err := m.createGoal(goalRecordRequest{OperationID: "create", Spec: s}); err != nil {
					t.Fatal(err)
				}
				if name == "opaque-seal" {
					if _, err := m.updateGoal(goalRecordRequest{OperationID: "opaque", ExpectedRevision: 1, Spec: s, Seal: []byte("opaque")}); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				admissionApproved(t, m, s)
			}
			if name != "pending-audit" {
				if _, err := m.mirrorNativeAudit("native", goalCommitIO{}); err != nil {
					t.Fatal(err)
				}
			}
			if name == "corrupt-journal" {
				if err := os.WriteFile(filepath.Join(m.store, goalJournalName), []byte("invalid complete journal"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if name == "corrupt-controls" {
				if err := os.WriteFile(filepath.Join(m.store, "lane-control.json"), []byte("invalid"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			v, err := m.admitGoals(env)
			if name == "corrupt-journal" {
				if err == nil {
					t.Fatal("corrupt journal admitted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			r := admissionResult(t, v, s.ID)
			if r.Startable || v.SelectedGoal != "" {
				t.Fatal("unhealthy or unapproved goal was startable")
			}
			if name == "pending-audit" && (!r.Ready || !admissionHas(r, "native_audit_unavailable")) {
				t.Fatal("pending mirror not explained", r)
			}
		})
	}
}

func TestGoalAdmissionPauseBarrier(t *testing.T) {
	m, s, env := admissionGoalFixture(t)
	admissionApproved(t, m, s)
	if _, err := m.mirrorNativeAudit("native", goalCommitIO{}); err != nil {
		t.Fatal(err)
	}
	other := &Manager{store: m.store, config: m.config}
	// The pause commits while this caller still has an old eligible observation.
	old, err := m.admitGoals(env)
	if err != nil || old.SelectedGoal != s.ID {
		t.Fatal("initial admission unavailable", err)
	}
	admissionPause(t, other, false)
	before := goalCommitTree(t, m.store)
	v, err := m.admitGoals(env)
	if err != nil || v.SelectedGoal != "" || admissionResult(t, v, s.ID).LaneFault == nil {
		t.Fatal("pause winner allowed stale admission", err)
	}
	if !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
		t.Fatal("pause refusal created reservation, run or prompt artifacts")
	}
	// Reverse order: a concurrent pause cannot commit until the admitted
	// critical section leaves the shared control lock. No launch is part of B10.
	revision := uint64(1)
	resume, err := other.setLaneControl(laneControlChange{Lane: "codex", Paused: false, ExpectedRevision: &revision}, "cli")
	_ = resume
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	env.SourceState = func(nativeGoalSpec) (string, error) { close(entered); <-release; return "observed-head-1", nil }
	done := make(chan error, 1)
	go func() { _, err := m.admitGoals(env); done <- err }()
	<-entered
	if other.mu.TryLock() {
		other.mu.Unlock()
	} else {
		t.Fatal("fixture has unexpected manager lock")
	}
	revision = 2
	_, blocked := other.setLaneControl(laneControlChange{Lane: "codex", Paused: true, ExpectedRevision: &revision}, "cli")
	var fault *laneControlFault
	if !errors.As(blocked, &fault) || fault.Code != "locked" {
		close(release)
		<-done
		t.Fatal("pause entered admission's control critical section")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := other.setLaneControl(laneControlChange{Lane: "codex", Paused: true, ExpectedRevision: &revision}, "cli"); err != nil {
		t.Fatal(err)
	}
}

func TestGoalAdmissionSourceFailure(t *testing.T) {
	m, s, env := admissionGoalFixture(t)
	admissionApproved(t, m, s)
	env.SourceState = func(nativeGoalSpec) (string, error) { return "", errors.New("private adapter failure") }
	r := admissionResult(t, admissionObserved(t, m, env), s.ID)
	if r.Startable || !admissionHas(r, "source_unavailable") || strings.Contains(fmt.Sprint(r), "private adapter") {
		t.Fatal("source failure leaked or admitted", r)
	}
}

func admissionSyntheticRun(t *testing.T, m *Manager, goal, lane, state string, recovery bool) Run {
	t.Helper()
	r := Run{ID: strings.Repeat("b", 32), Lane: lane, GoalID: goal, Started: time.Now(), State: state, WaitingToResume: recovery}
	if err := os.Mkdir(filepath.Join(m.store, "runs", r.ID), 0700); err != nil {
		t.Fatal(err)
	}
	if err := m.save(&r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestGoalAdmissionPoolReload(t *testing.T) {
	for _, removal := range []bool{false, true} {
		t.Run(fmt.Sprint(removal), func(t *testing.T) {
			m, s, env := admissionGoalFixture(t)
			c, _ := fixture(t)
			template := c.Lanes[1]
			otherTemplate := c.Lanes[0]
			otherTemplate.Name = ""
			template.Name = ""
			c.Lanes = nil
			c.Pools = map[string]AgentPool{"codex": {Size: 1, Template: template}, "claude": {Size: 1, Template: otherTemplate}}
			path := filepath.Join(t.TempDir(), "lanes.json")
			write := func() {
				b, err := json.Marshal(c)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, b, 0600); err != nil {
					t.Fatal(err)
				}
			}
			write()
			m.config.SourcePath = path
			s.PreferredLane = "codex-1"
			env.Capabilities["codex-1"] = env.Capabilities["codex"]
			admissionApproved(t, m, s)
			if !admissionResult(t, admissionObserved(t, m, env), s.ID).Startable {
				t.Fatal("configured pool lane not eligible")
			}
			if removal {
				delete(c.Pools, "codex")
			} else {
				pool := c.Pools["codex"]
				pool.Size = 0
				c.Pools["codex"] = pool
			}
			write()
			r := admissionResult(t, admissionObserved(t, m, env), s.ID)
			if !r.Ready || r.Startable || !admissionHas(r, "lane_not_configured") {
				t.Fatal("cached pool configuration bypassed zero/removal", r)
			}
		})
	}
}

func TestGoalAdmissionCanonicalPaths(t *testing.T) {
	for _, outside := range []bool{false, true} {
		t.Run(fmt.Sprint(outside), func(t *testing.T) {
			m, s, env := admissionGoalFixture(t)
			root := env.Roots["repo"]
			target := filepath.Join(root, "docs")
			if outside {
				target = t.TempDir()
			} else if err := os.Mkdir(target, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(root, "alias")); err != nil {
				t.Fatal(err)
			}
			s.Writes = []string{"repo/alias/new"}
			admissionApproved(t, m, s)
			held := s
			held.ID, held.Writes = "RESERVED", []string{"repo/docs"}
			admissionReservation(t, m, held, nativeGoalRunning, nil)
			r := admissionResult(t, admissionObserved(t, m, env), s.ID)
			code := "write_conflict"
			if outside {
				code = "write_scope_unavailable"
			}
			if r.Startable || !admissionHas(r, code) {
				t.Fatal("canonical symlink scope was not checked", r)
			}
		})
	}
	m, s, env := admissionGoalFixture(t)
	env.Roots["same-repository"] = env.Roots["repo"]
	s.Writes = []string{"same-repository/docs"}
	admissionApproved(t, m, s)
	held := s
	held.ID, held.Writes = "ALIAS-HELD", []string{"repo/docs"}
	admissionReservation(t, m, held, nativeGoalRunning, nil)
	r := admissionResult(t, admissionObserved(t, m, env), s.ID)
	if r.Startable || !admissionHas(r, "write_conflict") {
		t.Fatal("root aliases bypassed overlap")
	}
}

func TestGoalAdmissionLegacyReservations(t *testing.T) {
	for _, mode := range []string{"unknown", "exact", "wrong-id", "settled", "held-recovery"} {
		t.Run(mode, func(t *testing.T) {
			m, s, env := admissionGoalFixture(t)
			admissionApproved(t, m, s)
			state, recovering := "running", false
			if mode == "settled" {
				state = "ended"
			}
			if mode == "held-recovery" {
				state, recovering = "ended", true
			}
			run := admissionSyntheticRun(t, m, "LEGACY", "claude", state, recovering)
			if mode != "unknown" {
				id := run.ID
				if mode == "wrong-id" {
					id = "LEGACY"
				}
				env.LegacyScopes = map[string]goalAdmissionScope{id: {Writes: []string{"repo/docs"}, Resources: []string{}}}
			}
			r := admissionResult(t, admissionObserved(t, m, env), s.ID)
			if mode == "settled" {
				if !r.Startable {
					t.Fatal("settled legacy work retained reservation", r)
				}
				return
			}
			if r.Startable {
				t.Fatal("legacy work released scope")
			}
			if mode == "unknown" || mode == "wrong-id" {
				if !admissionHas(r, "reservation_unavailable") {
					t.Fatal("unknown legacy scope inferred", r)
				}
			} else if !admissionHas(r, "write_conflict") {
				t.Fatal("legacy scope overlap lost", r)
			}
		})
	}
}

func TestGoalAdmissionStrictSeal(t *testing.T) {
	m, s, _ := admissionGoalFixture(t)
	b := admissionApproved(t, m, s)
	p, err := replayGoalJournal(m.store)
	if err != nil {
		t.Fatal(err)
	}
	old := p.records[s.ID]
	data, err := goalRecordMetadata(old)
	if err != nil {
		t.Fatal(err)
	}
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, tc := range []struct {
		name  string
		bytes []byte
		valid bool
	}{
		{"canonical", b, true},
		{"escaped-exact", []byte(strings.Replace(string(b), `"spec_ref"`, `"spec_\u0072ef"`, 1)), true},
		{"case-alias", []byte(strings.Replace(string(b), `"spec_ref"`, `"Spec_ref"`, 1)), false},
		{"duplicate-case", []byte(strings.Replace(string(b), `"version":1`, `"version":1,"Version":1`, 1)), false},
		{"duplicate-escape", []byte(strings.Replace(string(b), `"version":1`, `"version":1,"\u0076ersion":1`, 1)), false},
		{"line-separator", []byte(strings.Replace(string(b), `"observed-head-1"`, `"observed-head-1\u2028"`, 1)), false},
		{"paragraph-separator", []byte(strings.Replace(string(b), `"observed-head-1"`, `"observed-head-1\u2029"`, 1)), false},
		{"next-line", []byte(strings.Replace(string(b), `"observed-head-1"`, `"observed-head-1\u0085"`, 1)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := old
			record.sealRef = goalReplayBlob(t, m.store, "goal-seal-", tc.bytes)
			_, accepted := goalAdmissionApprovedSeal(root, record, s, data)
			if accepted != tc.valid {
				t.Fatal("admission disagreed with shared strict seal predicate")
			}
		})
	}
	data.Decisions = nil
	if _, accepted := goalAdmissionApprovedSeal(root, old, s, data); accepted {
		t.Fatal("initial bookkeeping granted approval without a reviewed decision")
	}
}

func TestGoalAdmissionDetachedObservation(t *testing.T) {
	m, s, env := admissionGoalFixture(t)
	admissionApproved(t, m, s)
	held := s
	held.ID = "OBSERVATION-HELD"
	admissionReservation(t, m, held, nativeGoalRunning, nil)
	env.SourceState = func(observed nativeGoalSpec) (string, error) {
		observed.Writes[0] = "repo/unrelated"
		return "observed-head-1", nil
	}
	r := admissionResult(t, admissionObserved(t, m, env), s.ID)
	if r.Startable || !admissionHas(r, "write_conflict") {
		t.Fatal("source observation mutated approved write scope", r)
	}
}

func TestGoalAdmissionProcessHelper(t *testing.T) {
	store := os.Getenv("NUNTIUS_TEST_GOAL_ADMISSION_STORE")
	if store == "" {
		return
	}
	m := &Manager{store: store, config: Config{Lanes: []Lane{{Name: "codex", Kind: "codex"}, {Name: "claude", Kind: "claude"}}}}
	_, err := m.setLaneControl(laneControlChange{Lane: "codex", Paused: true}, "cli")
	if os.Getenv("NUNTIUS_TEST_GOAL_ADMISSION_MODE") == "locked" {
		var fault *laneControlFault
		if !errors.As(err, &fault) || fault.Code != "locked" {
			t.Fatal("independent pause entered the admission lock")
		}
		fmt.Println("pause-locked")
	} else {
		if err != nil {
			t.Fatal(err)
		}
		fmt.Println("pause-committed")
	}
}

func admissionPauseProcess(t *testing.T, store, mode string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestGoalAdmissionProcessHelper$", "-test.timeout=25s")
	cmd.Env = append(os.Environ(), "NUNTIUS_TEST_GOAL_ADMISSION_STORE="+store, "NUNTIUS_TEST_GOAL_ADMISSION_MODE="+mode)
	b, err := cmd.Output() // waits and reaps; no child survives this helper
	if cmd.ProcessState == nil || !cmd.ProcessState.Success() || err != nil {
		t.Fatal("offline pause process did not end successfully")
	}
	want := "pause-committed"
	if mode == "locked" {
		want = "pause-locked"
	}
	if !bytes.Contains(b, []byte(want+"\n")) {
		t.Fatal("independent pause result was not observed")
	}
}

func TestGoalAdmissionPauseProcesses(t *testing.T) {
	m, s, env := admissionGoalFixture(t)
	admissionApproved(t, m, s)
	if v := admissionObserved(t, m, env); v.SelectedGoal != s.ID {
		t.Fatal("initial admission unavailable")
	}
	admissionPauseProcess(t, m.store, "commit")
	before := goalCommitTree(t, m.store)
	v, err := m.admitGoals(env)
	if err != nil || v.SelectedGoal != "" || admissionResult(t, v, s.ID).LaneFault == nil {
		t.Fatal("independent pause winner allowed admission", err)
	}
	if !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
		t.Fatal("pause winner created a reservation or launch artifact")
	}
	if _, err := m.setLaneControl(laneControlChange{Lane: "codex", Paused: false}, "cli"); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	env.SourceState = func(nativeGoalSpec) (string, error) { close(entered); <-release; return "observed-head-1", nil }
	type observed struct {
		snapshot goalAdmissionSnapshot
		err      error
	}
	done := make(chan observed, 1)
	go func() { v, err := m.admitGoals(env); done <- observed{v, err} }()
	<-entered
	// Release even on a failing independent helper, so no goroutine is left.
	var result observed
	func() {
		defer func() { close(release); result = <-done }()
		admissionPauseProcess(t, m.store, "locked")
	}()
	if result.err != nil || result.snapshot.SelectedGoal != s.ID {
		t.Fatal("admission winner failed", result.err)
	}
	admissionPauseProcess(t, m.store, "commit")
	env.SourceState = func(nativeGoalSpec) (string, error) { return "observed-head-1", nil }
	v, err = m.admitGoals(env)
	if err != nil || v.SelectedGoal != "" {
		t.Fatal("pause after admission observation did not block final recheck", err)
	}
}
