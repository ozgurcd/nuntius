package nuntius

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"time"
)

// OpenCode's run stream uses sessionID and nested part/state data. Tool results
// never enter returned summaries. The CLI currently emits completed tool parts
// only; tolerate running parts without inventing unfinished-command evidence.
func openCodeActions(v map[string]any, kind string, at time.Time, line []byte) []actionEvent {
	part, _ := v["part"].(map[string]any)
	item := map[string]any{"type": kind}
	text, id, command, started, finished := "", "", false, false, false
	switch kind {
	case "tool_use":
		state, _ := part["state"].(map[string]any)
		input, _ := state["input"].(map[string]any)
		name := stringField(part, "tool")
		// Native MCP tool IDs are server_tool. Preserve the tail as one tool
		// name, since tool names may contain underscores too.
		known := map[string]bool{"bash": true, "edit": true, "write": true, "patch": true, "apply_patch": true, "multiedit": true, "read": true, "grep": true, "glob": true, "webfetch": true, "task": true, "question": true, "todowrite": true, "todoread": true, "list": true, "lsp": true, "skill": true}
		if !known[name] && !strings.HasPrefix(name, "mcp__") {
			if server, tool, ok := strings.Cut(name, "_"); ok {
				name = server + "." + tool
			}
		}
		item = map[string]any{"type": "tool_use", "name": name, "input": input}
		id = stringField(part, "callID")
		command = name == "bash"
		status := stringField(state, "status")
		started, finished = status == "running", status == "completed" || status == "error"
	case "text", "reasoning":
		text = stringField(part, "text")
		item["text"] = text
	case "step_start", "step_finish":
		item["detail"] = stringField(part, "reason")
	case "error":
		err, _ := v["error"].(map[string]any)
		data, _ := err["data"].(map[string]any)
		msg := stringField(data, "message")
		if msg == "" {
			msg = stringField(err, "message")
		}
		if msg == "" {
			msg = stringField(err, "name")
		}
		if msg == "" {
			msg = stringField(v, "error")
		}
		item["detail"] = msg
	default:
		// Identity/timestamp/output are metadata, not unknown action arguments.
		for k, value := range v {
			if k != "sessionID" && k != "type" && k != "timestamp" && k != "output" {
				item[k] = value
			}
		}
	}
	summary := actionSummary(item)
	if summary == "" {
		return nil
	}
	return []actionEvent{{Summary: summary, Text: text, ID: id, Key: fmt.Sprintf("%x:0", sha256.Sum256(line)), At: at, Command: command, Started: started, Finished: finished}}
}
