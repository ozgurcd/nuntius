package nuntius

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type idleGuardHost struct {
	now      func() time.Time
	ticks    <-chan time.Time
	snapshot func(time.Time) idleGuardSnapshot
	notifier idleGuardNotifier
	onFault  func()
}

// Only the explicit long-lived observe command calls this entry. Constructors,
// MCP clients, the dashboard and passive reads never become controllers.
func (m *Manager) startIdleGuard(ctx context.Context, host idleGuardHost) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.idleGuardStarted || ctx.Err() != nil || m.config.IdleGuard != "alert_only" {
		return errIdleGuard
	}
	if privateDir(m.store) != nil {
		m.idleGuardFault = "storage_unavailable"
		return errIdleGuard
	}
	o, err := newIdleGuard(m.store, host.notifier, idleGuardIO{})
	if err != nil {
		m.idleGuardFault = "observer_unavailable"
		return errIdleGuard
	}
	m.idleGuardStarted = true
	if host.now == nil {
		host.now = time.Now
	}
	if host.snapshot == nil {
		host.snapshot = m.idleGuardSnapshot
	}
	m.watcher.Go(func() {
		defer o.close()
		ticks := host.ticks
		if ticks == nil {
			timer := time.NewTicker(idleGuardPeriod)
			defer timer.Stop()
			ticks = timer.C
		}
		sample := func(at time.Time) bool {
			if err := o.sample(ctx, host.snapshot(at)); err != nil {
				m.mu.Lock()
				m.idleGuardFault = "persistence_unavailable"
				m.mu.Unlock()
				if host.onFault != nil {
					host.onFault()
				}
				return false
			}
			return true
		}
		if !sample(host.now()) {
			return
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-m.closing:
				return
			case at, ok := <-ticks:
				if !ok || !sample(at) {
					return
				}
			}
		}
	})
	return nil
}

func (m *Manager) idleGuardSnapshot(at time.Time) idleGuardSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := idleGuardSnapshot{At: at}
	if m.reload() != nil {
		s.Fault = "configuration_unavailable"
	}
	for _, l := range m.config.Lanes {
		s.Lanes = append(s.Lanes, idleGuardLane{ID: l.Name, Kind: l.Kind, Capacity: "unknown"})
	}
	// The queue reader is bounded and passive. Use a detached config copy so
	// it cannot acquire the parent mutex again or mix configuration revisions.
	reader := &Manager{config: m.config, store: m.store, readOnly: true}
	s.Queue = reader.queue()
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		s.Fault = "capacity_unavailable"
		return s
	}
	defer root.Close()
	guard, err := laneControlReadLock(root)
	if err != nil {
		s.Fault = "capacity_unavailable"
		return s
	}
	defer guard.Close()
	runs, err := m.history()
	if err != nil {
		s.Fault = "capacity_unavailable"
		return s
	}
	// history deliberately omits unparseable metadata and pending directories
	// for display. Such an inventory cannot prove free execution capacity.
	entries, err := os.ReadDir(filepath.Join(m.store, "runs"))
	if err != nil || len(entries) != len(runs) {
		s.Fault = "capacity_unavailable"
		return s
	}
	projection, err := replayGoalJournalRoot(root)
	if err != nil || projection.tornTail {
		s.Fault = "native_unavailable"
		return s
	}
	retained := map[string]bool{}
	for _, record := range projection.records {
		data, e := goalRecordMetadata(record)
		if e != nil {
			s.Fault = "native_unavailable"
			return s
		}
		if len(data.Dispatches) == 0 && (data.State == nativeGoalRunning || data.Attempt != nil && (!data.Attempt.Ended || data.Attempt.ActiveSuccessor || data.Attempt.LaunchUnconfirmed)) {
			s.Fault = "native_unavailable"
			return s
		}
		for _, d := range data.Dispatches {
			if d.Phase != "failed" && !goalAttemptReleased(data, d.RunID) {
				retained[d.Request.Lane] = true
			}
		}
	}
	controls := m.laneControlSnapshotLocked(runs, true)
	for i := range s.Lanes {
		l := &s.Lanes[i]
		l.Capacity = "free"
		for _, control := range controls.Lanes {
			if control.Name == l.ID {
				l.Controlled = control.Paused
				for _, code := range control.Blockers {
					switch code {
					case "lane_paused", "frozen":
						l.Controlled = true
					case "running": // The process identity is checked below.
					default:
						l.Capacity = "retained"
					}
				}
			}
		}
		if retained[l.ID] {
			l.Capacity = "retained"
		}
		for _, run := range runs {
			if run.Lane == l.ID {
				if run.State == "running" {
					if sameProcess(run) {
						l.Capacity = "running"
					} else {
						l.Capacity = "uncertain"
					}
				}
				if run.WaitingToResume || run.ContinueRunID != "" || run.NativeEnd != nil && run.NativeEnd.Held {
					if l.Capacity != "running" {
						l.Capacity = "recovery"
					}
				}
				break
			}
		}
		if l.Capacity == "free" && !idleGuardLaneFree(root, l.ID) {
			l.Capacity = "uncertain"
		}
	}
	return s
}

