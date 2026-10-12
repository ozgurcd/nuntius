package nuntius

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

const goalAttemptReportMaxBytes = 65536

var errGoalEndPending = errors.New("the run has ended; its native record or audit delivery still needs repair, so its resources remain held until confirmation")

// An end is an observation, never a judge decision. Its full dispatch identity
// and first report observation are immutable. Later report availability is a
// separate, bounded observation. Neither stores the report body.
type goalAttemptReport struct {
	RunID    string    `json:"run_id"`
	Digest   string    `json:"digest"`
	Bytes    int64     `json:"bytes"`
	Modified time.Time `json:"modified"`
}

type goalAttemptEnd struct {
	RunID        string             `json:"run_id"`
	Lane         string             `json:"lane"`
	PID          int                `json:"pid"`
	ProcessStart string             `json:"process_start"`
	Job          string             `json:"job"`
	Started      time.Time          `json:"started"`
	Ended        time.Time          `json:"ended"`
	Outcome      nativeGoalOutcome  `json:"outcome"`
	Held         bool               `json:"held"`
	Successor    string             `json:"successor,omitempty"`
	Report       *goalAttemptReport `json:"report,omitempty"`
}

func validGoalAttemptOutcome(outcome nativeGoalOutcome) bool {
	switch outcome {
	case nativeOutcomeCompleted, nativeOutcomeTimedOut, nativeOutcomeCancelled, nativeOutcomeInterrupted, nativeOutcomeReportDeliveryFailed, nativeOutcomeEndedNoReport, nativeOutcomeUnknownExit, nativeOutcomeFailed:
		return true
	}
	return false
}

func validGoalAttemptReport(r goalAttemptReport) bool {
	return runPattern.MatchString(r.RunID) && validGoalDigest(r.Digest) && r.Bytes >= 0 && r.Bytes <= goalAttemptReportMaxBytes && !r.Modified.IsZero()
}

func validGoalAttemptEnd(e goalAttemptEnd) bool {
	return runPattern.MatchString(e.RunID) && namePattern.MatchString(e.Lane) && e.PID > 1 && e.ProcessStart != "" && goalBriefLine(e.ProcessStart) && (e.Job == "" || goalBriefLine(e.Job)) && !e.Started.IsZero() && !e.Ended.Before(e.Started) && validGoalAttemptOutcome(e.Outcome) && (e.Successor == "" || runPattern.MatchString(e.Successor) && e.Successor != e.RunID && e.Held) && (e.Report == nil || validGoalAttemptReport(*e.Report) && e.Report.RunID == e.RunID && !e.Report.Modified.Before(e.Started))
}

