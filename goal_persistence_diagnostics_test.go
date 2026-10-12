package nuntius

import (
	"errors"
	"os"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func goalFixtureSave(t *testing.T, stage string, save func() error) {
	t.Helper()
	// A fork can temporarily retain a flock descriptor even with CLOEXEC.
	// Fixture setup never launches a child; exclude unrelated forks until
	// the save has closed its descriptors. Production locks remain nonblocking.
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	if err := save(); err != nil {
		goalSetupFailure(t, stage, err)
	}
}

func TestGoalFixtureSaveForkBarrier(t *testing.T) {
	started, entered, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	syscall.ForkLock.Lock()
	locked := true
	defer func() {
		if locked {
			syscall.ForkLock.Unlock()
		}
		<-finished
	}()
	go func() {
		close(started)
		goalFixtureSave(t, "barrier control", func() error { close(entered); return nil })
		close(finished)
	}()
	<-started
	select {
	case <-entered:
		t.Fatal("fixture mutation entered while a fork owns the barrier")
	case <-time.After(20 * time.Millisecond):
	}
	syscall.ForkLock.Unlock()
	locked = false
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("fixture mutation did not run after fork barrier release")
	}
}

func goalSetupFailure(t *testing.T, stage string, err error) {
	t.Helper()
	var detail interface{ PersistenceDiagnostic() (string, int) }
	if errors.As(err, &detail) {
		operation, errno := detail.PersistenceDiagnostic()
		t.Fatalf("%s failed: persistence stage=%s errno=%d; %s", stage, operation, errno, err)
	}
	t.Fatalf("%s failed: %s", stage, err)
}

func TestGoalPersistenceDiagnostics(t *testing.T) {
	m, req := goalRecordsFixture(t)
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	lock, err := laneControlLock(root, unix.LOCK_EX)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	_, err = m.createGoal(req)
	if !errors.Is(err, errGoalCommit) || err.Error() != errGoalCommit.Error() {
		t.Fatal("diagnostic changed fail-closed save refusal")
	}
	var detail interface{ PersistenceDiagnostic() (string, int) }
	if !errors.As(err, &detail) {
		t.Fatal("save refusal discarded persistence stage and errno")
	}
	stage, errno := detail.PersistenceDiagnostic()
	if stage != "record.lock" || errno != int(unix.EWOULDBLOCK) {
		t.Fatal("save refusal lost lock contention cause")
	}
	_, auditErr := m.nativeAuditStatus()
	if !errors.As(auditErr, &detail) || !errors.Is(auditErr, errNativeAudit) || auditErr.Error() != errNativeAudit.Error() {
		t.Fatal("audit contention lost safe diagnostic or changed refusal")
	}
	stage, errno = detail.PersistenceDiagnostic()
	if stage != "audit.lock" || errno != int(unix.EWOULDBLOCK) {
		t.Fatal("audit contention lost stage or errno")
	}
	wrapped := goalPersistenceError(errGoalCommit, "blob.sync", &os.PathError{Op: "sync", Path: "synthetic-private-path", Err: unix.EIO})
	if wrapped.Error() != errGoalCommit.Error() || !errors.Is(wrapped, errGoalCommit) || errors.Is(wrapped, unix.EIO) {
		t.Fatal("diagnostic exposed OS cause or changed save error class")
	}
	if !errors.As(wrapped, &detail) {
		t.Fatal("OS failure lost diagnostic")
	}
	stage, errno = detail.PersistenceDiagnostic()
	if stage != "blob.sync" || errno != int(unix.EIO) {
		t.Fatal("OS failure lost stage or errno")
	}
}
