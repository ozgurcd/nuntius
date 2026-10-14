package nuntius

import (
	"crypto/sha256"
	"sort"
	"strings"
	"sync"
)

const reportParserVersion = "overview-1"

// Covers the latest 50 dashboard attempts plus up to 32 menu-bar lanes, with
// room for changing report versions. Hits remain hot while writers append.
const reportProjectionCapacity = 128

// This is reported evidence only. Nothing here certifies work or grants authority.
type reportOverview struct {
	Blocks         map[string][]string `json:"blocks"`
	OwnerNeed      string              `json:"owner_need"`
	Incomplete     bool                `json:"incomplete"`
	StatusConflict bool                `json:"status_conflict"`
	Attention      bool                `json:"attention"`
	Warnings       []string            `json:"warnings"`
	ReportID       string              `json:"report_id"`
	EndID          string              `json:"end_id"`
	Sections       []reportSection     `json:"sections"`
	Consensus      reportConsensus     `json:"consensus"`
}

type reportSection struct {
	Name      string `json:"name"`
	Text      string `json:"text"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

type reportConsensus struct {
	State    string   `json:"state"`
	Findings []string `json:"findings"`
	Line     int      `json:"line"`
}

func (p reportOverview) sectionSpans(name string) []reportSection {
	rows := []reportSection{}
	for _, s := range p.Sections {
		if s.Name == name {
			rows = append(rows, s)
		}
	}
	return rows
}

type reportProjectionCache struct {
	mu      sync.Mutex
	entries map[[32]byte]cachedReportProjection
	clock   uint64
	parses  uint64
}

type cachedReportProjection struct {
	overview reportOverview
	used     uint64
}

// Only already-redacted, bounded bytes enter this cache. No paths, mtimes or
// pre-redaction text are retained. The size is bounded even with changing goals.
func (c *reportProjectionCache) project(redacted string) reportOverview {
	key := sha256.Sum256([]byte(reportParserVersion + "\x00" + redacted))
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clock++
	if p, ok := c.entries[key]; ok {
		p.used = c.clock
		c.entries[key] = p
		return p.overview
	}
	p := projectReport(redacted)
	c.parses++
	if c.entries == nil {
		c.entries = make(map[[32]byte]cachedReportProjection)
	}
	if len(c.entries) == reportProjectionCapacity {
		var oldest [32]byte
		age := ^uint64(0)
		for id, entry := range c.entries {
			if entry.used < age {
				oldest, age = id, entry.used
			}
		}
		delete(c.entries, oldest)
	}
	c.entries[key] = cachedReportProjection{overview: p, used: c.clock}
	return p
}

func reportHeading(line string) (string, string, bool) {
	head := strings.TrimLeft(strings.TrimSuffix(line, "\r"), " \t")
	markdown := strings.HasPrefix(head, "#")
	if markdown {
		hashes := len(head) - len(strings.TrimLeft(head, "#"))
		if hashes > 6 || len(head) == hashes || head[hashes] != ' ' {
			return "", "", false
		}
		head = strings.TrimLeft(head[hashes:], " \t")
	}
	label, inline, colon := strings.Cut(head, ":")
	if markdown && !colon {
		label = strings.TrimSpace(strings.TrimRight(label, "#"))
	}
	label = strings.TrimSpace(label)
	if markdown {
		label = strings.ToUpper(label)
	}
	switch label {
	case "PLAIN SUMMARY", "STATUS", "ITEMS", "QUESTIONS", "DECISIONS", "IDS", "HEADS", "BRIEF'S ERRORS", "DENIALS", "WIKI-DELTA", "OPEN",
		"CWD", "FILES INSPECTED", "FILES CHANGED", "BEHAVIOUR BEFORE", "BEHAVIOUR AFTER", "TESTS/VERIFICATION", "DEVIATIONS", "DENIALS AND BLOCKS", "GOGRAPH FINDINGS", "OPEN AND ADJACENT", "NO-TOUCH CONFIRMATION":
		if !colon {
			inline = ""
		} else {
			inline = strings.TrimPrefix(inline, " ")
		}
		return label, inline, true
	}
	return "", "", false
}

func reportFence(line string) string {
	s := strings.TrimSpace(line)
	if strings.HasPrefix(s, "```") {
		return strings.Repeat("`", len(s)-len(strings.TrimLeft(s, "`")))
	}
	if strings.HasPrefix(s, "~~~") {
		return strings.Repeat("~", len(s)-len(strings.TrimLeft(s, "~")))
	}
	return ""
}

