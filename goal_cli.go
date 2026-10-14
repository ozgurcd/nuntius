package nuntius

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

const goalCLIUsage = `Native goals (global --config/--store go first):
goal create ID --op OP --stdin [--json]
goal update ID --revision N --op OP --stdin [--json]
goal seal ID --revision N --op OP --stdin [--json]
goal approve|verify|cancel|park ID --revision N --op OP --reason TEXT --authority ID --evidence ID [--json]
goal verify additionally requires --attempt RUN and --report-digest SHA256, or --alternate-evidence --attempt RUN
goal approve optionally accepts --source-state VALUE from the sealed review
goal cover ID --revision N --op OP --by ID --reason TEXT [--json]
goal note ID --revision N --op OP --text TEXT [--evidence ID] [--json]
goal ended ID --run RUN [--json]
goal show ID [--json] | goal queue [--state STATE] [--search TEXT] [--json] | goal audit [--json]
dispatch ID --revision N --op OP --lane LANE [--json]
Create/update read one typed spec from stdin. Seal reads one typed brief object. No path reader, state setter, or owner grant. Cancel changes the goal only; it sends no process signal.`

type goalCLIRequest struct {
	action, id, op, lane, run, reason, authority, evidence, text, by, digest, source string
	revision                                                                         uint64
	json, stdin, alternate                                                           bool
	spec                                                                             nativeGoalSpec
	seal                                                                             goalBriefInput
	states                                                                           map[string]bool
	search                                                                           string
}

type goalCLIResult struct {
	Schema           string   `json:"schema"`
	Result           string   `json:"result"`
	Code             string   `json:"code,omitempty"`
	ExpectedRevision uint64   `json:"expected_revision"`
	FoundRevision    uint64   `json:"found_revision"`
	ExpectedState    string   `json:"expected_state,omitempty"`
	FoundState       string   `json:"found_state,omitempty"`
	Cycle            []string `json:"cycle,omitempty"`
	Data             any      `json:"data"`
}

func goalCLIJSON(args []string) bool {
	for _, arg := range args {
		if arg == "--json" {
			return true
		}
	}
	return false
}

