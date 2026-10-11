package nuntius

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

var terminalPrivateText = []*regexp.Regexp{
	regexp.MustCompile(`(?i)https?://[^\s<>"']*[?&](?:[a-z_]*token|code|state|secret|password)=[^\s<>"']*`),
	regexp.MustCompile(`(?i)\b(?:session_?id|thread_?id)\s*[:=]\s*["']?[a-z0-9_-]+["']?`),
	regexp.MustCompile(`\bses_[A-Za-z0-9_-]+`),
}

func terminalMessage(text string) string {
	for _, pattern := range terminalPrivateText {
		text = pattern.ReplaceAllString(text, "[REDACTED]")
	}
	return bounded(text, 8192)
}

// terminalTurn reads a complete OpenCode stream, never a tail window. A stop
// observes a model turn, not a process exit. Callers must check liveness first.
// Unknown, damaged, unterminated or over-budget streams stay recovery eligible.
// Memory is bounded by one event and a redacted final message; raw tool results
// and session identifiers never become returned classification data.
func (m *Manager) terminalTurn(r Run) (bool, string) {
	if r.Kind != "opencode" {
		return false, ""
	}
	f, err := privateFile(filepath.Join(m.store, "runs", r.ID, "events.jsonl"), os.O_RDONLY)
	if err != nil {
		return false, ""
	}
	defer f.Close()
	const maxStream = 64 << 20
	const maxEvent = 1 << 20
	before, err := f.Stat()
	if err != nil || before.Size() == 0 || before.Size() > maxStream {
		return false, ""
	}
	reader := bufio.NewReaderSize(io.LimitReader(f, maxStream+1), maxEvent)
	var read int64
	active, stopped := false, false
	final, session := "", ""
	private := false
	for {
		line, err := reader.ReadSlice('\n')
		if err == io.EOF && len(line) == 0 {
			break
		}
		if err != nil {
			return false, ""
		}
		read += int64(len(line))
		begin, end, uncertain := privateEventMarkers(line)
		private = private || begin || end || uncertain
		if read > maxStream {
			return false, ""
		}
		var v struct {
			Type      string `json:"type"`
			SessionID string `json:"sessionID"`
			Part      struct {
				Type   string `json:"type"`
				Text   string `json:"text"`
				Reason string `json:"reason"`
			} `json:"part"`
		}
		if json.Unmarshal(line, &v) != nil {
			return false, ""
		}
		if v.SessionID != "" {
			if !sessionPattern.MatchString(v.SessionID) || (session != "" && session != v.SessionID) {
				return false, ""
			}
			session = v.SessionID
		}
		switch v.Type {
		case "step_start":
			if active || v.Part.Type != "step-start" {
				return false, ""
			}
			active, stopped, final = true, false, ""
		case "step_finish":
			if !active || v.Part.Type != "step-finish" {
				return false, ""
			}
			active = false
			stopped = v.Part.Reason == "stop"
		case "text", "tool_use":
			// Pre-step diagnostic text is allowed, but nothing after a stop
			// can silently leave the previous turn classified as terminal.
			if stopped || (v.Type == "text" && v.Part.Type != "text") || (v.Type == "tool_use" && (v.Part.Type != "tool" || !active)) {
				return false, ""
			}
			if v.Type == "text" && active {
				if private {
					final = "[REDACTED]"
				} else {
					final = terminalMessage(v.Part.Text)
				}
			}
		default:
			return false, ""
		}
	}
	after, err := f.Stat()
	if err != nil || read != before.Size() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) || active || !stopped {
		return false, ""
	}
	return true, final
}
