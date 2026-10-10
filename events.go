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
	IsError   bool   `json:"is_error"`
	Type      string `json:"type"`
	SessionID string `json:"session_id"`
	ThreadID  string `json:"thread_id"`
	Session   struct {
		ID string `json:"id"`
	} `json:"session"`
	Thread struct {
		ID string `json:"id"`
	} `json:"thread"`
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
		for _, s := range []string{v.SessionID, v.ThreadID, v.Session.ID, v.Thread.ID} {
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
	b, e := readWindow(filepath.Join(m.store, "runs", r.ID, "events.jsonl"), true)
	if e != nil {
		return nil, e
	}
	if r.State != "running" && !m.reportExists(r.GoalID) {
		final, _ := questionSummary(b)
		if final != "" {
			return []string{bounded(final, 8192)}, nil
		}
	}
	for _, action := range streamActions(b) {
		if action.Summary != "" {
			out = append(out, action.Summary)
		}
	}
	out = strings.Split(Redact(strings.Join(out, "\n")), "\n")
	for i := range out {
		out[i] = bounded(out[i], 4096)
	}
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
		var v event
		if json.Unmarshal(line, &v) != nil {
			continue
		}
		var parts []string
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
		if len(parts) > 0 {
			final = strings.Join(parts, "\n")
		}
	}
	return final, asked
}

func (m *Manager) reportExists(goal string) bool {
	fi, e := os.Lstat(filepath.Join(m.config.ReportsDir, goal+".txt"))
	return e == nil && fi.Mode().IsRegular()
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
