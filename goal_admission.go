package nuntius

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// These observations come from trusted adapters, never a goal spec or actor field.
// B10 does not expose an adapter or acquire an external repository claim.
type goalAdmissionCapability struct {
	Kind      nativeEngineKind
	WorkKinds []nativeWorkKind
}

type goalAdmissionClaim struct {
	Root       string
	Holder     string
	ObservedAt time.Time
}

type goalAdmissionScope struct {
	Writes    []string
	Resources []string
}

type goalAdmissionEnvironment struct {
	Roots        map[string]string
	Capabilities map[string]goalAdmissionCapability
	SourceState  func(nativeGoalSpec) (string, error)
	Claims       func([]string) ([]goalAdmissionClaim, error)
	LegacyScopes map[string]goalAdmissionScope // exact full run ID, including held recovery
}

type goalAdmissionBlocker struct {
	Code        string
	Explanation string
}

type goalAdmissionResult struct {
	GoalID    string
	Revision  uint64
	Ready     bool
	Startable bool
	Lane      string
	Blockers  []goalAdmissionBlocker
	LaneFault *laneAdmissionFault
}

type goalAdmissionSnapshot struct {
	Sequence     uint64
	Goals        []goalAdmissionResult
	SelectedGoal string
	SelectedLane string
}

// admitGoals is an internal, launch-free observation. B11 must recheck its
// locked variant inside the reservation/launch critical section.
func (m *Manager) admitGoals(env goalAdmissionEnvironment) (goalAdmissionSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	lock, err := m.admissionControlLock()
	if err != nil {
		return goalAdmissionSnapshot{}, err
	}
	defer lock.Close()
	return m.admitGoalsLocked(env)
}

// Caller holds manager -> common control, including through any later B11
// reservation. No recursive mutex/flock, reconciliation, lane acquisition or
// new journal/prompt/run/child artifact is permitted here.
func (m *Manager) admitGoalsLocked(env goalAdmissionEnvironment) (goalAdmissionSnapshot, error) {
	if err := m.reload(); err != nil {
		return goalAdmissionSnapshot{}, errors.New("goal admission is unavailable because lane configuration could not be read")
	}
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		return goalAdmissionSnapshot{}, errGoalReplay
	}
	defer root.Close()
	p, err := replayGoalJournalRoot(root)
	if err != nil {
		return goalAdmissionSnapshot{}, err
	}
	runs, err := m.history()
	if err != nil {
		return goalAdmissionSnapshot{}, errors.New("goal admission is unavailable because run history could not be read")
	}
	controls := m.laneControlSnapshotLocked(runs, true)
	auditOK := requireNativeAuditLocked(root, goalCommitIO{}) == nil && !p.tornTail
	specs := []nativeGoalSpec{}
	for id := range p.records {
		s, ok := p.spec(id)
		if !ok {
			return goalAdmissionSnapshot{}, errGoalReplay
		}
		specs = append(specs, s)
	}
	sort.Slice(specs, func(i, j int) bool {
		if specs[i].Order != specs[j].Order {
			return specs[i].Order < specs[j].Order
		}
		return specs[i].ID < specs[j].ID
	})
	reserved, unknown := goalAdmissionReservations(p, runs, root, env)
	out := goalAdmissionSnapshot{Sequence: p.sequence, Goals: []goalAdmissionResult{}}
	for _, spec := range specs {
		record := p.records[spec.ID]
		data, err := goalRecordMetadata(record)
		if err != nil {
			return goalAdmissionSnapshot{}, errGoalReplay
		}
		r := goalAdmissionResult{GoalID: spec.ID, Revision: record.revision, Blockers: []goalAdmissionBlocker{}}
		if data.State != nativeGoalApproved || data.ApprovalRevision == 0 {
			r.block("approval_required")
		}
		if spec.OwnerHold != nil {
			r.block("owner_hold")
		}
		if spec.Coverage != nil {
			r.block("coverage_hold")
		}
		if !goalDependenciesVerified(p, spec) {
			r.block("dependencies_unverified")
		}
		seal, sealOK := goalAdmissionApprovedSeal(root, record, spec, data)
		if !sealOK {
			r.block("seal_invalid")
		}
		r.Ready = len(r.Blockers) == 0
		if r.Ready {
			if code := goalAdmissionSourceState(spec, seal, env.SourceState); code != "" {
				r.block(code)
			}
			if !auditOK {
				r.block("native_audit_unavailable")
			}
			if m.closed || m.readOnly {
				r.block("manager_unavailable")
			}
			writes, repos, ok := goalAdmissionPaths(spec.Writes, env.Roots)
			if !ok {
				r.block("write_scope_unavailable")
			}
			if unknown {
				r.block("reservation_unavailable")
			}
			for _, held := range reserved {
				if held.id == spec.ID {
					r.block("goal_reserved")
				}
				if goalAdmissionOverlaps(writes, held.writes) {
					r.block("write_conflict")
				}
				for _, resource := range spec.Resources {
					for _, other := range held.resources {
						if resource == other {
							r.block("resource_conflict")
						}
					}
				}
			}
			if ok && len(repos) > 0 {
				goalAdmissionClaims(&r, repos, env.Claims)
			}
			m.goalAdmissionLane(&r, spec, controls, env.Capabilities, root, runs)
			r.Startable = len(r.Blockers) == 0 && r.Lane != ""
		}
		if r.Startable && out.SelectedGoal == "" {
			out.SelectedGoal, out.SelectedLane = r.GoalID, r.Lane
		}
		out.Goals = append(out.Goals, r)
	}
	return out, nil
}

