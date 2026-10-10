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
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// AuditEntry is a bounded, redacted presentation of one complete audit record.
type AuditEntry struct {
	Time    time.Time `json:"time"`
	Action  string    `json:"action"`
	Summary string    `json:"summary"`
	Lane    string    `json:"lane"`
	GoalID  string    `json:"goal_id"`
	Result  string    `json:"result"`
	Reason  string    `json:"reason"`
	Digest  string    `json:"digest"`
	Raw     string    `json:"raw"`
}

func parseAudit(b []byte, since time.Time) []AuditEntry {
	out := []AuditEntry{}
	for _, line := range bytes.Split(b, []byte("\n")) {
		var v struct {
			Time   time.Time `json:"time"`
			Tool   string    `json:"tool"`
			Lane   string    `json:"lane"`
			GoalID string    `json:"goal_id"`
			SHA256 string    `json:"sha256"`
			Result string    `json:"result"`
			Reason string    `json:"reason"`
		}
		if len(line) > 16384 || json.Unmarshal(line, &v) != nil || v.Time.IsZero() || v.Time.Before(since) {
			continue
		}
		entry := AuditEntry{Time: v.Time, Action: cell(bounded(Redact(v.Tool), 128)), Lane: cell(bounded(Redact(v.Lane), 128)), GoalID: cell(bounded(Redact(v.GoalID), 128)), Result: "ok", Reason: cell(bounded(Redact(v.Reason), 512)), Raw: bounded(Redact(string(line)), 16384)}
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
