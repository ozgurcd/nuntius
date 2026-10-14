package nuntius

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func judgeRead(t *testing.T, config, store string, args ...string) (int, map[string]any) {
	t.Helper()
	var out, errs bytes.Buffer
	code := CLI(context.Background(), append([]string{"--config", config, "--store", store}, args...), strings.NewReader(""), &out, &errs)
	var v map[string]any
	if json.Unmarshal(out.Bytes(), &v) != nil {
		t.Fatal("read did not return a JSON object")
	}
	if v["schema"] != "nuntius.judge.v1" {
		t.Fatal("wrong schema")
	}
	return code, v
}

func TestJudgeLegacyPassive(t *testing.T) {
	for _, command := range []string{"audit", "history"} {
		t.Run(command, func(t *testing.T) {
			c, store := fixture(t)
			config := laneCLIConfig(t, c)
			before := laneCLITree(t, store)
			_, _, _ = laneCLI(t, config, store, command, "--json")
			if !reflect.DeepEqual(before, laneCLITree(t, store)) {
				t.Fatal("legacy read created or changed its store")
			}
		})
	}
}

func judgeReportText(id, body, ending string) string {
	return "Final Report — " + id + "\nPLAIN SUMMARY: Safe summary.\nSTATUS: complete\n" + body + "\nend of report for " + id + "\n" + ending
}

func TestJudgeReadPassiveAndIsolation(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "faulted"}[existing], func(t *testing.T) {
			c, store := fixture(t)
			if existing {
				m, err := NewManager(c, store)
				if err != nil {
					t.Fatal(err)
				}
				_ = m.Close()
				if os.WriteFile(filepath.Join(store, "lane-control-intent.json"), []byte("{}"), 0600) != nil {
					t.Fatal("seed failed")
				}
				if os.WriteFile(filepath.Join(store, "queue.json"), []byte("{}"), 0600) != nil {
					t.Fatal("seed failed")
				}
			}
			config := laneCLIConfig(t, c)
			if os.WriteFile(filepath.Join(c.ReportsDir, "READ.txt"), []byte(judgeReportText("READ", "QUESTIONS: none", "CONSENSUS: AGREE\n")), 0600) != nil {
				t.Fatal("seed failed")
			}
			before := laneCLITree(t, store)
			code, _ := judgeRead(t, config, store, "queue", "--json")
			if code != 2 {
				t.Fatal("unknown queue looked complete")
			}
			code, v := judgeRead(t, config, store, "report", "show", "READ", "--section", "CONSENSUS", "--json")
			if code != 0 || v["complete"] != true {
				t.Fatal("independent report failed")
			}
			code, _ = judgeRead(t, config, store, "tail", "codex", "--json")
			if (existing && code != 0) || (!existing && code != 2) {
				t.Fatal("independent tail failed")
			}
			for _, args := range [][]string{{"audit", "--json"}, {"history", "--json"}, {"status", "--json"}} {
				code, _, _ := laneCLI(t, config, store, args...)
				if code != 0 && !(args[0] == "history" && !existing && code == 2) {
					t.Fatal("existing read failed")
				}
			}
			if !reflect.DeepEqual(before, laneCLITree(t, store)) {
				t.Fatal("reader changed file tree")
			}
		})
	}
}

