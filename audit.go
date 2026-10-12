package nuntius

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"
)

// AuditEntry is a bounded, redacted presentation of one complete audit record.
type AuditEntry struct {
	Time     time.Time `json:"time"`
	Action   string    `json:"action"`
	ReadOnly bool      `json:"read_only"`
	Summary  string    `json:"summary"`
	Lane     string    `json:"lane"`
	GoalID   string    `json:"goal_id"`
	Result   string    `json:"result"`
	Reason   string    `json:"reason"`
	Digest   string    `json:"digest"`
	Raw      string    `json:"raw"`
	EventID  string    `json:"event_id,omitempty"`
	Source   string    `json:"source,omitempty"`
	Sequence uint64    `json:"sequence,omitempty"`
	Revision uint64    `json:"goal_revision,omitempty"`
}

type auditRecord struct {
	Time   time.Time `json:"time"`
	Tool   string    `json:"tool"`
	Lane   string    `json:"lane"`
	GoalID string    `json:"goal_id"`
	SHA256 string    `json:"sha256"`
	Result string    `json:"result"`
	Reason string    `json:"reason"`
}

// Derive names from both decoding schemas so new fields receive the same guard.
var auditRecordKeys = func() []string {
	var keys []string
	for _, schema := range []reflect.Type{reflect.TypeFor[auditRecord](), reflect.TypeFor[nativeAuditRow]()} {
		for i := 0; i < schema.NumField(); i++ {
			keys = append(keys, schema.Field(i).Tag.Get("json"))
		}
	}
	return keys
}()

// Recognition and public decoding share one exact-key pass. Token unescapes
// names; EqualFold covers the case aliases accepted by encoding/json, including
// Unicode aliases. No entry or Raw text exists until this pass and, for native
// rows, the closed schema validation have succeeded.
func decodeAuditRecord(line []byte) (auditRecord, bool, error) {
	var record auditRecord
	if len(line) > 16384 || !utf8.Valid(line) {
		return record, false, errNativeAuditLog
	}
	d := json.NewDecoder(bytes.NewReader(line))
	if token, err := d.Token(); err != nil || token != json.Delim('{') {
		return record, false, errNativeAuditLog
	}
	seen := make(map[string]bool)
	isNative := false
	for d.More() {
		token, err := d.Token()
		key, ok := token.(string)
		if err != nil || !ok || seen[key] {
			return record, false, errNativeAuditLog
		}
		seen[key] = true
		for _, canonical := range auditRecordKeys {
			if key != canonical && strings.EqualFold(key, canonical) {
				return record, false, errNativeAuditLog
			}
		}
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return record, false, errNativeAuditLog
		}
		switch key {
		case "native_audit_version", "event_id", "goal_revision":
			isNative = true
		case "tool":
			var tool string
			if json.Unmarshal(value, &tool) == nil && tool == nativeAuditTool {
				isNative = true
			}
		}
	}
	if token, err := d.Token(); err != nil || token != json.Delim('}') {
		return record, false, errNativeAuditLog
	}
	if _, err := d.Token(); err != io.EOF || json.Unmarshal(line, &record) != nil {
		return record, false, errNativeAuditLog
	}
	return record, isNative, nil
}

