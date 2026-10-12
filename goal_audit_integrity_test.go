package nuntius

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func nativeAuditIntegrityFixture(t *testing.T) (*Manager, []byte) {
	t.Helper()
	m, req := goalCommitFixture(t)
	req.Seal = []byte("private-spec-sentinel")
	if _, _, err := m.commitGoal(req, goalCommitIO{}); err != nil {
		t.Fatal("fixture commit failed")
	}
	if _, err := m.mirrorNativeAudit("web", goalCommitIO{}); err != nil {
		t.Fatal("fixture mirror failed")
	}
	b, err := os.ReadFile(filepath.Join(m.store, "audit.jsonl"))
	if err != nil {
		t.Fatal("fixture read failed")
	}
	return m, b
}

func replaceNativeAuditField(t *testing.T, b []byte, key, value string) []byte {
	t.Helper()
	var fields map[string]json.RawMessage
	if json.Unmarshal(b, &fields) != nil {
		t.Fatal("fixture decode failed")
	}
	needle := []byte(`"` + key + `":` + string(fields[key]))
	if !bytes.Contains(b, needle) {
		t.Fatal("fixture field missing")
	}
	return bytes.Replace(b, needle, []byte(`"`+key+`":`+value), 1)
}

func TestNativeAuditIntegrityLoss(t *testing.T) {
	for _, kind := range []string{"newline-only", "legacy-only", "damaged-source", "different-valid-event", "complete-line-loss", "last-line-loss", "wrong-goal", "wrong-sequence", "empty-file", "missing-file"} {
		t.Run(kind, func(t *testing.T) {
			m, first := nativeAuditIntegrityFixture(t)
			replacement := []byte("\n")
			switch kind {
			case "legacy-only":
				replacement = []byte(`{"time":"2026-10-12T00:00:00Z","tool":"future_action","result":"ok"}` + "\n")
			case "damaged-source":
				replacement = replaceNativeAuditField(t, first, "source", `"bad"`)
			case "different-valid-event":
				replacement = replaceNativeAuditField(t, first, "event_id", `"native-goal-`+strings.Repeat("0", 64)+`"`)
			case "wrong-goal":
				replacement = replaceNativeAuditField(t, first, "goal_id", `"OTHER-GOAL"`)
			case "wrong-sequence":
				replacement = replaceNativeAuditField(t, first, "sequence", `2`)
			case "empty-file", "missing-file":
				replacement = nil
			case "complete-line-loss", "last-line-loss":
				_, req := goalCommitFixture(t)
				req.OperationID, req.ExpectedRevision = "operation-2", 1
				if _, _, err := m.commitGoal(req, goalCommitIO{}); err != nil {
					t.Fatal("second fixture commit failed")
				}
				if _, err := m.mirrorNativeAudit("cli", goalCommitIO{}); err != nil {
					t.Fatal("second fixture mirror failed")
				}
				all, _ := os.ReadFile(filepath.Join(m.store, "audit.jsonl"))
				replacement = bytes.Clone(all[len(first):]) // Lose an earlier whole line, keep the latest receipt's row.
				if kind == "last-line-loss" {
					replacement, first = first, bytes.Clone(all[len(first):])
				}
			}
			path := filepath.Join(m.store, "audit.jsonl")
			if os.WriteFile(path, replacement, 0600) != nil {
				t.Fatal("fixture replacement failed")
			}
			if kind == "missing-file" && os.Remove(path) != nil {
				t.Fatal("fixture removal failed")
			}
			before := goalCommitTree(t, m.store)
			if state, err := m.nativeAuditStatus(); err == nil && state.Available {
				t.Error("receipt certified missing or damaged delivery")
			} else if state.Explanation == "" {
				t.Error("missing delivery had no plain explanation")
			}
			root, err := openLaneControlRoot(m.store)
			if err != nil {
				t.Fatal("fixture root failed")
			}
			if requireNativeAuditLocked(root, goalCommitIO{}) == nil {
				t.Error("admission certified missing or damaged delivery")
			}
			root.Close()
			if !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
				t.Error("observation changed history")
			}
			state, err := m.mirrorNativeAudit("mcp", goalCommitIO{})
			after, _ := os.ReadFile(path)
			if !bytes.HasPrefix(after, replacement) {
				t.Fatal("reconciliation rewrote raw history")
			}
			if err != nil || !state.Available {
				t.Fatal("reconciliation did not restore missing delivery")
			}
			appended := parseAudit(after[len(replacement):], time.Time{})
			if len(appended) != 1 || appended[0].EventID != parseAudit(first, time.Time{})[0].EventID {
				t.Fatal("reconciliation did not append only the missing required event")
			}
			if _, err := m.mirrorNativeAudit("cli", goalCommitIO{}); err != nil {
				t.Fatal("confirmed replay failed")
			}
			again, _ := os.ReadFile(path)
			if !bytes.Equal(after, again) {
				t.Fatal("confirmed replay appended another row")
			}
			if state, err := m.nativeAuditStatus(); err != nil || !state.Available || state.Pending != 0 {
				t.Fatal("restored delivery was not healthy")
			}
		})
	}
}