func TestJudgeQueueFilters(t *testing.T) {
	c, store := fixture(t)
	path := filepath.Join(t.TempDir(), "queue.json")
	c.QueueFile = &path
	config := laneCLIConfig(t, c)
	rows := []map[string]any{}
	for i, state := range []string{"ready", "planned", "blocked", "parked"} {
		rows = append(rows, map[string]any{"id": []string{"Z", "A", "B", "C"}[i], "title": "Safe title", "size": "M", "lane_kinds": []string{"codex"}, "order": i, "depends_on": []string{}, "needs_owner": false, "state": state, "reason": "Safe reason", "created": "2026-10-14T10:00:00Z", "updated": "2026-10-14T10:00:00Z", "brief_link": "https://example.com/brief"})
	}
	b, _ := json.Marshal(map[string]any{"version": 1, "updated": "2026-10-14T10:00:00Z", "goals": rows})
	if os.WriteFile(path, b, 0600) != nil {
		t.Fatal("seed failed")
	}
	code, v := judgeRead(t, config, store, "queue", "--state", "ready", "--state", "parked", "--json")
	if code != 0 || v["mode"] != "portable" || v["state_domain"] != "portable" {
		t.Fatal("wrong domain")
	}
	d := v["data"].(map[string]any)
	if d["total"] != float64(4) || d["matched"] != float64(2) || d["returned"] != float64(2) || d["population"] != "pending" || d["authority"] != false {
		t.Fatal("wrong counts")
	}
	r := d["rows"].([]any)
	if r[0].(map[string]any)["id"] != "Z" || r[1].(map[string]any)["order"] != float64(3) {
		t.Fatal("filter changed order")
	}
	if _, ok := r[0].(map[string]any)["brief_link"]; ok {
		t.Fatal("link escaped")
	}
	code, v = judgeRead(t, config, store, "queue", "--project", "IDP", "--json")
	if code != 2 || v["result"] != "cannot_evaluate" {
		t.Fatal("project was guessed")
	}
	for _, args := range [][]string{{"queue", "--state", "approved", "--json"}, {"queue", "--wat", "--json"}, {"tail", "codex", "--lines", "0", "--json"}, {"tail", "codex", "--json", "--json"}, {"report", "show", "READ", "--section", "UNKNOWN", "--json"}} {
		code, v = judgeRead(t, config, store, args...)
		if code != 2 || v["result"] != "invalid_request" {
			t.Fatal("invalid syntax accepted")
		}
	}
}

func TestJudgeReportConsensus(t *testing.T) {
	c, store := fixture(t)
	config := laneCLIConfig(t, c)
	for _, tc := range []struct {
		name, body, ending string
		ok                 bool
	}{
		{"agree", "QUESTIONS: none", "CONSENSUS: AGREE\n", true},
		{"disagree", "QUESTIONS: none", "CONSENSUS: DISAGREE R01 R02\n", true},
		{"missing", "QUESTIONS: none", "", false},
		{"nonterminal", "QUESTIONS: none", "CONSENSUS: AGREE\nmore\n", false},
		{"duplicate", "CONSENSUS: AGREE", "CONSENSUS: AGREE\n", false},
		{"conflict", "CONSENSUS: DISAGREE R01", "CONSENSUS: AGREE\n", false},
		{"fenced", "ITEMS:\n```text\nCONSENSUS: AGREE\n```", "", false},
		{"quoted", "ITEMS:\n> CONSENSUS: AGREE", "", false},
		{"indent", "QUESTIONS: none", " CONSENSUS: AGREE\n", false},
		{"invalid", "QUESTIONS: none", "CONSENSUS: DISAGREE\n", false},
		{"redacted", "QUESTIONS: [REDACTED]", "CONSENSUS: AGREE\n", false},
		{"statusconflict", "STATUS: partial", "CONSENSUS: AGREE\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := judgeReportText("READ", tc.body, tc.ending)
			if os.WriteFile(filepath.Join(c.ReportsDir, "READ.txt"), []byte(text), 0600) != nil {
				t.Fatal("seed failed")
			}
			code, v := judgeRead(t, config, store, "report", "show", "READ", "--section", "CONSENSUS", "--json")
			if (code == 0) != tc.ok || v["complete"] != tc.ok {
				t.Fatal("unsafe consensus interpretation")
			}
		})
	}
	for _, text := range []string{judgeReportText("OTHER", "QUESTIONS: none", "CONSENSUS: AGREE\n"), strings.Repeat("a", 65537)} {
		if os.WriteFile(filepath.Join(c.ReportsDir, "READ.txt"), []byte(text), 0600) != nil {
			t.Fatal("seed failed")
		}
		code, v := judgeRead(t, config, store, "report", "show", "READ", "--json")
		if code != 2 || v["complete"] != false {
			t.Fatal("incomplete source accepted")
		}
	}
}

