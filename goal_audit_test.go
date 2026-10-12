package nuntius

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestNativeAuditMirror(t *testing.T) {
	t.Run("pending-and-safe-row", func(t *testing.T) {
		m, req := goalCommitFixture(t)
		req.Seal = []byte("private fixture body report and session material")
		if _, _, err := m.commitGoal(req, goalCommitIO{}); err != nil {
			t.Fatal("fixture commit failed")
		}
		before := goalCommitTree(t, m.store)
		state, err := m.nativeAuditStatus()
		if err != nil || state.Sequence != 1 || state.Pending != 1 || state.Available || state.Explanation == "" {
			t.Fatal("unmirrored committed event was not visible and held")
		}
		if !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
			t.Fatal("audit status mutated private state")
		}
		state, err = m.mirrorNativeAudit("mcp", goalCommitIO{})
		if err != nil || !state.Available || state.Mirrored != 1 || state.Pending != 0 {
			t.Fatal("mirror did not durably acknowledge committed event")
		}
		b, err := os.ReadFile(filepath.Join(m.store, "audit.jsonl"))
		if err != nil {
			t.Fatal("mirror row missing")
		}
		for _, forbidden := range []string{"private fixture", "report", "session", "credential", m.store, "goal-spec-", "goal-seal-", "operation-1", "Review documentation"} {
			if bytes.Contains(b, []byte(forbidden)) {
				t.Fatal("mirror exposed private material")
			}
		}
		rows := parseAudit(b, time.Time{})
		if len(rows) != 1 || rows[0].Action != "native_goal_spec" || rows[0].ReadOnly || rows[0].Summary != "Saved native goal EXAMPLE-DOCS" || rows[0].Source != "mcp" || rows[0].Sequence != 1 || rows[0].Revision != 1 || !validNativeAuditEventID(rows[0].EventID) {
			t.Fatal("native audit presentation was not safe and readable")
		}
		if !strings.Contains(string(b), `"source":"mcp"`) || !strings.Contains(string(b), `"event_id":"native-goal-`) {
			t.Fatal("trusted source or replay identity missing")
		}
		first := bytes.Clone(b)
		if _, err = (&Manager{store: m.store}).mirrorNativeAudit("cli", goalCommitIO{}); err != nil {
			t.Fatal("restart reconciliation failed")
		}
		b, _ = os.ReadFile(filepath.Join(m.store, "audit.jsonl"))
		if !bytes.Equal(first, b) {
			t.Fatal("confirmed replay appended another row")
		}
	})
	for _, stage := range []string{"journal_sync", "journal_directory_sync", "native_intent.write", "native_intent.fsync", "native_intent.rename", "native_intent.renamed", "native_intent.dirsync", "native_mirror.append", "native_mirror.fsync", "native_mirror.dirsync", "native_receipt.write", "native_receipt.fsync", "native_receipt.rename"} {
		t.Run(stage, func(t *testing.T) {
			m, req := goalCommitFixture(t)
			if _, _, err := m.commitGoal(req, goalCommitIO{}); err != nil {
				t.Fatal("fixture commit failed")
			}
			hit := false
			state, err := m.mirrorNativeAudit("web", goalCommitIO{checkpoint: func(at string) error {
				if at == stage {
					hit = true
					return errors.New("injected mirror failure")
				}
				return nil
			}})
			if !hit || err == nil || state.Available {
				t.Fatal("fault falsely acknowledged required audit durability")
			}
			state, err = (&Manager{store: m.store}).nativeAuditStatus()
			if err != nil || state.Pending != 1 || state.Available {
				t.Fatal("failed mirror lost pending state on restart")
			}
			state, err = (&Manager{store: m.store}).mirrorNativeAudit("cli", goalCommitIO{})
			if err != nil || !state.Available {
				t.Fatal("restart did not replay pending mirror")
			}
			b, _ := os.ReadFile(filepath.Join(m.store, "audit.jsonl"))
			rows := parseAudit(b, time.Time{})
			if len(rows) != 1 {
				t.Fatal("consumer did not deduplicate mirror redelivery")
			}
			if strings.HasPrefix(stage, "native_") && stage != "native_intent.write" && stage != "native_intent.fsync" && stage != "native_intent.rename" && !bytes.Contains(b, []byte(`"source":"web"`)) {
				t.Fatal("restart changed persisted adapter source")
			}
		})
	}
	t.Run("journal-authority-and-generic-exclusion", func(t *testing.T) {
		m, req := goalCommitFixture(t)
		if _, err := m.mirrorNativeAudit("caller-supplied-owner", goalCommitIO{}); err == nil {
			t.Fatal("untrusted adapter source accepted")
		}
		if _, _, err := m.commitGoal(req, goalCommitIO{}); err != nil {
			t.Fatal("fixture commit failed")
		}
		if err := m.audit("native_goal_spec", "", "EXAMPLE-DOCS", "", "updated"); err == nil {
			t.Fatal("generic MCP seam could counterfeit a journalled outcome")
		}
		if _, err := os.Stat(filepath.Join(m.store, "audit.jsonl")); !os.IsNotExist(err) {
			t.Fatal("generic outcome created a duplicate native row")
		}
		if _, err := m.mirrorNativeAudit("mcp", goalCommitIO{}); err != nil {
			t.Fatal("mirror failed")
		}
		before := goalCommitTree(t, m.store)
		if err := os.WriteFile(filepath.Join(m.store, goalJournalName), []byte("corrupt"), 0600); err != nil {
			t.Fatal("fixture corruption failed")
		}
		if state, err := m.nativeAuditStatus(); err == nil || state.Available {
			t.Fatal("audit receipt replaced journal authority")
		}
		if state, err := m.mirrorNativeAudit("mcp", goalCommitIO{}); err == nil || state.Available {
			t.Fatal("mirror bypassed corrupt authority")
		}
		if got := goalCommitTree(t, m.store); got[filepath.Join(m.store, "audit.jsonl")] != before[filepath.Join(m.store, "audit.jsonl")] {
			t.Fatal("invalid history emitted mirror rows")
		}
	})
	for _, stage := range []string{"native_receipt.renamed", "native_receipt.dirsync"} {
		t.Run(stage, func(t *testing.T) {
			m, req := goalCommitFixture(t)
			if _, _, err := m.commitGoal(req, goalCommitIO{}); err != nil {
				t.Fatal("fixture commit failed")
			}
			state, err := m.mirrorNativeAudit("web", goalCommitIO{checkpoint: func(at string) error {
				if at == stage {
					return errors.New("receipt uncertainty")
				}
				return nil
			}})
			if err == nil || state.Available {
				t.Fatal("failed receipt durability was reported confirmed")
			}
			first, _ := os.ReadFile(filepath.Join(m.store, "audit.jsonl"))
			state, err = (&Manager{store: m.store}).mirrorNativeAudit("mcp", goalCommitIO{})
			if err != nil || !state.Available {
				t.Fatal("visible receipt was not reconfirmed after restart")
			}
			again, _ := os.ReadFile(filepath.Join(m.store, "audit.jsonl"))
			if !bytes.Equal(first, again) {
				t.Fatal("reconfirming receipt needlessly repeated audit")
			}
		})
	}
	t.Run("only-committed-prefix-and-source-refusal", func(t *testing.T) {
		m, req := goalCommitFixture(t)
		spec := nativeSchemaFixture()
		spec["source"] = "mcp"
		spoof := req
		spoof.Spec = nativeSchemaBytes(t, spec)
		if _, _, err := m.commitGoal(spoof, goalCommitIO{}); err == nil {
			t.Fatal("caller JSON selected native audit source")
		}
		if _, _, err := m.commitGoal(req, goalCommitIO{}); err != nil {
			t.Fatal("fixture commit failed")
		}
		req.OperationID, req.ExpectedRevision = "operation-2", 1
		if _, _, err := m.commitGoal(req, goalCommitIO{checkpoint: func(at string) error {
			if at == "partial_write" {
				return errors.New("torn tail")
			}
			return nil
		}}); err == nil {
			t.Fatal("torn write claimed success")
		}
		prefix, _ := os.ReadFile(filepath.Join(m.store, goalJournalName))
		if state, err := m.mirrorNativeAudit("cli", goalCommitIO{}); err != nil || state.Sequence != 1 || state.Mirrored != 1 {
			t.Fatal("uncommitted tail created a native audit event")
		}
		after, _ := os.ReadFile(filepath.Join(m.store, goalJournalName))
		if !bytes.Equal(prefix, after) {
			t.Fatal("audit mirror repaired journal history")
		}
	})
	t.Run("crash-after-append-before-receipt", testNativeAuditCrash)
	t.Run("admission-and-control-independence", testNativeAuditAdmission)
	t.Run("private-state-and-consumer-validation", testNativeAuditSafety)
}