func projectReport(redacted string) reportOverview {
	p := reportOverview{Blocks: make(map[string][]string), OwnerNeed: "unknown", Warnings: []string{}, Sections: []reportSection{}, Consensus: reportConsensus{State: "missing", Findings: []string{}}}
	if strings.Contains(redacted, "[truncated]") || strings.Contains(redacted, "[REDACTED]") {
		p.Incomplete = true
		p.Warnings = append(p.Warnings, "Incomplete report interpretation: text was truncated or redacted.")
	}
	lines := strings.Split(redacted, "\n")
	first, last := 0, len(lines)-1
	for first <= last && strings.TrimSpace(lines[first]) == "" {
		first++
	}
	for last >= first && strings.TrimSpace(lines[last]) == "" {
		last--
	}
	terminalLine := last
	outsideConsensus := ""
	if first <= last {
		opening := strings.TrimSpace(lines[first])
		if opening == "```text" || opening == "```" || opening == "~~~text" || opening == "~~~" {
			fence := reportFence(opening)
			if strings.HasPrefix(lines[last], "CONSENSUS:") && last > first && strings.TrimSpace(lines[last-1]) == fence {
				outsideConsensus = lines[last]
				last--
			}
			first++
			if last >= first && strings.TrimSpace(lines[last]) == fence {
				last--
			} else {
				p.Incomplete = true
				p.Warnings = append(p.Warnings, "Incomplete outer report fence.")
			}
		}
	}
	current, fence := "", ""
	expectedEnd := last
	if outsideConsensus == "" && expectedEnd >= first && strings.HasPrefix(lines[expectedEnd], "CONSENSUS:") {
		expectedEnd--
	}
	for expectedEnd >= first && strings.TrimSpace(lines[expectedEnd]) == "" {
		expectedEnd--
	}
	startLine, endLine, lastEnd, consensusCount := 0, 0, -1, 0
	addConsensus := func(line string, index int, terminal bool) {
		consensusCount++
		p.Blocks["CONSENSUS"] = append(p.Blocks["CONSENSUS"], strings.TrimPrefix(line, "CONSENSUS: ")+"\n")
		p.Sections = append(p.Sections, reportSection{Name: "CONSENSUS", Text: line, StartLine: index + 1, EndLine: index + 1})
		p.Consensus = reportConsensus{State: "invalid", Findings: []string{}, Line: index + 1}
		if !terminal || consensusCount != 1 || lastEnd < 0 {
			return
		}
		line = strings.TrimSuffix(line, "\r")
		if line == "CONSENSUS: AGREE" {
			p.Consensus.State = "agree"
			return
		}
		if !strings.HasPrefix(line, "CONSENSUS: DISAGREE ") {
			return
		}
		ids := strings.Split(strings.TrimPrefix(line, "CONSENSUS: DISAGREE "), " ")
		seen := map[string]bool{}
		for _, id := range ids {
			if !queueID.MatchString(id) || seen[id] {
				return
			}
			seen[id] = true
		}
		p.Consensus.State = "disagree"
		p.Consensus.Findings = ids
	}
	var body strings.Builder
	finish := func() {
		if current != "" {
			p.Blocks[current] = append(p.Blocks[current], body.String())
			p.Sections = append(p.Sections, reportSection{Name: current, Text: body.String(), StartLine: startLine, EndLine: endLine})
		}
		body.Reset()
		current = ""
	}
	for i := first; i <= last; i++ {
		line := lines[i]
		f := reportFence(line)
		if f != "" {
			if fence == "" {
				fence = f
			} else if strings.TrimSpace(line) == f && len(f) >= len(fence) && f[0] == fence[0] {
				fence = ""
			}
		} else if fence == "" {
			if strings.HasPrefix(line, "Final Report — ") {
				if p.ReportID != "" || i != first {
					p.Incomplete = true
				}
				p.ReportID = strings.TrimSuffix(strings.TrimPrefix(line, "Final Report — "), "\r")
			}
			if strings.HasPrefix(line, "CONSENSUS:") {
				if i == terminalLine {
					finish()
					addConsensus(line, i, true)
					continue
				}
				addConsensus(line, i, false)
				p.Incomplete = true
				p.Warnings = append(p.Warnings, "Nonterminal CONSENSUS line is report text, not a verdict.")
			}
			if name, inline, ok := reportHeading(line); ok {
				finish()
				current = name
				startLine = i + 1
				endLine = i + 1
				if current != "" {
					if inline != "" {
						body.WriteString(inline)
						body.WriteByte('\n')
					}
				}
				continue
			}
			if strings.HasPrefix(line, "end of report for ") {
				if i != expectedEnd {
					p.Incomplete = true
					p.Warnings = append(p.Warnings, "Nonterminal end marker is report text, not a boundary.")
				} else {
					p.EndID = strings.TrimSuffix(strings.TrimPrefix(line, "end of report for "), "\r")
					lastEnd = i
					finish()
					continue
				}
			}
		}
		if current != "" {
			endLine = i + 1
			body.WriteString(line)
			body.WriteByte('\n')
		}
	}
	finish()
	if outsideConsensus != "" {
		addConsensus(outsideConsensus, terminalLine, true)
	}
	if p.ReportID != "" {
		endAt := last
		if outsideConsensus == "" && endAt >= first && strings.HasPrefix(lines[endAt], "CONSENSUS:") {
			endAt--
		}
		for endAt >= first && strings.TrimSpace(lines[endAt]) == "" {
			endAt--
		}
		if lastEnd != endAt {
			p.Incomplete = true
			p.Warnings = append(p.Warnings, "Report end marker is not terminal.")
		}
	}
	if consensusCount > 1 {
		p.Consensus.State = "invalid"
		p.Consensus.Findings = []string{}
	}
	if fence != "" {
		p.Incomplete = true
		p.Warnings = append(p.Warnings, "Incomplete code excerpt fence.")
	}
	for name := range p.Blocks {
		if len(p.Blocks[name]) > 1 {
			p.Incomplete = true
			p.Warnings = append(p.Warnings, "Repeated "+name+" section; each source block is shown.")
		}
	}
	statuses := p.Blocks["STATUS"]
	for i, s := range statuses {
		if i > 0 && !strings.EqualFold(strings.TrimSpace(s), strings.TrimSpace(statuses[0])) {
			p.StatusConflict = true
		}
		s = strings.ToLower(strings.TrimSpace(s))
		if strings.HasPrefix(s, "partial") || strings.HasPrefix(s, "parked") || strings.HasPrefix(s, "stop") || strings.HasPrefix(s, "protective stop") {
			p.Attention = true
		}
	}
	if p.StatusConflict {
		p.Warnings = append(p.Warnings, "Conflicting report status.")
	}
	questions := p.Blocks["QUESTIONS"]
	if !p.Incomplete && len(questions) == 1 && strings.TrimSpace(questions[0]) != "" {
		q := strings.ToLower(strings.TrimSpace(questions[0]))
		q = strings.TrimSuffix(strings.TrimSuffix(q, "."), "!")
		if q == "none" || q == "none recorded" || q == "no open questions" {
			p.OwnerNeed = "none"
		} else {
			p.OwnerNeed = "decision"
		}
	}
	// Explicit reported requests stay visible even when QUESTIONS says none.
	if p.OwnerNeed == "none" {
		for _, name := range []string{"PLAIN SUMMARY", "STATUS", "OPEN"} {
			text := strings.ToLower(strings.Join(p.Blocks[name], "\n"))
			for _, request := range []string{"owner must", "you must", "await owner", "await the owner", "waiting for owner", "waiting for the owner", "requires owner approval", "needs owner decision", "needs your decision", "please approve"} {
				if strings.Contains(text, request) {
					p.Attention = true
					p.Warnings = append(p.Warnings, "Report sections may disagree: QUESTIONS says none; "+name+" contains an owner-action request. Inspect the exact wording.")
					break
				}
			}
		}
	}
	if p.OwnerNeed != "none" || p.Incomplete {
		p.Attention = true
	}
	if p.Incomplete || p.StatusConflict {
		p.Consensus.State = "incomplete"
		p.Consensus.Findings = []string{}
	}
	sort.SliceStable(p.Sections, func(i, j int) bool { return p.Sections[i].StartLine < p.Sections[j].StartLine })
	return p
}

func (p reportOverview) sections() map[string]string {
	sections := map[string]string{"QUESTIONS": "", "DECISIONS": ""}
	for name, blocks := range p.Blocks {
		sections[name] = strings.Join(blocks, "\n")
	}
	return sections
}