func TestNativeAuditIntegrityEnvelope(t *testing.T) {
	_, row := nativeAuditIntegrityFixture(t)
	for _, action := range []string{`null`, `""`, `"future_action"`} {
		for _, violation := range []string{"action", "body", "duplicate", "escaped-duplicate", "coerced", "marker-null", "escaped-marker"} {
			t.Run(action+"/"+violation, func(t *testing.T) {
				bad := replaceNativeAuditField(t, row, "tool", action)
				switch violation {
				case "body":
					bad = append([]byte(`{"body":"private-spec-sentinel",`), bad[1:]...)
				case "duplicate":
					bad = append([]byte(`{"sequence":1,`), bad[1:]...)
				case "escaped-duplicate":
					bad = append([]byte(`{"sequen\u0063e":1,`), bad[1:]...)
				case "coerced":
					bad = replaceNativeAuditField(t, bad, "sequence", `"1"`)
				case "marker-null":
					bad = replaceNativeAuditField(t, bad, "native_audit_version", `null`)
				case "escaped-marker":
					bad = bytes.Replace(bad, []byte(`"native_audit_version"`), []byte(`"native_audit_vers\u0069on"`), 1)
				}
				if len(parseAudit(bad, time.Time{})) != 0 {
					t.Fatal("malformed native envelope entered public audit presentation")
				}
			})
		}
	}
	legacy := []byte(`{"time":"2026-10-12T00:00:00Z","tool":"future_action","result":"ok"}` + "\n")
	if len(parseAudit(append(bytes.Clone(legacy), legacy...), time.Time{})) != 2 {
		t.Fatal("genuine unknown-action legacy rows were lost")
	}
}

func TestNativeAuditIntegrityBoundsAndLegacy(t *testing.T) {
	for _, kind := range []string{"whole-file-limit", "row-limit", "torn-line", "legacy-crlf"} {
		t.Run(kind, func(t *testing.T) {
			m, row := nativeAuditIntegrityFixture(t)
			path := filepath.Join(m.store, "audit.jsonl")
			switch kind {
			case "whole-file-limit":
				if os.Truncate(path, (64<<20)+1) != nil {
					t.Fatal("fixture size failed")
				}
			case "row-limit":
				if os.WriteFile(path, append([]byte(strings.Repeat(" ", (16<<10)+1)+"\n"), row...), 0600) != nil {
					t.Fatal("fixture row failed")
				}
			case "torn-line":
				if os.WriteFile(path, append(bytes.Clone(row), []byte(`{"tool":`)...), 0600) != nil {
					t.Fatal("fixture prefix failed")
				}
			case "legacy-crlf":
				legacy := []byte(`{"time":"2026-10-12T00:00:00Z","tool":"future_action","result":"ok"}` + "\r\n")
				if os.WriteFile(path, append(legacy, row...), 0600) != nil {
					t.Fatal("fixture legacy row failed")
				}
			}
			if kind == "legacy-crlf" {
				before, _ := os.ReadFile(path)
				if state, err := m.nativeAuditStatus(); err != nil || !state.Available {
					t.Fatal("genuine legacy line endings broke delivery")
				}
				if _, err := m.mirrorNativeAudit("cli", goalCommitIO{}); err != nil {
					t.Fatal("legacy control mirror failed")
				}
				after, _ := os.ReadFile(path)
				if !bytes.Equal(before, after) || len(parseAudit(after, time.Time{})) != 2 {
					t.Fatal("genuine legacy row was changed or hidden")
				}
				return
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal("fixture observation failed")
			}
			if state, err := m.nativeAuditStatus(); err == nil || state.Available || state.Explanation == "" {
				t.Error("unchecked history was certified healthy")
			}
			if state, err := m.mirrorNativeAudit("cli", goalCommitIO{}); err == nil || state.Available {
				t.Error("unsafe history was repaired or certified healthy")
			}
			after, _ := os.Stat(path)
			if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
				t.Error("refused history changed")
			}
		})
	}
}
