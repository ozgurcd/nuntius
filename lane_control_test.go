package nuntius

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

const laneControlSample = `{"lane_control_version":1,"revision":1,"lanes":{"codex":{"paused":true,"since":"2026-10-11T12:00:00Z","source":"cli","requested_after_current":false,"revision":1}}}`

// Admission effects exclude the append-only refusal audit and observation
// timestamps. Every run/prompt/event/message artifact and stop field is checked.
func admissionArtifacts(t *testing.T, store string) map[string][32]byte {
	t.Helper()
	out := map[string][32]byte{}
	err := filepath.WalkDir(filepath.Join(store, "runs"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			out[path] = [32]byte{}
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[path] = sha256.Sum256(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func admissionCall(m *Manager, op string) error {
	switch op {
	case "dispatch":
		_, err := m.Dispatch("codex", "REFUSED", "answer")
		return err
	case "reply":
		_, err := m.Reply("codex", "answer")
		return err
	case "send":
		return m.SendMessage("codex", "answer", false)
	default:
		return m.SendMessage("codex", "answer", true)
	}
}

func admissionPause(t *testing.T, m *Manager, after bool) {
	t.Helper()
	result, err := m.setLaneControl(laneControlChange{Lane: "codex", Paused: true, AfterCurrent: after}, "cli")
	if err != nil || !result.Available || !result.Paused {
		t.Fatal("pause failed")
	}
}

func TestLaneAdmissionRefusals(t *testing.T) {
	goalEndParallel(t)
	for _, after := range []bool{false, true} {
		for _, state := range []string{"idle", "ended", "busy"} {
			for _, op := range []string{"dispatch", "reply", "send", "stop-and-send"} {
				t.Run(fmt.Sprintf("after=%t/%s/%s", after, state, op), func(t *testing.T) {
					m, store := laneControlCommitFixture(t)
					if state != "idle" {
						text := "worked"
						if state == "busy" {
							text = "wait"
						}
						if _, err := m.Dispatch("codex", "ORIGINAL", text); err != nil {
							t.Fatal(err)
						}
						if state == "busy" {
							waitReady(t, m, "codex")
						} else {
							waitState(t, m, "codex", "ended")
						}
					}
					// Separate manager models another adapter/process, without adopting
					// a second supervisor or changing the running goal's lifetime lock.
					control := &Manager{config: m.config, store: store}
					admissionPause(t, control, after)
					m.mu.Lock()
					before := admissionArtifacts(t, store)
					m.mu.Unlock()
					err := admissionCall(m, op)
					if err == nil || !strings.Contains(err.Error(), "lane_paused") {
						t.Error("missing lane_paused refusal")
					}
					m.mu.Lock()
					afterArtifacts := admissionArtifacts(t, store)
					m.mu.Unlock()
					if !reflect.DeepEqual(before, afterArtifacts) {
						t.Error("refusal changed run/prompt/message/child or stop metadata")
					}
					if state == "busy" {
						r, err := m.latest("codex")
						if err != nil || r == nil || !sameProcess(*r) || r.StopAt != nil || r.StopReason != "" {
							t.Error("pause admission interrupted current run")
						}
					}
				})
			}
		}
	}
}

func TestLaneAdmissionDrainsAndIsolation(t *testing.T) {
	goalEndParallel(t)
	for _, after := range []bool{false, true} {
		t.Run(fmt.Sprint(after), func(t *testing.T) {
			m, store := laneControlCommitFixture(t)
			id, err := m.Dispatch("codex", "ADMISSION-RUN", "admission-barrier")
			if err != nil {
				t.Fatal(err)
			}
			waitReady(t, m, "codex")
			original, err := m.latest("codex")
			if err != nil {
				t.Fatal(err)
			}
			control := &Manager{config: m.config, store: store}
			admissionPause(t, control, after)
			for _, op := range []string{"dispatch", "reply", "send", "stop-and-send"} {
				if err := admissionCall(m, op); err == nil || !strings.Contains(err.Error(), "lane_paused") {
					t.Errorf("%s accepted during drain", op)
				}
			}
			current, err := m.latest("codex")
			if err != nil || current.ID != id || current.ProcessStart != original.ProcessStart || !sameProcess(*current) || current.StopAt != nil || current.StopReason != "" {
				t.Error("current process did not remain alive unchanged")
			}
			if _, err := m.Dispatch("claude", "OTHER-LANE", "worked"); err != nil {
				t.Fatal("other lane blocked")
			}
			waitState(t, m, "claude", "ended")
			if err := os.WriteFile(filepath.Join(m.config.ReportsDir, "admission-release"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			waitState(t, m, "codex", "ended")
			current, err = m.latest("codex")
			if err != nil || current.ID != id || current.StopReason != "completed" || current.Ended == nil || current.StopAt != nil || !m.reportExists(*current) {
				t.Error("draining goal did not deliver its normal report/end")
			}
			runs, err := m.history()
			if err != nil || len(runs) != 2 {
				t.Error("unexpected successor or child")
			}
		})
	}
}

func TestLaneAdmissionUnavailable(t *testing.T) {
	goalEndParallel(t)
	for _, health := range []string{"corrupt", "resume-intent", "missing-lock", "unsafe-lock"} {
		for _, op := range []string{"dispatch", "reply", "send", "stop-and-send"} {
			t.Run(health+"/"+op, func(t *testing.T) {
				m, store := laneControlCommitFixture(t)
				if _, err := m.Dispatch("codex", "ORIGINAL", "worked"); err != nil {
					t.Fatal(err)
				}
				waitState(t, m, "codex", "ended")
				switch health {
				case "corrupt":
					if err := os.WriteFile(filepath.Join(store, "lane-control.json"), []byte("{"), 0600); err != nil {
						t.Fatal(err)
					}
				case "resume-intent":
					admissionPause(t, m, false)
					_, err := m.setLaneControlIO(laneControlChange{Lane: "codex"}, "cli", &laneControlIO{checkpoint: func(stage string) error {
						if stage == "audit.outcome.append" {
							return errors.New("injected")
						}
						return nil
					}})
					if laneControlCode(err) != "committed_audit_failed" || laneControlRaw(t, store).Lanes["codex"].Paused {
						t.Fatal("unresolved resume setup")
					}
				case "missing-lock", "unsafe-lock":
					if err := os.Remove(filepath.Join(store, "locks", "control.lock")); err != nil {
						t.Fatal(err)
					}
					if health == "unsafe-lock" {
						if err := os.Symlink(filepath.Join(store, "untouched"), filepath.Join(store, "locks", "control.lock")); err != nil {
							t.Fatal(err)
						}
					}
				}
				before := admissionArtifacts(t, store)
				err := admissionCall(m, op)
				if err == nil || !strings.Contains(err.Error(), "lane_control_unavailable") {
					t.Error("unhealthy controls admitted work")
				}
				if !reflect.DeepEqual(before, admissionArtifacts(t, store)) {
					t.Error("unavailable controls changed artifacts")
				}
				if health == "resume-intent" {
					if _, err := os.Stat(filepath.Join(store, "lane-control-intent.json")); err != nil {
						t.Error("admission reconciled intent")
					}
				}
				if health == "missing-lock" {
					if _, err := os.Lstat(filepath.Join(store, "locks", "control.lock")); !os.IsNotExist(err) {
						t.Error("admission created coordination inode")
					}
				}
				if health == "unsafe-lock" {
					if _, err := os.Stat(filepath.Join(store, "untouched")); !os.IsNotExist(err) {
						t.Error("admission followed unsafe coordination link")
					}
				}
			})
		}
	}
}

func TestLaneAdmissionFrozenAndValidation(t *testing.T) {
	m, _ := laneControlCommitFixture(t)
	admissionPause(t, m, false)
	if err := m.Freeze("fixture"); err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"dispatch", "reply", "send", "stop-and-send"} {
		err := admissionCall(m, op)
		if err == nil || !strings.Contains(err.Error(), "lane_paused") {
			t.Errorf("%s freeze replaced primary pause refusal", op)
		}
		b, _ := json.Marshal(err)
		var refusal struct {
			Code, Lane        string
			Paused, Retryable bool
			Blockers          []string
		}
		if json.Unmarshal(b, &refusal) != nil || refusal.Code != "lane_paused" || refusal.Lane != "codex" || !refusal.Paused || refusal.Retryable || !reflect.DeepEqual(refusal.Blockers, []string{"lane_paused", "frozen"}) {
			t.Error("missing safe typed pause/freeze contract")
		}
	}
	if _, err := m.Dispatch("unknown", "VALID", "answer"); err == nil || err.Error() != "unknown lane" {
		t.Error("unknown name lost validation")
	}
	if _, err := m.Dispatch("codex", "bad goal", "answer"); err == nil || err.Error() != "invalid goal_id" {
		t.Error("invalid goal lost validation")
	}
	if _, err := m.Reply("codex", ""); err == nil || !strings.Contains(err.Error(), "text must contain") {
		t.Error("invalid reply text lost validation")
	}
	if err := m.SendMessage("unknown", "answer", true); err == nil || err.Error() != "unknown lane" {
		t.Error("invalid send name lost validation")
	}
	m.mu.Lock()
	m.config.Lanes = m.config.Lanes[:1]
	m.mu.Unlock()
	if _, err := m.Dispatch("codex", "VALID", "answer"); err == nil || err.Error() != "unknown lane" {
		t.Error("retained removed lane became configured")
	}
}

func TestLaneAdmissionSafetyControls(t *testing.T) {
	goalEndParallel(t)
	for _, after := range []bool{false, true} {
		for _, action := range []string{"cancel", "freeze", "timeout"} {
			t.Run(fmt.Sprintf("after=%t/%s", after, action), func(t *testing.T) {
				m, _ := laneControlCommitFixture(t)
				m.mu.Lock()
				m.Grace = 10 * time.Millisecond
				m.mu.Unlock()
				if _, err := m.Dispatch("codex", "SAFETY", "ignore-int"); err != nil {
					t.Fatal(err)
				}
				waitReady(t, m, "codex")
				admissionPause(t, m, after)
				switch action {
				case "cancel":
					if err := m.Cancel("codex"); err != nil {
						t.Fatal(err)
					}
				case "freeze":
					if err := m.Freeze("fixture"); err != nil {
						t.Fatal(err)
					}
				case "timeout":
					m.mu.Lock()
					m.runtime = time.Nanosecond
					m.mu.Unlock()
				}
				status := waitState(t, m, "codex", "ended")
				if action == "timeout" && status.Outcome != "timed_out" {
					t.Error("timeout outcome missing")
				}
				r, err := m.latest("codex")
				if err != nil || r.StopAt == nil || sameProcess(*r) {
					t.Fatal("independent safety stop failed")
				}
			})
		}
	}
}

// Direct successors bypass start. A03 protects launch itself; A04 owns
// successor bookkeeping and the held-message binding above that boundary.
func TestLaneAdmissionFinalLaunch(t *testing.T) {
	for _, health := range []string{"paused", "intent", "missing-lock", "unsafe-lock"} {
		t.Run(health, func(t *testing.T) {
			m, store := laneControlCommitFixture(t)
			admissionPause(t, m, false)
			if health == "missing-lock" || health == "unsafe-lock" {
				laneControlSet(t, m, false, 1)
			}
			if health == "intent" {
				if err := os.WriteFile(filepath.Join(store, "lane-control-intent.json"), []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			m.mu.Lock()
			guard, err := m.controlLock()
			if err != nil {
				m.mu.Unlock()
				t.Fatal(err)
			}
			if health == "missing-lock" || health == "unsafe-lock" {
				if err := os.Remove(filepath.Join(store, "locks", "control.lock")); err != nil {
					guard.Close()
					m.mu.Unlock()
					t.Fatal(err)
				}
				if health == "unsafe-lock" {
					if err := os.Symlink(filepath.Join(store, "untouched"), filepath.Join(store, "locks", "control.lock")); err != nil {
						guard.Close()
						m.mu.Unlock()
						t.Fatal(err)
					}
				}
			}
			lock, err := m.lock("codex")
			if err != nil {
				guard.Close()
				m.mu.Unlock()
				t.Fatal(err)
			}
			before := admissionArtifacts(t, store)
			lane, _ := m.lane("codex")
			_, err = m.launch(lane, "SUCCESSOR", "worked", "fixture-thread", lock, nil)
			expected := "lane_paused"
			if health != "paused" {
				expected = "lane_control_unavailable"
			}
			if err == nil || !strings.Contains(err.Error(), expected) {
				t.Error("final launch guard missing")
			}
			if !reflect.DeepEqual(before, admissionArtifacts(t, store)) {
				t.Error("final guard allocated artifacts")
			}
			if health == "missing-lock" {
				if _, err := os.Lstat(filepath.Join(store, "locks", "control.lock")); !os.IsNotExist(err) {
					t.Error("final admission created lock inode")
				}
			}
			if health == "unsafe-lock" {
				if _, err := os.Stat(filepath.Join(store, "untouched")); !os.IsNotExist(err) {
					t.Error("final admission followed unsafe inode")
				}
			}
			guard.Close()
			m.mu.Unlock()
			if err != nil {
				lock.Close()
			}
		})
	}
}

func TestLaneAdmissionSharedReadNoCreate(t *testing.T) {
	m, path := laneControlFixture(t)
	locks := filepath.Join(m.store, "locks")
	if err := os.Mkdir(locks, 0700); err != nil {
		t.Fatal(err)
	}
	laneControlPut(t, path, laneControlSample)
	beforeState, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeRoot, err := os.Stat(m.store)
	if err != nil {
		t.Fatal(err)
	}
	beforeLocks, err := os.Stat(locks)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.laneControls(); !errors.Is(err, errLaneControlUnavailable) {
		t.Error("missing coordination inode must refuse read")
	}
	if _, err := os.Lstat(filepath.Join(locks, "control.lock")); !os.IsNotExist(err) {
		t.Error("shared read created coordination inode")
	}
	afterState, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(beforeState, afterState) {
		t.Error("shared read changed state bytes")
	}
	afterRoot, err := os.Stat(m.store)
	if err != nil || !beforeRoot.ModTime().Equal(afterRoot.ModTime()) {
		t.Error("shared read changed root directory")
	}
	afterLocks, err := os.Stat(locks)
	if err != nil || !beforeLocks.ModTime().Equal(afterLocks.ModTime()) {
		t.Error("shared read changed locks directory")
	}
}

func laneControlFixture(t *testing.T) (*Manager, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	return &Manager{store: root, config: Config{Lanes: []Lane{{Name: "codex"}, {Name: "claude"}}}}, filepath.Join(root, "lane-control.json")
}

func laneControlPut(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

func laneControlState(revision uint64, names ...string) laneControlStore {
	s := laneControlStore{Version: 1, Revision: revision, Lanes: map[string]laneControlRecord{}}
	for _, name := range names {
		s.Lanes[name] = laneControlRecord{Paused: true, Since: "2026-10-11T12:00:00Z", Source: "cli", Revision: revision}
	}
	return s
}

func TestLaneControlMissingAndTruncated(t *testing.T) {
	m, path := laneControlFixture(t)
	s, err := m.readLaneControlsLocked()
	if err != nil || s.Version != 1 || s.Revision != 0 || len(s.Lanes) != 0 {
		t.Fatal("initial absence is an empty versioned snapshot")
	}
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("read created control state")
	}
	for _, text := range []string{"", "{", laneControlSample[:len(laneControlSample)-1]} {
		laneControlPut(t, path, text)
		if _, err = m.readLaneControlsLocked(); !errors.Is(err, errLaneControlUnavailable) {
			t.Fatal("truncated state must fail closed")
		}
	}
}

func TestLaneControlClosedCodec(t *testing.T) {
	cases := map[string]string{
		"truncated": "{", "empty": "", "null": "null", "array": "[]",
		"unknown_top":       strings.Replace(laneControlSample, `"revision":1`, `"unknown":true,"revision":1`, 1),
		"unknown_record":    strings.Replace(laneControlSample, `"paused":true`, `"unknown":0,"paused":true`, 1),
		"duplicate_top":     strings.Replace(laneControlSample, `"revision":1`, `"revision":1,"revision":2`, 1),
		"duplicate_record":  strings.Replace(laneControlSample, `"paused":true`, `"paused":false,"paused":true`, 1),
		"escaped_duplicate": strings.Replace(laneControlSample, `"paused":true`, `"paus\u0065d":false,"paused":true`, 1),
		"duplicate_name":    `{"lane_control_version":1,"revision":1,"lanes":{"codex":{},"codex":{}}}`,
		"case_key":          strings.Replace(laneControlSample, `"paused"`, `"Paused"`, 1),
		"wrong_version":     strings.Replace(laneControlSample, `"lane_control_version":1`, `"lane_control_version":2`, 1),
		"missing_version":   strings.Replace(laneControlSample, `"lane_control_version":1,`, ``, 1),
		"missing_boolean":   strings.Replace(laneControlSample, `"paused":true,`, ``, 1),
		"null_boolean":      strings.Replace(laneControlSample, `"paused":true`, `"paused":null`, 1),
		"coerced_boolean":   strings.Replace(laneControlSample, `"paused":true`, `"paused":"true"`, 1),
		"null_lanes":        `{"lane_control_version":1,"revision":0,"lanes":null}`,
		"null_record":       `{"lane_control_version":1,"revision":1,"lanes":{"codex":null}}`,
		"trailing_document": laneControlSample + `{}`, "trailing_junk": laneControlSample + `x`,
		"oversized":    laneControlSample + strings.Repeat(" ", laneControlMaxBytes),
		"invalid_utf8": strings.Replace(laneControlSample, "codex", "code\xff", 1),
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeLaneControls([]byte(text)); !errors.Is(err, errLaneControlUnavailable) {
				t.Fatal("closed codec accepted invalid document")
			}
		})
	}
}

func TestLaneControlNamesRevisionAndProvenance(t *testing.T) {
	cases := map[string]string{}
	for _, name := range []string{"", "../codex", "a/b", "a_b", "*", "a b", "é", strings.Repeat("a", 65)} {
		b, _ := json.Marshal(name)
		cases["name_"+fmt.Sprint(len(cases))] = strings.Replace(laneControlSample, `"codex"`, string(b), 1)
	}
	for _, rev := range []string{"-1", "0", "1.0", "1e0", `"1"`, "null", "9007199254740992", "18446744073709551616"} {
		cases["store_revision_"+rev] = strings.Replace(laneControlSample, `"revision":1`, `"revision":`+rev, 1)
	}
	for _, rev := range []string{"0", "2", "-1", "1.5", "null"} {
		cases["record_revision_"+rev] = strings.Replace(laneControlSample, `"revision":1}}}`, `"revision":`+rev+`}}}`, 1)
	}
	for _, since := range []string{"", "not-a-time", "2026-10-11T13:00:00+01:00", "2026-10-11T12:00:00+00:00", "0001-01-01T00:00:00Z"} {
		cases["since_"+since] = strings.Replace(laneControlSample, "2026-10-11T12:00:00Z", since, 1)
	}
	for _, source := range []string{"", "unknown", "CLI", "owner"} {
		cases["source_"+source] = strings.Replace(laneControlSample, `"cli"`, `"`+source+`"`, 1)
	}
	cases["enabled_after_current"] = strings.Replace(strings.Replace(laneControlSample, `"paused":true`, `"paused":false`, 1), `"requested_after_current":false`, `"requested_after_current":true`, 1)
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeLaneControls([]byte(text)); !errors.Is(err, errLaneControlUnavailable) {
				t.Fatal("invalid identity, revision or provenance accepted")
			}
		})
	}
}

func TestLaneControlUnsafeInputs(t *testing.T) {
	for _, kind := range []string{"oversized", "directory", "fifo", "symlink", "dangling", "hardlink", "inaccessible", "public_file", "public_root", "inaccessible_root", "linked_root", "linked_ancestor", "missing_root"} {
		t.Run(kind, func(t *testing.T) {
			m, path := laneControlFixture(t)
			laneControlPut(t, path, laneControlSample)
			switch kind {
			case "oversized":
				laneControlPut(t, path, laneControlSample+strings.Repeat(" ", laneControlMaxBytes))
			case "directory", "fifo", "symlink", "dangling":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "directory":
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
				case "fifo":
					if err := syscall.Mkfifo(path, 0600); err != nil {
						t.Fatal(err)
					}
				default:
					target := filepath.Join(m.store, "target")
					if kind == "symlink" {
						laneControlPut(t, target, laneControlSample)
					}
					if err := os.Symlink(target, path); err != nil {
						t.Fatal(err)
					}
				}
			case "hardlink":
				if err := os.Link(path, filepath.Join(m.store, "alias")); err != nil {
					t.Fatal(err)
				}
			case "inaccessible":
				if err := os.Chmod(path, 0000); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(path, 0600) })
			case "public_file":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "public_root":
				if err := os.Chmod(m.store, 0755); err != nil {
					t.Fatal(err)
				}
			case "inaccessible_root":
				if err := os.Chmod(m.store, 0000); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(m.store, 0700) })
			case "linked_root", "linked_ancestor":
				parent, err := filepath.EvalSymlinks(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				link := filepath.Join(parent, "linked")
				if kind == "linked_root" {
					if err = os.Symlink(m.store, link); err != nil {
						t.Fatal(err)
					}
					m.store = link
				} else {
					if err = os.Symlink(filepath.Dir(m.store), link); err != nil {
						t.Fatal(err)
					}
					m.store = filepath.Join(link, filepath.Base(m.store))
				}
			case "missing_root":
				m.store = filepath.Join(m.store, "absent")
			}
			if _, err := m.readLaneControlsLocked(); !errors.Is(err, errLaneControlUnavailable) {
				t.Fatal("unsafe input opened admission")
			}
		})
	}
}

