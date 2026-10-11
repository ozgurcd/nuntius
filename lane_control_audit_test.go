package nuntius

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

func laneControlRevision(n uint64) *uint64 { return &n }

func laneControlCommitFixture(t *testing.T) (*Manager, string) {
	t.Helper()
	c, store := fixture(t)
	dir, err := filepath.EvalSymlinks(filepath.Dir(store))
	if err != nil {
		t.Fatal(err)
	}
	store = filepath.Join(dir, "store")
	m, err := NewManager(c, store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m, store
}

func laneControlCode(err error) string {
	var fault *laneControlFault
	if errors.As(err, &fault) {
		return fault.Code
	}
	if err == nil {
		return ""
	}
	return "unexpected_error"
}

func laneControlSet(t *testing.T, m *Manager, paused bool, revision uint64) laneControlResult {
	t.Helper()
	r, err := m.setLaneControl(laneControlChange{Lane: "codex", Paused: paused, ExpectedRevision: laneControlRevision(revision)}, "web")
	if err != nil || !r.Changed || !r.Available || r.Revision != revision+1 || r.Paused != paused || r.Code != "committed" || r.Origin == "" {
		t.Fatalf("set contract: code=%s revision=%d changed=%t", laneControlCode(err), r.Revision, r.Changed)
	}
	return r
}

func laneControlAuditRows(t *testing.T, store string) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(store, "audit.jsonl"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	rows := []map[string]any{}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	for _, line := range bytes.Split(bytes.TrimSpace(b), []byte{'\n'}) {
		var row map[string]any
		if json.Unmarshal(line, &row) != nil {
			t.Fatal("invalid audit row")
		}
		if strings.HasPrefix(fmt.Sprint(row["tool"]), "lane_") {
			rows = append(rows, row)
		}
	}
	return rows
}

func laneControlRaw(t *testing.T, store string) laneControlStore {
	t.Helper()
	root, err := openLaneControlRoot(store)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	s, err := readLaneControlFile(root)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestLaneControlCommitCAS(t *testing.T) {
	m, store := laneControlCommitFixture(t)
	for _, source := range []string{"web", "mcp"} {
		_, err := m.setLaneControl(laneControlChange{Lane: "codex", Paused: true}, source)
		if laneControlCode(err) != "invalid_request" {
			t.Fatal("remote revision must be mandatory")
		}
	}
	_, err := m.setLaneControl(laneControlChange{Lane: "codex", Paused: true}, "caller")
	if laneControlCode(err) != "invalid_request" {
		t.Fatal("source must come from trusted adapter")
	}
	first := laneControlSet(t, m, true, 0)
	r, err := m.setLaneControl(laneControlChange{Lane: "codex", Paused: true, AfterCurrent: true, ExpectedRevision: laneControlRevision(1)}, "mcp")
	if err != nil || r.Code != "unchanged" || r.Changed || r.Revision != 1 {
		t.Fatal("fresh same-value changes no revision")
	}
	if laneControlRaw(t, store).Lanes["codex"].RequestedAfterCurrent {
		t.Fatal("same-value must preserve provenance")
	}
	_, err = m.setLaneControl(laneControlChange{Lane: "codex", Paused: true, ExpectedRevision: laneControlRevision(0)}, "web")
	if laneControlCode(err) != "revision_conflict" {
		t.Fatal("stale same-value request must conflict")
	}
	laneControlSet(t, m, false, 1)
	laneControlSet(t, m, true, 2)
	_, err = m.setLaneControl(laneControlChange{Lane: "codex", Paused: true, ExpectedRevision: laneControlRevision(1)}, "mcp")
	if laneControlCode(err) != "revision_conflict" {
		t.Fatal("ABA revision must conflict")
	}
	r, err = m.setLaneControl(laneControlChange{Lane: "codex", Paused: false, ExpectedRevision: laneControlRevision(0)}, "cli")
	if err != nil || r.Revision != 4 || !r.Changed {
		t.Fatal("CLI must read fresh revision under lock")
	}
	r, err = m.setLaneControl(laneControlChange{Lane: "codex"}, "cli")
	if err != nil || r.Code != "unchanged" || r.Revision != 4 {
		t.Fatal("CLI repeat must be unchanged")
	}
	rows := laneControlAuditRows(t, store)
	intents, outcomes := 0, 0
	for _, row := range rows {
		if row["result"] == "intent" {
			intents++
		}
		if row["result"] == "committed" {
			outcomes++
		}
		if row["result"] == "intent" || row["result"] == "committed" {
			if row["origin"] == nil || row["source"] == nil || row["revision"] == nil || row["before"] == nil || row["after"] == nil || row["after_current"] == nil || row["time"] == nil {
				t.Fatal("missing audit contract field")
			}
		}
	}
	if intents != 4 || outcomes != 4 || rows[0]["origin"] != first.Origin {
		t.Fatal("accepted changes must have one correlated intent/outcome pair")
	}
	if len(m.active) != 0 {
		t.Fatal("control operation launched work")
	}
}

func TestLaneControlCommitCrashWindows(t *testing.T) {
	cases := []struct {
		stage, code string
		committed   bool
	}{
		{"intent.write", "audit_unavailable", false},
		{"intent.fsync", "audit_unavailable", false},
		{"intent.rename", "audit_unavailable", false},
		{"intent.renamed", "audit_unavailable", false},
		{"intent.dirsync", "audit_unavailable", false},
		{"audit.intent.append", "audit_unavailable", false},
		{"audit.intent.fsync", "audit_unavailable", false},
		{"audit.intent.dirsync", "audit_unavailable", false},
		{"state.write", "save_unconfirmed", false},
		{"state.fsync", "save_unconfirmed", false},
		{"state.rename", "save_unconfirmed", false},
		{"state.renamed", "save_unconfirmed", true},
		{"state.dirsync", "save_unconfirmed", true},
		{"audit.outcome.append", "committed_audit_failed", true},
		{"audit.outcome.fsync", "committed_audit_failed", true},
		{"audit.outcome.dirsync", "committed_audit_failed", true},
		{"hold.fsync", "committed_audit_failed", true},
		{"reconcile.state.fsync", "committed_audit_failed", true},
		{"reconcile.state.dirsync", "committed_audit_failed", true},
		{"intent.cleanup", "committed_audit_failed", true},
	}
	for _, paused := range []bool{true, false} {
		for _, tc := range cases {
			if !paused && tc.stage == "hold.fsync" {
				continue
			}
			t.Run(fmt.Sprintf("paused_%t/%s", paused, tc.stage), func(t *testing.T) {
				m, store := laneControlCommitFixture(t)
				revision := uint64(0)
				if !paused {
					laneControlSet(t, m, true, 0)
					revision = 1
				}
				seen := false
				ops := &laneControlIO{checkpoint: func(stage string) error {
					if stage == tc.stage {
						seen = true
						return errors.New("injected")
					}
					return nil
				}}
				r, err := m.setLaneControlIO(laneControlChange{Lane: "codex", Paused: paused, ExpectedRevision: laneControlRevision(revision)}, "web", ops)
				if !seen || laneControlCode(err) != tc.code || r.Available {
					t.Fatalf("fault not classified at %s: %s", tc.stage, laneControlCode(err))
				}
				before := laneControlRaw(t, store)
				if (before.Revision == revision+1) != tc.committed || (before.Lanes["codex"].Paused == paused) != tc.committed {
					t.Fatal("old/new readback disagrees with rename boundary")
				}
				intentPath := filepath.Join(store, "lane-control-intent.json")
				intent, intentErr := os.ReadFile(intentPath)
				if tc.code != "audit_unavailable" && intentErr != nil {
					t.Fatal("uncertainty lost intent")
				}
				if intentErr == nil {
					rowsBefore := laneControlAuditRows(t, store)
					_, getErr := m.laneControls()
					if !errors.Is(getErr, errLaneControlUnavailable) {
						t.Fatal("GET opened unresolved admission")
					}
					after, e := os.ReadFile(intentPath)
					if e != nil || !bytes.Equal(intent, after) || !reflect.DeepEqual(rowsBefore, laneControlAuditRows(t, store)) {
						t.Fatal("GET reconciled intent")
					}
				}
				// A fresh real Manager runs the startup reconciler on this temporary store.
				fresh, e := NewManager(m.config, store)
				if e != nil {
					t.Fatal(e)
				}
				defer fresh.Close()
				snap, e := fresh.laneControls()
				if e != nil || snap.Revision != before.Revision {
					t.Fatal("restart failed old/new reconciliation")
				}
				if _, e = os.Lstat(intentPath); !os.IsNotExist(e) {
					t.Fatal("restart left resolved intent")
				}
				if intentErr == nil {
					rows := laneControlAuditRows(t, store)
					outcome := rows[len(rows)-1]
					want := "not_committed"
					if tc.committed {
						want = "committed"
					}
					if outcome["result"] != want {
						t.Fatal("restart outcome does not match actual state")
					}
				}
			})
		}
	}
}

func TestLaneControlCommitAuditOrder(t *testing.T) {
	m, store := laneControlCommitFixture(t)
	var stages []string
	r, err := m.setLaneControlIO(laneControlChange{Lane: "codex", Paused: true, AfterCurrent: true}, "cli", &laneControlIO{checkpoint: func(stage string) error {
		stages = append(stages, stage)
		if stage == "state.write" {
			if _, e := os.Stat(filepath.Join(store, "lane-control-intent.json")); e != nil {
				t.Fatal("state preceded durable intent")
			}
			rows := laneControlAuditRows(t, store)
			if len(rows) != 1 || rows[0]["result"] != "intent" || rows[0]["after_current"] != true || laneControlRaw(t, store).Revision != 0 {
				t.Fatal("state preceded audit intent")
			}
		}
		return nil
	}})
	if err != nil || !r.Available {
		t.Fatal("ordered transaction failed")
	}
	wanted := []string{"intent.fsync", "intent.rename", "intent.dirsync", "audit.intent.fsync", "audit.intent.dirsync", "state.write", "state.fsync", "state.rename", "state.dirsync", "hold.fsync", "audit.outcome.fsync", "audit.outcome.dirsync", "intent.cleanup"}
	pos := 0
	for _, stage := range stages {
		if pos < len(wanted) && stage == wanted[pos] {
			pos++
		}
	}
	if pos != len(wanted) {
		t.Fatal("durability order changed")
	}
	// Existing readers retain legacy row compatibility and classify the new
	// canonical actions as writes, even though new transport surfaces are absent.
	rows, err := m.Audit("")
	if err != nil || len(rows) != 2 || rows[0].ReadOnly || rows[1].ReadOnly {
		t.Fatal("control audit broke existing reader")
	}
}

func TestLaneControlCommitTornAudit(t *testing.T) {
	m, store := laneControlCommitFixture(t)
	p := filepath.Join(store, "audit.jsonl")
	laneControlPut(t, p, `{`)
	_, err := m.setLaneControl(laneControlChange{Lane: "codex", Paused: true}, "cli")
	if laneControlCode(err) != "audit_unavailable" || laneControlRaw(t, store).Revision != 0 {
		t.Fatal("torn audit allowed state mutation")
	}
	b, err := os.ReadFile(p)
	if err != nil || string(b) != `{` {
		t.Fatal("torn audit history rewritten")
	}
	if m.recoverLaneControls(nil) == nil {
		t.Fatal("torn audit reconciliation opened admission")
	}
	if _, err = m.readLaneControlsLocked(); !errors.Is(err, errLaneControlUnavailable) {
		t.Fatal("torn audit lost fail-closed intent")
	}
}

func TestLaneControlCommitRetainedResume(t *testing.T) {
	m, store := laneControlCommitFixture(t)
	laneControlSet(t, m, true, 0)
	marker, err := os.ReadFile(filepath.Join(store, "lane-control-hold-codex.json"))
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.config.Lanes = []Lane{m.config.Lanes[0]} // claude only; codex is retained.
	config := m.config
	m.mu.Unlock()
	r, err := m.setLaneControl(laneControlChange{Lane: "codex", ExpectedRevision: laneControlRevision(1)}, "web")
	if err != nil || !r.Changed || !r.Available || r.Paused || r.Revision != 2 {
		t.Fatal("retained resume must clear hold without recreating lane")
	}
	if !reflect.DeepEqual(config, m.config) || laneControlRaw(t, store).Lanes["codex"].Paused {
		t.Fatal("retained resume changed config or lost state")
	}
	after, err := os.ReadFile(filepath.Join(store, "lane-control-hold-codex.json"))
	if err != nil || !bytes.Equal(marker, after) {
		t.Fatal("retained resume erased message repair obligation")
	}
	_, err = m.setLaneControl(laneControlChange{Lane: "codex", Paused: true}, "cli")
	if laneControlCode(err) != "lane_not_configured" {
		t.Fatal("pause must not recreate a removed lane")
	}
	_, err = m.setLaneControl(laneControlChange{Lane: "never-seen"}, "cli")
	if laneControlCode(err) != "lane_not_found" {
		t.Fatal("unknown resume accepted")
	}
}

func TestLaneControlCommitResumeFailClosed(t *testing.T) {
	m, store := laneControlCommitFixture(t)
	laneControlSet(t, m, true, 0)
	_, err := m.setLaneControlIO(laneControlChange{Lane: "codex", ExpectedRevision: laneControlRevision(1)}, "mcp", &laneControlIO{checkpoint: func(stage string) error {
		if stage == "audit.outcome.append" {
			return errors.New("injected")
		}
		return nil
	}})
	if laneControlCode(err) != "committed_audit_failed" || laneControlRaw(t, store).Lanes["codex"].Paused {
		t.Fatal("resume uncertainty setup")
	}
	if _, err = m.laneControls(); !errors.Is(err, errLaneControlUnavailable) {
		t.Fatal("unresolved resume enabled reads")
	}
	if _, err = m.readLaneControlsLocked(); !errors.Is(err, errLaneControlUnavailable) {
		t.Fatal("unresolved resume opened final admission read")
	}
	// Audit remains unusable even when the state bytes say enabled.
	if err = m.recoverLaneControls(&laneControlIO{checkpoint: func(stage string) error {
		if stage == "audit.outcome.append" {
			return errors.New("injected")
		}
		return nil
	}}); laneControlCode(err) != "audit_unavailable" {
		t.Fatal("unresolved startup error")
	}
	if _, err = m.laneControls(); !errors.Is(err, errLaneControlUnavailable) {
		t.Fatal("failed reconcile removed admission hold")
	}
	r, err := m.setLaneControl(laneControlChange{Lane: "codex", ExpectedRevision: laneControlRevision(2)}, "mcp")
	if err != nil || r.Code != "unchanged" || r.Revision != 2 {
		t.Fatal("next mutation must reconcile before unchanged")
	}
	if _, err = m.laneControls(); err != nil {
		t.Fatal("resolved resume stayed unavailable")
	}
}

func TestLaneControlCommitRepeatedCorrelation(t *testing.T) {
	m, store := laneControlCommitFixture(t)
	_, err := m.setLaneControlIO(laneControlChange{Lane: "codex", Paused: true}, "cli", &laneControlIO{checkpoint: func(stage string) error {
		if stage == "intent.cleanup" {
			return errors.New("crash after outcome")
		}
		return nil
	}})
	if laneControlCode(err) != "committed_audit_failed" {
		t.Fatal("repeat setup")
	}
	for range 2 {
		if m.recoverLaneControls(&laneControlIO{checkpoint: func(stage string) error {
			if stage == "intent.cleanup" {
				return errors.New("crash after outcome")
			}
			return nil
		}}) == nil {
			t.Fatal("cleanup fault hidden")
		}
	}
	if err = m.recoverLaneControls(nil); err != nil {
		t.Fatal(err)
	}
	rows := laneControlAuditRows(t, store)
	if len(rows) != 5 {
		t.Fatal("replayed outcomes were not exercised")
	}
	for _, row := range rows {
		if row["origin"] != rows[0]["origin"] || row["revision"] != float64(1) || row["source"] != "cli" {
			t.Fatal("reconciliation changed correlation")
		}
	}
	if err = m.recoverLaneControls(nil); err != nil || len(laneControlAuditRows(t, store)) != 5 {
		t.Fatal("completed reconcile appended another outcome")
	}
}

func TestLaneControlCommitConcurrentWinner(t *testing.T) {
	m, store := laneControlCommitFixture(t)
	second, err := NewManager(m.config, store)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	start := make(chan struct{})
	results := make(chan string, 2)
	var wg sync.WaitGroup
	for _, mgr := range []*Manager{m, second} {
		wg.Go(func() {
			<-start
			r, err := mgr.setLaneControl(laneControlChange{Lane: "codex", Paused: true, ExpectedRevision: laneControlRevision(0)}, "web")
			if err != nil {
				results <- laneControlCode(err)
			} else {
				results <- r.Code
			}
		})
	}
	close(start)
	wg.Wait()
	close(results)
	winners := 0
	for code := range results {
		if code == "committed" {
			winners++
		} else if code != "locked" && code != "revision_conflict" {
			t.Fatal("unexpected concurrent result")
		}
	}
	if winners != 1 || laneControlRaw(t, store).Revision != 1 {
		t.Fatal("concurrent CAS has no unique revision winner")
	}
	if len(laneControlAuditRows(t, store)) < 2 {
		t.Fatal("winning mutation lacked audit")
	}
}

func TestLaneControlCommitLockContention(t *testing.T) {
	m, store := laneControlCommitFixture(t)
	m.mu.Lock()
	_, err := m.setLaneControl(laneControlChange{Lane: "codex", Paused: true}, "cli")
	m.mu.Unlock()
	if laneControlCode(err) != "locked" {
		t.Fatal("manager contention must be bounded")
	}
	guard, err := m.controlLock()
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.setLaneControl(laneControlChange{Lane: "codex", Paused: true}, "cli")
	guard.Close()
	if laneControlCode(err) != "locked" || laneControlRaw(t, store).Revision != 0 {
		t.Fatal("shared control contention must be bounded and effect-free")
	}
	root, err := openLaneControlRoot(store)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	fd, err := unix.Openat(int(root.Fd()), "audit.jsonl", unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	_, err = m.setLaneControl(laneControlChange{Lane: "codex", Paused: true}, "cli")
	if laneControlCode(err) != "audit_unavailable" || laneControlRaw(t, store).Revision != 0 {
		t.Fatal("audit contention opened state")
	}
}

func TestLaneControlCommitCapacityAndValidation(t *testing.T) {
	m, store := laneControlCommitFixture(t)
	s := laneControlState(1, "codex")
	for i := 1; i < laneControlMaxNames; i++ {
		s.Lanes[fmt.Sprintf("retained-%d", i)] = s.Lanes["codex"]
	}
	b, err := encodeLaneControls(s)
	if err != nil {
		t.Fatal(err)
	}
	laneControlPut(t, filepath.Join(store, "lane-control.json"), string(b))
	before := laneControlRaw(t, store)
	_, err = m.setLaneControl(laneControlChange{Lane: "claude", Paused: true}, "cli")
	if laneControlCode(err) != "lane_control_capacity" || !reflect.DeepEqual(before, laneControlRaw(t, store)) {
		t.Fatal("capacity must never evict a hold")
	}
	r := laneControlSet(t, m, false, 1)
	if r.Revision != 2 || len(laneControlRaw(t, store).Lanes) != laneControlMaxNames {
		t.Fatal("existing resume at capacity")
	}
	_, err = m.setLaneControl(laneControlChange{Lane: "retained-1", Paused: false}, "cli")
	if err != nil || laneControlRaw(t, store).Lanes["retained-1"].Paused || len(m.config.Lanes) != 2 {
		t.Fatal("retained resume must clear hold without recreating configuration")
	}
	_, err = m.setLaneControl(laneControlChange{Lane: "codex", AfterCurrent: true}, "cli")
	if laneControlCode(err) != "invalid_request" {
		t.Fatal("after-current resume must be invalid")
	}
	s.Revision = laneControlMaxRevision
	for name, rec := range s.Lanes {
		rec.Revision = s.Revision
		s.Lanes[name] = rec
	}
	b, err = encodeLaneControls(s)
	if err != nil {
		t.Fatal(err)
	}
	laneControlPut(t, filepath.Join(store, "lane-control.json"), string(b))
	_, err = m.setLaneControl(laneControlChange{Lane: "codex"}, "cli")
	if laneControlCode(err) != "lane_control_unavailable" {
		t.Fatal("revision overflow accepted")
	}
}

func TestLaneControlCommitHeldPreparation(t *testing.T) {
	m, store := laneControlCommitFixture(t)
	laneControlSet(t, m, true, 0)
	p := filepath.Join(store, "lane-control-hold-codex.json")
	b, err := os.ReadFile(p)
	var marker map[string]any
	if err != nil || json.Unmarshal(b, &marker) != nil || marker["lane"] != "codex" || marker["through_revision"] != float64(1) || marker["origin"] == nil {
		t.Fatal("pause lacked durable A04 latch preparation")
	}
	laneControlSet(t, m, false, 1)
	after, err := os.ReadFile(p)
	if err != nil || !bytes.Equal(b, after) {
		t.Fatal("resume erased held-message preparation")
	}
	laneControlSet(t, m, true, 2)
	after, err = os.ReadFile(p)
	var next map[string]any
	if err != nil || json.Unmarshal(after, &next) != nil || next["through_revision"] != float64(3) || marker["since"] != next["since"] {
		t.Fatal("repeated pause lost original held-message boundary")
	}
}

func TestLaneControlCommitUnsafeAndUncertain(t *testing.T) {
	for _, target := range []string{"audit.jsonl", "lane-control-intent.json", "lane-control-hold-codex.json"} {
		t.Run(target, func(t *testing.T) {
			m, store := laneControlCommitFixture(t)
			outside := filepath.Join(filepath.Dir(store), "outside")
			laneControlPut(t, outside, "unchanged")
			if err := os.Symlink(outside, filepath.Join(store, target)); err != nil {
				t.Fatal(err)
			}
			_, err := m.setLaneControl(laneControlChange{Lane: "codex", Paused: true}, "cli")
			if err == nil {
				t.Fatal("linked transaction file accepted")
			}
			b, e := os.ReadFile(outside)
			if e != nil || string(b) != "unchanged" {
				t.Fatal("transaction followed symlink")
			}
		})
	}
	t.Run("divergent_state", func(t *testing.T) {
		m, store := laneControlCommitFixture(t)
		_, err := m.setLaneControlIO(laneControlChange{Lane: "codex", Paused: true}, "cli", &laneControlIO{checkpoint: func(stage string) error {
			if stage == "state.rename" {
				return errors.New("crash")
			}
			return nil
		}})
		if laneControlCode(err) != "save_unconfirmed" {
			t.Fatal("divergence setup")
		}
		b, err := encodeLaneControls(laneControlState(2, "claude"))
		if err != nil {
			t.Fatal(err)
		}
		laneControlPut(t, filepath.Join(store, "lane-control.json"), string(b))
		if m.recoverLaneControls(nil) == nil {
			t.Fatal("unknown state reconciled as committed")
		}
		if _, err = m.laneControls(); !errors.Is(err, errLaneControlUnavailable) {
			t.Fatal("uncertain intent enabled read")
		}
		rows := laneControlAuditRows(t, store)
		if rows[len(rows)-1]["result"] != "uncertain" {
			t.Fatal("divergence must have fixed uncertain outcome")
		}
	})
}