func goalAdmissionSourceState(spec nativeGoalSpec, seal goalBriefSeal, observe func(nativeGoalSpec) (string, error)) string {
	if observe == nil {
		return "source_unavailable"
	}
	// Observers never receive the caller's arrays or the approved scope.
	b, err := json.Marshal(spec)
	if err != nil {
		return "source_unavailable"
	}
	detached, err := decodeNativeGoalSpec(b)
	if err != nil {
		return "source_unavailable"
	}
	state, err := observe(detached)
	if err != nil || !goalBriefLine(state) {
		return "source_unavailable"
	}
	if state != seal.SourceState {
		return "source_changed"
	}
	return ""
}

// A retained approval points to its original sealed revision, not a later
// title/order/note revision. Reuse the decision writer/replay seal predicate,
// then compare execution scope. Never treat initial bookkeeping as approval.
func goalAdmissionApprovedSeal(root *os.File, record goalReplayRecord, spec nativeGoalSpec, data goalRecordData) (goalBriefSeal, bool) {
	var seal goalBriefSeal
	if data.State != nativeGoalApproved || data.ApprovalRevision == 0 || record.sealRef == "" || len(data.Decisions) == 0 {
		return seal, false
	}
	d := data.Decisions[len(data.Decisions)-1]
	if d.Action != "approve" || d.Revision != data.ApprovalRevision {
		return seal, false
	}
	var total int64
	b, err := readGoalBlob(root, record.sealRef, "goal-seal-", goalSealMaxBytes, &total)
	// This tentative lookup confers no authority; validateGoalBrief performs
	// the closed exact-key decoder before any readiness can be returned.
	if err != nil || json.Unmarshal(b, &seal) != nil {
		return goalBriefSeal{}, false
	}
	prior, err := readGoalBlob(root, seal.SpecRef, "goal-spec-", nativeGoalMaxBytes, &total)
	if err != nil {
		return goalBriefSeal{}, false
	}
	approved, err := decodeNativeGoalSpec(prior)
	if err != nil || goalScopeChanged(approved, spec) {
		return goalBriefSeal{}, false
	}
	checked := validateGoalBrief(goalReplayRecord{revision: data.ApprovalRevision, spec: string(prior), sealRef: record.sealRef}, d.Authority, b)
	if checked == nil || checked.SourceState != d.SourceState {
		return goalBriefSeal{}, false
	}
	return seal, true
}

