package nuntius

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Expire at the first context check after StopAt has reached the real store.
// Done and Err change together; no scheduler race or wall-clock sleep is used.
type p0SavedDeadline struct {
	context.Context
	cancel  context.CancelFunc
	dir, id string
	expired bool
}

func (c *p0SavedDeadline) Err() error {
	if !c.expired {
		if r, err := loadRun(c.dir, c.id); err == nil && r != nil && r.StopAt != nil {
			c.expired = true
			c.cancel()
		}
	}
	if c.expired {
		return context.DeadlineExceeded
	}
	return c.Context.Err()
}

func TestP0AfterSaveDeadlineAndSignalOrder(t *testing.T) {
	for _, expire := range []bool{false, true} {
		t.Run(fmt.Sprint(expire), func(t *testing.T) {
			m, _, _ := manager(t)
			m.Grace = 50 * time.Millisecond
			id, err := m.Dispatch("codex", "P0-STOP-ORDER", "ignore-int")
			if err != nil {
				t.Fatal(err)
			}
			waitReady(t, m, "codex")
			r, _ := loadRun(filepath.Join(m.store, "runs", id), id)
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			var ctx context.Context = base
			if expire {
				ctx = &p0SavedDeadline{Context: base, cancel: cancel, dir: filepath.Join(m.store, "runs", id), id: id}
			}
			guard, err := m.controlLock()
			if err != nil {
				t.Fatal(err)
			}
			signalled := false
			err = m.requestStopContext(ctx, r, "cancelled", func(run Run, sig syscall.Signal) error {
				durable, e := loadRun(filepath.Join(m.store, "runs", id), id)
				if e != nil || durable == nil || durable.StopAt == nil || sig != syscall.SIGINT {
					t.Error("signal preceded durable stop")
				}
				signalled = true
				return signalRun(run, sig)
			})
			guard.Close()
			if expire {
				var outcome *operationOutcome
				if !errors.As(err, &outcome) || outcome.Code != "stop_recorded" || signalled || !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("after-save deadline lost durable outcome")
				}
			} else if err != nil || !signalled {
				t.Fatal("stop signal order proof failed")
			}
			waitState(t, m, "codex", "ended")
		})
	}
}

func TestP0StopEscalationOrder(t *testing.T) {
	m, _, _ := manager(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := lockContext(ctx, &m.mu); err != nil {
		t.Fatal(err)
	}
	m.Grace = time.Hour // Only the explicit competing supervisor may escalate.
	m.mu.Unlock()
	id, err := m.Dispatch("codex", "P0-STOP-ESCALATION", "ignore-int")
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, m, "codex")
	r, err := loadRun(filepath.Join(m.store, "runs", id), id)
	if err != nil || r == nil {
		t.Fatal("missing owned fixture")
	}
	defer func() { _ = signalRun(*r, syscall.SIGKILL) }()
	other := &Manager{store: m.store}
	escalated := false
	err = m.requestStopContext(ctx, r, "cancelled", func(run Run, sig syscall.Signal) error {
		durable, e := loadRun(filepath.Join(m.store, "runs", id), id)
		if e != nil || durable == nil || durable.StopAt == nil || sig != syscall.SIGINT {
			t.Error("initial signal preceded durable stop")
		}
		// Force the exact post-save/pre-SIGINT boundary without a scheduler race.
		escalated = other.escalate(ctx, run, 0)
		if escalated {
			tick := time.NewTicker(time.Millisecond)
			defer tick.Stop()
			for sameProcess(run) {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-tick.C:
				}
			}
		}
		return signalRun(run, sig)
	})
	var outcome *operationOutcome
	if escalated || err != nil {
		isRecorded := errors.As(err, &outcome) && outcome.Code == "stop_recorded"
		t.Fatalf("supervisor overtook initial signal: escalated=%t stop_recorded=%t cause=%v", escalated, isRecorded, err)
	}
	if !other.escalate(ctx, *r, 0) {
		t.Fatal("completed initial signal blocked independent escalation")
	}
	waitState(t, m, "codex", "ended")
}

