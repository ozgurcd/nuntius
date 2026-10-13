package nuntius

import (
	"os"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// Dup and fork share the same open-file description. Keep that description
// alive after Close, without scheduling luck or starting another process.
func TestControlLockReleaseInheritedDescriptor(t *testing.T) {
	type lockFile interface {
		Fd() uintptr
		Close() error
	}
	for _, kind := range []string{"native", "reader", "legacy", "admission", "dispatch"} {
		t.Run(kind, func(t *testing.T) {
			m, req := goalRecordsFixture(t)
			root, err := openLaneControlRoot(m.store)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			seed, err := openLaneControlLockFile(root, true)
			if err != nil {
				t.Fatal(err)
			}
			if err = seed.Close(); err != nil {
				t.Fatal(err)
			}
			var lock lockFile
			switch kind {
			case "native":
				lock, err = laneControlLock(root, unix.LOCK_EX)
			case "reader":
				lock, err = laneControlReadLock(root)
			case "legacy":
				lock, err = m.controlLock()
			case "admission":
				lock, err = m.admissionControlLock()
			case "dispatch":
				lock, err = m.nativeDispatchControlLock()
			}
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			flags, err := unix.FcntlInt(lock.Fd(), unix.F_GETFD, 0)
			if err != nil || flags&unix.FD_CLOEXEC == 0 {
				t.Fatal("control lock lacks CLOEXEC")
			}
			fd, err := unix.Dup(int(lock.Fd()))
			if err != nil {
				t.Fatal(err)
			}
			syscall.CloseOnExec(fd)
			inherited := os.NewFile(uintptr(fd), "inherited-control-lock")
			defer inherited.Close()
			// Preserve the real nonblocking contention refusal while the owner
			// still holds the lock; no retry and no ignored persistence error.
			if _, err = m.createGoal(req); err == nil {
				t.Fatal("active owner did not exclude competing save")
			}
			if err = lock.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err = m.createGoal(req); err != nil {
				t.Fatal("closed owner still blocks save through inherited descriptor")
			}
			if _, err = inherited.Stat(); err != nil {
				t.Fatal("test dropped the inherited descriptor before its assertion")
			}
		})
	}
}