type goalAdmissionReservation struct {
	id                string
	writes, resources []string
}

func goalAdmissionReservations(p *goalProjection, runs []Run, root *os.File, env goalAdmissionEnvironment) ([]goalAdmissionReservation, bool) {
	out := []goalAdmissionReservation{}
	unknown := false
	add := func(id string, scope goalAdmissionScope) {
		writes, _, ok := goalAdmissionPaths(scope.Writes, env.Roots)
		if !ok || !nativeStringSet(scope.Resources, nativeGoalMaxList, nativeResourceName.MatchString) {
			unknown = true
			return
		}
		out = append(out, goalAdmissionReservation{id: id, writes: writes, resources: scope.Resources})
	}
	for id, record := range p.records {
		data, err := goalRecordMetadata(record)
		if err != nil {
			unknown = true
			continue
		}
		if len(data.Dispatches) == 0 && (data.State == nativeGoalRunning || data.Attempt != nil && (!data.Attempt.Ended || data.Attempt.ActiveSuccessor || data.Attempt.LaunchUnconfirmed)) {
			// An unsettled attempt outside running may retain an older scope.
			// Until lifecycle accounting binds that scope, do not infer release.
			if data.State != nativeGoalRunning {
				unknown = true
			}
			s, ok := p.spec(id)
			if !ok {
				unknown = true
				continue
			}
			add(id, goalAdmissionScope{Writes: s.Writes, Resources: s.Resources})
		}
		for _, d := range data.Dispatches {
			if d.Phase == "failed" {
				continue
			}
			s, err := decodeNativeGoalSpec([]byte(d.Spec))
			if err != nil {
				unknown = true
				continue
			}
			out = append(out, goalAdmissionReservation{id: id, writes: d.Writes, resources: s.Resources})
		}
	}
	for _, run := range runs {
		// Keep unknown/surviving process observations reserved. No recovery,
		// guessed PID absence, child signal or counter change in this component.
		held, err := laneHasHeldMessage(root, run.Lane)
		if err != nil {
			unknown = true
		}
		if run.State != "running" && !run.WaitingToResume && !held {
			continue
		}
		linked := false
		if record, ok := p.records[run.GoalID]; ok {
			data, err := goalRecordMetadata(record)
			if err == nil {
				for _, d := range data.Dispatches {
					if d.Phase != "failed" && d.RunID == run.ID && d.Request.Lane == run.Lane && string(d.Delivery.Engine) == run.Kind && d.Delivery.PromptSHA256 == run.SHA256 {
						linked = true
					}
				}
			}
		}
		if linked {
			continue
		}
		// A goal ID alone does not bind a legacy run to the current native
		// scope. Require an explicit observation keyed by its full run ID.
		if scope, exists := env.LegacyScopes[run.ID]; exists {
			add(run.GoalID, scope)
		} else {
			unknown = true
		}
	}
	return out, unknown
}

func goalAdmissionPaths(writes []string, roots map[string]string) ([]string, []string, bool) {
	if !nativeStringSet(writes, nativeGoalMaxList, nativeWrite) {
		return nil, nil, false
	}
	out, repos := []string{}, []string{}
	seen := map[string]bool{}
	for _, write := range writes {
		name, rest, _ := strings.Cut(write, "/")
		base := roots[name]
		if !filepath.IsAbs(base) || filepath.Clean(base) != base {
			return nil, nil, false
		}
		base, err := filepath.EvalSymlinks(base)
		if err != nil {
			return nil, nil, false
		}
		info, err := os.Stat(base)
		if err != nil || !info.IsDir() {
			return nil, nil, false
		}
		resolved, err := goalAdmissionCanonical(filepath.Join(base, filepath.FromSlash(rest)))
		if err != nil || !goalAdmissionContains(base, resolved) {
			return nil, nil, false
		}
		out = append(out, resolved)
		if !seen[base] {
			seen[base] = true
			repos = append(repos, base)
		}
	}
	sort.Strings(repos)
	return out, repos, true
}

