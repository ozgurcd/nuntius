package nuntius

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type judgeCLIRequest struct {
	command string
	json    bool
	id      string
	section string
	states  map[string]bool
	project bool
	lines   int
}

type judgeSource struct {
	Kind     string  `json:"kind"`
	Revision *string `json:"revision"`
	Sequence *uint64 `json:"sequence"`
	Validity string  `json:"validity"`
}

type judgeError struct {
	Code      string `json:"code"`
	Retryable bool   `json:"retryable"`
}

type judgeEnvelope struct {
	Schema      string      `json:"schema"`
	Command     string      `json:"command"`
	Mode        string      `json:"mode"`
	StateDomain string      `json:"state_domain"`
	Result      string      `json:"result"`
	ObservedAt  string      `json:"observed_at"`
	Complete    bool        `json:"complete"`
	Source      judgeSource `json:"source"`
	Data        any         `json:"data"`
	Warnings    []string    `json:"warnings"`
	Error       *judgeError `json:"error"`
}

var judgeLinks = regexp.MustCompile(`(?i)(?:[a-z][a-z0-9+.-]*://|www\.)[^\s<>]+`)

func judgeSafeText(s string) string {
	return judgeLinks.ReplaceAllString(approvePlain(s, false), redactionHidden)
}

func judgeFailure(command, result, code string) judgeEnvelope {
	domain := "portable"
	if command == "tail" {
		domain = "run"
	}
	if command == "report show" {
		domain = "report"
	}
	return judgeEnvelope{Schema: "nuntius.judge.v1", Command: command, Mode: domain, StateDomain: domain, Result: result, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Source: judgeSource{Kind: "unknown", Validity: "unknown"}, Warnings: []string{}, Error: &judgeError{Code: code}}
}

func judgeSection(name string) bool {
	switch name {
	case "PLAIN SUMMARY", "STATUS", "ITEMS", "QUESTIONS", "DECISIONS", "IDS", "HEADS", "BRIEF'S ERRORS", "DENIALS", "WIKI-DELTA", "OPEN", "CONSENSUS":
		return true
	}
	return false
}

// Parse the closed read interface before loading any source. Positional IDs
// stay canonical; error responses never echo argv or paths.
func parseJudgeCLI(args []string) (judgeCLIRequest, bool) {
	r := judgeCLIRequest{command: args[0], states: map[string]bool{}, lines: 50}
	for _, arg := range args {
		if arg == "--json" {
			r.json = true
		}
	}
	i := 1
	if r.command == "report" {
		r.command = "report show"
		if len(args) < 3 || args[1] != "show" || !goalPattern.MatchString(args[2]) {
			return r, false
		}
		r.id = args[2]
		i = 3
	} else if r.command == "tail" {
		if len(args) < 2 || !queueID.MatchString(args[1]) {
			return r, false
		}
		r.id = args[1]
		i = 2
	}
	seen := map[string]bool{}
	for ; i < len(args); i++ {
		key, value, equals := strings.Cut(args[i], "=")
		if key == "--json" {
			if equals || seen[key] {
				return r, false
			}
			seen[key] = true
			continue
		}
		if key != "--state" && key != "--project" && key != "--section" && key != "--lines" {
			return r, false
		}
		if seen[key] && key != "--state" {
			return r, false
		}
		seen[key] = true
		if !equals {
			i++
			if i >= len(args) {
				return r, false
			}
			value = args[i]
		}
		if value == "" {
			return r, false
		}
		switch key {
		case "--state":
			if r.command != "queue" || (value != "planned" && value != "blocked" && value != "ready" && value != "parked") {
				return r, false
			}
			r.states[value] = true
		case "--project":
			if r.command != "queue" || !queueID.MatchString(value) {
				return r, false
			}
			r.project = true
		case "--section":
			if r.command != "report show" || !judgeSection(value) {
				return r, false
			}
			r.section = value
		case "--lines":
			n, err := strconv.Atoi(value)
			if r.command != "tail" || err != nil || n < 1 || n > 200 {
				return r, false
			}
			r.lines = n
		}
	}
	return r, true
}

func writeJudgeResult(out, errOut io.Writer, r judgeCLIRequest, v judgeEnvelope) int {
	// Encode before writing: never publish a cut JSON object or an oversized
	// complete population. The existing source bounds still apply upstream.
	b, err := json.Marshal(v)
	if err != nil || len(b) > queueMaxBytes {
		v = judgeFailure(r.command, "cannot_evaluate", "response_too_large")
		b, _ = json.Marshal(v)
	}
	code := 0
	if v.Result != "ok" {
		code = 2
		fmt.Fprintln(errOut, "judge read unavailable; inspect the result code")
	}
	if r.json {
		_, err = fmt.Fprintln(out, string(b))
	} else if v.Result != "ok" {
		_, err = fmt.Fprintln(out, v.Result+": "+v.Error.Code)
	} else {
		// Human output uses the same safe DTO; machine identity is never shortened.
		body, _ := json.MarshalIndent(v.Data, "", "  ")
		_, err = fmt.Fprintln(out, string(body))
	}
	if err != nil {
		fmt.Fprintln(errOut, "cannot write judge result")
		return 1
	}
	return code
}