// Parse before opening the store. Every action has a closed flag set and arity.
func parseGoalCLI(args []string, in io.Reader) (goalCLIRequest, bool) {
	r := goalCLIRequest{json: goalCLIJSON(args), states: map[string]bool{}}
	if len(args) == 0 {
		return r, false
	}
	offset := 1
	if args[0] == "goal" {
		if len(args) < 2 {
			return r, false
		}
		r.action = args[1]
		offset = 2
	} else if args[0] == "dispatch" {
		r.action = "dispatch"
	} else {
		return r, false
	}
	allowed := map[string]bool{"--json": true}
	required := []string{}
	switch r.action {
	case "queue":
		allowed["--state"] = true
		allowed["--search"] = true
	case "audit":
	case "show":
	case "ended":
		required = []string{"--run"}
	case "create":
		required = []string{"--op", "--stdin"}
	case "update", "seal":
		required = []string{"--op", "--revision", "--stdin"}
	case "cover":
		required = []string{"--op", "--revision", "--by", "--reason"}
	case "note":
		required = []string{"--op", "--revision", "--text"}
		allowed["--evidence"] = true
	case "approve", "verify", "cancel", "park":
		required = []string{"--op", "--revision", "--reason", "--authority", "--evidence"}
		if r.action == "approve" {
			allowed["--source-state"] = true
		}
		if r.action == "verify" {
			required = append(required, "--attempt")
			allowed["--attempt"] = true
			allowed["--report-digest"] = true
			allowed["--alternate-evidence"] = true
		}
	case "dispatch":
		required = []string{"--op", "--revision", "--lane"}
	default:
		return r, false
	}
	for _, key := range required {
		allowed[key] = true
	}
	if r.action != "queue" && r.action != "audit" {
		if offset >= len(args) || !goalPattern.MatchString(args[offset]) {
			return r, false
		}
		r.id = args[offset]
		offset++
	}
	seen := map[string]bool{}
	for i := offset; i < len(args); i++ {
		key, value, equals := strings.Cut(args[i], "=")
		if !allowed[key] || seen[key] && key != "--state" {
			return r, false
		}
		seen[key] = true
		if key == "--json" || key == "--stdin" || key == "--alternate-evidence" {
			if equals {
				return r, false
			}
			r.stdin = r.stdin || key == "--stdin"
			r.alternate = r.alternate || key == "--alternate-evidence"
			continue
		}
		if !equals {
			i++
			if i >= len(args) {
				return r, false
			}
			value = args[i]
		}
		if value == "" {
			return r, false
		}
		switch key {
		case "--state":
			if value != "drafted" && value != "approved" && value != "running" && value != "ended" && value != "parked" {
				return r, false
			}
			r.states[value] = true
		case "--search":
			if !queueText(value, 240, false) {
				return r, false
			}
			r.search = value
		case "--op":
			r.op = value
		case "--revision":
			n, err := strconv.ParseUint(value, 10, 64)
			if err != nil || n == 0 || n >= goalJournalMaxEvents || strconv.FormatUint(n, 10) != value {
				return r, false
			}
			r.revision = n
		case "--lane":
			r.lane = value
		case "--run", "--attempt":
			r.run = value
		case "--reason":
			r.reason = value
		case "--authority":
			r.authority = value
		case "--evidence":
			r.evidence = value
		case "--text":
			r.text = value
		case "--by":
			r.by = value
		case "--report-digest":
			r.digest = value
		case "--source-state":
			r.source = value
		}
	}
	for _, key := range required {
		if !seen[key] {
			return r, false
		}
	}
	if r.op != "" && !validGoalOperationID(r.op) || r.run != "" && !validGoalOperationID(r.run) || r.lane != "" && !namePattern.MatchString(r.lane) || r.by != "" && !goalPattern.MatchString(r.by) {
		return r, false
	}
	if r.action == "approve" || r.action == "verify" || r.action == "cancel" || r.action == "park" {
		if !validGoalDecision(r.decision()) {
			return r, false
		}
	}
	if r.action == "note" && !validGoalNote(goalRecordNote{Text: r.text, Evidence: r.evidence}) {
		return r, false
	}
	if r.action == "cover" && (!goalDecisionReason(r.reason) || r.by == r.id) {
		return r, false
	}
	if r.stdin {
		b, err := io.ReadAll(io.LimitReader(in, goalSealMaxBytes+1))
		if err != nil || len(b) > goalSealMaxBytes {
			return r, false
		}
		if r.action == "seal" {
			var payload struct {
				Body                string           `json:"body"`
				Standards           string           `json:"standards"`
				Profile             goalBriefProfile `json:"profile"`
				Authority           string           `json:"authority"`
				SourceState         string           `json:"source_state"`
				ReviewedSourceState string           `json:"reviewed_source_state"`
			}
			d := json.NewDecoder(bytes.NewReader(b))
			d.UseNumber()
			raw, e := queueJSON(d, 1)
			fields, ok := raw.(map[string]any)
			if e != nil || !ok || !nativeClosedFields(fields, []string{"body", "standards", "profile", "authority", "source_state", "reviewed_source_state"}, nil) {
				return r, false
			}
			d = json.NewDecoder(bytes.NewReader(b))
			d.DisallowUnknownFields()
			if d.Decode(&payload) != nil || d.Decode(new(any)) != io.EOF {
				return r, false
			}
			r.seal = goalBriefInput{Body: []byte(payload.Body), Standards: []byte(payload.Standards), Profile: payload.Profile, Authority: payload.Authority, SourceState: payload.SourceState, ReviewedSourceState: payload.ReviewedSourceState}
		} else {
			r.spec, err = decodeNativeGoalSpec(b)
			if err != nil || r.spec.ID != r.id || r.spec.After == nil || r.spec.Writes == nil || r.spec.Resources == nil {
				return r, false
			}
		}
	}
	return r, true
}

func (r goalCLIRequest) decision() goalDecisionRequest {
	return goalDecisionRequest{OperationID: r.op, ExpectedRevision: r.revision, GoalID: r.id, Action: r.action, Reason: r.reason, Authority: r.authority, Evidence: r.evidence, AttemptID: r.run, ReportDigest: r.digest, AlternateEvidence: r.alternate, SourceState: r.source}
}