func idleGuardLaneFree(root *os.File, lane string) bool {
	fd, err := unix.Openat(int(root.Fd()), "locks", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return false
	}
	defer unix.Close(fd)
	lock, err := unix.Openat(fd, lane+".lock", unix.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return true
	}
	if err != nil {
		return false
	}
	defer unix.Close(lock)
	if checkLaneControlFile(lock) != nil || unix.Flock(lock, unix.LOCK_EX|unix.LOCK_NB) != nil {
		return false
	}
	return unix.Flock(lock, unix.LOCK_UN) == nil
}

type idleGuardHealth struct {
	Mode          string     `json:"mode"`
	Coverage      string     `json:"coverage"`
	Fault         string     `json:"fault,omitempty"`
	LastSample    *time.Time `json:"last_sample,omitempty"`
	OpenIncidents int        `json:"open_incidents"`
}

func readIdleGuardHealth(store string, at time.Time) idleGuardHealth {
	h := idleGuardHealth{Mode: "alert_only", Coverage: "unknown"}
	frames, err := idleGuardReadStore(store)
	if err != nil || len(frames) == 0 {
		h.Fault = "storage_unavailable"
		return h
	}
	f := frames[len(frames)-1]
	h.LastSample = &f.At
	h.Coverage = "known"
	if at.Before(f.At) || at.Sub(f.At) > 2*idleGuardPeriod {
		h.Coverage, h.Fault = "unknown", "heartbeat_unavailable"
	}
	if f.Fault != "" {
		h.Coverage, h.Fault = "unknown", f.Fault
	}
	if f.Sequence >= idleGuardMaxFrames {
		h.Coverage, h.Fault = "unknown", "storage_limit"
	}
	for _, p := range f.Lanes {
		if p.Category == idleGuardUnknown || len(p.UnknownCandidates) > 0 {
			h.Coverage = "unknown"
		}
		if p.IncidentID != "" && p.Open {
			h.OpenIncidents++
		}
	}
	return h
}

func uniqueIdleGuardMode(b []byte) bool {
	d := json.NewDecoder(bytes.NewReader(b))
	start, err := d.Token()
	if err != nil || start != json.Delim('{') {
		return false
	}
	seen := false
	for d.More() {
		t, err := d.Token()
		key, ok := t.(string)
		if err != nil || !ok {
			return false
		}
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return false
		}
		if strings.EqualFold(key, "idle_guard") {
			var mode string
			if key != "idle_guard" || seen || bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, &mode) != nil || mode != "" && mode != "alert_only" {
				return false
			}
			seen = true
		}
	}
	end, err := d.Token()
	return err == nil && end == json.Delim('}')
}