func TestNativeAuditMirrorProcessHelper(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "b04-crash" {
		return
	}
	fault := goalCommitIO{checkpoint: func(stage string) error {
		if stage == "native_receipt.write" {
			fmt.Println("ready")
			var signal [1]byte
			_, err := io.ReadFull(os.Stdin, signal[:])
			return err
		}
		return nil
	}}
	if _, err := (&Manager{store: os.Args[len(os.Args)-1]}).mirrorNativeAudit("web", fault); err != nil {
		t.Fatal("fixture mirror failed")
	}
}

func testNativeAuditCrash(t *testing.T) {
	m, req := goalCommitFixture(t)
	if _, _, err := m.commitGoal(req, goalCommitIO{}); err != nil {
		t.Fatal("fixture commit failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestNativeAuditMirrorProcessHelper$", "b04-crash", m.store)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal("fixture pipe failed")
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal("fixture pipe failed")
	}
	start := time.Now().UTC()
	if cmd.Start() != nil {
		t.Fatal("fixture start failed")
	}
	waited := false
	t.Cleanup(func() {
		stdin.Close()
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		if !goalTestProcessEnded(cmd) {
			t.Error("offline crash child was not reaped inside test")
		}
		t.Logf("offline B04 child start=%s end=%s teardown verified", start.Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano))
	})
	if line, _ := bufio.NewReader(stdout).ReadString('\n'); line != "ready\n" {
		t.Fatal("child did not reach mirror append crash barrier")
	}
	// The shared lock, not in-memory ownership, protects independent writers.
	if state, err := (&Manager{store: m.store}).mirrorNativeAudit("mcp", goalCommitIO{}); err == nil || state.Available {
		t.Fatal("another process bypassed mirror control lock")
	}
	if cmd.Process.Kill() != nil {
		t.Fatal("fixture crash failed")
	}
	_ = cmd.Wait()
	waited = true
	stdin.Close()
	fresh := &Manager{store: m.store}
	state, err := fresh.nativeAuditStatus()
	if err != nil || state.Pending != 1 || state.Available {
		t.Fatal("crash lost pending delivery")
	}
	first, _ := os.ReadFile(filepath.Join(m.store, "audit.jsonl"))
	if bytes.Count(first, []byte("\n")) != 1 {
		t.Fatal("crash barrier did not follow a durable mirror append")
	}
	if state, err = fresh.mirrorNativeAudit("mcp", goalCommitIO{}); err != nil || !state.Available {
		t.Fatal("restart did not replay unreceipted row")
	}
	all, _ := os.ReadFile(filepath.Join(m.store, "audit.jsonl"))
	rows := parseAudit(all, time.Time{})
	if bytes.Count(all, []byte("\n")) != 2 || len(rows) != 1 || rows[0].Source != "web" || !bytes.Equal(all, append(bytes.Clone(first), first...)) {
		t.Fatal("at-least-once replay or stable consumer identity failed")
	}
}