func writeGoalCLI(out, errOut io.Writer, machine bool, v goalCLIResult) int {
	v.Schema = "nuntius.goal-cli.v1"
	b, err := json.Marshal(v)
	if err != nil || len(b) > queueMaxBytes {
		v = goalCLIResult{Schema: "nuntius.goal-cli.v1", Result: "uncertain", Code: "response_too_large"}
		b, _ = json.Marshal(v)
	}
	// Reuse R01's public text policy on all values, retaining valid JSON and
	// list/number types. Never redact a serialized object into malformed JSON.
	var safe any
	if json.Unmarshal(b, &safe) != nil {
		return 1
	}
	safe = goalCLISafeValue(safe)
	b, err = json.Marshal(safe)
	if err != nil || len(b) > queueMaxBytes {
		return 1
	}
	if json.Unmarshal(b, &v) != nil {
		return 1
	}
	code := 0
	switch v.Result {
	case "invalid_request":
		code = 2
	case "refused":
		code = 3
	case "uncertain":
		code = 4
	}
	if machine {
		_, err = fmt.Fprintln(out, string(b))
	} else {
		_, err = fmt.Fprintf(out, "%s", v.Result)
		if v.Code != "" {
			fmt.Fprintf(out, ": %s", v.Code)
		}
		if code != 0 {
			fmt.Fprintf(out, "; expected revision %d, found %d", v.ExpectedRevision, v.FoundRevision)
		}
		if v.ExpectedState != "" {
			fmt.Fprintf(out, "; expected %s, found %s", v.ExpectedState, v.FoundState)
		}
		if len(v.Cycle) > 0 {
			fmt.Fprintf(out, "; cycle %s", strings.Join(v.Cycle, " -> "))
		}
		fmt.Fprintln(out)
		if v.Data != nil {
			body, _ := json.MarshalIndent(v.Data, "", "  ")
			_, err = fmt.Fprintln(out, string(body))
		}
	}
	if err != nil {
		fmt.Fprintln(errOut, "cannot write goal result")
		return 1
	}
	return code
}

func goalCLISafeValue(value any) any {
	switch v := value.(type) {
	case string:
		return judgeSafeText(v)
	case []any:
		for i := range v {
			v[i] = goalCLISafeValue(v[i])
		}
		return v
	case map[string]any:
		for key, item := range v {
			v[key] = goalCLISafeValue(item)
		}
		return v
	default:
		return value
	}
}

func runGoalCLI(args []string, in io.Reader, config, store string, out, errOut io.Writer) int {
	r, ok := parseGoalCLI(args, in)
	if !ok {
		return writeGoalCLI(out, errOut, r.json, goalCLIResult{Result: "invalid_request", Code: "invalid_flags_or_input"})
	}
	c, err := LoadConfig(config)
	if err != nil {
		return writeGoalCLI(out, errOut, r.json, goalCLIResult{Result: "uncertain", Code: "config_unavailable"})
	}
	m, err := newReadManager(c, store)
	if err != nil {
		return writeGoalCLI(out, errOut, r.json, goalCLIResult{Result: "uncertain", Code: "store_unavailable"})
	}
	defer m.Close()
	// A command adapter never starts the background recovery watcher.
	if r.action != "show" && r.action != "queue" && r.action != "audit" && r.action != "dispatch" {
		if err = m.prepareControlStore(); err != nil {
			return writeGoalCLI(out, errOut, r.json, goalCLIResult{Result: "uncertain", Code: "store_unavailable"})
		}
		m.readOnly = false
	}
	return writeGoalCLI(out, errOut, r.json, m.goalCLI(r, nil))
}

// Dispatch contexts come from a trusted integration, never flags or stdin.
// B17 parses and delegates; an unconfigured integration refuses before effects.
type goalCLIContext func(goalDispatchRequest) (goalDispatchContext, error)

