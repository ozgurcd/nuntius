package nuntius

import (
	"bytes"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrivateEOFReviewEOFControls(t *testing.T) {
	body := "synthetic-eof-review-body"
	for _, newline := range []bool{false, true} {
		name := "without-newline"
		if newline {
			name = "with-newline"
		}
		t.Run(name, func(t *testing.T) {
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
			data := append(append(privateMessage("---"), privateMessage(body)...), bytes.TrimSuffix(privateMessage("-----END PRIVATE KEY-----"), []byte("\n"))...)
			if newline {
				data = append(data, '\n')
			}
			// Full bytes correctly classify the orphan END even without a newline.
			for _, a := range streamActions(data) {
				if strings.Contains(a.Summary, body) {
					t.Fatal("direct classifier control failed")
				}
			}
			if e = os.WriteFile(p, data, 0600); e != nil {
				t.Fatal("event fixture")
			}
			window, e := readWindow(p, true)
			if e != nil {
				t.Fatal("read fixture")
			}
			var uncached bool
			for _, a := range streamActions(window) {
				uncached = uncached || strings.Contains(a.Summary, body)
			}
			for pass := 0; pass < 2; pass++ {
				rows, e := m.Tail("claude", 50)
				if e != nil {
					t.Fatal("tail fixture")
				}
				w, e := NewWeb(m, "127.0.0.1:0")
				if e != nil {
					t.Fatal("web fixture")
				}
				req := httptest.NewRequest("GET", "http://127.0.0.1:0/data", nil)
				req.Header.Set("X-Nuntius-Token", w.token)
				out := httptest.NewRecorder()
				w.ServeHTTP(out, req)
				if out.Code != 200 {
					t.Fatal("data request failed")
				}
				retained := false
				for _, v := range m.eventWindows {
					retained = retained || strings.Contains(v.final+strings.Join(v.tail, "\n")+v.latest.Summary, body)
					for _, line := range v.lines {
						retained = retained || strings.Contains(line.final, body)
					}
				}
				tailLeak := strings.Contains(strings.Join(rows, "\n"), body)
				dataLeak := strings.Contains(out.Body.String(), body)
				t.Logf("pass=%d tail=%t cache=%t uncached=%t data-status=%d data=%t marker-retained=%t", pass, tailLeak, retained, uncached, out.Code, dataLeak, bytes.Contains(window, []byte("END PRIVATE KEY")))
				if tailLeak || retained || uncached || dataLeak {
					t.Error("EOF marker synthetic body leaked")
				}
			}
			// Newline append must clear a previously warm leaking cache.
			if !newline {
				if e = os.WriteFile(p, append(data, '\n'), 0600); e != nil {
					t.Fatal("append fixture")
				}
				assertPrivateOutputs(t, m, p, body)
			}
		})
	}
}

func TestPrivateEOFReviewEOFTerminalFallback(t *testing.T) {
	body := "synthetic-eof-terminal-body"
	for _, newline := range []bool{false, true} {
		name := "without-newline"
		if newline {
			name = "with-newline"
		}
		t.Run(name, func(t *testing.T) {
			c, store := opencodeFixture(t)
			m, e := NewManager(c, store)
			if e != nil {
				t.Fatal("fixture")
			}
			t.Cleanup(func() { _ = m.Close() })
			// An incomplete terminal stream intentionally selects Tail's fallback.
			stream := terminalStart + terminalText(body) + strings.TrimSuffix(terminalText("-----END PRIVATE KEY-----"), "\n")
			if newline {
				stream += "\n"
			}
			r := seedTerminalRun(t, m, stream, "ended")
			terminal, final := m.terminalTurn(r)
			rows, e := m.Tail("opencode-1", 50)
			if e != nil {
				t.Fatal("Tail request failed")
			}
			leak := strings.Contains(strings.Join(rows, "\n"), body)
			t.Logf("terminal=%t terminal-body=%t fallback-body=%t", terminal, strings.Contains(final, body), leak)
			if strings.Contains(final, body) || leak {
				t.Error("EOF marker terminal fallback leaked")
			}
		})
	}
}
