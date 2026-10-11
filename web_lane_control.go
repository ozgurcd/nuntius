package nuntius

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

func (w *Web) controlRoute(path string) bool {
	return path == "/lane-control" || path == "/held-message"
}

// One authenticated mutation budget for every dashboard write, including
// Settings and the pre-existing action endpoint. Authentication runs first.
func (w *Web) allowMutation() error {
	if !w.mutationMu.TryLock() {
		return &laneControlFault{Code: "locked"}
	}
	defer w.mutationMu.Unlock()
	if !w.settingsState.post.allow(time.Now(), 1.0/6, 3) {
		return &laneControlFault{Code: "rate_limited"}
	}
	return nil
}

func webControlReason(code string) string {
	switch code {
	case "forbidden":
		return "Sign in with the current dashboard link and use its exact address and origin."
	case "method_not_allowed":
		return "This operation requires a POST request."
	case "invalid_request":
		return "Use the version 1 JSON fields and a current revision; queries and extra fields are not accepted."
	case "revision_conflict":
		return "The saved state or reviewed content changed. Read it again before deciding whether to retry."
	case "lane_paused":
		return "This lane is paused. Resume clears only the hold; sending still requires explicit review."
	case "locked":
		return "Another operation holds the required lock. No new operation was started; read current state before retrying."
	case "rate_limited":
		return "Too many dashboard changes were requested. Wait briefly, then read current state before trying again."
	case "uncertain":
		return "The change may have taken effect. Read current state and review the message receipt; do not automatically retry."
	case "unavailable":
		return "The required lane, configuration, session or saved control state is unavailable. Read current state and check it locally."
	default:
		return "The dashboard could not complete this operation. Read current state before trying again."
	}
}

func webControlReply(out http.ResponseWriter, status int, code string, value map[string]any) {
	if value == nil {
		value = map[string]any{}
	}
	if code != "" {
		value["code"], value["reason"] = code, webControlReason(code)
	}
	b, err := json.Marshal(value)
	if err != nil || len(b) > 32768 {
		status = 503
		b = []byte(`{"code":"unavailable","reason":"The response exceeds the safe display limit. Read current state locally."}`)
	}
	out.Header().Set("Content-Type", "application/json")
	if code == "locked" {
		out.Header().Set("Retry-After", "1")
	}
	out.WriteHeader(status)
	_, _ = out.Write(b)
}

// Decode exact keys, duplicate-free JSON and native booleans/numbers. Missing,
// null, aliases and coerced values fail before any manager operation.
func webControlJSON(b []byte, held bool) (map[string]any, error) {
	if !utf8.Valid(b) {
		return nil, errors.New("invalid_request")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	value, err := queueJSON(d, 1)
	if err != nil {
		return nil, err
	}
	if _, err = d.Token(); err != io.EOF {
		return nil, errors.New("invalid_request")
	}
	v, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("invalid_request")
	}
	allowed := map[string]bool{"schema_version": true, "lane": true, "operation": true, "expected_revision": true, "after_current": true}
	if held {
		allowed = map[string]bool{"schema_version": true, "lane": true, "operation": true, "run_id": true, "content_revision": true, "content_sha256": true}
	}
	for k := range v {
		if !allowed[k] {
			return nil, errors.New("invalid_request")
		}
	}
	version, ok := laneControlUint(v["schema_version"])
	lane, laneOK := v["lane"].(string)
	operation, opOK := v["operation"].(string)
	if !ok || version != 1 || !laneOK || !namePattern.MatchString(lane) || !opOK {
		return nil, errors.New("invalid_request")
	}
	if held {
		run, runOK := v["run_id"].(string)
		digest, digestOK := v["content_sha256"].(string)
		revision, revisionOK := laneControlUint(v["content_revision"])
		if !runOK || !runPattern.MatchString(run) || !digestOK || len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" || !revisionOK || revision == 0 || (operation != "send-held" && operation != "discard-held") {
			return nil, errors.New("invalid_request")
		}
	} else {
		if _, ok := laneControlUint(v["expected_revision"]); !ok || (operation != "pause" && operation != "resume") {
			return nil, errors.New("invalid_request")
		}
		if a, exists := v["after_current"]; exists {
			if _, ok := a.(bool); !ok || operation != "pause" {
				return nil, errors.New("invalid_request")
			}
		}
	}
	return v, nil
}