func TestLaneControlUnresolvedIntent(t *testing.T) {
	for _, state := range []string{"missing", "paused", "enabled"} {
		for _, intent := range []string{"regular", "empty", "symlink", "directory", "inaccessible"} {
			t.Run(state+"/"+intent, func(t *testing.T) {
				m, path := laneControlFixture(t)
				if state != "missing" {
					text := laneControlSample
					if state == "enabled" {
						text = strings.Replace(text, `"paused":true`, `"paused":false`, 1)
					}
					laneControlPut(t, path, text)
				}
				p := filepath.Join(m.store, "lane-control-intent.json")
				switch intent {
				case "directory":
					if err := os.Mkdir(p, 0700); err != nil {
						t.Fatal(err)
					}
				case "symlink":
					if err := os.Symlink(filepath.Join(m.store, "absent"), p); err != nil {
						t.Fatal(err)
					}
				default:
					text := "{}"
					if intent == "empty" {
						text = ""
					}
					laneControlPut(t, p, text)
					if intent == "inaccessible" {
						if err := os.Chmod(p, 0000); err != nil {
							t.Fatal(err)
						}
					}
				}
				before, _ := os.ReadFile(path)
				if _, err := m.readLaneControlsLocked(); !errors.Is(err, errLaneControlUnavailable) {
					t.Fatal("unresolved intent opened admission")
				}
				after, _ := os.ReadFile(path)
				if !bytes.Equal(before, after) {
					t.Fatal("read reconciled state")
				}
				if _, err := os.Lstat(p); err != nil {
					t.Fatal("read removed intent")
				}
			})
		}
	}
}