func goalAdmissionCanonical(path string) (string, error) {
	// Resolve an existing ancestor and preserve absent descendants. Broken
	// symlinks and traversal outside the explicitly known repository refuse.
	missing := []string{}
	for {
		_, err := os.Lstat(path)
		if err == nil {
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				return "", err
			}
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved, nil
		}
		if !os.IsNotExist(err) || filepath.Dir(path) == path {
			return "", err
		}
		missing = append(missing, filepath.Base(path))
		path = filepath.Dir(path)
	}
}

func goalAdmissionContains(parent, child string) bool {
	return parent == child || strings.HasPrefix(child, parent+string(filepath.Separator))
}
func goalAdmissionOverlaps(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if goalAdmissionContains(x, y) || goalAdmissionContains(y, x) {
				return true
			}
		}
	}
	return false
}

func goalAdmissionClaims(r *goalAdmissionResult, repos []string, observe func([]string) ([]goalAdmissionClaim, error)) {
	if observe == nil {
		r.block("claims_unavailable")
		return
	}
	rows, err := observe(append([]string(nil), repos...))
	if err != nil || len(rows) != len(repos) {
		r.block("claims_unavailable")
		return
	}
	wanted, seen := map[string]bool{}, map[string]bool{}
	for _, root := range repos {
		wanted[root] = true
	}
	now := time.Now()
	for _, row := range rows {
		if !wanted[row.Root] || seen[row.Root] || row.ObservedAt.IsZero() || row.ObservedAt.After(now) || now.Sub(row.ObservedAt) > 30*time.Second || row.Holder != "" && !goalBriefLine(row.Holder) {
			r.block("claims_unavailable")
			continue
		}
		seen[row.Root] = true
		if row.Holder != "" {
			r.block("repository_claimed")
		}
	}
	if len(seen) != len(wanted) {
		r.block("claims_unavailable")
	}
}

func (m *Manager) goalAdmissionLane(r *goalAdmissionResult, spec nativeGoalSpec, controls laneControlSnapshot, capabilities map[string]goalAdmissionCapability, root *os.File, runs []Run) {
	lanes := append([]Lane(nil), m.config.Lanes...)
	sort.Slice(lanes, func(i, j int) bool {
		if (lanes[i].Name == spec.PreferredLane) != (lanes[j].Name == spec.PreferredLane) {
			return lanes[i].Name == spec.PreferredLane
		}
		if (lanes[i].Kind == string(spec.LaneKind)) != (lanes[j].Kind == string(spec.LaneKind)) {
			return lanes[i].Kind == string(spec.LaneKind)
		}
		return lanes[i].Name < lanes[j].Name
	})
	failures := goalAdmissionResult{Blockers: []goalAdmissionBlocker{}}
	found := false
	for _, lane := range lanes {
		if spec.LaneBinding == "required" && lane.Name != spec.PreferredLane {
			continue
		}
		found = true
		candidate := goalAdmissionResult{Blockers: []goalAdmissionBlocker{}}
		kindOK := false
		for _, kind := range spec.Kinds {
			if string(kind) == lane.Kind {
				kindOK = true
			}
		}
		if !kindOK {
			candidate.block("lane_kind")
		}
		capability, exists := capabilities[lane.Name]
		capable := false
		for _, work := range capability.WorkKinds {
			if work == spec.WorkKind {
				capable = true
			}
		}
		if !exists || string(capability.Kind) != lane.Kind || !capable {
			candidate.block("lane_capability")
		}
		// Always preserve the final A guard and its primary paused refusal.
		if err := m.admitLaneLocked(lane.Name); err != nil {
			var fault *laneAdmissionFault
			if errors.As(err, &fault) {
				candidate.LaneFault = fault
				for _, code := range fault.Blockers {
					candidate.block(code)
				}
			} else if err.Error() == "pending_message_requires_review" {
				candidate.block("held_message")
			} else {
				candidate.block("lane_control_unavailable")
			}
		}
		for _, view := range controls.Lanes {
			if view.Name == lane.Name {
				for _, code := range view.Blockers {
					candidate.block(code)
				}
			}
		}
		for _, run := range runs {
			if run.Lane != lane.Name {
				continue
			}
			if run.State == "running" {
				candidate.block("running")
			}
			if run.WaitingToResume {
				candidate.block("waiting_to_resume")
			}
		}
		if err := goalAdmissionLaneBusy(root, lane.Name); err != nil {
			candidate.block("lane_busy")
		}
		if len(candidate.Blockers) == 0 {
			r.Lane = lane.Name
			return
		}
		for _, blocker := range candidate.Blockers {
			failures.block(blocker.Code)
		}
		if failures.LaneFault == nil {
			failures.LaneFault = candidate.LaneFault
		}
	}
	if !found {
		failures.block("lane_not_configured")
	}
	for _, blocker := range failures.Blockers {
		r.block(blocker.Code)
	}
	r.LaneFault = failures.LaneFault
}

