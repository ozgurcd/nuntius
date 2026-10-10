package nuntius

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"text/tabwriter"
	"time"
)

// HistoryFilter selects finished runs only. Limit must be 1..200.
type HistoryFilter struct {
	Limit  int
	Lane   string
	GoalID string
	Since  string
}

// HistoryEntry excludes process identity, sessions and private run text.
type HistoryEntry struct {
	Kind            string    `json:"kind,omitempty"`
	Model           string    `json:"model,omitempty"`
	ShortRunID      string    `json:"short_run_id"`
	Relation        string    `json:"relation"`
	ChainID         string    `json:"chain_id"`
	ChainDepth      int       `json:"chain_depth"`
	RunID           string    `json:"run_id"`
	Lane            string    `json:"lane"`
	GoalID          string    `json:"goal_id"`
	Ended           time.Time `json:"ended"`
	Outcome         string    `json:"outcome"`
	DurationSeconds int64     `json:"duration_seconds"`
	ReportExists    bool      `json:"report_exists"`
	Exit            *int      `json:"exit"`
	ParentRunID     string    `json:"parent_run_id,omitempty"`
	ContinueRunID   string    `json:"continue_run_id,omitempty"`
	ResumeCount     int       `json:"resume_count,omitempty"`
	DisplayState    string    `json:"display_state,omitempty"`
}

func (m *Manager) History(f HistoryFilter) ([]HistoryEntry, error) {
	if f.Limit < 1 || f.Limit > 200 || (f.Lane != "" && !namePattern.MatchString(f.Lane)) || (f.GoalID != "" && !goalPattern.MatchString(f.GoalID)) {
		return nil, errors.New("history requires limit 1..200 and valid lane/goal filters")
	}
	var since time.Time
	if f.Since != "" {
		age, err := time.ParseDuration(f.Since)
		if len(f.Since) > 64 || err != nil || age <= 0 {
			return nil, errors.New("since must be a positive duration, such as 24h")
		}
		since = time.Now().Add(-age)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	runs, err := m.history()
	if err != nil {
		return nil, err
	}
	rows := []HistoryEntry{}
	index := runIndex(runs)
	for _, r := range runs {
		if r.State == "running" || r.Ended == nil || (f.Lane != "" && r.Lane != f.Lane) || (f.GoalID != "" && r.GoalID != f.GoalID) || r.Ended.Before(since) {
			continue
		}
		rows = append(rows, HistoryEntry{RunID: r.ID, Lane: r.Lane, GoalID: r.GoalID, Ended: *r.Ended, Outcome: runOutcome(r), DurationSeconds: max(0, int64(r.Ended.Sub(r.Started).Seconds())), ReportExists: m.reportExists(r.GoalID), Exit: r.Exit, ParentRunID: r.ParentRunID, ContinueRunID: r.ContinueRunID, ResumeCount: r.ResumeCount, DisplayState: terminalDisplay(r, runOutcome(r))})
		row := &rows[len(rows)-1]
		row.Kind, row.Model = r.Kind, r.Model
		row.ShortRunID = shortRunID(r.ID)
		row.Relation, row.ChainID, row.ChainDepth = runRelation(r, index)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Ended.Equal(rows[j].Ended) {
			return rows[i].RunID > rows[j].RunID
		}
		return rows[i].Ended.After(rows[j].Ended)
	})
	if len(rows) > f.Limit {
		rows = rows[:f.Limit]
	}
	return rows, nil
}

func runOutcome(r Run) string {
	if r.StopReason != "" {
		return bounded(Redact(r.StopReason), 256)
	}
	if r.State == "timed_out" {
		return "timed_out"
	}
	if r.Exit == nil {
		return "unknown_exit"
	}
	if *r.Exit != 0 {
		return "failed"
	}
	return "completed"
}

func writeHistory(out io.Writer, rows []HistoryEntry) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ENDED\tAGENT\tRUN\tGOAL\tOUTCOME\tDURATION\tREPORT\tEXIT\tRELATION\tKIND\tMODEL")
	for _, r := range groupedHistory(rows) {
		report, exit := "no", "-"
		if r.ReportExists {
			report = "yes"
		}
		if r.Exit != nil {
			exit = fmt.Sprint(*r.Exit)
		}
		state := r.Outcome
		if r.DisplayState != "" {
			state = r.DisplayState
		}
		indent := ""
		if r.ChainDepth > 0 {
			indent = "  ↳ "
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Ended.Local().Format("2006-01-02 15:04:05 MST"), cell(r.Lane), shortRunID(r.RunID), indent+cell(r.GoalID), cell(state), shortDuration(time.Duration(r.DurationSeconds)*time.Second), report, exit, cell(r.Relation), cell(r.Kind), cell(r.Model))
	}
	return w.Flush()
}

