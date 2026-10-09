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
	RunID           string    `json:"run_id"`
	Lane            string    `json:"lane"`
	GoalID          string    `json:"goal_id"`
	Ended           time.Time `json:"ended"`
	Outcome         string    `json:"outcome"`
	DurationSeconds int64     `json:"duration_seconds"`
	ReportExists    bool      `json:"report_exists"`
	Exit            *int      `json:"exit"`
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
	for _, r := range runs {
		if r.State == "running" || r.Ended == nil || (f.Lane != "" && r.Lane != f.Lane) || (f.GoalID != "" && r.GoalID != f.GoalID) || r.Ended.Before(since) {
			continue
		}
		rows = append(rows, HistoryEntry{RunID: r.ID, Lane: r.Lane, GoalID: r.GoalID, Ended: *r.Ended, Outcome: runOutcome(r), DurationSeconds: max(0, int64(r.Ended.Sub(r.Started).Seconds())), ReportExists: m.reportExists(r.GoalID), Exit: r.Exit})
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
	fmt.Fprintln(w, "ENDED\tAGENT\tGOAL\tOUTCOME\tDURATION\tREPORT\tEXIT")
	for _, r := range rows {
		report, exit := "no", "-"
		if r.ReportExists {
			report = "yes"
		}
		if r.Exit != nil {
			exit = fmt.Sprint(*r.Exit)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Ended.Local().Format("2006-01-02 15:04:05 MST"), cell(r.Lane), cell(r.GoalID), cell(r.Outcome), shortDuration(time.Duration(r.DurationSeconds)*time.Second), report, exit)
	}
	return w.Flush()
}

func displayState(s Status) string {
	if s.DisplayState != "" {
		return s.DisplayState
	}
	return s.State
}

func terminalDisplay(r Run, fallback string) string {
	if r.State != "running" {
		if outcome := runOutcome(r); outcome == "cancelled" || outcome == "timed_out" {
			return outcome
		}
	}
	return fallback
}