func TestLaneControlDurableSnapshotAndRetention(t *testing.T) {
	c, store := fixture(t)
	store, err := filepath.EvalSymlinks(filepath.Dir(store))
	if err != nil {
		t.Fatal(err)
	}
	store = filepath.Join(store, "store")
	m, err := NewManager(c, store)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	next := laneControlState(1, "codex", "removed")
	lock, err := m.controlLock()
	if err != nil {
		t.Fatal(err)
	}
	err = m.saveLaneControlsLocked(next)
	lock.Close()
	if err != nil {
		t.Fatal(err)
	}
	first, err := m.laneControls()
	if err != nil {
		t.Fatal(err)
	}
	if first.Version != 1 || first.Revision != 1 || len(first.Lanes) != 3 {
		t.Fatal("configured and retained union")
	}
	if first.Lanes[0].Name != "claude" || first.Lanes[0].Paused || !first.Lanes[0].Configured || first.Lanes[1].Name != "codex" || !first.Lanes[1].Paused || first.Lanes[2].Name != "removed" || first.Lanes[2].Configured || !first.Lanes[2].Paused {
		t.Fatal("deterministic exact-name controls")
	}
	fresh, err := NewManager(c, store)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	second, err := fresh.laneControls()
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatal("fresh Manager lost durable snapshot")
	}
	// An enabled record still carries its name, source, time and revision.
	next.Revision = 2
	r := next.Lanes["codex"]
	r.Paused = false
	r.Revision = 2
	r.Source = "web"
	r.Since = "2026-10-11T12:01:00Z"
	next.Lanes["codex"] = r
	lock, err = m.controlLock()
	if err != nil {
		t.Fatal(err)
	}
	err = m.saveLaneControlsLocked(next)
	lock.Close()
	if err != nil {
		t.Fatal(err)
	}
	after, err := fresh.laneControls()
	if err != nil || after.Revision != 2 || after.Lanes[1].Paused || after.Lanes[1].Revision != 2 || after.Lanes[2].Revision != 1 {
		t.Fatal("read cached state or erased enabled revision")
	}
	// Config removal and re-add, even a changed kind, preserve exact identity.
	fresh.mu.Lock()
	fresh.config.Lanes = []Lane{{Name: "removed", Kind: "opencode"}}
	fresh.mu.Unlock()
	after, err = fresh.laneControls()
	if err != nil || len(after.Lanes) != 2 || after.Lanes[0].Configured || !after.Lanes[1].Configured || !after.Lanes[1].Paused {
		t.Fatal("retained identity changed with configuration")
	}
	st, err := os.Stat(filepath.Join(store, "lane-control.json"))
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("control file is not private")
	}
	st, err = os.Stat(store)
	if err != nil || st.Mode().Perm() != 0700 {
		t.Fatal("control directory is not private")
	}
	if !reflect.DeepEqual(m.config, c) {
		t.Fatal("controls altered configuration")
	}
}

