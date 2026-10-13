package nuntius

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEventCacheTailFreshnessAndGuards(t *testing.T) {
	m, c, store := manager(t)
	seedHistory(t, m, 1)
	r, err := m.latest("claude")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.ReportsDir, r.GoalID+".txt"), []byte("done"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store, "runs", r.ID, "events.jsonl")
	write := func(text string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	tail := func(want string) {
		t.Helper()
		rows, err := m.Tail("claude", 50)
		if err != nil || strings.Join(rows, "\n") != want {
			t.Fatal("cached tail freshness or redaction")
		}
	}
	first := `{"type":"item.completed","item":{"type":"command_execution","command":"go test ./aaa"}}` + "\n"
	second := strings.ReplaceAll(first, "aaa", "bbb")
	write(first)
	tail("go test ./aaa")
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	write(second)
	if err := os.Chtimes(path, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	tail("go test ./bbb") // Same size and timestamp, different contents.
	write(second + strings.TrimSuffix(first, "\n"))
	tail("go test ./bbb\n[REDACTED]") // Unfinished scope supplies only a fixed indicator.
	write(second + first)
	tail("go test ./bbb\ngo test ./aaa")
	write("")
	tail("") // Truncation must clear the old summaries.
	write(first)
	tail("go test ./aaa")
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Tail("claude", 50); err == nil {
		t.Fatal("cache bypassed private event permissions")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path+".original", path); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Tail("claude", 50); err == nil {
		t.Fatal("cache bypassed symlink refusal")
	}
}

func TestEventCacheWindowSemantics(t *testing.T) {
	m := &Manager{}
	data := []byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"-----BEGIN PRIVATE KEY-----"}]}}
{"type":"assistant","message":{"content":[{"type":"text","text":"synthetic material"}]}}
{"type":"assistant","message":{"content":[{"type":"text","text":"-----END PRIVATE KEY-----"}]}}
{"type":"item.started","item":{"type":"request_user_input"}}
{"type":"item.started","item":{"type":"command_execution","id":"cmd","command":"make verify"}}
`)
	for range 2 {
		w := m.eventWindow("semantics", data, false)
		if !w.asked || !w.commandRunning || w.latest.Summary != "make verify" || w.activity != "running tests" || strings.Contains(strings.Join(w.tail, "\n"), "synthetic material") {
			t.Fatal("cached question, command or private-block semantics")
		}
		for _, line := range w.lines {
			if strings.Contains(line.final, "synthetic material") {
				t.Fatal("private-block message retained by cache")
			}
			for _, a := range line.actions {
				if a.event.Text != "" || strings.Contains(a.event.Summary, "synthetic material") {
					t.Fatal("private-block action retained by cache")
				}
			}
		}
	}
	data = append(data, []byte(`{"type":"item.completed","item":{"type":"command_execution","id":"cmd","command":"make verify"}}
{"type":"item.completed","item":{"type":"agent_message","text":""}}
`)...)
	w := m.eventWindow("semantics", data, false)
	if w.commandRunning || w.final != "" {
		t.Fatal("completion or empty final retained previous state")
	}
	// A window that no longer contains a question cannot retain its asked bit.
	w = m.eventWindow("semantics", []byte(`{"type":"item.completed","item":{"type":"file_change","changes":[{"path":"example.go","kind":"update"}]}}`+"\n"), false)
	if w.asked || w.commandRunning || w.activity != "editing files" {
		t.Fatal("sliding window retained removed state")
	}
	for i := range 80 {
		m.eventWindow(fmt.Sprint(i), data, i%2 == 0)
	}
	if len(m.eventWindows) > 64 {
		t.Fatal("event cache exceeded window bound")
	}
}

func longActionHistory() ([]byte, []byte) {
	var b strings.Builder
	for i := range 6000 {
		fmt.Fprintf(&b, `{"type":"item.completed","item":{"type":"command_execution","id":"cmd-%d","command":"go test ./pkg/p%d"}}`+"\n", i, i)
	}
	a := []byte(b.String())
	// The bounded tail slides forward when the next event arrives.
	next := append(append([]byte{}, a[strings.IndexByte(string(a), '\n')+1:]...), []byte(`{"type":"item.completed","item":{"type":"command_execution","id":"new","command":"make verify"}}`+"\n")...)
	return a, next
}

func TestActionHistoryIncrementalAllocations(t *testing.T) {
	a, b := longActionHistory()
	m := &Manager{}
	now := time.Now()
	for _, data := range [][]byte{a, b} {
		m.currentAction("history", data, now, now)
	}
	i := 0
	allocs := testing.AllocsPerRun(5, func() {
		data := a
		if i%2 != 0 {
			data = b
		}
		i++
		if m.currentAction("history", data, now, now) == nil {
			t.Fatal("missing long-history action")
		}
	})
	t.Logf("allocations per sliding event window: %.0f", allocs)
	if allocs > 5000 {
		t.Fatal("a single new event reparses the long action history")
	}
}

func BenchmarkActionHistory(b *testing.B) {
	a, next := longActionHistory()
	for _, sliding := range []bool{false, true} {
		b.Run(fmt.Sprintf("sliding=%t", sliding), func(b *testing.B) {
			m := &Manager{}
			now := time.Now()
			m.currentAction("history", a, now, now)
			m.currentAction("history", next, now, now)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				data := a
				if sliding && i%2 != 0 {
					data = next
				}
				m.currentAction("history", data, now, now)
			}
		})
	}
}
