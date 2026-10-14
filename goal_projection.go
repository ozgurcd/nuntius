package nuntius

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"time"
)

// goalQueueSnapshot is a passive observation, never dispatch authority.
type goalQueueSnapshot struct {
	Version             int                    `json:"version"`
	Revision            string                 `json:"revision"`
	Sequence            uint64                 `json:"sequence"`
	ControlRevision     uint64                 `json:"control_revision"`
	ObservedAt          time.Time              `json:"observed_at"`
	Complete            bool                   `json:"complete"`
	StorePresent        bool                   `json:"store_present"`
	Counts              goalQueueCounts        `json:"counts"`
	Goals               []goalQueueRow         `json:"goals"`
	Lanes               []goalQueueLane        `json:"lanes"`
	Observations        []goalQueueObservation `json:"observations"`
	Consistency         []goalAdmissionBlocker `json:"consistency"`
	Attention           bool                   `json:"attention"`
	AutomationAvailable bool                   `json:"automation_available"`
	AutomationReason    goalAdmissionBlocker   `json:"automation_reason"`
}

type goalQueueCounts struct {
	Open       int `json:"open"`
	Drafted    int `json:"drafted"`
	Approved   int `json:"approved"`
	Running    int `json:"running"`
	Ended      int `json:"ended"`
	Parked     int `json:"parked"`
	Verified   int `json:"verified"`
	Cancelled  int `json:"cancelled"`
	Ready      int `json:"ready"`
	Startable  int `json:"startable"`
	NeedsOwner int `json:"needs_owner"`
}

type goalQueueRow struct {
	nativeGoalDTO
	Revision  uint64                 `json:"revision"`
	Ready     bool                   `json:"ready"`
	Startable bool                   `json:"startable"`
	Lane      string                 `json:"lane,omitempty"`
	Blockers  []goalAdmissionBlocker `json:"blockers"`
}

type goalQueueLane struct {
	Status     Status               `json:"status"`
	Candidates []goalQueueCandidate `json:"candidates"`
	Reason     goalAdmissionBlocker `json:"reason"`
	Attention  bool                 `json:"attention"`
}

type goalQueueCandidate struct {
	GoalID    string                 `json:"goal_id"`
	Ready     bool                   `json:"ready"`
	Startable bool                   `json:"startable"`
	Blockers  []goalAdmissionBlocker `json:"blockers"`
}

// Keys are declared repository aliases or goal IDs, never physical paths.
type goalQueueObservation struct {
	Kind       string    `json:"kind"`
	Key        string    `json:"key"`
	ObservedAt time.Time `json:"observed_at"`
	Provenance string    `json:"provenance"`
	Available  bool      `json:"available"`
}

func (m *Manager) projectGoals(env goalAdmissionEnvironment) (goalQueueSnapshot, error) {
	in, err := m.captureGoalQueue(env)
	if err != nil {
		return goalQueueSnapshot{}, err
	}
	return computeGoalQueue(in)
}

// Only detached, safe values leave capture. No status supervision, fsync,
// repair, delivery, config reload into m, or process launch occurs here.
type goalQueueInput struct {
	snapshot     goalQueueSnapshot
	rows         []goalQueueRow
	lanes        []Lane
	controls     laneControlSnapshot
	statuses     map[string]Status
	capabilities map[string]goalAdmissionCapability
	busy         map[string]bool
}

