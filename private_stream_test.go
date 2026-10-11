package nuntius

import (
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Ported from the independent release review's T2 and T3 fixtures.
func TestReleasePrivateVariants(t *testing.T) {
	body := "synthetic-release-review-body"
	for _, tc := range []struct {
		name  string
		texts []string
	}{
		{"short-delimiter-fragments", []string{"--", "--", "-BEGIN PRIVATE KEY", "--", "--", "-", body}},
		{"nested-open-block", []string{"-----BEGIN PRIVATE KEY-----", "-----BEGIN RSA PRIVATE KEY-----", "-----END RSA PRIVATE KEY-----", body}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, c, store := manager(t)
			seedHistory(t, m, 1)
			r, err := m.latest("claude")
			if err != nil || r == nil {
				t.Fatal("run fixture failed")
			}
			if os.WriteFile(filepath.Join(c.ReportsDir, r.GoalID+".txt"), []byte("done"), 0600) != nil {
				t.Fatal("report fixture failed")
			}
			path := filepath.Join(store, "runs", r.ID, "events.jsonl")
			var data []byte
			for _, text := range tc.texts {
				data = append(data, privateMessage(text)...)
			}
			if os.WriteFile(path, data, 0600) != nil {
				t.Fatal("events fixture failed")
			}
			assertPrivateOutputs(t, m, path, body)
		})
	}
}

func TestReleaseSplitTerminal(t *testing.T) {
	c, store := opencodeFixture(t)
	m, err := NewManager(c, store)
	if err != nil {
		t.Fatal("terminal manager fixture failed")
	}
	t.Cleanup(func() { _ = m.Close() })
	body := "synthetic-release-terminal-body"
	text := terminalStart
	for _, fragment := range []string{"--", "--", "-BEGIN PRIVATE KEY", "--", "--", "-", body} {
		text += terminalText(fragment)
	}
	r := seedTerminalRun(t, m, text+terminalStop, "ended")
	terminal, final := m.terminalTurn(r)
	if !terminal || strings.Contains(final, body) {
		t.Fatal("split marker reached terminal fallback")
	}
}

func TestReleasePublicControl(t *testing.T) {
	m := &Manager{}
	var data []byte
	for _, text := range []string{"ordinary public prefix", "-----BEGIN PRIVATE KEY-----", "synthetic-control-body", "-----END PRIVATE KEY-----", "ordinary public suffix"} {
		data = append(data, privateMessage(text)...)
	}
	for range 2 {
		w := m.eventWindow("public-control", data, false)
		joined := strings.Join(w.tail, "\n")
		if !strings.Contains(joined, "ordinary public prefix") || !strings.Contains(joined, "ordinary public suffix") || strings.Contains(joined, "synthetic-control-body") {
			t.Fatal("normal public/private control failed")
		}
	}
	joinedMarker := strings.Join([]string{"--", "--", "-BEGIN PRIVATE KEY", "--", "--", "-"}, "")
	if joinedMarker != "-----BEGIN PRIVATE KEY-----" {
		t.Fatal("split marker control failed")
	}
	data = append(privateMessage(joinedMarker), privateMessage("synthetic-control-body")...)
	w := m.eventWindow("joined-control", data, false)
	if strings.Contains(w.final+strings.Join(w.tail, "\n"), "synthetic-control-body") {
		t.Fatal("reassembled marker control failed")
	}
}

func TestReleasePublicMarkdown(t *testing.T) {
	text := "Public progress\n---\nAll checks passed"
	if Redact(text) != text {
		t.Error("ordinary public markdown hidden by whole-text redaction")
	}
	m := &Manager{}
	w := m.eventWindow("markdown-control", privateMessage(text), false)
	// Tail's established contract selects the first physical line. The review's
	// third-line Tail oracle was incompatible with actionSummary on both heads.
	if strings.Join(w.tail, "\n") != "Public progress" || w.final != text {
		t.Error("ordinary public markdown hidden in parsed event output")
	}
}

