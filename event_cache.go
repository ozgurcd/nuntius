package nuntius

import (
	"bytes"
	"crypto/sha256"
	"strings"
)

type eventWindowID struct {
	runID  string
	prefix bool
}

type cachedAction struct {
	event actionEvent
}

type cachedEventLine struct {
	actions         []cachedAction
	final           string
	hasFinal, asked bool
	activity        string
}

type parsedEventWindow struct {
	digest           [32]byte
	used             uint64
	lines            map[[32]byte]cachedEventLine
	latest, command  actionEvent
	commandRunning   bool
	final            string
	asked            bool
	activity, effort string
	tail             []string
}

// eventWindow is called with m.mu held. Callers still read through privateFile
// on every request: cached data cannot bypass permissions, symlink checks or
// same-sized rewrites. Only hashes and redacted summaries survive the call.
// Prefix/tail entries are separate, bounded to 64 windows (32 lanes), with at
// most 8192 distinct cached lines per tail. Replaced lines are discarded.
func (m *Manager) eventWindow(runID string, b []byte, prefix bool) *parsedEventWindow {
	if m.eventWindows == nil {
		m.eventWindows = make(map[eventWindowID]*parsedEventWindow)
	}
	key := eventWindowID{runID, prefix}
	digest := sha256.Sum256(b)
	m.eventCacheClock++
	old := m.eventWindows[key]
	if old != nil && old.digest == digest {
		old.used = m.eventCacheClock
		return old
	}
	if old == nil && len(m.eventWindows) >= 64 {
		var oldest eventWindowID
		var age uint64 = ^uint64(0)
		for id, window := range m.eventWindows {
			if window.used < age {
				oldest, age = id, window.used
			}
		}
		delete(m.eventWindows, oldest)
	}
	w := &parsedEventWindow{digest: digest, used: m.eventCacheClock, activity: "-"}
	if prefix {
		w.effort = codexEffort(b)
		m.eventWindows[key] = w
		return w
	}
	lines := bytes.Split(b, []byte("\n"))
	private := privateEventLines(b, lines)
	w.lines = make(map[[32]byte]cachedEventLine, min(len(lines), 8192))
	pending := map[string]actionEvent{}
	var summaries []string
	for i, line := range lines {
		if private != nil && private[i] {
			w.final = "[REDACTED]"
			w.latest = actionEvent{Summary: "[REDACTED]"}
			pending = map[string]actionEvent{}
			if len(summaries) == 0 || summaries[len(summaries)-1] != "[REDACTED]" {
				summaries = append(summaries, "[REDACTED]")
			}
			continue
		}
		hash := sha256.Sum256(line)
		parsed, ok := w.lines[hash]
		if !ok && old != nil {
			parsed, ok = old.lines[hash]
		}
		if !ok {
			parsed.activity = activity(line)
			final, asked, hasFinal := questionEvent(line)
			parsed.final, parsed.asked, parsed.hasFinal = bounded(final, 8192), asked, hasFinal
			for _, a := range eventActions(line) {
				a.Text = ""
				a.Summary = strings.Clone(a.Summary)
				parsed.actions = append(parsed.actions, cachedAction{a})
			}
		}
		w.asked = w.asked || parsed.asked
		if parsed.hasFinal {
			w.final = parsed.final
		}
		if parsed.activity != "-" {
			w.activity = parsed.activity
		}
		for _, cached := range parsed.actions {
			a := cached.event
			if a.Started && a.Command && a.ID != "" {
				pending[a.ID] = a
			}
			if a.Finished {
				delete(pending, a.ID)
			}
			if a.Summary != "" {
				w.latest = a
				summaries = append(summaries, a.Summary)
			}
		}
		// A private block can span JSON events. Individually parsed body lines
		// have no marker of their own; never retain their text in the cache.
		if len(w.lines) < 8192 {
			w.lines[hash] = parsed
		}
	}
	w.command, w.commandRunning = pending[w.latest.ID]
	// actionSummary already redacts the complete source before making a single
	// physical line, and the loop above suppresses private blocks across events.
	// Only the last 200 summaries can reach Tail; do not redact older history
	// again on each append. Keep the final redaction and per-line output bounds.
	if len(summaries) > 200 {
		summaries = summaries[len(summaries)-200:]
	}
	tail := strings.Split(Redact(strings.Join(summaries, "\n")), "\n")
	if len(tail) > 200 {
		tail = tail[len(tail)-200:]
	}
	w.tail = make([]string, len(tail))
	for i := range tail {
		w.tail[i] = strings.Clone(bounded(tail[i], 4096))
	}
	m.eventWindows[key] = w
	return w
}