func (m *Manager) captureGoalQueue(env goalAdmissionEnvironment) (goalQueueInput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	in := goalQueueInput{snapshot: goalQueueSnapshot{Version: 1, ObservedAt: time.Now().UTC(), Complete: true, Goals: []goalQueueRow{}, Lanes: []goalQueueLane{}, Observations: []goalQueueObservation{}, Consistency: []goalAdmissionBlocker{}}, statuses: map[string]Status{}, busy: map[string]bool{}, capabilities: map[string]goalAdmissionCapability{}}
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		if goalRecordRootMissing(m.store) {
			return in, errGoalStoreMissing
		}
		return in, errGoalReplay
	}
	defer root.Close()
	guard, err := laneControlReadLock(root)
	if err != nil {
		return in, errLaneControlUnavailable
	}
	defer guard.Close()
	journal, err := readGoalPrivate(root, goalJournalName, goalJournalMaxBytes)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return in, errGoalReplay
	}
	in.snapshot.StorePresent = !errors.Is(err, os.ErrNotExist)
	p, err := replayGoalJournalData(root, journal)
	if err != nil {
		return in, err
	}
	in.snapshot.Sequence = p.sequence
	in.snapshot.AutomationReason = goalQueueReason("grant_unavailable")
	config := m.config
	if config.SourcePath != "" {
		config, err = LoadConfig(config.SourcePath)
		if err != nil {
			return in, errLaneControlUnavailable
		}
	}
	// projectLaneControls consumes only config and the stored lane-control DTO.
	observer := &Manager{config: config, store: m.store}
	in.lanes = append([]Lane(nil), config.Lanes...)
	roots := make(map[string]string, len(env.Roots))
	for alias, path := range env.Roots {
		roots[alias] = path
	}
	env.Roots = roots
	legacy := make(map[string]goalAdmissionScope, len(env.LegacyScopes))
	for id, scope := range env.LegacyScopes {
		legacy[id] = goalAdmissionScope{Writes: append([]string(nil), scope.Writes...), Resources: append([]string(nil), scope.Resources...)}
	}
	env.LegacyScopes = legacy
	for key, capability := range env.Capabilities {
		capability.WorkKinds = append([]nativeWorkKind(nil), capability.WorkKinds...)
		in.capabilities[key] = capability
	}
	runs, err := m.history()
	if err != nil {
		return in, errGoalSnapshot
	}
	for i := range runs {
		observer.observeEndedRun(&runs[i])
	}
	control, controlErr := readLaneControlsAt(root)
	in.controls = observer.projectLaneControls(control, root, runs, controlErr == nil)
	for _, view := range in.controls.Lanes {
		if !view.Configured {
			in.lanes = append(in.lanes, Lane{Name: view.Name})
		}
	}
	in.snapshot.ControlRevision = in.controls.Revision
	if controlErr != nil {
		in.snapshot.Complete = false
	}
	audit, _, _, _, auditErr := readNativeAudit(root)
	auditOK := auditErr == nil && audit.Pending == 0 && !p.tornTail
	if !auditOK {
		in.snapshot.problem("native_audit_unavailable")
	}
	if !in.snapshot.StorePresent {
		in.snapshot.problem("native_store_missing")
	}
	reserved, unknown := goalAdmissionReservations(p, runs, root, env)
	for _, record := range p.records {
		data, err := goalRecordMetadata(record)
		if err != nil {
			return in, errGoalReplay
		}
		if data.State == nativeGoalRunning && len(data.Dispatches) == 0 {
			unknown = true
			in.snapshot.problem("attempt_observation_mismatch")
		}
	}
	if unknown {
		in.snapshot.problem("reservation_unavailable")
	}
	for _, lane := range in.lanes {
		capability, exists := in.capabilities[lane.Name]
		available := exists && string(capability.Kind) == lane.Kind
		in.snapshot.Observations = append(in.snapshot.Observations, goalQueueObservation{Kind: "capability", Key: lane.Name, ObservedAt: in.snapshot.ObservedAt, Provenance: "trusted_admission_capability", Available: available})
		if !available && lane.Kind != "" {
			in.snapshot.Complete = false
		}
		in.busy[lane.Name] = goalAdmissionLaneBusy(root, lane.Name) != nil
		status := Status{Lane: lane.Name, Kind: lane.Kind, State: "idle", LaneControlVersion: 1}
		for _, view := range in.controls.Lanes {
			if view.Name == lane.Name {
				status.LaneControl = view
				break
			}
		}
		for _, run := range runs {
			if run.Lane != lane.Name {
				continue
			}
			status.RunID, status.GoalID, status.State = run.ID, run.GoalID, run.State
			status.Started, status.Exit = &run.Started, run.Exit
			status.Outcome = bounded(Redact(run.StopReason), 256)
			break
		}
		in.statuses[lane.Name] = status
	}
	ids := make([]string, 0, len(p.records))
	for id := range p.records {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	// One complete claim observation is shared by all candidates. A callback
	// returning different answers on successive calls cannot mix one snapshot.
	needed := map[string]bool{}
	for _, id := range ids {
		spec, ok := p.spec(id)
		data, err := goalRecordMetadata(p.records[id])
		if !ok || err != nil {
			return in, errGoalReplay
		}
		if goalRecordTerminal(data.State) {
			continue
		}
		_, repos, ok := goalAdmissionPaths(spec.Writes, env.Roots)
		if ok {
			for _, repo := range repos {
				needed[repo] = true
			}
		}
	}
	repos := make([]string, 0, len(needed))
	for repo := range needed {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	claimRows := []goalAdmissionClaim{}
	var claimErr error
	if len(repos) > 0 {
		if env.Claims == nil {
			claimErr = errGoalSnapshot
		} else {
			claimRows, claimErr = env.Claims(append([]string(nil), repos...))
		}
		check := goalAdmissionResult{}
		goalAdmissionClaims(&check, repos, func([]string) ([]goalAdmissionClaim, error) { return claimRows, claimErr })
		for _, b := range check.Blockers {
			if b.Code == "claims_unavailable" {
				claimErr = errGoalSnapshot
			}
		}
		if claimErr != nil {
			in.snapshot.Complete = false
		}
		for _, repo := range repos {
			stamp := time.Time{}
			for _, row := range claimRows {
				if row.Root == repo {
					stamp = row.ObservedAt
				}
			}
			key := ""
			for alias, path := range env.Roots {
				if path == repo && nativeWrite(alias+"/scope") && (key == "" || alias < key) {
					key = alias
				}
			}
			if key != "" {
				in.snapshot.Observations = append(in.snapshot.Observations,
					goalQueueObservation{Kind: "workspace", Key: key, ObservedAt: in.snapshot.ObservedAt, Provenance: "allowlisted_physical_root", Available: true},
					goalQueueObservation{Kind: "claim", Key: key, ObservedAt: stamp, Provenance: "trusted_admission_claim", Available: claimErr == nil})
			}
		}
	}
	claimObserver := func(wanted []string) ([]goalAdmissionClaim, error) {
		rows := []goalAdmissionClaim{}
		for _, want := range wanted {
			for _, row := range claimRows {
				if row.Root == want {
					rows = append(rows, row)
				}
			}
		}
		return rows, claimErr
	}
	for _, id := range ids {
		record := p.records[id]
		spec, ok := p.spec(id)
		data, err := goalRecordMetadata(record)
		if !ok || err != nil {
			return in, errGoalReplay
		}
		v, err := nativeGoalView(spec, data.State, data.Outcome)
		if err != nil {
			return in, errGoalSnapshot
		}
		if goalRecordTerminal(data.State) {
			if data.State == nativeGoalVerified {
				in.snapshot.Counts.Verified++
			} else {
				in.snapshot.Counts.Cancelled++
			}
			continue
		}
		r := goalAdmissionResult{GoalID: id, Revision: record.revision, Blockers: []goalAdmissionBlocker{}}
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
		// A stale native state cannot count an in-flight goal as Ready.
		for _, held := range reserved {
			if held.id == id {
				r.block("goal_reserved")
			}
		}
		for _, run := range runs {
			if run.GoalID != id {
				continue
			}
			if run.State == "running" || run.WaitingToResume {
				r.block("goal_reserved")
			}
		}
		r.Ready = len(r.Blockers) == 0
		if r.Ready {
			code := goalAdmissionSourceState(spec, seal, env.SourceState)
			in.snapshot.Observations = append(in.snapshot.Observations, goalQueueObservation{Kind: "source", Key: id, ObservedAt: time.Now().UTC(), Provenance: "trusted_admission_source", Available: code == "" || code == "source_changed"})
			if code != "" {
				r.block(code)
			}
			if !auditOK {
				r.block("native_audit_unavailable")
			}
			if m.closed {
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
				goalAdmissionClaims(&r, repos, claimObserver)
			}
		}
		// Inspect every retained dispatch, even on draining/paused lanes.
		for _, dispatch := range data.Dispatches {
			if dispatch.Phase == "failed" || goalAttemptReleased(data, dispatch.RunID) {
				continue
			}
			matched := false
			for _, run := range runs {
				if goalDispatchRunMatches(dispatch, run) {
					matched = true
					if dispatch.Phase == "running" && run.State != "running" {
						in.snapshot.problem("attempt_end_unreconciled")
					}
					break
				}
			}
			if !matched || dispatch.Phase == "starting" {
				if !matched {
					in.snapshot.problem("attempt_observation_mismatch")
				}
				if dispatch.Phase == "starting" {
					in.snapshot.problem("launch_unconfirmed")
				}
				status := in.statuses[dispatch.Request.Lane]
				status.LaneControl.Blockers = append(status.LaneControl.Blockers, "goal_reserved")
				in.statuses[dispatch.Request.Lane] = status
			}
		}
		in.rows = append(in.rows, goalQueueRow{nativeGoalDTO: v, Revision: record.revision, Ready: r.Ready, Blockers: r.Blockers})
	}
	return in, nil
}

func (v *goalQueueSnapshot) problem(code string) {
	for _, b := range v.Consistency {
		if b.Code == code {
			return
		}
	}
	v.Consistency = append(v.Consistency, goalQueueReason(code))
	v.Complete, v.Attention = false, true
}

func goalQueueReason(code string) goalAdmissionBlocker {
	text := map[string]string{
		"manual_ready":                 "Reviewed work can start manually; ask the owner before dispatch.",
		"grant_unavailable":            "Automation grant authority is unavailable; no automatic work can start.",
		"no_matching_kind":             "No open goal matches this lane's engine and required binding.",
		"native_store_missing":         "Native goal history has not been created.",
		"attempt_observation_mismatch": "A retained native attempt lacks a matching run observation; review bookkeeping.",
		"attempt_end_unreconciled":     "The run ended but native end accounting still needs reconciliation.",
		"launch_unconfirmed":           "A retained launch has not been confirmed; review bookkeeping before starting another attempt.",
		"observation_unavailable":      "A complete current observation is unavailable; review the missing evidence.",
	}
	if explanation := text[code]; explanation != "" {
		return goalAdmissionBlocker{Code: code, Explanation: explanation}
	}
	r := goalAdmissionResult{}
	r.block(code)
	return r.Blockers[0]
}

func goalQueueMatches(row goalQueueRow, lane Lane) bool {
	if row.LaneBinding == "required" && row.PreferredLane != lane.Name {
		return false
	}
	for _, kind := range row.Kinds {
		if string(kind) == lane.Kind {
			return true
		}
	}
	return false
}

func goalQueueLaneBlockers(in goalQueueInput, row goalQueueRow, lane Lane) []goalAdmissionBlocker {
	r := goalAdmissionResult{Blockers: []goalAdmissionBlocker{}}
	capability, exists := in.capabilities[lane.Name]
	capable := false
	for _, work := range capability.WorkKinds {
		if work == row.WorkKind {
			capable = true
		}
	}
	if !exists || string(capability.Kind) != lane.Kind || !capable {
		r.block("lane_capability")
	}
	if !in.statuses[lane.Name].LaneControl.Configured {
		r.block("lane_control_unavailable")
	}
	for _, code := range in.statuses[lane.Name].LaneControl.Blockers {
		r.block(code)
	}
	if in.busy[lane.Name] {
		r.block("lane_busy")
	}
	return r.Blockers
}

func computeGoalQueue(in goalQueueInput) (goalQueueSnapshot, error) {
	v := in.snapshot
	sort.Slice(in.rows, func(i, j int) bool {
		if in.rows[i].Order != in.rows[j].Order {
			return in.rows[i].Order < in.rows[j].Order
		}
		return in.rows[i].ID < in.rows[j].ID
	})
	sort.Slice(in.lanes, func(i, j int) bool { return in.lanes[i].Name < in.lanes[j].Name })
	for _, lane := range in.lanes {
		v.Lanes = append(v.Lanes, goalQueueLane{Status: in.statuses[lane.Name], Candidates: []goalQueueCandidate{}})
	}
	for _, row := range in.rows {
		failures := goalAdmissionResult{Blockers: []goalAdmissionBlocker{}}
		matched := false
		selectedRank := 3
		for i, lane := range in.lanes {
			if !goalQueueMatches(row, lane) {
				continue
			}
			matched = true
			blockers := append([]goalAdmissionBlocker{}, row.Blockers...)
			laneBlocks := goalQueueLaneBlockers(in, row, lane)
			blockers = append(blockers, laneBlocks...)
			startable := row.Ready && len(blockers) == 0
			v.Lanes[i].Candidates = append(v.Lanes[i].Candidates, goalQueueCandidate{GoalID: row.ID, Ready: row.Ready, Startable: startable, Blockers: blockers})
			rank := 2
			if lane.Kind == string(row.LaneKind) {
				rank = 1
			}
			if lane.Name == row.PreferredLane {
				rank = 0
			}
			if startable && rank < selectedRank {
				row.Lane, row.Startable = lane.Name, true
				selectedRank = rank
			}
			for _, b := range laneBlocks {
				failures.block(b.Code)
			}
		}
		if row.Ready && !row.Startable {
			if !matched {
				failures.block("lane_not_configured")
			}
			row.Blockers = append(row.Blockers, failures.Blockers...)
		}
		v.Goals = append(v.Goals, row)
		c := &v.Counts
		c.Open++
		switch row.State {
		case nativeGoalDrafted:
			c.Drafted++
		case nativeGoalApproved:
			c.Approved++
		case nativeGoalRunning:
			c.Running++
		case nativeGoalEnded:
			c.Ended++
		case nativeGoalParked:
			c.Parked++
		default:
			return goalQueueSnapshot{}, errGoalSnapshot
		}
		if row.Ready {
			c.Ready++
		}
		if row.Startable {
			c.Startable++
		}
		if row.OwnerHold != nil {
			c.NeedsOwner++
		}
		if row.State == nativeGoalEnded || row.OwnerHold != nil {
			v.Attention = true
		}
		for _, blocker := range row.Blockers {
			switch blocker.Code {
			case "source_unavailable", "claims_unavailable", "write_scope_unavailable", "reservation_unavailable", "native_audit_unavailable", "manager_unavailable", "lane_capability", "lane_control_unavailable", "freeze_unavailable", "held_message_unavailable":
				v.Complete = false
			}
		}
	}
	for i := range v.Lanes {
		lane := &v.Lanes[i]
		code := "no_matching_kind"
		// Controls/running have precedence. Candidate blockers never hide a
		// lower-priority manual-ready goal, and manual work never permits quiet stop.
		for _, c := range lane.Candidates {
			if c.Startable {
				code = "manual_ready"
				lane.Attention = true
				break
			}
		}
		if code != "manual_ready" && len(lane.Candidates) > 0 {
			code = "grant_unavailable"
			for _, preferred := range []string{"owner_hold", "coverage_hold", "dependencies_unverified", "write_conflict", "resource_conflict", "repository_claimed", "source_changed", "seal_invalid", "approval_required", "lane_capability", "goal_reserved"} {
				found := false
				for _, c := range lane.Candidates {
					for _, b := range c.Blockers {
						if b.Code == preferred {
							found = true
						}
					}
				}
				if found {
					code = preferred
					break
				}
			}
		}
		for _, preferred := range []string{"lane_control_unavailable", "not_configured", "freeze_unavailable", "held_message_unavailable", "running", "lane_paused", "frozen", "waiting_to_resume", "held_message"} {
			found := false
			for _, b := range lane.Status.LaneControl.Blockers {
				if b == preferred {
					found = true
				}
			}
			if found {
				code = preferred
				break
			}
		}
		if !v.Complete && code != "running" && code != "lane_paused" && code != "frozen" && code != "lane_control_unavailable" && code != "freeze_unavailable" && code != "held_message_unavailable" {
			code = "observation_unavailable"
			lane.Attention = true
		}
		lane.Reason = goalQueueReason(code)
		lane.Status.ComputedIdle = &lane.Reason
		v.Attention = v.Attention || lane.Attention
	}
	// This binds one complete observation, including input revisions and
	// timestamps; it is not a journal/control CAS revision or a grant.
	b, err := json.Marshal(v)
	if err != nil || len(b) > goalRecordSnapshotMaxBytes {
		return goalQueueSnapshot{}, errGoalSnapshot
	}
	digest := sha256.Sum256(b)
	v.Revision = hex.EncodeToString(digest[:])
	for i := range v.Lanes {
		v.Lanes[i].Status.GoalProjectionRevision = v.Revision
	}
	b, err = json.Marshal(v)
	if err != nil || len(b) > goalRecordSnapshotMaxBytes {
		return goalQueueSnapshot{}, errGoalSnapshot
	}
	return v, nil
}