func testNativeAuditAdmission(t *testing.T) {
	m, req := goalCommitFixture(t)
	control := []byte("independent A control fixture")
	if err := os.WriteFile(filepath.Join(m.store, "lane-control-intent.json"), control, 0600); err != nil {
		t.Fatal("fixture write failed")
	}
	if _, _, err := m.commitGoal(req, goalCommitIO{}); err != nil {
		t.Fatal("fixture commit failed")
	}
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		t.Fatal("fixture root failed")
	}
	defer root.Close()
	lock, err := laneControlLock(root, unix.LOCK_EX)
	if err != nil {
		t.Fatal("fixture lock failed")
	}
	if requireNativeAuditLocked(root, goalCommitIO{}) == nil {
		t.Fatal("unmirrored event allowed native admission")
	}
	lock.Close()
	if _, err := m.mirrorNativeAudit("native", goalCommitIO{}); err != nil {
		t.Fatal("mirror failed")
	}
	lock, err = laneControlLock(root, unix.LOCK_EX)
	if err != nil {
		t.Fatal("fixture lock failed")
	}
	defer lock.Close()
	if requireNativeAuditLocked(root, goalCommitIO{}) != nil {
		t.Fatal("confirmed mirror was not admission-ready")
	}
	for _, at := range []string{"native_confirm." + goalJournalName, "native_confirm.audit.jsonl", "native_confirm." + nativeAuditIntentName, "native_confirm." + nativeAuditReceiptName, "native_confirm.dirsync"} {
		if requireNativeAuditLocked(root, goalCommitIO{checkpoint: func(stage string) error {
			if stage == at {
				return errors.New("sync failure")
			}
			return nil
		}}) == nil {
			t.Fatal("uncertain audit durability allowed admission")
		}
	}
	got, err := os.ReadFile(filepath.Join(m.store, "lane-control-intent.json"))
	if err != nil || !bytes.Equal(got, control) {
		t.Fatal("native mirror modified independent lane intent")
	}
	lock.Close()
	req.OperationID, req.ExpectedRevision = "operation-2", 1
	if _, _, err = m.commitGoal(req, goalCommitIO{}); err != nil {
		t.Fatal("next journal revision failed")
	}
	if state, err := m.nativeAuditStatus(); err != nil || state.Sequence != 2 || state.Mirrored != 1 || state.Pending != 1 || state.Available {
		t.Fatal("new revision reused earlier mirror readiness")
	}
	if state, err := m.mirrorNativeAudit("cli", goalCommitIO{}); err != nil || state.Mirrored != 2 || !state.Available {
		t.Fatal("second event did not mirror separately")
	}
	if err := os.WriteFile(filepath.Join(m.store, "audit.jsonl"), nil, 0600); err != nil {
		t.Fatal("fixture truncation failed")
	}
	if state, err := m.nativeAuditStatus(); err == nil || state.Available {
		t.Fatal("missing delivery bytes were silently considered healthy")
	}
}