func TestJudgeReportBoundaryAndSections(t *testing.T) {
	c, store := fixture(t)
	config := laneCLIConfig(t, c)
	for _, tc := range []struct {
		name, text string
		ok         bool
	}{
		{"outer", "```text\n" + judgeReportText("READ", "QUESTIONS: none", "") + "```\nCONSENSUS: AGREE\n", true},
		{"inside_outer", "```text\n" + judgeReportText("READ", "QUESTIONS: none", "CONSENSUS: AGREE\n") + "```\n", false},
		{"crlf", strings.ReplaceAll(judgeReportText("READ", "QUESTIONS: none", "CONSENSUS: AGREE\n"), "\n", "\r\n"), true},
		{"preamble", "extra\n" + judgeReportText("READ", "QUESTIONS: none", "CONSENSUS: AGREE\n"), false},
		{"after_end", judgeReportText("READ", "QUESTIONS: none", "extra\nCONSENSUS: AGREE\n"), false},
		{"duplicate_summary", judgeReportText("READ", "PLAIN SUMMARY: second\nQUESTIONS: none", "CONSENSUS: AGREE\n"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if os.WriteFile(filepath.Join(c.ReportsDir, "READ.txt"), []byte(tc.text), 0600) != nil {
				t.Fatal("seed failed")
			}
			code, v := judgeRead(t, config, store, "report", "show", "READ", "--section", "CONSENSUS", "--json")
			if (code == 0) != tc.ok || v["complete"] != tc.ok {
				t.Fatal("report boundary was misread")
			}
		})
	}
	text := judgeReportText("READ", "ITEMS: first\nITEMS: second\nQUESTIONS: none", "CONSENSUS: AGREE\n")
	if os.WriteFile(filepath.Join(c.ReportsDir, "READ.txt"), []byte(text), 0600) != nil {
		t.Fatal("seed failed")
	}
	code, v := judgeRead(t, config, store, "report", "show", "READ", "--section", "ITEMS", "--json")
	data := v["data"].(map[string]any)
	if code != 2 || len(data["blocks"].([]any)) != 2 || len(data["sections"].([]any)) != 2 {
		t.Fatal("duplicate sections were lost")
	}
	first := data["sections"].([]any)[0].(map[string]any)
	if first["start_line"] != float64(4) || first["end_line"] != float64(4) {
		t.Fatal("wrong source span")
	}
}

func TestJudgeReadInvalidAndDisabledSources(t *testing.T) {
	c, store := fixture(t)
	disabled := ""
	c.QueueFile = &disabled
	config := laneCLIConfig(t, c)
	code, v := judgeRead(t, config, store, "queue", "--json")
	if code != 2 || v["source"].(map[string]any)["validity"] != "disabled" {
		t.Fatal("disabled source looked known")
	}
	code, _ = judgeRead(t, config, store, "report", "show", "ABSENT", "--json")
	if code != 2 {
		t.Fatal("missing report accepted")
	}
	code, _ = judgeRead(t, config, store, "tail", "unknown", "--json")
	if code != 2 {
		t.Fatal("unknown lane accepted")
	}
	code, v = judgeRead(t, filepath.Join(t.TempDir(), "missing.json"), store, "queue", "--json")
	if code != 2 || v["result"] != "cannot_evaluate" {
		t.Fatal("missing config accepted")
	}
	text := judgeReportText("READ", "QUESTIONS: none", "")
	if os.WriteFile(filepath.Join(c.ReportsDir, "READ.txt"), []byte(text), 0600) != nil {
		t.Fatal("seed failed")
	}
	code, v = judgeRead(t, config, store, "report", "show", "READ", "--section", "PLAIN SUMMARY", "--json")
	if code != 0 || v["source"].(map[string]any)["revision"] == nil {
		t.Fatal("complete legacy report unavailable")
	}
	code, _ = judgeRead(t, config, store, "report", "show", "READ", "--section", "CONSENSUS", "--json")
	if code != 2 {
		t.Fatal("legacy consensus was inferred")
	}
	if os.Symlink(filepath.Join(c.ReportsDir, "READ.txt"), filepath.Join(c.ReportsDir, "LINK.txt")) != nil {
		t.Fatal("seed failed")
	}
	code, _ = judgeRead(t, config, store, "report", "show", "LINK", "--json")
	if code != 2 {
		t.Fatal("linked report accepted")
	}
}

