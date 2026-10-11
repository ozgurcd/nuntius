package nuntius

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrivateEOFLifecycle(t *testing.T) {
	body := "synthetic-eof-lifecycle-body"
	open := append(privateMessage("-----BEGIN EC PRIVATE KEY-----"), bytes.TrimSuffix(privateMessage(body), []byte("\n"))...)
	for _, transition := range []string{"append", "rotate", "truncate"} {
		t.Run(transition, func(t *testing.T) {
			m, c, store := manager(t)
			seedHistory(t, m, 1)
			r, err := m.latest("claude")
			if err != nil || r == nil {
				t.Fatal("run fixture failed")
			}
			if os.WriteFile(filepath.Join(c.ReportsDir, r.GoalID+".txt"), []byte("done"), 0600) != nil {
				t.Fatal("report fixture failed")
			}
			p := filepath.Join(store, "runs", r.ID, "events.jsonl")
			closed := append(bytes.Clone(open), '\n')
			closed = append(closed, privateMessage("-----END EC PRIVATE KEY-----")...)
			closed = append(closed, privateMessage("ordinary public suffix")...)
			if os.WriteFile(p, closed, 0600) != nil {
				t.Fatal("closed fixture failed")
			}
			assertPrivateOutputs(t, m, p, body)
			switch transition {
			case "rotate":
				if os.Rename(p, p+".old") != nil || os.WriteFile(p, open, 0600) != nil {
					t.Fatal("rotation fixture failed")
				}
			case "truncate":
				if os.Truncate(p, int64(len(open))) != nil {
					t.Fatal("truncation fixture failed")
				}
			case "append":
				if os.WriteFile(p, open, 0600) != nil {
					t.Fatal("open fixture failed")
				}
			}
			assertPrivateOutputs(t, m, p, body)
			f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0600)
			if err != nil {
				t.Fatal("append fixture failed")
			}
			defer f.Close()
			// Keep the block open through real appends, then close it at EOF
			// without a newline. The same cache is checked after every byte.
			suffix := append([]byte{'\n'}, privateMessage(body)...)
			suffix = append(suffix, bytes.TrimSuffix(privateMessage("-----END EC PRIVATE KEY-----"), []byte("\n"))...)
			for _, b := range suffix {
				if _, err := f.Write([]byte{b}); err != nil {
					t.Fatal("byte append fixture failed")
				}
				assertPrivateOutputs(t, m, p, body)
			}
		})
	}
}

func TestPrivateEOFTerminalLifecycle(t *testing.T) {
	body := "synthetic-eof-terminal-lifecycle-body"
	open := terminalStart + terminalText("-----BEGIN PRIVATE KEY-----") + strings.TrimSuffix(terminalText(body), "\n")
	for _, transition := range []string{"append", "rotate", "truncate"} {
		t.Run(transition, func(t *testing.T) {
			c, store := opencodeFixture(t)
			m, err := NewManager(c, store)
			if err != nil {
				t.Fatal("terminal fixture failed")
			}
			t.Cleanup(func() { _ = m.Close() })
			r := seedTerminalRun(t, m, open+"\n"+terminalText("-----END PRIVATE KEY-----")+terminalStop, "ended")
			p := filepath.Join(store, "runs", r.ID, "events.jsonl")
			check := func() {
				_, final := m.terminalTurn(r)
				rows, err := m.Tail(r.Lane, 50)
				if err != nil || strings.Contains(final+strings.Join(rows, "\n"), body) {
					t.Fatal("terminal or fallback exposes body")
				}
			}
			check()
			switch transition {
			case "rotate":
				if os.Rename(p, p+".old") != nil || os.WriteFile(p, []byte(open), 0600) != nil {
					t.Fatal("terminal rotation failed")
				}
			case "truncate":
				if os.Truncate(p, int64(len(open))) != nil {
					t.Fatal("terminal truncation failed")
				}
			case "append":
				if os.WriteFile(p, []byte(open), 0600) != nil {
					t.Fatal("terminal open fixture failed")
				}
			}
			check()
			f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0600)
			if err != nil {
				t.Fatal("terminal append failed")
			}
			defer f.Close()
			for _, b := range []byte("\n" + terminalText(body) + terminalText("-----END PRIVATE KEY-----") + terminalStop) {
				if _, err := f.Write([]byte{b}); err != nil {
					t.Fatal("terminal byte append failed")
				}
				check()
			}
		})
	}
}

func TestPrivateEOFSuffixContext(t *testing.T) {
	body := "synthetic-eof-fragment-body"
	for _, suffix := range [][]byte{
		bytes.TrimSuffix(privateMessage("-----END PRIVATE KEY-----"), []byte("\n")),
		bytes.TrimSuffix(privateMessage("-----END PRI"), []byte("\n")),
		[]byte(`{"type":"text","part":{"text":"-----END PRI`),
	} {
		p := filepath.Join(t.TempDir(), "events.jsonl")
		if os.WriteFile(p, append(privateMessage(body), suffix...), 0600) != nil {
			t.Fatal("EOF context fixture failed")
		}
		for _, tail := range []bool{false, true} {
			b, err := readWindow(p, tail)
			if err != nil || strings.Contains((&Manager{}).eventWindow("eof", b, false).final, body) {
				t.Fatal("EOF context exposed body")
			}
		}
	}
	// Incomplete marker-free events still do not enter the parsing window.
	public := privateMessage("ordinary complete public event")
	p := filepath.Join(t.TempDir(), "events.jsonl")
	if os.WriteFile(p, append(bytes.Clone(public), []byte(`{"text":"public suffix`)...), 0600) != nil {
		t.Fatal("public EOF fixture failed")
	}
	b, err := readWindow(p, true)
	if err != nil || !bytes.Equal(b, public) {
		t.Fatal("public EOF parsing contract changed")
	}
}
