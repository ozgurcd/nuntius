package nuntius

import (
	"context"
	"errors"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// This value never leaves a synchronous dispatch holding m.mu and the common
// EX flock. It is not a cache: dropping that guard invalidates the decision.
type dispatchAdmission struct {
	manager *Manager
	guard   *controlFileLock
	lane    string
	ctx     context.Context
	used    bool
}

type controlWaitKey struct{}
type controlWaitBudget struct{ remaining time.Duration }

// Private to one synchronous request: recovery and admission debit only their
// acquisition waits. No decision scan consumes D_req, and no worker shares it.
func controlWaitContext(parent context.Context) (context.Context, func()) {
	budget, _ := parent.Value(controlWaitKey{}).(*controlWaitBudget)
	if budget == nil {
		budget = &controlWaitBudget{remaining: dispatchControlLockWait}
	}
	ctx, cancel := context.WithTimeout(parent, max(0, budget.remaining))
	began, finished := time.Now(), false
	return ctx, func() {
		if finished {
			return
		}
		finished = true
		budget.remaining = max(0, budget.remaining-time.Since(began))
		cancel()
	}
}

func (m *Manager) owningContext() context.Context {
	if m.life != nil {
		return m.life
	}
	return context.Background()
}

// Caller holds m.mu. Restore the shared injected launcher before unlocking.
func (m *Manager) bindLauncherContext(ctx context.Context) func() {
	original := m.launcher
	if original != nil {
		copy := *original
		copy.ctx = ctx
		m.launcher = &copy
	}
	return func() { m.launcher = original }
}

func (a *dispatchAdmission) consume(m *Manager, lane string) error {
	if a == nil || a.manager != m || a.lane != lane || a.ctx == nil || a.guard == nil || a.used {
		return errLaneControlUnavailable
	}
	if a.ctx.Err() != nil || a.guard.closed.Load() || !a.guard.use.CompareAndSwap(0, 1) {
		return &laneControlFault{Code: "locked"}
	}
	defer a.guard.releaseUse()
	g := a.guard
	if g.closed.Load() || !g.acquired || g.File == nil || g.owner == nil {
		return errLaneControlUnavailable
	}
	if checkLaneControlFile(int(g.File.Fd())) != nil {
		return errLaneControlUnavailable
	}
	if a.ctx.Err() != nil {
		return &laneControlFault{Code: "locked"}
	}
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		return errLaneControlUnavailable
	}
	if a.ctx.Err() != nil {
		root.Close()
		return &laneControlFault{Code: "locked"}
	}
	current, err := openLaneControlLockFile(root, false)
	root.Close()
	if err != nil {
		return errLaneControlUnavailable
	}
	defer current.Close()
	var held, fresh unix.Stat_t
	if a.ctx.Err() != nil {
		return &laneControlFault{Code: "locked"}
	}
	if unix.Fstat(int(g.File.Fd()), &held) != nil {
		return errLaneControlUnavailable
	}
	if a.ctx.Err() != nil {
		return &laneControlFault{Code: "locked"}
	}
	if unix.Fstat(int(current.Fd()), &fresh) != nil || held.Dev != fresh.Dev || held.Ino != fresh.Ino || uint64(held.Dev) != g.owner.identity.device || held.Ino != g.owner.identity.inode {
		return errLaneControlUnavailable
	}
	if a.ctx.Err() != nil {
		return &laneControlFault{Code: "locked"}
	}
	a.used = true
	return nil
}

// No helper goroutine is needed to make mutex waiters cancellable.
func lockContext(ctx context.Context, mu *sync.Mutex, sites ...int) error {
	// A lifecycle context can be cancellable yet have no deadline. Each lock
	// wait still gets a finite budget; filesystem decisions keep their parent.
	if _, bounded := ctx.Deadline(); !bounded {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, controlLockWait)
		defer cancel()
	}
	site := 3
	if len(sites) > 1 || len(sites) == 1 && (sites[0] < 0 || sites[0] >= len(controlHistograms)) {
		return errLaneControlUnavailable
	}
	if len(sites) == 1 {
		site = sites[0]
	}
	began := time.Now()
	defer func() { controlMetric(site, 0, time.Since(began)) }()
	if err := ctx.Err(); err != nil {
		return err
	}
	if mu.TryLock() {
		return nil
	}
	timer := time.NewTicker(time.Millisecond)
	defer timer.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if mu.TryLock() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (m *Manager) DispatchContext(ctx context.Context, lane, goal, text string) (string, error) {
	return m.startContext(ctx, lane, goal, text, false)
}