func TestJudgeReviewNonterminalConsensusRetainsBody(t *testing.T) {
	c, store := fixture(t)
	config := laneCLIConfig(t, c)
	text := judgeReportText("READ", "ITEMS: first line\nCONSENSUS: AGREE\nsecond line\nthird line\nQUESTIONS: none", "")
	if os.WriteFile(filepath.Join(c.ReportsDir, "READ.txt"), []byte(text), 0600) != nil {
		t.Fatal("seed failed")
	}
	code, v := judgeRead(t, config, store, "report", "show", "READ", "--section", "ITEMS", "--json")
	data := v["data"].(map[string]any)
	body := data["blocks"].([]any)[0].(string)
	if code != 2 || v["complete"] != false || !strings.Contains(body, "CONSENSUS: AGREE\nsecond line\nthird line") {
		t.Fatal("nonterminal consensus lost report body")
	}
	if len(v["warnings"].([]any)) == 0 {
		t.Fatal("nonterminal consensus lacked a diagnostic")
	}
}

func TestJudgeReviewHistorySafeDiagnostic(t *testing.T) {
	c, store := fixture(t)
	config := laneCLIConfig(t, c)
	code, _, errs := laneCLI(t, config, store, "history", "--json")
	if code != 2 || strings.Contains(errs, store) || strings.Contains(errs, "no such file") {
		t.Fatal("history exposed a filesystem diagnostic")
	}
}

func TestJudgeReviewFailureDomains(t *testing.T) {
	c, store := fixture(t)
	config := laneCLIConfig(t, c)
	for _, tc := range []struct {
		args   []string
		domain string
	}{
		{[]string{"tail", "codex", "--lines", "201", "--json"}, "run"},
		{[]string{"tail", "codex", "--json"}, "run"},
		{[]string{"report", "show", "ABSENT", "--json"}, "report"},
		{[]string{"report", "show", "READ", "--section", "BOGUS", "--json"}, "report"},
	} {
		code, v := judgeRead(t, config, store, tc.args...)
		if code != 2 || v["mode"] != tc.domain || v["state_domain"] != tc.domain {
			t.Fatal("failure used another command domain")
		}
	}
}

func TestJudgeReviewMissingSectionFacts(t *testing.T) {
	c, store := fixture(t)
	config := laneCLIConfig(t, c)
	text := judgeReportText("READ", "QUESTIONS: none", "")
	if os.WriteFile(filepath.Join(c.ReportsDir, "READ.txt"), []byte(text), 0600) != nil {
		t.Fatal("seed failed")
	}
	for _, tc := range []struct{ section, code string }{{"CONSENSUS", "consensus_missing"}, {"IDS", "section_absent"}} {
		code, v := judgeRead(t, config, store, "report", "show", "READ", "--section", tc.section, "--json")
		data := v["data"].(map[string]any)
		if code != 2 || v["error"].(map[string]any)["code"] != tc.code || data["incomplete"] != false {
			t.Fatal("absence was mislabeled as an incomplete parse")
		}
	}
}

func TestJudgeReviewEndMarkerRetainsBody(t *testing.T) {
	c, store := fixture(t)
	config := laneCLIConfig(t, c)
	for _, marker := range []string{" end of report for OTHER", "end of report for OTHER"} {
		text := judgeReportText("READ", "ITEMS: first\n"+marker+"\nsecond line\nQUESTIONS: none", "CONSENSUS: AGREE\n")
		if os.WriteFile(filepath.Join(c.ReportsDir, "READ.txt"), []byte(text), 0600) != nil {
			t.Fatal("seed failed")
		}
		code, v := judgeRead(t, config, store, "report", "show", "READ", "--section", "ITEMS", "--json")
		data := v["data"].(map[string]any)
		body := data["blocks"].([]any)[0].(string)
		if !strings.Contains(body, marker+"\nsecond line") {
			t.Fatal("body marker silently cut source text")
		}
		if strings.HasPrefix(marker, " ") && code != 0 {
			t.Fatal("indented body text became a boundary")
		}
		if !strings.HasPrefix(marker, " ") && code != 2 {
			t.Fatal("nonterminal boundary was silently accepted")
		}
	}
}