func TestLaneControlCapacityWithoutEviction(t *testing.T) {
	m, path := laneControlFixture(t)
	state := laneControlState(1)
	for i := 0; i < laneControlMaxNames; i++ {
		state.Lanes[fmt.Sprintf("lane-%03d", i)] = laneControlState(1, "x").Lanes["x"]
	}
	if err := m.saveLaneControlsLocked(state); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	state.Revision = 2
	state.Lanes["overflow"] = laneControlState(2, "x").Lanes["x"]
	if err = m.saveLaneControlsLocked(state); !errors.Is(err, errLaneControlCapacity) {
		t.Fatal("capacity did not refuse")
	}
	delete(state.Lanes, "lane-000")
	if err = m.saveLaneControlsLocked(state); !errors.Is(err, errLaneControlUnavailable) {
		t.Fatal("capacity evicted a retained hold")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("refusal changed durable state")
	}
	delete(state.Lanes, "overflow")
	state.Lanes["lane-000"] = laneControlState(1, "x").Lanes["x"]
	r := state.Lanes["lane-001"]
	r.Paused = false
	r.Revision = 2
	state.Lanes["lane-001"] = r
	if err = m.saveLaneControlsLocked(state); err != nil {
		t.Fatal("existing name unusable at capacity")
	}
	loaded, err := m.readLaneControlsLocked()
	if err != nil || len(loaded.Lanes) != 256 || loaded.Lanes["lane-001"].Paused || !loaded.Lanes["lane-000"].Paused {
		t.Fatal("capacity retention")
	}
}