func validGoalRunEndJSON(b []byte) bool {
	// Legacy records have no end extension. This conservative prefilter may
	// match text in a value, but cannot miss an accepted spelling of the ASCII
	// native_end key: literal case variants fold here, and any Unicode escape
	// takes the full decoded-key path. strictJSON already checks the envelope.
	if !bytes.Contains(bytes.ToLower(b), []byte("native_end")) && !bytes.Contains(b, []byte(`\u`)) {
		return true
	}
	if !utf8.Valid(b) || !nativeJSONScalars(b) {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.UseNumber()
	raw, err := queueJSON(decoder, 1)
	if err != nil {
		return false
	}
	object, ok := raw.(map[string]any)
	if !ok {
		return false
	}
	for key, value := range object {
		if !strings.EqualFold(key, "native_end") {
			continue
		}
		if key != "native_end" {
			return false
		}
		encoded, err := json.Marshal(value)
		var end goalAttemptEnd
		if err != nil || !goalDispatchCanonical(encoded, &end, 4096) || !validGoalAttemptEnd(end) {
			return false
		}
	}
	return true
}

func validGoalAttemptsJSON(m map[string]any) bool {
	for _, key := range []string{"ends", "reports"} {
		v, exists := m[key]
		if !exists {
			continue
		}
		b, err := json.Marshal(v)
		if err != nil {
			return false
		}
		if key == "ends" {
			var rows []goalAttemptEnd
			if !goalDispatchCanonical(b, &rows, goalRecordMaxBytes) || len(rows) == 0 || len(rows) > 8 {
				return false
			}
			seen := map[string]bool{}
			for _, row := range rows {
				if !validGoalAttemptEnd(row) || seen[row.RunID] {
					return false
				}
				seen[row.RunID] = true
			}
		} else {
			var rows []goalAttemptReport
			if !goalDispatchCanonical(b, &rows, goalRecordMaxBytes) || len(rows) == 0 || len(rows) > 8 {
				return false
			}
			seen := map[string]bool{}
			for _, row := range rows {
				if !validGoalAttemptReport(row) || seen[row.RunID] {
					return false
				}
				seen[row.RunID] = true
			}
		}
	}
	return true
}

func goalAttemptReleased(data goalRecordData, id string) bool {
	if !goalDispatchScopesValid(data) {
		return false
	}
	for _, e := range data.Ends {
		if e.RunID == id {
			return !e.Held
		}
	}
	return false
}

// This is the sole end transition predicate, used by the writer and replay.
// All retained approval, scope, seal and unrelated history remain identical.
func applyGoalAttemptEnd(data goalRecordData, e goalAttemptEnd) (goalRecordData, error) {
	if !goalDispatchScopesValid(data) || !validGoalAttemptEnd(e) || goalRecordTerminal(data.State) {
		return data, errGoalEndPending
	}
	for _, previous := range data.Ends {
		if previous.RunID == e.RunID {
			return data, errGoalEndPending
		}
	}
	index := -1
	for i, d := range data.Dispatches {
		if d.RunID != e.RunID {
			continue
		}
		if d.Request.Lane != e.Lane || d.Phase != "starting" && d.Phase != "running" || d.Phase == "running" && (d.PID != e.PID || d.ProcessStart != e.ProcessStart || d.Job != e.Job) {
			return data, errGoalEndPending
		}
		index = i
	}
	if index < 0 || len(data.Ends) >= 8 {
		return data, errGoalEndPending
	}
	if index < len(data.Dispatches)-1 {
		if !e.Held || e.Successor != data.Dispatches[index+1].RunID {
			return data, errGoalEndPending
		}
	} else {
		if data.Attempt != nil && data.Attempt.ID != e.RunID || data.State != nativeGoalRunning && data.State != nativeGoalApproved {
			return data, errGoalEndPending
		}
		data.State, data.Outcome = nativeGoalEnded, e.Outcome
		data.Attempt = &goalDecisionAttempt{ID: e.RunID, Ended: true, ActiveSuccessor: e.Held}
		if e.Report != nil {
			data.Attempt.ReportDigest = e.Report.Digest
		}
	}
	data.Ends = append(append([]goalAttemptEnd(nil), data.Ends...), e)
	return data, nil
}

func applyGoalAttemptReport(data goalRecordData, report goalAttemptReport) (goalRecordData, error) {
	if !goalDispatchScopesValid(data) || !validGoalAttemptReport(report) || data.State != nativeGoalEnded || data.Attempt == nil || !data.Attempt.Ended || data.Attempt.ID != report.RunID || data.Attempt.ReportDigest != "" {
		return data, errGoalEndPending
	}
	for _, e := range data.Ends {
		if e.RunID == report.RunID && e.Report == nil && !report.Modified.Before(e.Started) {
			data.Reports = append(append([]goalAttemptReport(nil), data.Reports...), report)
			a := *data.Attempt
			a.ReportDigest = report.Digest
			data.Attempt = &a
			return data, nil
		}
	}
	return data, errGoalEndPending
}

func goalAttemptsHistoryValid(old, next goalReplayRecord, a, b goalRecordData) bool {
	if old.spec != next.spec || old.sealRef != next.sealRef {
		return false
	}
	var expected goalRecordData
	var err error
	if len(b.Ends) == len(a.Ends)+1 && (len(a.Ends) == 0 || reflect.DeepEqual(a.Ends, b.Ends[:len(a.Ends)])) {
		expected, err = applyGoalAttemptEnd(a, b.Ends[len(a.Ends)])
	} else if len(b.Reports) == len(a.Reports)+1 && (len(a.Reports) == 0 || reflect.DeepEqual(a.Reports, b.Reports[:len(a.Reports)])) {
		expected, err = applyGoalAttemptReport(a, b.Reports[len(a.Reports)])
	} else {
		return false
	}
	return err == nil && reflect.DeepEqual(expected, b)
}

// A PID alone, a failed inspector, terminal text or an existing report never
// proves an end. Require retained full identity and definite process absence.
func (m *Manager) goalAttemptRun(d goalDispatchRecord) *Run {
	r, err := loadRun(filepath.Join(m.store, "runs", d.RunID), d.RunID)
	if err != nil || r == nil || r.Integrity == "" || r.GoalID != d.Request.GoalID || r.Lane != d.Request.Lane || r.Kind != string(d.Delivery.Engine) || r.SHA256 != d.Delivery.PromptSHA256 || r.Brief != d.Delivery.Prompt || r.ParentRunID != "" || r.PID <= 1 || r.ProcessStart == "" {
		return nil
	}
	if d.Phase == "running" && (r.PID != d.PID || r.ProcessStart != d.ProcessStart || r.LaunchdLabel != d.Job) {
		return nil
	}
	return r
}

func goalAttemptDefinitelyGone(r Run) bool {
	if r.PID <= 1 || r.ProcessStart == "" {
		return false
	}
	return errors.Is(syscall.Kill(r.PID, 0), syscall.ESRCH)
}

func (m *Manager) observeGoalAttemptReport(r Run, checkpoint func() error) *goalAttemptReport {
	fi := m.currentReport(r)
	if fi == nil || fi.Size() > goalAttemptReportMaxBytes {
		return nil
	}
	root, err := os.OpenRoot(m.config.ReportsDir)
	if err != nil {
		return nil
	}
	defer root.Close()
	f, err := root.OpenFile(r.GoalID+".txt", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil || !before.Mode().IsRegular() || !os.SameFile(fi, before) || before.Size() != fi.Size() || !before.ModTime().Equal(fi.ModTime()) {
		return nil
	}
	if checkpoint != nil && checkpoint() != nil {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(f, goalAttemptReportMaxBytes+1))
	after, statErr := f.Stat()
	path, pathErr := root.Lstat(r.GoalID + ".txt")
	if err != nil || statErr != nil || pathErr != nil || int64(len(b)) != before.Size() || len(b) > goalAttemptReportMaxBytes || !os.SameFile(before, after) || !os.SameFile(before, path) || !path.Mode().IsRegular() || before.Size() != after.Size() || before.Size() != path.Size() || !before.ModTime().Equal(after.ModTime()) || !before.ModTime().Equal(path.ModTime()) {
		return nil
	}
	return &goalAttemptReport{RunID: r.ID, Digest: goalBriefDigest(b), Bytes: int64(len(b)), Modified: before.ModTime().UTC()}
}

func goalAttemptOperation(id string) string { return "attempt-end-" + id }

func saveGoalAttemptLocked(root *os.File, old goalReplayRecord, data goalRecordData, operation string, fault goalCommitIO) error {
	b, err := encodeGoalRecord(data)
	if err != nil {
		return err
	}
	var total int64
	seal, err := readGoalBlob(root, old.sealRef, "goal-seal-", goalSealMaxBytes, &total)
	if err != nil {
		return err
	}
	spec, err := decodeNativeGoalSpec([]byte(old.spec))
	if err != nil {
		return err
	}
	req := goalCommitRequest{OperationID: operation, ExpectedRevision: old.revision, Spec: []byte(old.spec), Seal: seal, Record: b, MutationSHA256: goalBriefDigest(b)}
	specRef := goalBlobName("goal-spec-", req.Spec)
	input := goalRecordFingerprint(goalInputFingerprint(spec.ID, old.revision, specRef, old.sealRef), goalBlobName("goal-record-", b), req.MutationSHA256)
	_, _, err = commitGoalLocked(root, req, spec.ID, specRef, old.sealRef, input, fault)
	return err
}

// Call only with manager/control held. Durable run metadata is the retry
// source; in-memory queues and coalesced legacy signals are not authority.
func (m *Manager) publishGoalAttemptEndLocked(r *Run, fault goalCommitIO) error {
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		return err
	}
	defer root.Close()
	p, err := replayGoalJournalRoot(root)
	if err != nil {
		return err
	}
	old, native := p.records[r.GoalID]
	if !native {
		return nil
	}
	data, err := goalRecordMetadata(old)
	if err != nil {
		return errGoalEndPending
	}
	index := -1
	for i, d := range data.Dispatches {
		if d.RunID == r.ID {
			index = i
		}
	}
	if index < 0 || !goalAttemptDefinitelyGone(*r) {
		return errGoalEndPending
	}
	d := data.Dispatches[index]
	if m.goalAttemptRun(d) == nil || r.Ended == nil || r.State == "running" {
		return errGoalEndPending
	}
	if receipt, ok := p.operations[goalAttemptOperation(r.ID)]; ok {
		matched := false
		for _, end := range data.Ends {
			if end.RunID == r.ID && (r.NativeEnd == nil || reflect.DeepEqual(end, *r.NativeEnd)) {
				matched = true
			}
		}
		if !matched {
			return errGoalEndPending
		}
		if _, _, err := confirmGoalOperation(root, p, receipt, fault); err != nil {
			return errGoalEndPending
		}
		_, err = mirrorNativeAuditLocked(root, "native", fault)
		return err
	}
	if r.NativeEnd == nil {
		held, e := laneHasHeldMessage(root, r.Lane)
		if e != nil {
			return errGoalEndPending
		}
		pending, e := m.pendingMessage(r.ID)
		if e != nil {
			return errGoalEndPending
		}
		outcome := nativeGoalOutcome(r.StopReason)
		if !validGoalAttemptOutcome(outcome) {
			outcome = nativeOutcomeUnknownExit
		}
		end := goalAttemptEnd{RunID: r.ID, Lane: r.Lane, PID: r.PID, ProcessStart: r.ProcessStart, Job: r.LaunchdLabel, Started: r.Started, Ended: *r.Ended, Outcome: outcome, Held: held || pending != "" || r.WaitingToResume || r.ContinueRunID != "", Successor: r.ContinueRunID, Report: m.observeGoalAttemptReport(*r, nil)}
		if !validGoalAttemptEnd(end) {
			return errGoalEndPending
		}
		r.NativeEnd = &end
	}
	if !validGoalAttemptEnd(*r.NativeEnd) || r.NativeEnd.RunID != r.ID || r.NativeEnd.PID != r.PID || r.NativeEnd.ProcessStart != r.ProcessStart || r.NativeEnd.Job != r.LaunchdLabel || !r.NativeEnd.Started.Equal(r.Started) || !r.NativeEnd.Ended.Equal(*r.Ended) {
		return errGoalEndPending
	}
	// Rewriting confirms a prior metadata rename whose directory sync failed.
	if err = m.save(r); err != nil || fault.check("end_meta_saved") != nil {
		return errGoalEndPending
	}
	next, err := applyGoalAttemptEnd(data, *r.NativeEnd)
	if err != nil {
		return err
	}
	if err = saveGoalAttemptLocked(root, old, next, goalAttemptOperation(r.ID), fault); err != nil {
		return errGoalEndPending
	}
	_, err = mirrorNativeAuditLocked(root, "native", fault)
	return err
}

func (m *Manager) reconcileGoalAttemptsLocked(fault goalCommitIO) error {
	if m.readOnly {
		return errGoalEndPending
	}
	// Legacy-only stores require no journal scan. Native records without any
	// actual run have no end to observe; their launch holds remain authoritative.
	if _, err := os.Lstat(filepath.Join(m.store, goalJournalName)); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	runs, err := m.history()
	if err != nil {
		return err
	}
	if len(runs) == 0 {
		return nil
	}
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		return err
	}
	p, err := replayGoalJournalRoot(root)
	root.Close()
	if err != nil {
		return err
	}
	for _, record := range p.records {
		data, err := goalRecordMetadata(record)
		if err != nil {
			return err
		}
		for _, d := range data.Dispatches {
			if d.Phase != "starting" && d.Phase != "running" {
				continue
			}
			r := m.goalAttemptRun(d)
			if r == nil {
				continue
			} // Uncertainty remains reserved.
			if !goalAttemptDefinitelyGone(*r) {
				if sameProcess(*r) {
					_ = m.superviseGoalDispatch(*r, nil)
				}
				continue
			}
			lock, err := m.lock("supervisor-" + r.ID)
			if errors.Is(err, errBusy) {
				continue
			}
			if err != nil {
				return err
			}
			if r.Ended == nil {
				r.State = "ended"
				now := time.Now().UTC()
				r.Ended = &now
				if r.StopReason == "" {
					r.StopReason = "ended_no_report"
					if m.reportExists(*r) {
						r.StopReason = "completed"
					}
					if terminal, _ := m.terminalTurn(*r); terminal && r.StopReason == "ended_no_report" {
						r.StopReason = "report_delivery_failed"
					}
				}
			}
			err = m.publishGoalAttemptEndLocked(r, fault)
			lock.Close()
			if err != nil {
				return err
			}
			if err = m.publishGoalAttemptReportLocked(*r, fault); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *Manager) publishGoalAttemptReportLocked(r Run, fault goalCommitIO) error {
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		return err
	}
	defer root.Close()
	p, err := replayGoalJournalRoot(root)
	if err != nil {
		return err
	}
	old := p.records[r.GoalID]
	data, err := goalRecordMetadata(old)
	if err != nil {
		return err
	}
	if data.State != nativeGoalEnded || data.Attempt == nil || data.Attempt.ID != r.ID {
		return nil
	}
	if len(data.Reports) > 0 && data.Reports[len(data.Reports)-1].RunID == r.ID {
		receipt, ok := p.operations["attempt-report-"+r.ID]
		if !ok {
			return errGoalEndPending
		}
		if _, _, err = confirmGoalOperation(root, p, receipt, fault); err != nil {
			return err
		}
		_, err = mirrorNativeAuditLocked(root, "native", fault)
		return err
	}
	if data.Attempt.ReportDigest != "" {
		return nil
	}
	report := m.observeGoalAttemptReport(r, nil)
	if report == nil {
		return nil
	}
	next, err := applyGoalAttemptReport(data, *report)
	if err != nil {
		return err
	}
	if err = saveGoalAttemptLocked(root, old, next, "attempt-report-"+r.ID, fault); err != nil {
		return err
	}
	_, err = mirrorNativeAuditLocked(root, "native", fault)
	return err
}