func shortRunID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func runIndex(runs []Run) map[string]Run {
	index := make(map[string]Run, len(runs))
	for _, r := range runs {
		index[r.ID] = r
	}
	return index
}

func runRelation(r Run, index map[string]Run) (string, string, int) {
	relation := "first run"
	parent, found := index[r.ParentRunID]
	if r.ParentRunID != "" {
		if r.RecoveryRun {
			relation = fmt.Sprintf("resumed %d/2 after interruption", r.ResumeCount)
			if found && parent.Ended != nil {
				relation += " at " + parent.Ended.Local().Format("15:04")
			}
		} else if found {
			relation = "continue step of the " + parent.Started.Local().Format("15:04") + " run"
		} else {
			relation = "continue step of an earlier run"
		}
	}
	if next, ok := index[r.ContinueRunID]; ok {
		verb := "continued"
		if next.RecoveryRun {
			verb = "resumed"
		}
		relation += "; " + verb + " by the " + next.Started.Local().Format("15:04") + " run"
	}
	root, depth := r.ID, 0
	seen := map[string]bool{root: true}
	for r.ParentRunID != "" {
		p, ok := index[r.ParentRunID]
		if !ok || seen[p.ID] || p.GoalID != r.GoalID || p.Lane != r.Lane {
			break
		}
		root, depth = p.ID, depth+1
		seen[root] = true
		r = p
	}
	return relation, root, depth
}

// Keep the newest chain first, then its ancestor order. Filtering and limits
// apply before grouping, so no hidden rows are added to the requested result.
func groupedHistory(rows []HistoryEntry) []HistoryEntry {
	groups := map[string][]HistoryEntry{}
	order := []string{}
	for _, row := range rows {
		key := row.ChainID
		if key == "" {
			key = row.RunID
		}
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], row)
	}
	out := make([]HistoryEntry, 0, len(rows))
	for _, key := range order {
		group := groups[key]
		sort.SliceStable(group, func(i, j int) bool { return group[i].ChainDepth < group[j].ChainDepth })
		out = append(out, group...)
	}
	return out
}

func publicHistory(rows []HistoryEntry) []HistoryEntry {
	out := groupedHistory(rows)
	for i := range out {
		out[i].RunID = shortRunID(out[i].RunID)
		out[i].ChainID = shortRunID(out[i].ChainID)
		out[i].ParentRunID = shortRunID(out[i].ParentRunID)
		out[i].ContinueRunID = shortRunID(out[i].ContinueRunID)
	}
	return out
}

func publicStatusView(view any) any {
	v := view.(map[string]any)
	copyView := make(map[string]any, len(v))
	for k, value := range v {
		copyView[k] = value
	}
	rows := append([]Status{}, v["lanes"].([]Status)...)
	for i := range rows {
		rows[i].RunID = shortRunID(rows[i].RunID)
		rows[i].LaunchdLabel = ""
	}
	copyView["lanes"] = rows
	return copyView
}

func displayState(s Status) string {
	if s.DisplayState != "" {
		return s.DisplayState
	}
	return s.State
}

func terminalDisplay(r Run, fallback string) string {
	if r.State == "interrupted" {
		if r.WaitingToResume && r.ContinueRunID == "" && r.ResumeCount < 2 {
			return "interrupted; waiting to resume"
		}
		return "interrupted"
	}
	if r.RecoveryRun {
		if r.State == "running" {
			return fmt.Sprintf("Running · resumed %d/2", r.ResumeCount)
		}
		label := fmt.Sprintf("resumed (%d/2)", r.ResumeCount)
		if outcome := runOutcome(r); r.State != "running" && (outcome == "cancelled" || outcome == "timed_out") {
			return outcome + "; " + label
		}
		return label
	}
	if r.State != "running" {
		if outcome := runOutcome(r); outcome == "cancelled" || outcome == "timed_out" {
			return outcome
		}
	}
	return fallback
}