func TestLaneControlMonotonicRevision(t *testing.T) {
	for _, kind := range []string{"same", "backward", "skip", "overflow", "erased", "old_record_revision"} {
		t.Run(kind, func(t *testing.T) {
			m, path := laneControlFixture(t)
			old := laneControlState(2, "codex")
			if kind == "overflow" {
				old = laneControlState(laneControlMaxRevision, "codex")
			}
			b, err := json.Marshal(old)
			if err != nil {
				t.Fatal(err)
			}
			laneControlPut(t, path, string(b))
			next := laneControlState(old.Revision+1, "codex")
			switch kind {
			case "same":
				next.Revision = old.Revision
				next.Lanes["codex"] = old.Lanes["codex"]
			case "backward":
				next = laneControlState(1, "codex")
			case "skip":
				next = laneControlState(4, "codex")
			case "erased":
				delete(next.Lanes, "codex")
			case "old_record_revision":
				r := old.Lanes["codex"]
				r.Paused = false
				next.Lanes["codex"] = r
			}
			if err = m.saveLaneControlsLocked(next); !errors.Is(err, errLaneControlUnavailable) {
				t.Fatal("invalid transition accepted")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(b, after) {
				t.Fatal("revision refusal rewrote state")
			}
		})
	}
}

func TestLaneControlCodecRoundTrip(t *testing.T) {
	state := laneControlState(4, "Codex", "codex", strings.Repeat("a", 64))
	r := state.Lanes["Codex"]
	r.Source = "mcp"
	r.Since = "2026-10-11T12:00:00.123456789Z"
	r.RequestedAfterCurrent = true
	state.Lanes["Codex"] = r
	b, err := encodeLaneControls(state)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeLaneControls(b)
	if err != nil || !reflect.DeepEqual(got, state) {
		t.Fatal("codec changed exact names or values")
	}
	// The byte ceiling includes whitespace, and its exact boundary is accepted.
	bounded := append([]byte(laneControlSample), bytes.Repeat([]byte(" "), laneControlMaxBytes-len(laneControlSample))...)
	if _, err = decodeLaneControls(bounded); err != nil {
		t.Fatal("exact byte boundary refused")
	}
	if _, err = decodeLaneControls(append(bounded, ' ')); !errors.Is(err, errLaneControlUnavailable) {
		t.Fatal("byte ceiling not enforced")
	}
	state.Lanes["bad/name"] = r
	if _, err = encodeLaneControls(state); !errors.Is(err, errLaneControlUnavailable) {
		t.Fatal("encoder wrote invalid state")
	}
}

func TestLaneControlReadLockAndNoMutation(t *testing.T) {
	m, path := laneControlFixture(t)
	if err := os.Mkdir(filepath.Join(m.store, "locks"), 0700); err != nil {
		t.Fatal(err)
	}
	// Startup/mutation prepares coordination; projection only opens it.
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		t.Fatal(err)
	}
	startup, err := laneControlLock(root, syscall.LOCK_EX)
	root.Close()
	if err != nil {
		t.Fatal(err)
	}
	startup.Close()
	initial, err := m.laneControls()
	if err != nil || initial.Version != 1 || initial.Revision != 0 || len(initial.Lanes) != 2 {
		t.Fatal("missing initial snapshot")
	}
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("snapshot created state")
	}
	lock, err := m.controlLock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.laneControls(); !errors.Is(err, errLaneControlUnavailable) {
		lock.Close()
		t.Fatal("contended read ignored shared control lock")
	}
	// Already-locked readers do not recursively flock. This is H2's admission seam.
	if _, err = m.readLaneControlsLocked(); err != nil {
		lock.Close()
		t.Fatal("locked read reacquired control lock")
	}
	lock.Close()
	if err = m.saveLaneControlsLocked(laneControlState(1, "codex")); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err = m.laneControls(); err != nil {
			t.Fatal(err)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("read changed state")
	}
	current, err := os.Stat(path)
	if err != nil || !current.ModTime().Equal(st.ModTime()) {
		t.Fatal("read rewrote state")
	}
}

