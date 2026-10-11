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
	"syscall"
	"testing"
)

const laneControlSample = `{"lane_control_version":1,"revision":1,"lanes":{"codex":{"paused":true,"since":"2026-10-11T12:00:00Z","source":"cli","requested_after_current":false,"revision":1}}}`

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