func TestPrivateStreamAdversarial(t *testing.T) {
	body := "synthetic-stream-protected-body"
	for _, tc := range []struct {
		name  string
		texts []string
	}{
		{"single-byte-header", append(strings.Split("-----BEGIN EC PRIVATE KEY-----", ""), body)},
		{"displaced-short-dashes", []string{"public prefix -", "-", "BEGIN RSA PRIVATE KEY", "-", body}},
		{"partial-label", []string{"-----BEGIN OPENSSH PRI", body}},
		{"unknown-partial-label", []string{"-BEGIN CUSTOM PRI", body}},
		{"missing-direction-label", []string{"public prefix", "-CUSTOM PRIVATE KEY", body}},
		{"numeric-malformed-label", []string{"public prefix", "-123PRIVATE KEY", body}},
		{"partial-label-before-newline", []string{"-----BE\n" + body}},
		{"mismatched-label", []string{"-----BEGIN RSA PRIVATE KEY-----", "-----END EC PRIVATE KEY-----", body}},
		{"nested-complete", []string{"-----BEGIN EC PRIVATE KEY-----", "-----BEGIN PRIVATE KEY-----", "-----END PRIVATE KEY-----", body, "-----END EC PRIVATE KEY-----"}},
		{"lowercase-unicode-space", []string{"-\u2003-\t-", "--b e g i n\u00a0encrypted\nprivate\tkey--", body}},
		{"label-before-dashes", []string{"BEGIN RSA PRIVATE KEY--", body}},
		{"whitespace-only-fragment", []string{"--", "\u2003\t", "---", "\n", "B", "EGIN PRIVATE KEY-----", body}},
		{"one-dash-end", []string{body, "-END PRIVATE KEY-"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertMixedPrivateStream(t, tc.texts, body, tc.name == "one-dash-end")
		})
	}
}

// Exercise each append, digest reuse, full-window displacement and all served
// paths with a synthetic sentinel. Failure messages never contain fixture text.
func assertMixedPrivateStream(t *testing.T, texts []string, body string, orphan bool) {
	t.Helper()
	m, c, store := manager(t)
	seedHistory(t, m, 1)
	r, err := m.latest("claude")
	if err != nil || r == nil {
		t.Fatal("run fixture failed")
	}
	if os.WriteFile(filepath.Join(c.ReportsDir, r.GoalID+".txt"), []byte("done"), 0600) != nil {
		t.Fatal("report fixture failed")
	}
	path := filepath.Join(store, "runs", r.ID, "events.jsonl")
	var data []byte
	for i, text := range texts {
		// Escape every dash and selected letters. Detection must use decoded
		// string values, not the JSON spelling of the marker.
		line := privateMessage(text)
		if i%3 == 1 {
			line, _ = json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}})
			line = append(line, '\n')
		} else if i%3 == 2 {
			line, _ = json.Marshal(map[string]any{"type": "item.completed", "item": map[string]any{"type": "command_execution", "command": text}})
			line = append(line, '\n')
		}
		if i%2 == 0 {
			line = []byte(strings.ReplaceAll(strings.ReplaceAll(string(line), "-", `\u002d`), "B", `\u0042`))
		}
		data = append(data, line...)
		if os.WriteFile(path, data, 0600) != nil {
			t.Fatal("event fixture failed")
		}
		if !orphan || i == len(texts)-1 {
			assertPrivateOutputs(t, m, path, body)
		}
	}
	if strings.Contains(Redact(strings.Join(texts, "")), body) {
		t.Error("private body reached whole-text redaction")
	}
	// The real terminal reader uses the same incremental fragment scanner.
	c, terminalStore := opencodeFixture(t)
	terminalManager, err := NewManager(c, terminalStore)
	if err != nil {
		t.Fatal("terminal manager fixture failed")
	}
	t.Cleanup(func() { _ = terminalManager.Close() })
	terminal := terminalStart
	for _, text := range texts {
		terminal += terminalText(text)
	}
	terminalRun := seedTerminalRun(t, terminalManager, terminal+terminalStop, "ended")
	if stopped, final := terminalManager.terminalTurn(terminalRun); !stopped || strings.Contains(final, body) {
		t.Error("private body reached terminal fallback")
	}
	if rows, err := terminalManager.Tail(terminalRun.Lane, 50); err != nil || strings.Contains(strings.Join(rows, "\n"), body) {
		t.Error("private body reached terminal Tail")
	}
	// Move the header and any fragment history outside the retained tail.
	for len(data) <= 2<<20 {
		data = append(data, privateMessage("ordinary public padding")...)
	}
	data = append(data, privateMessage(body)...)
	if os.WriteFile(path, data, 0600) != nil {
		t.Fatal("displaced event fixture failed")
	}
	assertPrivateOutputs(t, m, path, body)
}

