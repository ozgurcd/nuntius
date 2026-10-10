package nuntius

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"
)

var sizePattern = regexp.MustCompile(`\bsize\s+([SML])\b`)

func goalSize(b []byte) string {
	line, _, _ := bytes.Cut(b, []byte("\n"))
	match := sizePattern.FindSubmatch(line)
	if len(match) == 2 {
		return string(match[1])
	}
	return ""
}

func shortDuration(d time.Duration) string {
	return (max(0, d) / time.Second * time.Second).String()
}

func (m *Manager) runSize(id string) string {
	f, err := privateFile(filepath.Join(m.store, "runs", id, "goal.txt"), os.O_RDONLY)
	if err != nil {
		return ""
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 12001))
	if err != nil {
		return ""
	}
	return goalSize(b)
}

func (m *Manager) agentKind(name string) string {
	for _, l := range m.config.Lanes {
		if l.Name == name {
			return l.Kind
		}
	}
	for _, kind := range []string{"claude", "codex"} {
		if strings.HasPrefix(name, kind+"-") {
			return kind
		}
	}
	return ""
}

func (m *Manager) estimateETA(r Run, kind, size string, elapsed time.Duration) string {
	runs, err := m.history()
	if err != nil {
		return "-"
	}
	return m.etaFromHistory(runs, r, kind, size, elapsed)
}

func (m *Manager) etaFromHistory(runs []Run, r Run, kind, size string, elapsed time.Duration) string {
	if kind == "" || size == "" {
		return "-"
	}
	durations := []time.Duration{}
	for _, old := range runs {
		if old.ID == r.ID || old.State == "running" || old.Ended == nil || old.Ended.After(r.Started) || !old.Ended.After(old.Started) || m.agentKind(old.Lane) != kind || m.runSize(old.ID) != size {
			continue
		}
		durations = append(durations, old.Ended.Sub(old.Started))
	}
	if len(durations) < 3 {
		return "-"
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	median := durations[len(durations)/2]
	if len(durations)%2 == 0 {
		median = durations[len(durations)/2-1] + (median-durations[len(durations)/2-1])/2
	}
	minutes := func(d time.Duration) int64 { return max(1, int64((d+time.Minute-1)/time.Minute)) }
	if elapsed > median {
		return fmt.Sprintf("over usual (+%dm)", minutes(elapsed-median))
	}
	return fmt.Sprintf("~%dm left (n=%d)", minutes(median-elapsed), len(durations))
}

// Only fixed categories reach the owner. Tool inputs are inspected for kind,
// never printed; unfamiliar commands and tools use a generic label.
func activity(b []byte) string {
	last := "-"
	commandKind := func(command string) string {
		fields := strings.Fields(command)
		for i, field := range fields {
			if field == "git" && i+1 < len(fields) && fields[i+1] == "push" {
				return "git push"
			}
			if (field == "go" && i+1 < len(fields) && fields[i+1] == "test") || (field == "make" && i+1 < len(fields) && (strings.Contains(fields[i+1], "test") || strings.Contains(fields[i+1], "verify"))) {
				return "running tests"
			}
		}
		return "running command"
	}
	for _, line := range bytes.Split(b, []byte("\n")) {
		var v struct {
			Type string `json:"type"`
			Part struct {
				Tool  string `json:"tool"`
				State struct {
					Input struct {
						Command string `json:"command"`
					} `json:"input"`
				} `json:"state"`
			} `json:"part"`
			Message struct {
				Content []struct {
					Type, Name string
					Input      struct {
						Command string `json:"command"`
					} `json:"input"`
				} `json:"content"`
			} `json:"message"`
			Item struct{ Type, Command string } `json:"item"`
		}
		if json.Unmarshal(line, &v) != nil {
			continue
		}
		if v.Type == "tool_use" {
			switch v.Part.Tool {
			case "bash":
				last = commandKind(v.Part.State.Input.Command)
			case "edit", "write", "patch", "apply_patch", "multiedit":
				last = "editing files"
			case "read", "grep", "glob", "list":
				last = "reading files"
			default:
				last = "using tool"
			}
		}
		if v.Type == "assistant" {
			for _, c := range v.Message.Content {
				if c.Type != "tool_use" {
					continue
				}
				switch c.Name {
				case "Edit", "Write", "MultiEdit", "apply_patch":
					last = "editing files"
				case "Bash", "exec_command":
					last = commandKind(c.Input.Command)
				case "Read", "Glob", "Grep":
					last = "reading files"
				default:
					last = "using tool"
				}
			}
		}
		if v.Type == "item.started" || v.Type == "item.completed" || v.Type == "item.updated" {
			switch v.Item.Type {
			case "command_execution":
				last = commandKind(v.Item.Command)
			case "file_change":
				last = "editing files"
			case "mcp_tool_call":
				last = "MCP tool"
			}
		}
	}
	return last
}

func cell(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, Redact(s))
}

func (m *Manager) writeStatus(out io.Writer, view any, long ...bool) error {
	v := view.(map[string]any)
	rows := v["lanes"].([]Status)
	frozen := v["frozen"].(bool)
	freeze := "no"
	if frozen {
		freeze = "yes"
	}
	fmt.Fprintf(out, "frozen=%s  data=%s\n", freeze, cell(m.store))
	if len(long) > 0 && long[0] {
		fmt.Fprintln(out, webStatusLine(m.webDashboard()))
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	heading := "AGENT\tSTATE\tRUN\tGOAL\tELAPSED\tETA\tEVENT AGE\tACTIVITY\tREPORT\tKIND\tMODEL\tEFFORT"
	if len(long) > 0 && long[0] {
		heading += "\tCURRENT ACTION"
	}
	fmt.Fprintln(w, heading)
	m.mu.Lock()
	defer m.mu.Unlock()
	runs, _ := m.history()
	for _, s := range rows {
		state, goal, elapsed, eta, age, current, report := displayState(s), "-", "-", "-", "-", "-", "no"
		if frozen && s.State == "idle" {
			state = "frozen"
		}
		if s.GoalID != "" {
			goal, elapsed = s.GoalID, shortDuration(time.Duration(s.ElapsedSeconds)*time.Second)
		}
		if s.ReportExists {
			report = "yes"
		}
		if s.LastEventAgeSeconds != nil {
			age = shortDuration(time.Duration(*s.LastEventAgeSeconds) * time.Second)
		}
		if s.RunID != "" {
			b, _ := readWindow(filepath.Join(m.store, "runs", s.RunID, "events.jsonl"), true)
			current = activity(b)
			if s.State == "running" && s.Started != nil {
				r := Run{ID: s.RunID, Started: *s.Started}
				eta = m.etaFromHistory(runs, r, m.agentKind(s.Lane), m.runSize(s.RunID), time.Duration(s.ElapsedSeconds)*time.Second)
			}
		}
		if s.LaunchFallback {
			current += " (launchd fallback; app-quit survival unavailable)"
		}
		id := shortRunID(s.RunID)
		if id == "" {
			id = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\tmodel=%s\teffort=%s", cell(s.Lane), state, id, goal, elapsed, eta, age, current, report, cell(s.Kind), identityValue(s.Model), identityValue(s.Effort))
		if len(long) > 0 && long[0] {
			fmt.Fprintf(w, "\t%s", actionText(s.CurrentAction))
		}
		fmt.Fprintln(w)
	}
	return w.Flush()
}