func TestLaneControlUnsafeLocks(t *testing.T) {
	for _, kind := range []string{"linked_directory", "linked_file", "fifo", "public_file", "inaccessible_file"} {
		t.Run(kind, func(t *testing.T) {
			m, _ := laneControlFixture(t)
			locks := filepath.Join(m.store, "locks")
			if kind == "linked_directory" {
				if err := os.Symlink(t.TempDir(), locks); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(locks, 0700); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(locks, "control.lock")
				switch kind {
				case "linked_file":
					if err := os.Symlink(filepath.Join(m.store, "absent"), path); err != nil {
						t.Fatal(err)
					}
				case "fifo":
					if err := syscall.Mkfifo(path, 0600); err != nil {
						t.Fatal(err)
					}
				default:
					laneControlPut(t, path, "")
					mode := os.FileMode(0644)
					if kind == "inaccessible_file" {
						mode = 0000
					}
					if err := os.Chmod(path, mode); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := m.laneControls(); !errors.Is(err, errLaneControlUnavailable) {
				t.Fatal("unsafe shared lock opened")
			}
		})
	}
}

func TestLaneControlUnsafeWritePreservesTarget(t *testing.T) {
	for _, kind := range []string{"linked_root", "linked_ancestor", "linked_file", "hardlink", "truncated", "public_file"} {
		t.Run(kind, func(t *testing.T) {
			m, path := laneControlFixture(t)
			laneControlPut(t, path, laneControlSample)
			if kind == "linked_file" {
				target := filepath.Join(m.store, "target")
				if err := os.Rename(path, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			} else if kind == "linked_root" || kind == "linked_ancestor" {
				parent, err := filepath.EvalSymlinks(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				link := filepath.Join(parent, "link")
				if kind == "linked_root" {
					if err = os.Symlink(m.store, link); err != nil {
						t.Fatal(err)
					}
					m.store = link
				} else {
					if err = os.Symlink(filepath.Dir(m.store), link); err != nil {
						t.Fatal(err)
					}
					m.store = filepath.Join(link, filepath.Base(m.store))
				}
			} else if kind == "hardlink" {
				if err := os.Link(path, filepath.Join(m.store, "alias")); err != nil {
					t.Fatal(err)
				}
			} else if kind == "truncated" {
				laneControlPut(t, path, "{")
			} else {
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err = m.saveLaneControlsLocked(laneControlState(2, "codex")); !errors.Is(err, errLaneControlUnavailable) {
				t.Fatal("unsafe write accepted")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("unsafe write altered target")
			}
		})
	}
}

func TestLaneControlDecodeCapacity(t *testing.T) {
	s := laneControlState(1)
	for i := 0; i <= laneControlMaxNames; i++ {
		s.Lanes[fmt.Sprintf("lane-%03d", i)] = laneControlState(1, "x").Lanes["x"]
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > laneControlMaxBytes {
		t.Fatal("fixture tests name capacity independently of byte capacity")
	}
	if _, err = decodeLaneControls(b); !errors.Is(err, errLaneControlUnavailable) {
		t.Fatal("decoder accepted retained-name overflow")
	}
}
