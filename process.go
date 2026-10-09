package nuntius

import (
	"errors"
	"syscall"
	"time"
)

func sameProcess(r Run) bool {
	if r.PID <= 1 || r.ProcessStart == "" {
		return false
	}
	start, e := processStart(r.PID)
	if e != nil || start != r.ProcessStart {
		return false
	}
	pg, e := syscall.Getpgid(r.PID)
	return e == nil && pg == r.PID
}

func signalRun(r Run, sig syscall.Signal) error {
	if !sameProcess(r) {
		return errors.New("process identity cannot be verified; no signal sent")
	}
	return syscall.Kill(-r.PID, sig)
}

func (m *Manager) requestStop(r *Run, reason string) error {
	if !sameProcess(*r) {
		return errors.New("process identity cannot be verified; no signal sent")
	}
	if r.StopAt == nil {
		now := time.Now().UTC()
		r.StopAt = &now
		r.StopReason = reason
		if e := m.save(r); e != nil {
			return e
		}
	}
	return signalRun(*r, syscall.SIGINT)
}
