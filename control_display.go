package nuntius

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Receipt/control writes can come from another Manager in this process.
// One fixed counter invalidates local caches without any durable sidecar.
var controlDisplayChange atomic.Uint64

const displayCacheBudget = 8 << 20

// Conservative retained-size accounting includes every field, string, slice
// capacity and map entry in this fixed, acyclic DTO. Oversize results remain
// usable by this caller but are never retained. Stop at the budget/deadline.
func displayFitsBudget(ctx context.Context, snapshot displaySnapshot) bool {
	remaining := displayCacheBudget - 4096 // cache headers and fixed-format stamps
	var visit func(reflect.Value, int) bool
	visit = func(v reflect.Value, depth int) bool {
		if !v.IsValid() {
			return true
		}
		if depth > 32 || ctx.Err() != nil {
			return false
		}
		remaining -= int(v.Type().Size())
		if remaining < 0 {
			return false
		}
		switch v.Kind() {
		case reflect.String:
			remaining -= v.Len()
		case reflect.Pointer, reflect.Interface:
			if !v.IsNil() && !visit(v.Elem(), depth+1) {
				return false
			}
		case reflect.Struct:
			for i := 0; i < v.NumField(); i++ {
				if !visit(v.Field(i), depth+1) {
					return false
				}
			}
		case reflect.Slice, reflect.Array:
			if v.Kind() == reflect.Slice {
				remaining -= v.Cap() * int(v.Type().Elem().Size())
			}
			if remaining < 0 {
				return false
			}
			for i := 0; i < v.Len(); i++ {
				if !visit(v.Index(i), depth+1) {
					return false
				}
			}
		case reflect.Map:
			remaining -= 256 * (v.Len() + 1)
			if remaining < 0 {
				return false
			}
			it := v.MapRange()
			for it.Next() {
				if !visit(it.Key(), depth+1) || !visit(it.Value(), depth+1) {
					return false
				}
			}
		}
		return remaining >= 0
	}
	return visit(reflect.ValueOf(snapshot), 0)
}

type displaySnapshot struct {
	held     webHeldSnapshot
	rows     []Status
	controls laneControlSnapshot
	runs     []Run
	config   Config
	freeze   *FreezeState
}
type displayCache struct {
	mu    sync.Mutex
	until time.Time
	value displaySnapshot
	stamp [6]string
}

// A caller fills the cache inline. Waiters keep their own cancellation budget.
func (m *Manager) displaySnapshot(parent context.Context) (displaySnapshot, error) {
	ctx, cancel := context.WithTimeout(parent, time.Second)
	defer cancel()
	c := &m.display
	if err := lockContext(ctx, &c.mu); err != nil {
		return displaySnapshot{}, err
	}
	defer c.mu.Unlock()
	stamp := m.displayStamp(ctx)
	if ctx.Err() != nil {
		return displaySnapshot{}, ctx.Err()
	}
	if time.Now().Before(c.until) && stamp == c.stamp {
		return cloneDisplay(c.value), nil
	}
	v, err := m.readDisplay(ctx)
	c.value, c.until, c.stamp = displaySnapshot{}, time.Time{}, [6]string{}
	if err == nil && displayFitsBudget(ctx, v) {
		c.value, c.until = v, time.Now().Add(time.Second)
		c.stamp = stamp
	}
	return cloneDisplay(v), err
}

func cloneDisplay(s displaySnapshot) displaySnapshot {
	s.held.Messages = slices.Clone(s.held.Messages)
	s.rows = append([]Status(nil), s.rows...)
	s.runs = append([]Run(nil), s.runs...)
	return s
}

func (m *Manager) displayStamp(ctx context.Context) (stamp [6]string) {
	for i, name := range []string{"lane-control.json", "lane-control-intent.json", "freeze.json", "runs"} {
		if ctx.Err() != nil {
			return
		}
		if st, err := os.Lstat(filepath.Join(m.store, name)); err == nil {
			stamp[i] = st.ModTime().String()
		}
	}
	stamp[4] = strconv.FormatUint(m.displayChange.Load(), 10) + ":" + strconv.FormatUint(controlDisplayChange.Load(), 10)
	if st, err := os.Lstat(m.displayConfig.SourcePath); err == nil {
		stamp[5] = st.ModTime().String()
	}
	return
}

