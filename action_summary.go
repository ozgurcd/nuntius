package nuntius

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
)

// actionSummary is the single presentation of an agent action. Only request
// arguments are eligible; outputs, results, edit bodies and identities are not.
func actionSummary(v map[string]any) string {
	kind, _ := v["type"].(string)
	name, args := kind, v
	if kind == "tool_use" {
		name, _ = v["name"].(string)
		args, _ = v["input"].(map[string]any)
	}
	get := func(keys ...string) string {
		for _, k := range keys {
			if s, ok := args[k].(string); ok && s != "" {
				return s
			}
		}
		return ""
	}
	summary := ""
	switch name {
	case "Bash", "bash", "exec_command", "command_execution":
		summary = get("command", "cmd")
	case "Edit", "MultiEdit", "edit", "multiedit":
		summary = "edit " + get("file_path", "filePath", "path")
	case "Write", "write":
		summary = "write " + get("file_path", "filePath", "path")
	case "patch", "apply_patch":
		var paths []string
		for _, line := range strings.Split(get("patchText", "patch"), "\n") {
			for _, prefix := range []string{"*** Update File: ", "*** Add File: ", "*** Delete File: ", "*** Move to: "} {
				if strings.HasPrefix(line, prefix) {
					paths = append(paths, strings.TrimPrefix(line, prefix))
				}
			}
		}
		summary = "patch " + strings.Join(paths, "; ")
		if len(paths) == 0 {
			summary = "patch " + get("filePath", "file_path", "path")
		}
	case "NotebookEdit":
		summary = "edit notebook " + get("notebook_path", "file_path", "path")
	case "Read", "read":
		summary = "read " + get("file_path", "filePath", "path")
	case "Grep", "Glob", "grep", "glob":
		summary = name + " " + get("pattern") + " " + get("path")
	case "WebFetch", "webfetch":
		summary = name + " " + get("url")
	case "WebSearch", "ToolSearch", "web_search":
		summary = name + " " + get("query")
	case "Task", "Agent", "task":
		summary = name + " " + get("description")
	case "text", "reasoning", "agent_message":
		summary = get("text")
	case "file_change":
		var paths []string
		changes, _ := v["changes"].([]any)
		for _, change := range changes {
			c, _ := change.(map[string]any)
			path, _ := c["path"].(string)
			verb, _ := c["kind"].(string)
			if verb != "add" && verb != "delete" && verb != "update" {
				verb = "update"
			}
			if path != "" {
				paths = append(paths, verb+" "+path)
			}
		}
		summary = strings.Join(paths, "; ")
	default:
		if name == "mcp_tool_call" {
			server, _ := v["server"].(string)
			tool, _ := v["tool"].(string)
			name = strings.Trim(server+"."+tool, ".")
			args, _ = v["arguments"].(map[string]any)
			if raw, ok := v["arguments"].(string); ok {
				args = approvedSourceArguments(raw)
			}
		} else if strings.HasPrefix(name, "mcp__") {
			parts := strings.SplitN(strings.TrimPrefix(name, "mcp__"), "__", 2)
			name = strings.Join(parts, ".")
		}
		summary = name + " " + actionArgument(args)
	}
	if strings.TrimSpace(summary) == "" {
		summary = name
	}
	if strings.TrimSpace(summary) == name {
		if argument := actionArgument(args); argument != "" {
			summary = name + " " + argument
		}
	}
	// Redact the complete value before first-line selection or truncation.
	summary = strings.SplitN(Redact(summary), "\n", 2)[0]
	summary = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, summary)
	summary = strings.Join(strings.Fields(summary), " ")
	runes := []rune(summary)
	if len(runes) > 120 {
		summary = string(runes[:119]) + "…"
	}
	return summary
}