func TestP0StopSignalGuard(t *testing.T) {
	t.Run("external-holder", func(t *testing.T) {
		ctx, m, r := p0MetadataFixture(t)
		lock, err := m.stopSignalLock(ctx, r.ID, 50*time.Millisecond)
		if err != nil {
			t.Fatal("initial signal guard", err)
		}
		lock.Close()
		release := controlFileHolder(t, filepath.Join(m.store, "runs", r.ID, "stop.lock"), filepath.Dir(m.store))
		defer release()
		for range 2 {
			request, cancel := context.WithTimeout(ctx, 25*time.Millisecond)
			f, err := m.stopSignalLock(request, r.ID, time.Second)
			cancel()
			if f != nil {
				f.Close()
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("external holder lost ownership or request deadline", err)
			}
		}
		release()
		lock, err = m.stopSignalLock(ctx, r.ID, 50*time.Millisecond)
		if err != nil {
			t.Fatal("released guard remained held", err)
		}
		lock.Close()
	})
	t.Run("cancelled", func(t *testing.T) {
		ctx, m, r := p0MetadataFixture(t)
		request, cancel := context.WithCancel(ctx)
		cancel()
		f, err := m.stopSignalLock(request, r.ID, time.Second)
		if f != nil {
			f.Close()
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatal("signal guard dropped caller cancellation")
		}
		if _, err = os.Lstat(filepath.Join(m.store, "runs", r.ID, "stop.lock")); !os.IsNotExist(err) {
			t.Fatal("cancelled guard created a file")
		}
	})
	t.Run("names", func(t *testing.T) {
		ctx, m, r := p0MetadataFixture(t)
		for _, id := range []string{"..", "../" + r.ID, "/" + r.ID, "-" + r.ID, "a b", "a\n", "a\x00", "'quoted'", "\"quoted\"", "$(x)", "`x`", "a;b", "a|b", strings.Repeat("a", 33), strings.Repeat("a", 4096), strings.ToUpper(r.ID), "а" + r.ID[1:]} {
			f, err := m.stopSignalLock(ctx, id, 50*time.Millisecond)
			if f != nil {
				f.Close()
			}
			if err == nil {
				t.Error("invalid signal guard identity accepted")
			}
		}
		if _, err := os.Lstat(filepath.Join(m.store, "stop.lock")); !os.IsNotExist(err) {
			t.Fatal("signal guard identity escaped its run")
		}
	})
	for _, kind := range []string{"symlink", "hardlink", "public", "fifo", "directory"} {
		t.Run(kind, func(t *testing.T) {
			ctx, m, r := p0MetadataFixture(t)
			path := filepath.Join(m.store, "runs", r.ID, "stop.lock")
			target := filepath.Join(t.TempDir(), "unchanged")
			if os.WriteFile(target, []byte("unchanged"), 0600) != nil {
				t.Fatal("guard target fixture")
			}
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(target, path)
			case "hardlink":
				err = os.Link(target, path)
			case "public":
				err = os.WriteFile(path, nil, 0644)
				if err == nil {
					err = os.Chmod(path, 0644)
				}
			case "fifo":
				err = syscall.Mkfifo(path, 0600)
			case "directory":
				err = os.Mkdir(path, 0700)
			}
			if err != nil {
				t.Fatal("unsafe guard fixture")
			}
			f, err := m.stopSignalLock(ctx, r.ID, 50*time.Millisecond)
			if f != nil {
				f.Close()
			}
			if err == nil {
				t.Error("unsafe signal guard file accepted")
			}
			b, err := os.ReadFile(target)
			if err != nil || string(b) != "unchanged" {
				t.Fatal("unsafe guard changed target")
			}
		})
	}
}