func parseAudit(b []byte, since time.Time) []AuditEntry {
	out := []AuditEntry{}
	seenNative := make(map[string]bool)
	// Reuse the MCP registry; unknown and internal actions stay visible.
	reads := make(map[string]bool)
	for _, tool := range tools() {
		reads[tool.Name], _ = tool.Annotations["readOnlyHint"].(bool)
	}
	for _, line := range bytes.Split(b, []byte("\n")) {
		if len(line) > 16384 {
			continue
		}
		v, isNative, err := decodeAuditRecord(line)
		if err != nil || v.Time.IsZero() || v.Time.Before(since) {
			continue
		}
		var native nativeAuditRow
		if isNative && (decodeLaneControlObject(line, &native) != nil || !validNativeAuditRow(native)) {
			continue
		}
		if isNative {
			if seenNative[native.EventID] {
				continue
			}
			seenNative[native.EventID] = true
		}
		entry := AuditEntry{Time: v.Time, Action: cell(bounded(Redact(v.Tool), 128)), Lane: cell(bounded(Redact(v.Lane), 128)), GoalID: cell(bounded(Redact(v.GoalID), 128)), Result: "ok", Reason: cell(bounded(Redact(v.Reason), 512)), Raw: bounded(Redact(string(line)), 16384)}
		entry.ReadOnly = reads[entry.Action]
		if isNative {
			entry.EventID, entry.Source = native.EventID, native.Source
			entry.Sequence, entry.Revision = native.Sequence, native.Revision
		}
		switch v.Result {
		case "accepted", "pass", "started", "ended", "ok", "frozen", "unfrozen", "updated", "":
		default:
			entry.Result = "refused"
			if entry.Reason == "" {
				entry.Reason = cell(bounded(Redact(v.Result), 128))
			}
			if v.Result == "refused" && v.Reason == "" {
				entry.Reason = "reason not recorded"
			}
		}
		if digest, err := hex.DecodeString(v.SHA256); err == nil && len(digest) == 32 {
			entry.Digest = v.SHA256[:12]
		}
		entry.Summary = auditSummary(entry)
		out = append(out, entry)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	size, end := 0, 0
	for end < len(out) && end < 200 {
		encoded, _ := json.Marshal(out[end])
		if size+len(encoded) > 64000 {
			break
		}
		size += len(encoded)
		end++
	}
	return out[:end]
}

func auditSummary(e AuditEntry) string {
	goal, lane := e.GoalID, e.Lane
	if goal == "" {
		goal = "run"
	}
	if lane == "" {
		lane = "agent"
	}
	switch e.Action {
	case nativeAuditTool:
		return "Saved native goal " + goal
	case "nuntius_dispatch":
		return "Dispatched " + goal + " to " + lane
	case "nuntius_reply", "automatic_continue":
		return "Continued " + goal + " on " + lane
	case "nuntius_report":
		return "Read report " + goal
	case "nuntius_cancel":
		return "Cancelled " + goal + " on " + lane
	case "nuntius_freeze", "freeze":
		return "Froze dispatch"
	case "nuntius_unfreeze", "unfreeze":
		return "Enabled dispatch"
	case "run_end":
		return "Ended " + goal + " on " + lane
	case "nuntius_status":
		return "Read agent status"
	case "nuntius_history":
		return "Read run history"
	case "nuntius_tail":
		return "Read live tail for " + lane
	case "nuntius_owner_notes":
		return "Read owner notes"
	default:
		return strings.ReplaceAll(strings.TrimPrefix(e.Action, "nuntius_"), "_", " ")
	}
}

// Audit reads only the bounded private audit file, never the agent's settings.
func (m *Manager) Audit(since string) ([]AuditEntry, error) {
	var after time.Time
	if since != "" {
		age, err := time.ParseDuration(since)
		if len(since) > 64 || err != nil || age <= 0 {
			return nil, errors.New("since must be a positive duration, such as 1h")
		}
		after = time.Now().Add(-age)
	}
	path := filepath.Join(m.store, "audit.jsonl")
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return []AuditEntry{}, nil
	}
	b, err := readWindow(path, true)
	if os.IsNotExist(err) {
		return []AuditEntry{}, nil
	}
	if err != nil {
		return nil, err
	}
	return parseAudit(b, after), nil
}

func writeAudit(out io.Writer, rows []AuditEntry) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "TIME\tACTION\tLANE\tGOAL\tRESULT\tBRIEF")
	for _, e := range rows {
		result := e.Result
		if e.Reason != "" {
			result += ": " + e.Reason
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", e.Time.Local().Format("2006-01-02 15:04:05 MST"), e.Summary, e.Lane, e.GoalID, result, e.Digest)
	}
	return w.Flush()
}
