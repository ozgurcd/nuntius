package nuntius

import (
	"bytes"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Independent synthetic review; no real private material is used.
func TestPrivateEOFReviewShapes(t *testing.T) {
	body := "synthetic-review2-protected-body"
	many := []string{}
	for _, r := range "-----BEGIN ENCRYPTED PRIVATE KEY-----" {
		many = append(many, string(r))
	}
	many = append(many, body)
	shapes := []struct {
		name      string
		texts     []string
		noNewline bool
	}{
		{"unicode-lookalike-label", []string{"-----BEGIN PR\u0406VATE KEY-----", body}, false},
		{"unicode-whitespace-crlf", []string{"-----bEgIn\u2003RSA\r\nPRIVATE\u00a0KEY-----", body, "-----END RSA PRIVATE KEY-----"}, false},
		{"zero-width-label", []string{"-----BEGIN PRI\u200bVATE KEY-----", body}, false},
		{"code-fence-markers", []string{"```pem\n-----BEGIN PRIVATE KEY-----", body, "-----END PRIVATE KEY-----\n```"}, false},
		{"oversized-physical-event", []string{strings.Repeat("x", (1<<20)+64) + "-----BEGIN PRIVATE KEY-----", body}, false},
		{"one-character-events", many, false},
		{"orphan-end-last-file-bytes", []string{body, "-----END EC PRIVATE KEY-----"}, true},
		{"unknown-alphabetic-label", []string{"-----BEGIN SYNTHPRIVATEKEY-----", body, "-----END SYNTHPRIVATEKEY-----"}, false},
		{"mismatched-end-and-public-suffix", []string{"-----BEGIN EC PRIVATE KEY-----", "-----END RSA PRIVATE KEY-----", body, "public suffix"}, false},
		{"fragment-with-metadata-and-crlf", []string{"--", "\r\n", "--", "public metadata", "-", "BEGIN RSA PRI", "VATE KEY", "-----", body}, false},
	}
	for _, tc := range shapes {
		t.Run(tc.name, func(t *testing.T) {
			m, c, store := manager(t)
			seedHistory(t, m, 1)
			r, e := m.latest("claude")
			if e != nil || r == nil {
				t.Fatal("run fixture")
			}
			if e = os.WriteFile(filepath.Join(c.ReportsDir, r.GoalID+".txt"), []byte("done"), 0600); e != nil {
				t.Fatal("report fixture")
			}
			p := filepath.Join(store, "runs", r.ID, "events.jsonl")
			var data []byte
			for _, text := range tc.texts {
				data = append(data, privateMessage(text)...)
			}
			if tc.noNewline {
				data = bytes.TrimSuffix(data, []byte("\n"))
			}
			if e = os.WriteFile(p, data, 0600); e != nil {
				t.Fatal("event fixture")
			}
			assertPrivateOutputs(t, m, p, body)
			// Same shape through the real terminal parser and Tail fallback.
			config, terminalStore := opencodeFixture(t)
			tm, e := NewManager(config, terminalStore)
			if e != nil {
				t.Fatal("terminal fixture")
			}
			t.Cleanup(func() { _ = tm.Close() })
			stream := terminalStart
			for _, text := range tc.texts {
				stream += terminalText(text)
			}
			tr := seedTerminalRun(t, tm, stream+terminalStop, "ended")
			_, final := tm.terminalTurn(tr)
			rows, e := tm.Tail("opencode-1", 50)
			if e != nil {
				t.Fatal("terminal Tail fixture request failed")
			}
			if strings.Contains(final+strings.Join(rows, "\n"), body) {
				t.Error("terminal parser or fallback exposes body")
			}
		})
	}
}

func TestPrivateEOFReviewByteAppendsAndLaneIsolation(t *testing.T) {
	body := "synthetic-byte-review-body"
	m, c, store := manager(t)
	seedHistory(t, m, 1)
	r, e := m.latest("claude")
	if e != nil || r == nil {
		t.Fatal("run fixture")
	}
	if e = os.WriteFile(filepath.Join(c.ReportsDir, r.GoalID+".txt"), []byte("done"), 0600); e != nil {
		t.Fatal("report fixture")
	}
	p := filepath.Join(store, "runs", r.ID, "events.jsonl")
	data := append(privateMessage("-----BEGIN RSA PRIVATE KEY-----"), privateMessage(body)...)
	if e = os.WriteFile(p, nil, 0600); e != nil {
		t.Fatal("empty append fixture")
	}
	for _, b := range data {
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			t.Fatal("open append fixture")
		}
		_, e = f.Write([]byte{b})
		closeError := f.Close()
		if e != nil || closeError != nil {
			t.Fatal("append fixture")
		}
		// Warm the same cache through every partial JSON append.
		rows, e := m.Tail("claude", 50)
		if e != nil || strings.Contains(strings.Join(rows, "\n"), body) {
			t.Fatal("byte append Tail leak")
		}
		public := m.eventWindow("independent-public-run", privateMessage("public lane unchanged"), false)
		if public.final != "public lane unchanged" {
			t.Fatal("private lane hides public lane")
		}
	}
	assertPrivateOutputs(t, m, p, body)
	// Explicitly verify public lane remains served by cache after private suppression.
	if m.eventWindow("independent-public-run", privateMessage("public lane unchanged"), false).final != "public lane unchanged" {
		t.Fatal("lane cache isolation")
	}
}