func (m *Manager) ReplyContext(ctx context.Context, lane, text string) (string, error) {
	return m.startContext(ctx, lane, "", text, true)
}

// The durable stop is the authority, also when acknowledgement or SIGINT fails.
type operationOutcome struct {
	Code, RunID string
	cause       error
}

func (e *operationOutcome) Error() string {
	return e.Code + "; reconcile status/history before retrying"
}
func (e *operationOutcome) Unwrap() error { return e.cause }

func (m *Manager) CancelContext(parent context.Context, name string) (err error) {
	ctx, cancel := context.WithTimeout(parent, dispatchControlLockWait)
	defer cancel()
	goal, recorded := "", false
	defer func() {
		if err != nil && !recorded {
			_ = m.appendAuditContext(ctx, false, "nuntius_cancel", name, goal, "", "refused", err.Error())
		}
	}()
	if err := lockContext(ctx, &m.mu); err != nil {
		return &laneControlFault{Code: "locked", local: true}
	}
	heldAt := time.Now()
	defer func() { controlMetric(3, 1, time.Since(heldAt)); m.mu.Unlock() }()
	if _, err := m.lane(name); err != nil {
		return err
	}
	guard, err := m.admissionControlLockWithin(dispatchControlLockWait, ctx)
	if err != nil {
		return err
	}
	defer guard.Close()
	r, err := m.latestContext(ctx, name)
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return &laneControlFault{Code: "locked"}
		}
		return err
	}
	if r == nil || r.State != "running" {
		return errors.New("lane is not running")
	}
	goal = r.GoalID
	if ctx.Err() != nil {
		return &laneControlFault{Code: "locked"}
	}
	err = m.requestStopContext(ctx, r, "cancelled", signalRun)
	// Re-read: a failed save must never be mistaken for durable acceptance.
	current, readErr := loadRun(m.store+"/runs/"+r.ID, r.ID)
	recorded = readErr == nil && current != nil && current.StopAt != nil
	if readErr == nil && current != nil && current.StopAt != nil && (err != nil || ctx.Err() != nil) {
		return &operationOutcome{Code: "stop_recorded", RunID: r.ID, cause: err}
	}
	if err != nil {
		if !recorded && (ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
			return &laneControlFault{Code: "locked"}
		}
		return err
	}
	if err = m.appendAuditContext(ctx, false, "nuntius_cancel", name, r.GoalID, "", "accepted"); err != nil {
		return &operationOutcome{Code: "stop_recorded", RunID: r.ID, cause: err}
	}
	return nil
}

// Escalation changes no durable state and does not wait on either writer lock.
func (m *Manager) escalate(ctx context.Context, expected Run, grace time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	r, err := loadRun(m.store+"/runs/"+expected.ID, expected.ID)
	if err != nil || r == nil || r.PID != expected.PID || r.ProcessStart != expected.ProcessStart || r.Lane != expected.Lane || r.StopAt == nil || time.Since(*r.StopAt) < grace || ctx.Err() != nil {
		return false
	}
	lock, err := m.stopSignalLock(ctx, expected.ID, 50*time.Millisecond)
	if err != nil {
		return false
	}
	defer lock.Close()
	r, err = loadRun(m.store+"/runs/"+expected.ID, expected.ID)
	if err != nil || r == nil || r.PID != expected.PID || r.ProcessStart != expected.ProcessStart || r.Lane != expected.Lane || r.StopAt == nil || time.Since(*r.StopAt) < grace || ctx.Err() != nil {
		return false
	}
	return signalRun(*r, syscall.SIGKILL) == nil
}