func (w *Web) laneMutation(out http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		out.Header().Set("Allow", "POST")
		webControlReply(out, 405, "method_not_allowed", nil)
		return
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		webControlReply(out, 400, "invalid_request", nil)
		return
	}
	kind, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || kind != "application/json" || r.Context().Err() != nil {
		webControlReply(out, 400, "invalid_request", nil)
		return
	}
	controller := http.NewResponseController(out)
	if err = controller.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		webControlReply(out, 503, "unavailable", nil)
		return
	}
	defer controller.SetReadDeadline(time.Time{})
	b, err := io.ReadAll(http.MaxBytesReader(out, r.Body, 4096))
	if err != nil || r.Context().Err() != nil {
		webControlReply(out, 400, "invalid_request", nil)
		return
	}
	v, err := webControlJSON(b, r.URL.Path == "/held-message")
	if err != nil {
		webControlReply(out, 400, "invalid_request", nil)
		return
	}
	if r.URL.Path == "/held-message" {
		id, err := w.manager.webHeldMutation(v, w.controlIO)
		if err != nil {
			webControlFailure(out, err)
			return
		}
		webControlReply(out, 200, "", map[string]any{"schema_version": 1, "operation": v["operation"], "run_id": id, "reason": "Only the explicitly reviewed message was processed. Read its receipt before another action."})
		return
	}
	revision, _ := laneControlUint(v["expected_revision"])
	after, _ := v["after_current"].(bool)
	result, err := w.manager.setLaneControlIO(laneControlChange{Lane: v["lane"].(string), Paused: v["operation"] == "pause", AfterCurrent: after, ExpectedRevision: &revision}, "web", w.controlIO)
	if err != nil {
		webControlFailure(out, err)
		return
	}
	webControlReply(out, 200, "", map[string]any{"schema_version": 1, "revision": result.Revision, "paused": result.Paused, "changed": result.Changed, "lane_control": result.Snapshot, "reason": "The lane hold is saved. Current work can finish. Resume starts no work and never sends a held message."})
}

func webControlFailure(out http.ResponseWriter, err error) {
	code, status := "unavailable", 503
	var admission *laneAdmissionFault
	if errors.As(err, &admission) && admission.Code == "lane_paused" {
		webControlReply(out, 409, "lane_paused", map[string]any{"paused": true, "retryable": false, "blockers": admission.Blockers})
		return
	}
	var control *laneControlFault
	if errors.As(err, &control) {
		switch control.Code {
		case "invalid_request", "lane_not_found", "lane_not_configured":
			code, status = "invalid_request", 400
		case "locked":
			code = "locked"
		case "rate_limited":
			code, status = "rate_limited", 429
		case "revision_conflict":
			code, status = "revision_conflict", 409
		case "save_unconfirmed", "committed_audit_failed", "uncertain":
			code = "uncertain"
		}
	} else {
		switch err.Error() {
		case "revision_conflict":
			code, status = "revision_conflict", 409
		case "held_message_launch_uncertain", "held_message_consume_unconfirmed":
			code = "uncertain"
		}
	}
	webControlReply(out, status, code, nil)
}

// Keep H2's manager -> shared control lock ordering and never recurse into
// A05's lock-taking entry points. The shared locked helpers retain its exact
// validation, final admission, receipts, launch and audit behavior.
func (m *Manager) webHeldMutation(v map[string]any, ops *laneControlIO) (string, error) {
	if !m.mu.TryLock() {
		return "", &laneControlFault{Code: "locked"}
	}
	defer m.mu.Unlock()
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		return "", err
	}
	defer root.Close()
	guard, err := openLaneControlLock(root, unix.LOCK_EX, false)
	if err != nil {
		return "", err
	}
	defer guard.Close()
	h, _, _, err := m.heldSelectionLocked(v["lane"].(string), v["run_id"].(string))
	if err != nil {
		return "", err
	}
	revision, _ := laneControlUint(v["content_revision"])
	if h.Revision != revision || h.ContentSHA256 != v["content_sha256"] {
		return "", errors.New("revision_conflict")
	}
	if v["operation"] == "discard-held" {
		return h.RunID, webHeldError(m.discardHeldLocked(h, ops))
	}
	id, err := m.sendHeldLocked(h, ops)
	return id, webHeldError(err)
}

// After validated review, an I/O failure can follow receipt/launch persistence.
// Never describe that as a confirmed rollback. Known admission/session refusals
// retain their primary code; arbitrary filesystem errors never enter responses.
func webHeldError(err error) error {
	if err == nil {
		return nil
	}
	var admission *laneAdmissionFault
	if errors.As(err, &admission) {
		return err
	}
	switch err.Error() {
	case "held_message_session_unavailable", "held_message_discarded", "lane is busy", "manager is closing":
		return &laneControlFault{Code: "lane_control_unavailable"}
	default:
		return &laneControlFault{Code: "uncertain"}
	}
}

// Observe existing signal files without signalRoot's directory creation or
// Signals' status-maintenance call. No lock file, intent or audit is repaired.
func webObservedSignals(store string) ([]string, error) {
	path := filepath.Join(store, "signals")
	st, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	if !st.IsDir() || st.Mode().Perm()&0077 != 0 {
		return nil, errLaneControlUnavailable
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(10001)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(entries) > 10000 {
		return nil, errLaneControlUnavailable
	}
	names := []string{}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".nuntius-") && entry.Type().IsRegular() {
			continue
		}
		if !signalPattern.MatchString(entry.Name()) || !entry.Type().IsRegular() {
			return nil, errLaneControlUnavailable
		}
		st, err := root.Lstat(entry.Name())
		if err != nil {
			return nil, err
		}
		if st.Size() == 0 {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}