func (m *Manager) readDisplay(ctx context.Context) (displaySnapshot, error) {
	config := m.displayConfig
	if m.mu.TryLock() {
		config = cloneDisplayConfig(m.config)
		m.mu.Unlock()
	}
	reader, err := newReadManager(config, m.store)
	if err != nil {
		return displaySnapshot{}, err
	}
	defer reader.cancelLife()
	reader.metadataSource = m.metadataCache()
	if ctx.Err() != nil {
		return displaySnapshot{}, ctx.Err()
	}
	if err = reader.reload(); err != nil {
		return displaySnapshot{}, err
	}
	// Preserve the dashboard's existing config reload for independent readers
	// such as Queue, without waiting for a serving operation's mutex.
	if m.mu.TryLock() {
		if m.config.SourcePath != "" && m.config.SourcePath == reader.config.SourcePath {
			m.config = cloneDisplayConfig(reader.config)
		}
		m.mu.Unlock()
	}
	root, err := openLaneControlRoot(m.store)
	before := laneControlStore{Version: 1, Lanes: map[string]laneControlRecord{}}
	controlErr := err
	if root != nil {
		defer root.Close()
		// Validate the coordination inode's existence and shape, never acquire it.
		lock, lockErr := openLaneControlLockFile(root, false)
		if lockErr != nil {
			controlErr = lockErr
		} else {
			_ = lock.File.Close()
			before, controlErr = readLaneControlsAt(root)
		}
	}
	runs, err := reader.historyContext(ctx)
	if os.IsNotExist(err) {
		runs, err = []Run{}, nil
	}
	if err != nil {
		return displaySnapshot{}, err
	}
	held, heldErr := map[string]bool{}, controlErr
	if root != nil {
		held, heldErr = heldMessagesByLane(ctx, root, runs, m.metadataCache())
	}
	controls := reader.projectLaneControlsHeld(before, runs, controlErr == nil, held, heldErr)
	after, afterErr := before, controlErr
	if root != nil {
		after, afterErr = readLaneControlsAt(root)
	}
	if controlErr != nil || afterErr != nil || before.Revision != after.Revision {
		for i := range controls.Lanes {
			v := &controls.Lanes[i]
			v.Phase, v.DispatchEligible = "unavailable", false
			if !slices.Contains(v.Blockers, "lane_control_unavailable") {
				v.Blockers = append(v.Blockers, "lane_control_unavailable")
			}
			v.Blockers = append(v.Blockers, "change_pending")
			v.Explanation = laneControlExplanation(*v) + "; change pending. Admission checks again."
		}
	}
	reader.observation = &displaySnapshot{runs: runs, controls: controls}
	rows, err := reader.statusContext(ctx, true)
	if err != nil {
		return displaySnapshot{}, err
	}
	heldView := reader.webHeldMessages(ctx, controls, runs)
	frozen, err := reader.freezeState()
	if ctx.Err() != nil {
		return displaySnapshot{}, ctx.Err()
	}
	return displaySnapshot{held: heldView, rows: rows, controls: controls, runs: runs, config: reader.config, freeze: frozen}, err
}

func cloneDisplayConfig(c Config) Config {
	b, _ := json.Marshal(c)
	var copy Config
	_ = json.Unmarshal(b, &copy)
	copy.SourcePath = c.SourcePath
	// Empty pools explicitly disable all lanes. omitempty must not turn this
	// into nil, which has different validation/default semantics.
	if c.Pools != nil && copy.Pools == nil {
		copy.Pools = map[string]AgentPool{}
	}
	if c.Lanes != nil && copy.Lanes == nil {
		copy.Lanes = []Lane{}
	}
	return copy
}

// Decode metadata once, then inspect only message authority in each run folder.
func heldMessagesByLane(ctx context.Context, root *os.File, runs []Run, caches ...*runMetadataCache) (map[string]bool, error) {
	out := map[string]bool{}
	if len(caches) > 1 || len(caches) == 1 && caches[0] == nil {
		return out, errLaneControlUnavailable
	}
	dir, err := heldDirectory(root, "runs")
	if err != nil {
		return out, err
	}
	defer dir.Close()
	for _, r := range runs {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		run, err := heldDirectory(dir, r.ID)
		if err != nil {
			return out, err
		}
		receipt, err := readMessageReceipt(run, r)
		if err == nil && receipt != nil && receipt.State != "reserved" {
			run.Close()
			continue
		}
		if err == nil {
			if len(caches) == 1 {
				_, err = caches[0].readMessage(ctx, run, r.ID)
			} else {
				_, err = readLaneControlPrivate(run, "message.json", 16384)
			}
			if errors.Is(err, os.ErrNotExist) {
				err = nil
			}
		}
		if err == nil && receipt != nil && receipt.State == "reserved" {
			out[r.Lane] = true
		}
		if err == nil {
			_, err = readLaneControlPrivate(run, "message-hold.json", 4096)
			if err == nil {
				out[r.Lane] = true
			}
			if errors.Is(err, os.ErrNotExist) {
				err = nil
			}
		}
		run.Close()
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// The policy part is shared by advisory display and guarded admission.
func lanePolicyBlockers(paused, frozen, freezeUnknown, held, heldUnknown bool) []string {
	b := []string{}
	if paused {
		b = append(b, "lane_paused")
	}
	if freezeUnknown {
		b = append(b, "freeze_unavailable")
	} else if frozen {
		b = append(b, "frozen")
	}
	if heldUnknown {
		b = append(b, "held_message_unavailable")
	} else if held {
		b = append(b, "held_message")
	}
	return b
}

func runEligibilityBlockers(r Run) []string {
	b := []string{}
	if r.State == "running" {
		b = append(b, "running")
	}
	if r.WaitingToResume {
		b = append(b, "waiting_to_resume")
	}
	return b
}

func (m *Manager) StatusViewContext(ctx context.Context) (any, error) {
	s, err := m.displaySnapshot(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]any{"frozen": s.freeze != nil, "freeze": s.freeze, "lanes": s.rows, "lane_control_version": 1, "lane_control": s.controls,
		"concurrency": map[string]any{"live": runtime.NumGoroutine(), "follow": m.followCount.Load(), "notify": "legacy, short-lived, unjoined", "http": "legacy, uncapped; P1", "new_goroutines": 0}, "control_metrics": controlMetricsSnapshot()}, nil
}

func (m *Manager) historyContext(ctx context.Context) ([]Run, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if m.readOnly && m.observation != nil {
		return slices.Clone(m.observation.runs), nil
	}
	m.historyScans.Add(1)
	entries, err := os.ReadDir(filepath.Join(m.store, "runs"))
	if err != nil {
		return nil, err
	}
	if len(entries) > 10000 {
		return nil, errors.New("run store exceeds 10000 entries")
	}
	return m.readHistoryEntries(ctx, entries)
}