func testNativeAuditSafety(t *testing.T) {
	for _, target := range []string{nativeAuditIntentName, nativeAuditReceiptName, "audit.jsonl"} {
		for _, kind := range []string{"symlink", "hardlink", "public", "directory", "corrupt"} {
			t.Run(target+"-"+kind, func(t *testing.T) {
				m, req := goalCommitFixture(t)
				if _, _, err := m.commitGoal(req, goalCommitIO{}); err != nil {
					t.Fatal("fixture commit failed")
				}
				path := filepath.Join(m.store, target)
				outside := filepath.Join(t.TempDir(), "sentinel")
				if os.WriteFile(outside, []byte("unchanged"), 0600) != nil {
					t.Fatal("sentinel failed")
				}
				switch kind {
				case "symlink":
					if os.Symlink(outside, path) != nil {
						t.Fatal("fixture link failed")
					}
				case "hardlink":
					if os.Link(outside, path) != nil {
						t.Fatal("fixture link failed")
					}
				case "public":
					if os.WriteFile(path, []byte("{}\n"), 0644) != nil {
						t.Fatal("fixture write failed")
					}
				case "directory":
					if os.Mkdir(path, 0700) != nil {
						t.Fatal("fixture directory failed")
					}
				case "corrupt":
					if os.WriteFile(path, []byte("unfinished"), 0600) != nil {
						t.Fatal("fixture write failed")
					}
				}
				if state, err := m.mirrorNativeAudit("mcp", goalCommitIO{}); err == nil || state.Available {
					t.Fatal("unsafe audit state allowed mirror readiness")
				}
				got, _ := os.ReadFile(outside)
				if string(got) != "unchanged" {
					t.Fatal("unsafe destination changed sentinel")
				}
			})
		}
	}
	m, req := goalCommitFixture(t)
	if _, _, err := m.commitGoal(req, goalCommitIO{}); err != nil {
		t.Fatal("fixture commit failed")
	}
	if _, err := m.mirrorNativeAudit("cli", goalCommitIO{}); err != nil {
		t.Fatal("fixture mirror failed")
	}
	b, _ := os.ReadFile(filepath.Join(m.store, "audit.jsonl"))
	var row map[string]any
	if json.Unmarshal(bytes.TrimSpace(b), &row) != nil {
		t.Fatal("row decode failed")
	}
	for _, field := range []string{"body", "report", "session", "actor", "path"} {
		row[field] = "private fixture text"
		invalid, _ := json.Marshal(row)
		if len(parseAudit(invalid, time.Time{})) != 0 {
			t.Fatal("consumer accepted unexpected native mirror field")
		}
		delete(row, field)
	}
	row["source"] = "json-actor"
	invalid, _ := json.Marshal(row)
	if len(parseAudit(invalid, time.Time{})) != 0 {
		t.Fatal("consumer accepted spoofed source")
	}
	if len(parseAudit(append(bytes.Clone(b), b...), time.Time{})) != 1 {
		t.Fatal("native duplicate identity was not deduplicated")
	}
	legacy := []byte(`{"time":"2026-10-12T00:00:00Z","tool":"future_action","result":"ok"}` + "\n")
	if len(parseAudit(append(bytes.Clone(legacy), legacy...), time.Time{})) != 2 {
		t.Fatal("consumer incorrectly deduplicated legacy rows")
	}
}
