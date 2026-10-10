package nuntius

import (
	"os"
	"path/filepath"
	"time"
)

// checkStuck requires the manager and control locks. Delivery is durable per
// run, independently of acknowledgement of the coalesced lane/goal signal.
func (m *Manager) checkStuck(r *Run, now time.Time) (bool, error) {
	if r.State != "running" {
		return false, nil
	}
	last := r.Started
	if fi, err := os.Lstat(filepath.Join(m.store, "runs", r.ID, "events.jsonl")); err == nil && fi.Mode().IsRegular() {
		last = fi.ModTime()
	}
	stuck := now.Sub(last) > 15*time.Minute
	if fi := m.currentReport(*r); fi != nil && now.Sub(fi.ModTime()) > 5*time.Minute {
		stuck = true
	}
	if !stuck || r.StuckSignalled {
		return stuck, nil
	}
	if err := m.emitSignal(*r, "stuck"); err != nil {
		return true, err
	}
	r.StuckSignalled = true
	if err := m.save(r); err != nil {
		return true, err
	}
	return true, nil
}
