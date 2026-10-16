package nuntius

import (
	"context"
	"errors"
	"os"
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
	return m.requestStopContext(context.Background(), r, reason, signalRun)
}

func (m *Manager) requestStopContext(ctx context.Context, r *Run, reason string, signal func(Run, syscall.Signal) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !sameProcess(*r) {
		return errors.New("process identity cannot be verified; no signal sent")
	}
	// StopAt must be durable before SIGINT, but an independent supervisor must
	// not overtake that signal while the save or caller is descheduled. A run's
	// private flock covers only this boundary, independently of m.mu/control.lock.
	lock, err := m.stopSignalLock(ctx, r.ID, controlLockWait)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := ctx.Err(); err != nil {
		return err
	}
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
	if err := ctx.Err(); err != nil {
		return &operationOutcome{Code: "stop_recorded", RunID: r.ID, cause: err}
	}
	if err := signal(*r, syscall.SIGINT); err != nil {
		return &operationOutcome{Code: "stop_recorded", RunID: r.ID, cause: err}
	}
	return nil
}

func (m *Manager) stopSignalLock(parent context.Context, id string, wait time.Duration) (*os.File, error) {
	ctx, cancel := context.WithTimeout(parent, wait)
	defer cancel()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if !runPattern.MatchString(id) {
		return nil, errLaneControlUnavailable
	}
	root, err := os.OpenRoot(m.store)
	if err != nil {
		return nil, errLaneControlUnavailable
	}
	defer root.Close()
	f, err := root.OpenFile("runs/"+id+"/stop.lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, errLaneControlUnavailable
	}
	var st syscall.Stat_t
	if syscall.Fstat(int(f.Fd()), &st) != nil || st.Mode&syscall.S_IFMT != syscall.S_IFREG || st.Mode&0077 != 0 || st.Nlink != 1 {
		f.Close()
		return nil, errLaneControlUnavailable
	}
	if err = boundedFlock(int(f.Fd()), syscall.LOCK_EX, wait, ctx); err != nil {
		f.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &laneControlFault{Code: "locked"}
	}
	// Close releases only this open description; never unlock another caller.
	return f, nil
}