func runJudgeCLI(r judgeCLIRequest, config, store string, out, errOut io.Writer) int {
	if r.project {
		return writeJudgeResult(out, errOut, r, judgeFailure(r.command, "cannot_evaluate", "unsupported_filter"))
	}
	c, err := LoadConfig(config)
	if err != nil {
		return writeJudgeResult(out, errOut, r, judgeFailure(r.command, "cannot_evaluate", "config_unavailable"))
	}
	m, err := newReadManager(c, store)
	if err != nil {
		return writeJudgeResult(out, errOut, r, judgeFailure(r.command, "cannot_evaluate", "store_unavailable"))
	}
	defer m.Close()
	v := judgeFailure(r.command, "ok", "")
	v.Error = nil
	v.Complete = true
	switch r.command {
	case "queue":
		q := m.queue()
		v.Source = judgeSource{Kind: "portable_queue", Validity: q.Status}
		if q.Status != "ok" {
			v.Result = "cannot_evaluate"
			v.Complete = false
			v.Error = &judgeError{Code: "queue_" + q.Status}
			v.Data = map[string]any{"status": q.Status}
			break
		}
		rows := []queueGoal{}
		allRows := []queueGoal{}
		counts := map[string]int{"planned": 0, "blocked": 0, "ready": 0, "parked": 0}
		for _, g := range q.Goals {
			counts[g.State]++
			for _, id := range append([]string{g.ID, g.Lane}, g.DependsOn...) {
				if judgeSafeText(id) != id {
					return writeJudgeResult(out, errOut, r, judgeFailure(r.command, "cannot_evaluate", "redacted_identity"))
				}
			}
			g.Title = judgeSafeText(g.Title)
			g.Reason = judgeSafeText(g.Reason)
			g.OwnerReason = judgeSafeText(g.OwnerReason)
			g.BriefLink = ""
			allRows = append(allRows, g)
			if len(r.states) > 0 && !r.states[g.State] {
				continue
			}
			// IDs and typed fields were validated by the existing portable codec.
			rows = append(rows, g)
		}
		identity, _ := json.Marshal(struct {
			Updated string
			Rows    []queueGoal
		}{q.Updated, allRows})
		digest := sha256.Sum256(identity)
		revision := hex.EncodeToString(digest[:])
		v.Source.Revision = &revision
		v.Source.Kind = "safe_portable_projection"
		v.Data = map[string]any{"population": "pending", "authority": false, "total": len(q.Goals), "matched": len(rows), "returned": len(rows), "counts": counts, "rows": rows, "next_cursor": nil, "updated": q.Updated}
	case "report show":
		text, e := m.Report(r.id)
		if e != nil {
			v = judgeFailure(r.command, "cannot_evaluate", "report_unavailable")
			break
		}
		text = judgeSafeText(text)
		p := projectReport(text)
		valid := !p.Incomplete && !p.StatusConflict && p.ReportID == r.id && p.EndID == r.id
		errorCode := ""
		if !valid {
			errorCode = "report_incomplete"
			p.Consensus.State = "incomplete"
			p.Consensus.Findings = []string{}
		}
		v.Source = judgeSource{Kind: "redacted_report", Validity: "incomplete"}
		if valid {
			digest := sha256.Sum256([]byte(text))
			revision := hex.EncodeToString(digest[:])
			v.Source.Revision = &revision
			v.Source.Validity = "stable_redacted"
		}
		v.Warnings = p.Warnings
		v.Data = map[string]any{"id": r.id, "parser_version": 1, "section": r.section, "blocks": p.Blocks, "sections": p.Sections, "consensus": p.Consensus, "status_conflict": p.StatusConflict, "incomplete": !valid}
		if r.section != "" {
			v.Data = map[string]any{"id": r.id, "parser_version": 1, "section": r.section, "blocks": p.Blocks[r.section], "sections": p.sectionSpans(r.section), "consensus": p.Consensus, "status_conflict": p.StatusConflict, "incomplete": !valid}
			if len(p.Blocks[r.section]) == 0 {
				v.Data.(map[string]any)["blocks"] = []string{}
				if errorCode == "" {
					errorCode = "section_absent"
				}
			}
		} else {
			v.Data.(map[string]any)["text"] = text
		}
		if r.section == "CONSENSUS" && valid {
			switch p.Consensus.State {
			case "missing":
				errorCode = "consensus_missing"
			case "agree", "disagree":
			default:
				errorCode = "consensus_invalid"
			}
		}
		if errorCode != "" {
			v.Result = "cannot_evaluate"
			v.Complete = false
			v.Error = &judgeError{Code: errorCode}
		}
	case "tail":
		rows, e := m.Tail(r.id, r.lines)
		v.StateDomain = "run"
		v.Source = judgeSource{Kind: "action_tail", Validity: "bounded_window"}
		if e != nil {
			v.Result = "cannot_evaluate"
			v.Complete = false
			v.Error = &judgeError{Code: "tail_unavailable"}
			break
		}
		for i := range rows {
			rows[i] = judgeSafeText(rows[i])
		}
		v.Data = map[string]any{"lane": r.id, "lines": rows, "returned": len(rows), "requested": r.lines, "coverage": "bounded_window"}
	}
	return writeJudgeResult(out, errOut, r, v)
}