func actionArgument(args map[string]any) string {
	for _, key := range []string{"command", "cmd", "file_path", "notebook_path", "path", "pattern", "query", "url", "symbol", "term", "struct", "code", "description", "text", "message"} {
		if s, ok := args[key].(string); ok && s != "" {
			return s
		}
	}
	keys := make([]string, 0, len(args))
	for key := range args {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		switch key {
		case "type", "name", "id", "status", "server", "tool", "output", "aggregated_output", "result", "error", "content", "new_string", "old_string", "new_source", "intention", "session_id", "thread_id":
			continue
		}
		if s, ok := args[key].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

type actionEvent struct {
	Summary, ID, Key           string
	Text                       string
	At                         time.Time
	Command, Started, Finished bool
}

// eventActions accepts both event envelopes and deliberately excludes result
// payloads. Their IDs may close a command, but their content is never displayed.
func eventActions(line []byte) []actionEvent {
	var v map[string]any
	if json.Unmarshal(line, &v) != nil {
		return nil
	}
	kind, _ := v["type"].(string)
	at, _ := time.Parse(time.RFC3339Nano, stringField(v, "timestamp"))
	if at.IsZero() {
		at, _ = time.Parse(time.RFC3339Nano, stringField(v, "time"))
	}
	if millis, ok := v["timestamp"].(float64); ok && millis > 0 && millis < 1e15 {
		at = time.UnixMilli(int64(millis)).UTC()
	}
	var out []actionEvent
	if _, hasPart := v["part"]; hasPart || stringField(v, "sessionID") != "" {
		return openCodeActions(v, kind, at, line)
	}
	add := func(item map[string]any, index int, started, finished bool) {
		t := stringField(item, "type")
		name := stringField(item, "name")
		summary := actionSummary(item)
		if summary == "" {
			return
		}
		key := fmt.Sprintf("%x:%d", sha256.Sum256(line), index)
		out = append(out, actionEvent{Summary: summary, Text: stringField(item, "text"), ID: stringField(item, "id"), Key: key, At: at, Command: t == "command_execution" || (t == "tool_use" && (name == "Bash" || name == "exec_command")), Started: started, Finished: finished})
	}
	if kind == "assistant" || kind == "user" {
		message, _ := v["message"].(map[string]any)
		content, _ := message["content"].([]any)
		for i, c := range content {
			item, _ := c.(map[string]any)
			t := stringField(item, "type")
			if kind == "assistant" && (t == "tool_use" || t == "text") {
				add(item, i, t == "tool_use", false)
			}
			if t == "tool_result" {
				out = append(out, actionEvent{ID: stringField(item, "tool_use_id"), Finished: true})
			}
		}
	}
	if kind == "item.started" || kind == "item.updated" || kind == "item.completed" {
		item, _ := v["item"].(map[string]any)
		if item != nil {
			add(item, 0, kind == "item.started", kind == "item.completed" || stringField(item, "status") == "completed" || stringField(item, "status") == "failed")
		}
	}
	return out
}

func stringField(v map[string]any, key string) string { s, _ := v[key].(string); return s }

func streamActions(b []byte) []actionEvent {
	var out []actionEvent
	projection := approveEvents(b, false)
	for _, approved := range projection.events {
		if approved.hidden {
			out = append(out, actionEvent{Summary: "[REDACTED]"})
			continue
		}
		for _, a := range eventActions(approved.wire) {
			a.Text = ""
			out = append(out, a)
		}
	}
	return out
}

type CurrentAction struct {
	Summary        string `json:"summary"`
	AgeSeconds     int64  `json:"age_seconds"`
	AgeSource      string `json:"age_source"`
	RunningSeconds *int64 `json:"running_seconds,omitempty"`
}

type actionObservation struct {
	Key       string
	At        time.Time
	Source    string
	CommandID string
	CommandAt time.Time
}

// Untimestamped streams cannot supply historical wall times. Remember when an
// action was first observed, initially estimated from the file modification
// time. Later result/output writes never reset this clock. No run data is saved.
func (m *Manager) currentAction(runID string, b []byte, modified, now time.Time) *CurrentAction {
	if m.actionClocks == nil {
		m.actionClocks = map[string]actionObservation{}
	}
	old := m.actionClocks[runID]
	window := m.eventWindow(runID, b, false)
	latest := window.latest
	if latest.Summary == "" {
		return nil
	}
	observed := actionObservation{Key: latest.Key, At: latest.At, Source: "event"}
	if latest.At.IsZero() {
		observed.At, observed.Source = modified, "observed"
		if latest.Key == old.Key {
			observed.At, observed.Source = old.At, old.Source
		}
	}
	result := &CurrentAction{Summary: latest.Summary, AgeSeconds: max(0, int64(now.Sub(observed.At).Seconds())), AgeSource: observed.Source}
	if window.commandRunning && latest.Command {
		started := window.command.At
		if started.IsZero() {
			started = observed.At
			if old.CommandID == latest.ID && !old.CommandAt.IsZero() {
				started = old.CommandAt
			}
		}
		seconds := max(0, int64(now.Sub(started).Seconds()))
		result.RunningSeconds = &seconds
		observed.CommandID, observed.CommandAt = latest.ID, started
	}
	m.actionClocks[runID] = observed
	return result
}

func actionText(a *CurrentAction) string {
	if a == nil {
		return "-"
	}
	estimate := ""
	if a.AgeSource != "event" {
		estimate = "~"
	}
	s := fmt.Sprintf("%s (%s%ds ago)", a.Summary, estimate, a.AgeSeconds)
	if a.RunningSeconds != nil {
		s += fmt.Sprintf("; running for %s%ds", estimate, *a.RunningSeconds)
	}
	return s
}