func TestPrivateEOFReviewPublicMarkdownContract(t *testing.T) {
	for _, text := range []string{"Public progress\n---\nAll checks passed", "Public progress\r\n-----\r\nAll checks passed", "Public code\n```\n---\n```", "Public separator --- remains", "Public progress\n<!-- ordinary comment -->"} {
		if Redact(text) != text {
			t.Error("marker-free full text changed")
		}
		m := &Manager{}
		for range 2 {
			w := m.eventWindow("markdown", privateMessage(text), false)
			first := strings.Split(text, "\n")[0]
			first = strings.TrimSpace(first)
			if w.final != text || strings.Join(w.tail, "\n") != first {
				t.Error("full final or first-line summary changed")
			}
		}
	}
}

func TestPrivateEOFReviewLookalikeBoundaryLimit(t *testing.T) {
	// These are not the rule's ASCII boundaries. Record public classification;
	// do not claim a PEM private-block absence proof for them.
	for _, text := range []string{"\u2010\u2010\u2010\u2010\u2010BEGIN PRIVATE KEY\u2010\u2010\u2010\u2010\u2010", "-----B\u200bEGIN PR\u200bIVATE KEY-----"} {
		b, e, u := privateMarkers(text)
		t.Logf("lookalike recognized=%t", b || e || u)
	}
}

func TestPrivateEOFReviewMarkerLikePublicProse(t *testing.T) {
	text := "Public documentation: -----BEGIN is a header example."
	hidden := Redact(text) != text
	t.Logf("marker-like-public-prose-hidden=%t", hidden)
	if !hidden {
		t.Fatal("recorded conservative classification changed")
	}
}

func TestPrivateEOFReviewLastMarkerTailData(t *testing.T) {
	m, c, store := manager(t)
	seedHistory(t, m, 1)
	r, e := m.latest("claude")
	if e != nil || r == nil {
		t.Fatal("fixture")
	}
	_ = os.WriteFile(filepath.Join(c.ReportsDir, r.GoalID+".txt"), []byte("done"), 0600)
	p := filepath.Join(store, "runs", r.ID, "events.jsonl")
	body := "synthetic-last-marker-body"
	data := append(privateMessage(body), bytes.TrimSuffix(privateMessage("-----END PRIVATE KEY-----"), []byte("\n"))...)
	if e = os.WriteFile(p, data, 0600); e != nil {
		t.Fatal("fixture")
	}
	assertPrivateOutputs(t, m, p, body)
	w, e := NewWeb(m, "127.0.0.1:0")
	if e != nil {
		t.Fatal("fixture")
	}
	req := httptest.NewRequest("GET", "http://127.0.0.1:0/data", nil)
	req.Header.Set("X-Nuntius-Token", w.token)
	out := httptest.NewRecorder()
	w.ServeHTTP(out, req)
	if out.Code != 200 || strings.Contains(out.Body.String(), body) {
		t.Fatal("last marker data leak")
	}
}
