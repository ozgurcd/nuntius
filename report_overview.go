package nuntius

import (
	"crypto/sha256"
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
	p := reportOverview{Blocks: make(map[string][]string), OwnerNeed: "unknown", Warnings: []string{}}
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
	if first <= last {
		opening := strings.TrimSpace(lines[first])
		if opening == "```text" || opening == "```" || opening == "~~~text" || opening == "~~~" {
			fence := reportFence(opening)
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
	var body strings.Builder
	finish := func() {
		if current != "" {
			p.Blocks[current] = append(p.Blocks[current], body.String())
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
			if name, inline, ok := reportHeading(line); ok {
				finish()
				switch name {
				case "PLAIN SUMMARY", "STATUS", "QUESTIONS", "DECISIONS", "IDS", "HEADS", "OPEN":
					current = name
				}
				if current != "" {
					if inline != "" {
						body.WriteString(inline)
						body.WriteByte('\n')
					}
				}
				continue
			}
			if strings.HasPrefix(strings.TrimSpace(line), "end of report for ") {
				finish()
				continue
			}
		}
		if current != "" {
			body.WriteString(line)
			body.WriteByte('\n')
		}
	}
	finish()
	if fence != "" {
		p.Incomplete = true
		p.Warnings = append(p.Warnings, "Incomplete code excerpt fence.")
	}
	for _, name := range []string{"PLAIN SUMMARY", "STATUS", "QUESTIONS", "DECISIONS", "IDS", "HEADS", "OPEN"} {
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
	return p
}

func (p reportOverview) sections() map[string]string {
	sections := map[string]string{"QUESTIONS": "", "DECISIONS": ""}
	for name, blocks := range p.Blocks {
		sections[name] = strings.Join(blocks, "\n")
	}
	return sections
}