func (m *Manager) goalCLI(r goalCLIRequest, trusted goalCLIContext) goalCLIResult {
	v := goalCLIResult{Result: "ok", ExpectedRevision: r.revision}
	if r.action == "queue" {
		snapshot, err := m.projectGoals(goalAdmissionEnvironment{})
		rows := []goalQueueRow{}
		for _, row := range snapshot.Goals {
			if len(r.states) > 0 && !r.states[string(row.State)] {
				continue
			}
			if r.search != "" && !strings.Contains(strings.ToLower(row.ID+" "+row.Title), strings.ToLower(r.search)) {
				continue
			}
			rows = append(rows, row)
		}
		v.Data = map[string]any{"snapshot": snapshot, "matched": len(rows), "rows": rows}
		if err != nil || !snapshot.Complete {
			v.Result, v.Code = "uncertain", "projection_unavailable"
		}
		return v
	}
	if r.action == "audit" {
		state, err := m.nativeAuditStatus()
		v.Data = state
		if err != nil || !state.Available || state.Pending != 0 {
			v.Result, v.Code = "uncertain", "native_audit_unavailable"
		}
		return v
	}
	p, err := m.goalCLIRecords()
	if err != nil && !(r.action == "create" && errors.Is(err, errGoalStoreMissing)) {
		v.Result, v.Code = "uncertain", "native_store_unavailable"
		return v
	}
	if p == nil {
		p = &goalProjection{records: map[string]goalReplayRecord{}}
	}
	record, exists := p.records[r.id]
	v.FoundRevision = record.revision
	data, err := goalRecordMetadata(record)
	if err != nil {
		v.Result, v.Code = "uncertain", "native_store_unavailable"
		return v
	}
	v.FoundState = string(data.State)
	switch r.action {
	case "update", "seal", "cover":
		v.ExpectedState = "drafted, approved or parked"
	case "approve":
		v.ExpectedState = "drafted or parked with a reviewed seal"
	case "verify":
		v.ExpectedState = "ended with settled attempt and matching review"
	case "cancel", "park":
		v.ExpectedState = "open with no unsettled attempt"
	}
	if r.action != "create" && !exists {
		v.Result, v.Code = "refused", "goal_missing"
		return v
	}
	if r.action == "show" {
		view, e := goalRecordView(record)
		if e != nil {
			v.Result, v.Code = "uncertain", "native_store_unavailable"
			return v
		}
		dependents := []string{}
		blockers := []string{}
		for id, other := range p.records {
			s, _ := decodeNativeGoalSpec([]byte(other.spec))
			for _, dep := range s.After {
				if dep == r.id {
					dependents = append(dependents, id)
				}
			}
		}
		sort.Strings(dependents)
		for _, dep := range view.After {
			other, found := p.records[dep]
			metadata, e := goalRecordMetadata(other)
			if !found {
				blockers = append(blockers, "waits for "+dep+" to be created and verified")
			} else if e != nil {
				blockers = append(blockers, "verification of "+dep+" is unavailable")
			} else if metadata.State == nativeGoalCancelled {
				blockers = append(blockers, "blocked: "+dep+" was cancelled")
			} else if metadata.State != nativeGoalVerified {
				blockers = append(blockers, "waits for "+dep+" to be verified")
			}
		}
		attempt := ""
		if data.Attempt != nil {
			attempt = data.Attempt.ID
		}
		runs := []string{}
		for _, dispatch := range data.Dispatches {
			runs = append(runs, dispatch.RunID)
		}
		v.Data = map[string]any{"goal": view, "depends_on": view.After, "dependents": dependents, "blockers": blockers, "attempt_id": attempt, "attempt_runs": runs}
		return v
	}
	if r.action == "dispatch" {
		if trusted == nil {
			v.Result, v.Code = "refused", "trusted_dispatch_context_unavailable"
			v.ExpectedState = "trusted source, scope and claims"
			v.FoundState = "unavailable"
			return v
		}
		req := goalDispatchRequest{GoalID: r.id, ExpectedRevision: r.revision, Key: r.op, Lane: r.lane}
		ctx, e := trusted(req)
		if e != nil {
			v.Result, v.Code = "uncertain", "trusted_dispatch_context_unavailable"
			return v
		}
		result, e := m.dispatchGoal(req, ctx, goalCommitIO{})
		v.Data = result
		if e != nil {
			return m.goalCLIRefusal(v, r, e)
		}
		v.Result = "committed"
		return v
	}
	if r.action == "ended" {
		v.ExpectedState = "latest recorded run ended"
		if data.Attempt == nil || data.Attempt.ID != r.run {
			v.Result, v.Code = "refused", "attempt_mismatch"
			v.Data = map[string]string{"expected_run": r.run, "found_run": func() string {
				if data.Attempt != nil {
					return data.Attempt.ID
				}
				return ""
			}()}
			return v
		}
		if data.Attempt.Ended {
			v.Result = "no_op"
			if _, e := m.mirrorNativeAudit("cli", goalCommitIO{}); e != nil {
				v.Result, v.Code = "uncertain", "committed_audit_pending"
			}
			return v
		}
		var dispatch *goalDispatchRecord
		for i := range data.Dispatches {
			if data.Dispatches[i].RunID == r.run {
				dispatch = &data.Dispatches[i]
			}
		}
		if dispatch == nil {
			v.Result, v.Code = "refused", "attempt_mismatch"
			return v
		}
		m.mu.Lock()
		run := m.goalAttemptRun(*dispatch)
		m.mu.Unlock()
		if run == nil || run.Ended == nil || run.State == "running" || !goalAttemptDefinitelyGone(*run) {
			v.Result, v.Code = "refused", "run_not_observed_ended"
			return v
		}
		m.mu.Lock()
		guard, e := m.controlLock()
		if e == nil {
			e = m.publishGoalAttemptEndLocked(run, goalCommitIO{})
		}
		if e == nil {
			e = m.publishGoalAttemptReportLocked(*run, goalCommitIO{})
		}
		if guard != nil {
			guard.Close()
		}
		m.mu.Unlock()
		if e != nil {
			return m.goalCLIRefusal(v, r, e)
		}
		v.Result = "committed"
		if fresh, e := m.goalCLIRecords(); e == nil {
			current := fresh.records[r.id]
			v.FoundRevision = current.revision
			if metadata, e := goalRecordMetadata(current); e == nil {
				v.FoundState = string(metadata.State)
			}
		} else {
			v.Result, v.Code = "uncertain", "committed_observation_unavailable"
		}
		return v
	}
	var result goalCommitResult
	switch r.action {
	case "create":
		result, err = m.createGoal(goalRecordRequest{OperationID: r.op, Spec: r.spec})
	case "update":
		result, err = m.updateGoal(goalRecordRequest{OperationID: r.op, ExpectedRevision: r.revision, Spec: r.spec})
	case "seal":
		spec, e := m.goalCLIInputSpec(r, p, record)
		if e != nil {
			err = e
			break
		}
		result, err = m.sealGoalBrief(goalRecordRequest{OperationID: r.op, ExpectedRevision: r.revision, Spec: spec}, r.seal)
	case "cover":
		spec, e := m.goalCLIInputSpec(r, p, record)
		if e != nil {
			err = e
			break
		}
		spec.Coverage = &nativeCoverageHold{CoveredBy: r.by, Reason: r.reason}
		result, err = m.updateGoal(goalRecordRequest{OperationID: r.op, ExpectedRevision: r.revision, Spec: spec})
	case "note":
		result, err = m.noteGoal(goalNoteRequest{OperationID: r.op, ExpectedRevision: r.revision, GoalID: r.id, Note: goalRecordNote{Text: r.text, Evidence: r.evidence}})
	default:
		req := r.decision()
		if receipt, found := p.operations[r.op]; found {
			request, _ := json.Marshal(req)
			sum := sha256.Sum256(append([]byte("decision:"), request...))
			if receipt.mutation != hex.EncodeToString(sum[:]) {
				return m.goalCLIRefusal(v, r, errGoalOperation)
			}
		}
		// Repeated verification is a no-op only for the same complete evidence.
		if r.action == "verify" && data.State == nativeGoalVerified && len(data.Decisions) > 0 {
			last := data.Decisions[len(data.Decisions)-1]
			if last.Action == "verify" && (r.revision == record.revision || r.revision == last.Revision) && last.Reason == req.Reason && last.Authority == req.Authority && last.Evidence == req.Evidence && last.AttemptID == req.AttemptID && last.ReportDigest == req.ReportDigest && last.AlternateEvidence == req.AlternateEvidence {
				v.Result = "no_op"
				if _, e := m.mirrorNativeAudit("cli", goalCommitIO{}); e != nil {
					v.Result, v.Code = "uncertain", "committed_audit_pending"
				}
				return v
			}
		}
		result, err = m.decideGoal(req)
	}
	if err != nil {
		return m.goalCLIRefusal(v, r, err)
	}
	v.Result = "committed"
	v.FoundRevision = result.Revision
	v.FoundState = ""
	v.ExpectedState = ""
	v.Data = result
	if _, e := m.mirrorNativeAudit("cli", goalCommitIO{}); e != nil {
		v.Result, v.Code = "uncertain", "committed_audit_pending"
	}
	return v
}

