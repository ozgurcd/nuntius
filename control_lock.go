package nuntius

import (
	"context"
	"errors"
	"math/rand/v2"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

const (
	controlLockWait = 250 * time.Millisecond
	// Public dispatch/reply can bridge a brief transaction in another process.
	// Primitive and native reconciliation budgets remain independent.
	dispatchControlLockWait = 2 * time.Second
)

// Every control.lock entry point shares one process-local owner per inode.
// The *Locked helpers execute under that owner's guard; they do not acquire it
// again. An accidental nested public acquisition refuses within its deadline,
// rather than opening a second flock description that waits on its own process.
// Shared readers also serialize locally, so a nested close cannot drop a guard.
type controlLockIdentity struct{ device, inode uint64 }
type controlLockOwner struct {
	identity controlLockIdentity
	done     chan struct{}
	slot     *atomic.Pointer[controlLockOwner]
}

// Exact identity lookup with a finite entry cap. Release clears its stable
// atomic slot without taking the registry mutex; empty bookkeeping slots are
// reclaimed at capacity. Only acquisition waits for the bounded registry gate.
var processControlLocks = struct {
	sync.Mutex
	owners map[controlLockIdentity]*atomic.Pointer[controlLockOwner]
}{owners: make(map[controlLockIdentity]*atomic.Pointer[controlLockOwner])}

func reserveControlOwner(ctx context.Context, identity controlLockIdentity) (*controlLockOwner, *controlLockOwner, error) {
	registryCtx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	err := lockContext(registryCtx, &processControlLocks.Mutex)
	cancel()
	if err != nil {
		return nil, nil, &laneControlFault{Code: "locked", local: true}
	}
	defer processControlLocks.Unlock()
	slot := processControlLocks.owners[identity]
	if slot != nil {
		if held := slot.Load(); held != nil {
			return nil, held, nil
		}
	} else {
		if len(processControlLocks.owners) >= 4096 {
			for id, entry := range processControlLocks.owners {
				if ctx.Err() != nil {
					return nil, nil, &laneControlFault{Code: "locked"}
				}
				if entry.Load() == nil {
					delete(processControlLocks.owners, id)
				}
			}
			if len(processControlLocks.owners) >= 4096 {
				return nil, nil, &laneControlFault{Code: "locked", local: true}
			}
		}
		slot = &atomic.Pointer[controlLockOwner]{}
		processControlLocks.owners[identity] = slot
	}
	if ctx.Err() != nil {
		return nil, nil, &laneControlFault{Code: "locked"}
	}
	owner := &controlLockOwner{identity: identity, done: make(chan struct{}), slot: slot}
	slot.Store(owner)
	return owner, nil, nil
}

func acquireControlLock(root *os.File, mode int, create bool, wait time.Duration, contexts ...context.Context) (*controlFileLock, error) {
	site := 0
	if mode == unix.LOCK_SH {
		site = 1
	} else if wait == dispatchControlLockWait {
		site = 2
	}
	began := time.Now()
	defer func() { controlMetric(site, 0, time.Since(began)) }()
	ctx := context.Background()
	if len(contexts) != 0 {
		ctx = contexts[0]
	}
	if wait > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, wait)
		defer cancel()
	}
	deadline := time.Now().Add(wait)
	for {
		if ctx.Err() != nil {
			return nil, &laneControlFault{Code: "locked"}
		}
		lock, err := openLaneControlLockFile(root, create)
		if err != nil {
			return nil, err
		}
		var st unix.Stat_t
		if unix.Fstat(int(lock.Fd()), &st) != nil {
			lock.File.Close()
			return nil, errLaneControlUnavailable
		}
		identity := controlLockIdentity{uint64(st.Dev), uint64(st.Ino)}
		owner, held, reserveErr := reserveControlOwner(ctx, identity)
		if reserveErr != nil {
			lock.File.Close()
			return nil, reserveErr
		}
		if owner == nil {
			// This descriptor never owned a flock. Close it without LOCK_UN.
			lock.File.Close()
			remaining := time.Until(deadline)
			if wait == 0 || remaining <= 0 {
				return nil, &laneControlFault{Code: "locked", local: true}
			}
			if held == nil {
				continue
			} // released between CAS and observation
			timer := time.NewTimer(remaining)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, &laneControlFault{Code: "locked", local: true}
			case <-held.done:
				timer.Stop()
			case <-timer.C:
				return nil, &laneControlFault{Code: "locked", local: true}
			}
			continue
		}
		lock.owner = owner
		err = boundedFlock(int(lock.Fd()), mode, max(0, time.Until(deadline)), ctx)
		if err == nil {
			lock.acquired = true
			lock.metricSite, lock.acquiredAt = site, time.Now()
			return lock, nil
		}
		lock.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || ctx.Err() != nil {
			return nil, &laneControlFault{Code: "locked"}
		}
		return nil, errLaneControlUnavailable
	}
}

// Audit append runs under some control guards. Its separate inode must not
// introduce an unbounded wait while the process owns the coordination lock.
func boundedFlock(fd, mode int, wait time.Duration, contexts ...context.Context) error {
	ctx := context.Background()
	if len(contexts) != 0 {
		ctx = contexts[0]
	}
	deadline := time.Now().Add(wait)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := unix.Flock(fd, mode|unix.LOCK_NB)
		if err == nil || !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			return err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return unix.EWOULDBLOCK
		}
		timer := time.NewTimer(min(time.Duration(5+rand.IntN(11))*time.Millisecond, remaining))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