func goalAdmissionLaneBusy(root *os.File, lane string) error {
	fd, err := unix.Openat(int(root.Fd()), "locks", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	lfd, err := unix.Openat(fd, lane+".lock", unix.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(lfd)
	if err := checkLaneControlFile(lfd); err != nil {
		return err
	}
	return unix.Flock(lfd, unix.LOCK_EX|unix.LOCK_NB)
}

func (r *goalAdmissionResult) block(code string) {
	for _, b := range r.Blockers {
		if b.Code == code {
			return
		}
	}
	reasons := map[string]string{
		"approval_required": "Review and approve the exact brief before starting this goal.",
		"owner_hold":        "The goal still needs an owner decision.", "coverage_hold": "This goal is held for work covered by another goal.",
		"dependencies_unverified": "A prerequisite is missing or has not been verified.", "seal_invalid": "The approved brief seal is missing, invalid or no longer matches the execution scope.",
		"source_unavailable": "The current source observation could not be checked safely.", "source_changed": "The source changed after review; review and seal the updated brief.",
		"native_audit_unavailable": "Native history or its audit delivery is incomplete or unavailable.", "manager_unavailable": "The manager cannot admit new work in its current mode.",
		"write_scope_unavailable": "A declared write path cannot be resolved inside a known repository.", "reservation_unavailable": "Existing work has unknown scope; review it before admitting another goal.",
		"goal_reserved": "This goal still has reserved or unconfirmed work.", "write_conflict": "Existing work reserves an overlapping write path.", "resource_conflict": "Existing work reserves an exclusive resource needed by this goal.",
		"claims_unavailable": "A fresh complete repository claim observation is unavailable.", "repository_claimed": "Another writer holds a required repository claim.",
		"lane_not_configured": "The required lane is not configured; it cannot fall back to another lane.", "lane_kind": "The lane's current engine is not allowed by this goal.", "lane_capability": "The lane has no current capability for this kind of work.",
		"lane_paused": "The owner paused this lane; resume removes the hold and starts no work.", "frozen": "The owner froze new work.", "freeze_unavailable": "The owner freeze state cannot be read safely.",
		"held_message": "A retained owner message needs explicit review before this lane can start another goal.", "waiting_to_resume": "The lane retains work waiting for recovery.",
		"running": "The lane still has current work.", "lane_busy": "The lane is busy or its lifetime lock cannot be checked safely.",
		"lane_control_unavailable": "Lane controls cannot be checked safely; new work is held.", "held_message_unavailable": "Retained messages cannot be checked safely.", "not_configured": "The retained lane is no longer configured.",
	}
	explanation := reasons[code]
	if explanation == "" {
		explanation = "A safety condition could not be checked; new work is held for review."
	}
	r.Blockers = append(r.Blockers, goalAdmissionBlocker{Code: code, Explanation: explanation})
}