func TestPrivateStreamPublicMarkdown(t *testing.T) {
	for _, text := range []string{
		"# Public progress\n\n---\n\nAll checks passed",
		"```go\n// normal public code\n```\n~~~\npublic fenced text\n~~~",
		"> Public quote\n> ---\n> All checks passed",
		"<!-- Public comment -->\n<!-- BEGIN public section -->\nPublic conclusion",
		"-----\nPublic separator\n-----\nAll checks passed",
		"---",
		"- Public list\n- Another list item\n[public link](https://example.invalid)",
	} {
		if Redact(text) != text {
			t.Error("marker-free public Markdown changed")
		}
		m := &Manager{}
		for range 2 {
			w := m.eventWindow("public-markdown", privateMessage(text), false)
			if w.final != text {
				t.Error("marker-free public final changed")
			}
		}
		data := []byte(strings.Repeat(string(privateMessage(text)), 24000))
		if !publicEventPrefix(strings.NewReader(string(data)), int64(len(data))) {
			t.Error("marker-free public history classified private")
		}
	}
}

func TestPrivateStreamFragmentsWithinEvent(t *testing.T) {
	m, c, store := manager(t)
	seedHistory(t, m, 1)
	r, err := m.latest("claude")
	if err != nil || r == nil {
		t.Fatal("run fixture failed")
	}
	if os.WriteFile(filepath.Join(c.ReportsDir, r.GoalID+".txt"), []byte("done"), 0600) != nil {
		t.Fatal("report fixture failed")
	}
	var content []any
	for _, fragment := range strings.Split("-----BEGIN CUSTOM PRIVATE KEY-----", "") {
		content = append(content, map[string]any{"type": "text", "text": fragment})
	}
	data, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"content": content}})
	body := "synthetic-field-fragment-body"
	data = append(append(data, '\n'), privateMessage(body)...)
	path := filepath.Join(store, "runs", r.ID, "events.jsonl")
	if os.WriteFile(path, data, 0600) != nil {
		t.Fatal("event fixture failed")
	}
	assertPrivateOutputs(t, m, path, body)
}

func TestPrivateStreamRandom(t *testing.T) {
	random := rand.New(rand.NewSource(34))
	for i := range 24 {
		t.Run(string(rune('a'+i)), func(t *testing.T) {
			seed := make([]byte, 32)
			_, _ = random.Read(seed)
			texts, body := mixedPrivateStream(seed)
			assertMixedPrivateStream(t, texts, body, false)
		})
	}
}

func FuzzPrivateStream(f *testing.F) {
	for _, seed := range [][]byte{{}, {0}, {1, 2, 3}, {4, 5, 6}, {255, 128, 42}, []byte("fragmented-markers")} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, seed []byte) {
		if len(seed) > 64 {
			seed = seed[:64]
		}
		texts, body := mixedPrivateStream(seed)
		assertMixedPrivateStream(t, texts, body, false)
	})
}

// Fuzzer bytes choose label, casing, whitespace, nesting and every cut point.
// The private sentinel is separate from the generator bytes and never printed.
func mixedPrivateStream(seed []byte) ([]string, string) {
	body := "synthetic-fuzz-protected-body"
	choice := func(i int) byte {
		if len(seed) == 0 {
			return 0
		}
		return seed[i%len(seed)]
	}
	label := []string{"", "RSA ", "EC ", "DSA ", "OPENSSH ", "ENCRYPTED "}[int(choice(0))%6]
	header := "-----BEGIN " + label + "PRIVATE KEY-----"
	if choice(1)%2 == 0 {
		header = strings.ToLower(header)
	}
	if choice(2)%2 == 0 {
		header = strings.ReplaceAll(header, " ", "\t\u2003")
	}
	texts := []string{"# Public progress\n---\nPublic prefix"}
	remaining := []rune(header)
	for i := 0; len(remaining) > 0; i++ {
		n := min(len(remaining), 1+int(choice(3+i))%8)
		texts = append(texts, string(remaining[:n]))
		remaining = remaining[n:]
	}
	if choice(4)%2 == 0 {
		texts = append(texts, "-----BEGIN PRIVATE KEY-----", "-----END PRIVATE KEY-----")
	}
	texts = append(texts, body, "-----END "+label+"PRIVATE KEY-----", "Public suffix\n<!-- ordinary comment -->")
	return texts, body
}