func TestP0LaunchctlGrandchildStdout(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NUNTIUS_FAKE_LAUNCHCTL", dir)
	t.Setenv("NUNTIUS_FAKE_LSOF", "hang")
	lsof := filepath.Join(dir, "lsof")
	if err := os.Symlink(fakeBinary, lsof); err != nil {
		t.Fatal("offline stdout holder")
	}
	plist := "<?xml version=\"1.0\"?><plist><dict><key>ProgramArguments</key><array><string>" + lsof + "</string></array><key>StandardOutPath</key><string>/dev/fd/1</string></dict></plist>"
	job := filepath.Join(dir, "job.plist")
	if os.WriteFile(job, []byte(plist), 0600) != nil || os.WriteFile(filepath.Join(dir, "job.path"), []byte(job), 0600) != nil {
		t.Fatal("offline launch specification")
	}
	// The pre-existing compiled launchctl fake starts an isolated lsof fake;
	// the outer fixture lease and this exact identity own its cleanup.
	var child Run
	t.Cleanup(func() {
		if sameProcess(child) {
			_ = signalRun(child, syscall.SIGKILL)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for sameProcess(child) {
			select {
			case <-ctx.Done():
				t.Error("stdout holder survived cleanup")
				return
			case <-tick.C:
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := launchctlContext(ctx, "kickstart", "fixture")
	elapsed := time.Since(start)
	returned := time.Now()
	b, e := os.ReadFile(filepath.Join(dir, "job.pid"))
	pid, parseErr := strconv.Atoi(strings.TrimSpace(string(b)))
	if e != nil || parseErr != nil {
		t.Fatal("missing grandchild identity")
	}
	birth, e := processStart(pid)
	child = Run{PID: pid, ProcessStart: birth}
	if e != nil || !sameProcess(child) {
		t.Fatal("grandchild did not hold inherited stdout")
	}
	// job.pid is written immediately before the launchctl parent releases its
	// child and exits. Exclude executable startup from the pipe-drain bound.
	marker, e := os.Stat(filepath.Join(dir, "job.pid"))
	if e != nil {
		t.Fatal("missing parent exit marker")
	}
	drain := returned.Sub(marker.ModTime())
	if err == nil || drain >= 300*time.Millisecond {
		t.Fatal("launchctl waited for grandchild stdout beyond WaitDelay")
	}
	t.Logf("grandchild_alive=true total_ms=%.3f pipe_drain_ms=%.3f", float64(elapsed)/float64(time.Millisecond), float64(drain)/float64(time.Millisecond))
}

func TestP0LaunchctlRequestCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	j := &jobLauncher{ctx: ctx, call: func(...string) ([]byte, error) { called = true; return nil, nil }}
	if _, err := j.command("kickstart", "fixture"); !errors.Is(err, context.Canceled) || called {
		t.Fatal("cancelled request reached the launcher")
	}
}

func TestP0ReservedReceiptBlocksAdmission(t *testing.T) {
	m, h := heldReviewFixture(t, "fixture-thread")
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	dir, r, _, _, err := m.reviewedMessage(h)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	if err := writeMessageReceipt(dir, messageReceipt{Version: 1, Binding: h, State: "reserved", ChildID: strings.Repeat("f", 32)}, nil); err != nil {
		t.Fatal(err)
	}
	// A reserved receipt remains authoritative even if a crash left no hold file.
	if err := os.Rename(filepath.Join(m.store, "runs", r.ID, "message-hold.json"), filepath.Join(m.store, "runs", r.ID, "message-hold.saved")); err != nil {
		t.Fatal(err)
	}
	before := admissionArtifacts(t, m.store)
	if _, err := m.Dispatch(h.Lane, "P0-RESERVED-REFUSAL", "ack-goal"); err == nil {
		t.Fatal("reserved receipt admitted new work")
	}
	after := admissionArtifacts(t, m.store)
	if fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatal("reserved refusal changed run artifacts")
	}
}