func (m *Manager) goalCLIRecords() (*goalProjection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, _, err := readGoalRecords(m.store)
	return p, err
}

// Seal/cover synthesize a full manager request. An old retry must use its
// immutable committed spec, not a newer title/scope from the current record.
func (m *Manager) goalCLIInputSpec(r goalCLIRequest, p *goalProjection, current goalReplayRecord) (nativeGoalSpec, error) {
	receipt, found := p.operations[r.op]
	if !found {
		return decodeNativeGoalSpec([]byte(current.spec))
	}
	if receipt.result.GoalID != r.id || receipt.result.Revision != r.revision+1 {
		return nativeGoalSpec{}, errGoalOperation
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		return nativeGoalSpec{}, errGoalReplay
	}
	defer root.Close()
	lock, err := laneControlReadLock(root)
	if err != nil {
		return nativeGoalSpec{}, errGoalReplay
	}
	defer lock.Close()
	journal, err := readGoalPrivate(root, goalJournalName, goalJournalMaxBytes)
	if err != nil {
		return nativeGoalSpec{}, errGoalReplay
	}
	end := 0
	for sequence := uint64(1); sequence <= receipt.result.Sequence; sequence++ {
		if !validGoalFrameHeader(journal[end:], sequence) || len(journal)-end < goalJournalHeaderBytes {
			return nativeGoalSpec{}, errGoalReplay
		}
		size := int(binary.BigEndian.Uint32(journal[end+16 : end+20]))
		if size > len(journal)-end-goalJournalHeaderBytes {
			return nativeGoalSpec{}, errGoalReplay
		}
		end += goalJournalHeaderBytes + size
	}
	historical, err := replayGoalJournalData(root, journal[:end])
	if err != nil || historical.tornTail {
		return nativeGoalSpec{}, errGoalReplay
	}
	old := historical.records[r.id]
	if old.revision != receipt.result.Revision {
		return nativeGoalSpec{}, errGoalReplay
	}
	return decodeNativeGoalSpec([]byte(old.spec))
}

