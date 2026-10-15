package nuntius

import (
	"errors"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const controlLockWait = 250 * time.Millisecond

// Every control.lock entry point shares one process-local owner per inode.
// The *Locked helpers execute under that owner's guard; they do not acquire it
// again. An accidental nested public acquisition refuses within its deadline,
// rather than opening a second flock description that waits on its own process.
// Shared readers also serialize locally, so a nested close cannot drop a guard.
type controlLockIdentity struct{ device, inode uint64 }
type controlLockOwner struct {
	identity controlLockIdentity
	done     chan struct{}
}

var processControlLocks = struct {
	sync.Mutex
	owners map[controlLockIdentity]*controlLockOwner
}{owners: make(map[controlLockIdentity]*controlLockOwner)}

func acquireControlLock(root *os.File, mode int, create bool, wait time.Duration) (*controlFileLock, error) {
	deadline := time.Now().Add(wait)
	for {
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
		processControlLocks.Lock()
		if owner := processControlLocks.owners[identity]; owner != nil {
			processControlLocks.Unlock()
			// This descriptor never owned a flock. Close it without LOCK_UN.
			lock.File.Close()
			remaining := time.Until(deadline)
			if wait == 0 || remaining <= 0 {
				return nil, &laneControlFault{Code: "locked", local: true}
			}
			timer := time.NewTimer(remaining)
			select {
			case <-owner.done:
				timer.Stop()
			case <-timer.C:
				return nil, &laneControlFault{Code: "locked", local: true}
			}
			continue
		}
		owner := &controlLockOwner{identity: identity, done: make(chan struct{})}
		processControlLocks.owners[identity] = owner
		lock.owner = owner
		processControlLocks.Unlock()
		err = boundedFlock(int(lock.Fd()), mode, max(0, time.Until(deadline)))
		if err == nil {
			lock.acquired = true
			return lock, nil
		}
		lock.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, &laneControlFault{Code: "locked"}
		}
		return nil, errLaneControlUnavailable
	}
}

// Audit append runs under some control guards. Its separate inode must not
// introduce an unbounded wait while the process owns the coordination lock.
func boundedFlock(fd, mode int, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		err := unix.Flock(fd, mode|unix.LOCK_NB)
		if err == nil || !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			return err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return unix.EWOULDBLOCK
		}
		time.Sleep(min(5*time.Millisecond, remaining))
	}
}
