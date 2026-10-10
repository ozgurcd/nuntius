package nuntius

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Recover is the only crash-recovery launch entry. Display commands never call
// it. The control lock serializes every chain transition across MCP instances.
func (m *Manager) Recover() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errors.New("server is closing")
	}
	guard, err := m.controlLock()
	if err != nil {
		return err
	}
	defer guard.Close()
	if err = m.reload(); err != nil {
		return err
	}
	runs, err := m.history()
	if err != nil {
		return err
	}
	// Repair a crash between child publication and parent-link publication.
	for _, r := range runs {
		if r.ParentRunID == "" {
			continue
		}
		parent, err := loadRun(filepath.Join(m.store, "runs", r.ParentRunID), r.ParentRunID)
		if err != nil {
			return err
		}
		if parent != nil && parent.ContinueRunID == "" {
			parent.ContinueRunID = r.ID
			if err = m.save(parent); err != nil {
				return err
			}
		}
		if r.RecoveryRun && !r.ResumedSignalled {
			if err = m.emitSignal(r, "resumed"); err != nil {
				return err
			}
			r.ResumedSignalled = true
			if err = m.save(&r); err != nil {
				return err
			}
		}
	}
	runs, err = m.history()
	if err != nil {
		return err
	}
	for i := range runs {
		r := &runs[i]
		if r.State != "running" && r.LaunchdLabel != "" && !r.LaunchdCleaned {
			if err = m.cleanupJob(r); err != nil {
				_ = m.audit("launchd_cleanup", r.Lane, r.GoalID, r.SHA256, "failed")
			} else if err = m.save(r); err != nil {
				return err
			}
		}
		if r.State == "running" {
			if err = m.pendingJob(r); err != nil {
				return err
			}
			// A live legacy PID with no identity is busy, but never signalled.
			if sameProcess(*r) || (r.ProcessStart == "" && processExists(r.PID)) {
				continue
			}
			supervisor, err := m.lock("supervisor-" + r.ID)
			if errors.Is(err, errBusy) {
				continue
			}
			if err != nil {
				return err
			}
			err = m.recoverEnded(r)
			_ = supervisor.Close()
			if err != nil {
				return err
			}
		}
		if r.State == "ended" && r.StopReason == "ended_no_report" && r.StopAt == nil && !m.reportExists(*r) {
			r.State = "interrupted"
			r.StopReason = "interrupted"
			if r.Brief == "" {
				f, err := privateFile(filepath.Join(m.store, "runs", r.ID, "goal.txt"), os.O_RDONLY)
				if err != nil {
					return err
				}
				b, err := readBrief(f)
				_ = f.Close()
				if err != nil {
					return err
				}
				r.Brief = string(b)
			}
			if err = m.save(r); err != nil {
				return err
			}
		}
		if r.State == "interrupted" {
			dirty := false
			if !r.InterruptedSignalled {
				if err = m.emitSignal(*r, "interrupted"); err != nil {
					return err
				}
				r.InterruptedSignalled = true
				dirty = true
			}
			waiting := r.ContinueRunID == "" && r.ResumeCount < 2 && (m.config.AutoResume == nil || *m.config.AutoResume)
			if waiting != r.WaitingToResume {
				r.WaitingToResume = waiting
				dirty = true
			}
			if dirty {
				if err = m.save(r); err != nil {
					return err
				}
			}
		}
	}
	if m.checkFrozen() != nil || (m.config.AutoResume != nil && !*m.config.AutoResume) {
		return nil
	}
	// Oldest interrupted goal first. A busy lane leaves every queued goal intact.
	sort.Slice(runs, func(i, j int) bool { return runs[i].Started.Before(runs[j].Started) })
	for _, r := range runs {
		if r.State != "interrupted" || r.ResumeCount >= 2 || r.ContinueRunID != "" || m.reportExists(r) {
			continue
		}
		current, err := loadRun(filepath.Join(m.store, "runs", r.ID), r.ID)
		if err != nil {
			return err
		}
		if current == nil || current.ContinueRunID != "" {
			continue
		}
		lane, err := m.lane(r.Lane)
		if err != nil {
			continue
		} // Removed lane stays visible in history.
		busy := false
		all, err := m.history()
		if err != nil {
			return err
		}
		for _, other := range all {
			if other.Lane == r.Lane && other.State == "running" && (sameProcess(other) || (other.ProcessStart == "" && processExists(other.PID))) {
				busy = true
				break
			}
		}
		if busy {
			continue
		}
		lock, err := m.lock(r.Lane)
		if errors.Is(err, errBusy) {
			continue
		}
		if err != nil {
			return err
		}
		session := r.SessionID
		if session == "" {
			session = m.session(r.ID)
		}
		if !sessionPattern.MatchString(session) {
			session = ""
		}
		when := time.Now().UTC()
		if r.Ended != nil {
			when = *r.Ended
		}
		note := fmt.Sprintf("You were interrupted by a restart or crash at %s. Some of your work may already be done. Re-measure the repositories (git status, git log, your claims, any scratch containers), keep what is committed, then finish the goal.\n\n", when.Format(time.RFC3339))
		id, launchErr := m.launch(lane, r.GoalID, note+r.Brief, session, lock, &r)
		err = launchErr
		if err != nil {
			_ = lock.Close()
			return err
		}
		child, err := loadRun(filepath.Join(m.store, "runs", id), id)
		if err != nil || child == nil {
			return errors.New("recovery child unavailable")
		}
		if err = m.emitSignal(*child, "resumed"); err != nil {
			return err
		}
		child.ResumedSignalled = true
		if err = m.save(child); err != nil {
			return err
		}
	}
	return nil
}
