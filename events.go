package nuntius

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type event struct {
	IsError           bool   `json:"is_error"`
	Type              string `json:"type"`
	SessionID         string `json:"session_id"`
	OpenCodeSessionID string `json:"sessionID"`
	ThreadID          string `json:"thread_id"`
	Session           struct {
		ID string `json:"id"`
	} `json:"session"`
	Thread struct {
		ID string `json:"id"`
	} `json:"thread"`
	Part struct {
		Text string `json:"text"`
		Tool string `json:"tool"`
	} `json:"part"`
	Message struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
			Name string `json:"name"`
		} `json:"content"`
	} `json:"message"`
	Item struct {
		Type string `json:"type"`
		Text string `json:"text"`
		Tool string `json:"tool"`
	} `json:"item"`
}

func (m *Manager) session(id string) string {
	b, e := readWindow(filepath.Join(m.store, "runs", id, "events.jsonl"), false)
	if e != nil {
		return ""
	}
	for _, line := range bytes.Split(b, []byte("\n")) {
		var v event
		if json.Unmarshal(line, &v) != nil {
			continue
		}
		for _, s := range []string{v.SessionID, v.ThreadID, v.Session.ID, v.Thread.ID, v.OpenCodeSessionID} {
			if sessionPattern.MatchString(s) {
				return s
			}
		}
	}
	return ""
}
func (m *Manager) Tail(lane string, lines int) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, e := m.lane(lane); e != nil {
		return nil, e
	}
	if lines < 1 || lines > 200 {
		return nil, errors.New("lines must be 1..200")
	}
	r, e := m.latest(lane)
	if e != nil {
		return nil, e
	}
	out := []string{}
	if r == nil {
		return out, nil
	}
	if r.State != "running" && !m.reportExists(*r) {
		if terminal, final := m.terminalTurn(*r); terminal && final != "" {
			return []string{final}, nil
		}
	}
	b, e := readWindow(filepath.Join(m.store, "runs", r.ID, "events.jsonl"), true)
	if e != nil {
		return nil, e
	}
	if r.State != "running" && !m.reportExists(*r) {
		final := m.eventWindow(r.ID, b, false).final
		if final != "" {
			return []string{bounded(final, 8192)}, nil
		}
	}
	out = append(out, m.eventWindow(r.ID, b, false).tail...)
	if len(out) > lines {
		out = out[len(out)-lines:]
	}
	size := 0
	start := len(out)
	for start > 0 && size+len(out[start-1]) <= 60000 {
		start--
		size += len(out[start])
	}
	return out[start:], nil
}

// A malformed, oversized or unfamiliar event cannot prove that no work ran.
func (m *Manager) noWork(id string) bool {
	f, e := privateFile(filepath.Join(m.store, "runs", id, "events.jsonl"), os.O_RDONLY)
	if e != nil {
		return false
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 4096), 1<<20)
	seen := false
	finished := false
	for s.Scan() {
		var v event
		if json.Unmarshal(s.Bytes(), &v) != nil {
			return false
		}
		if v.IsError {
			return false
		}
		switch v.Type {
		case "system":
		case "result":
			finished = true
		case "assistant":
			seen = true
			for _, c := range v.Message.Content {
				if c.Type != "text" && c.Type != "thinking" {
					return false
				}
			}
		default:
			return false
		}
	}
	return seen && finished && s.Err() == nil
}

// Question attempts are diagnostic, never instructions to the server.
func questionSummary(b []byte) (string, bool) {
	final := ""
	asked := false
	for _, line := range bytes.Split(b, []byte("\n")) {
		text, question, hasText := questionEvent(line)
		asked = asked || question
		if hasText {
			final = text
		}
	}
	return final, asked
}

// Keep the presence bit: an empty assistant message clears an older final.
// Redact the complete message before any cached bound is applied.
func questionEvent(line []byte) (string, bool, bool) {
	var v event
	if json.Unmarshal(line, &v) != nil {
		return "", false, false
	}
	var parts []string
	asked := false
	if v.OpenCodeSessionID != "" || v.Part.Text != "" || v.Part.Tool != "" {
		if v.Type == "text" {
			parts = append(parts, v.Part.Text)
		}
		if v.Type == "tool_use" && (v.Part.Tool == "question" || strings.Contains(v.Part.Tool, "request_user_input")) {
			asked = true
		}
	}
	if v.Type == "assistant" {
		for _, c := range v.Message.Content {
			if c.Type == "text" {
				parts = append(parts, c.Text)
			}
			if c.Type == "tool_use" && (c.Name == "AskUserQuestion" || strings.Contains(c.Name, "request_user_input")) {
				asked = true
			}
		}
	}
	if v.Type == "item.completed" || v.Type == "item.started" {
		if v.Item.Type == "agent_message" {
			parts = append(parts, v.Item.Text)
		}
		if strings.Contains(v.Item.Tool, "request_user_input") || v.Item.Type == "request_user_input" {
			asked = true
		}
	}
	return Redact(strings.Join(parts, "\n")), asked, len(parts) > 0
}

func (m *Manager) reportExists(r Run) bool {
	return m.currentReport(r) != nil
}

// A goal name can be dispatched again. A pre-existing report belongs to its
// earlier run, and a later report must not retroactively complete old history.
func (m *Manager) currentReport(r Run) os.FileInfo {
	fi, e := os.Lstat(filepath.Join(m.config.ReportsDir, r.GoalID+".txt"))
	if e != nil || !fi.Mode().IsRegular() || fi.ModTime().Before(r.Started) {
		return nil
	}
	if r.Ended != nil {
		// Late report delivery is valid until a newer dispatch of this goal.
		runs, err := m.history()
		if err != nil {
			return nil
		}
		for _, other := range runs {
			if other.GoalID == r.GoalID && other.Started.After(r.Started) && !fi.ModTime().Before(other.Started) {
				return nil
			}
		}
	}
	return fi
}

func (m *Manager) Report(goal string) (string, error) {
	if !goalPattern.MatchString(goal) {
		return "", errors.New("invalid goal_id")
	}
	root, e := os.OpenRoot(m.config.ReportsDir)
	if e != nil {
		return "", errors.New("reports directory unavailable")
	}
	defer root.Close()
	name := goal + ".txt"
	st, e := root.Lstat(name)
	if e != nil || !st.Mode().IsRegular() {
		return "", errors.New("report unavailable or not a regular file")
	}
	f, e := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return "", errors.New("report unavailable")
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, 65537))
	if e != nil {
		return "", errors.New("report cannot be read")
	}
	return bounded(string(b), 65536), nil
}