func (m *Manager) goalCLIRefusal(v goalCLIResult, r goalCLIRequest, err error) goalCLIResult {
	v.Result, v.Code = "refused", "goal_operation_refused"
	switch {
	case errors.Is(err, errGoalConflict):
		v.Code = "revision_conflict"
	case errors.Is(err, errGoalOperation):
		v.Code = "operation_id_reused"
	case errors.Is(err, errGoalDecisionBusy):
		v.Code = "attempt_unsettled"
	case errors.Is(err, errGoalApprovalUnavailable):
		v.Code = "sealed_brief_required"
	case errors.Is(err, errGoalReview):
		v.Code = "review_evidence_mismatch"
	case errors.Is(err, errGoalTransition):
		v.Code = "state_transition_refused"
	case errors.Is(err, errGoalDependencies):
		v.Code = "dependencies_refused"
	case errors.Is(err, errGoalReplay), errors.Is(err, errGoalCommit), errors.Is(err, errGoalLaunchHeld), errors.Is(err, errGoalEndPending):
		v.Result, v.Code = "uncertain", "persistence_or_observation_unavailable"
	}
	// Observe refusal details without exposing input bodies or internal errors.
	if p, e := m.goalCLIRecords(); e == nil {
		record := p.records[r.id]
		v.FoundRevision = record.revision
		data, e := goalRecordMetadata(record)
		if e == nil {
			v.FoundState = string(data.State)
		}
		if errors.Is(err, errGoalDependencies) {
			specs := map[string]nativeGoalSpec{}
			for id, record := range p.records {
				s, e := decodeNativeGoalSpec([]byte(record.spec))
				if e == nil {
					specs[id] = s
				}
			}
			if r.action == "create" || r.action == "update" {
				specs[r.id] = r.spec
			} else if r.action == "cover" {
				s := specs[r.id]
				s.Coverage = &nativeCoverageHold{CoveredBy: r.by, Reason: r.reason}
				specs[r.id] = s
			}
			v.Cycle = goalCLICycle(specs)
		}
	}
	return v
}

// The manager remains the authority for accepting graph changes. This helper
// only explains a refused graph, with a stable path across map iteration order.
func goalCLICycle(specs map[string]nativeGoalSpec) []string {
	keys := []string{}
	for id := range specs {
		keys = append(keys, id)
	}
	sort.Strings(keys)
	colors := map[string]int{}
	stack := []string{}
	var cycle []string
	var visit func(string) bool
	visit = func(id string) bool {
		if colors[id] == 1 {
			for i, key := range stack {
				if key == id {
					cycle = append(append([]string{}, stack[i:]...), id)
					return true
				}
			}
		}
		if colors[id] == 2 {
			return false
		}
		s, ok := specs[id]
		if !ok {
			return false
		}
		colors[id] = 1
		stack = append(stack, id)
		targets := append([]string{}, s.After...)
		if s.Coverage != nil {
			targets = append(targets, s.Coverage.CoveredBy)
		}
		sort.Strings(targets)
		for _, target := range targets {
			if visit(target) {
				return true
			}
		}
		stack = stack[:len(stack)-1]
		colors[id] = 2
		return false
	}
	for _, id := range keys {
		if visit(id) {
			return cycle
		}
	}
	return nil
}
