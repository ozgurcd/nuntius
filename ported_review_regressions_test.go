package nuntius

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewBaselineCollision(t *testing.T) {
	_, body := reviewKey(t)
	m, _, store := manager(t)
	seedHistory(t, m, 1)
	r, _ := m.latest("claude")
	data := []byte(`{"type":"text","part/text":"-----BEGIN PRIVATE KEY-----","part":{"text":"-----END PRIVATE KEY-----"}}` + "\n")
	data = append(data, privateMessage(body)...)
	if os.WriteFile(filepath.Join(store, "runs", r.ID, "events.jsonl"), data, 0600) != nil {
		t.Fatal("B-J04 fixture")
	}
	rows, e := m.Tail("claude", 200)
	if e != nil {
		t.Fatal("B-J04 entrance")
	}
	exposed := strings.Contains(strings.Join(rows, "\n"), body)
	t.Logf("B-J04/tail exposed=%t", exposed)
	if exposed {
		t.Error("B-J04/no-key=false")
	}
}
func TestReviewBaselineTerminalRewind(t *testing.T) {
	_, body := reviewKey(t)
	c, store := opencodeFixture(t)
	m, e := NewManager(c, store)
	if e != nil {
		t.Fatal("B-T01 fixture")
	}
	defer m.Close()
	events := terminalStart + terminalText("--") + terminalText(body) + `{"type":"tool_use","part":{"type":"tool","metadata":"BEGIN PRIVATE KEY"}}` + "\n" + terminalStop
	r := seedTerminalRun(t, m, events, "ended")
	ended, final := m.terminalTurn(r)
	if !ended {
		t.Fatal("B-T01 lifecycle-positive=false")
	}
	exposed := strings.Contains(final, body)
	t.Logf("B-T01/terminal exposed=%t", exposed)
	if exposed {
		t.Error("B-T01/no-key=false")
	}
}
