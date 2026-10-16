package nuntius

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
	"time"
)

func p0TestJoin[T any](t *testing.T, result <-chan T) T {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	select {
	case value := <-result:
		return value
	case <-ctx.Done():
		t.Fatal("owned test worker did not join within bound")
	}
	var zero T
	return zero
}

// Cleanup cannot abandon an owner merely because an unrelated caller holds a
// bookkeeping mutex. Keep the changed release path mechanically free of waits.
func TestP0GuardReleaseNoMutexWait(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "lane_control.go", nil, 0)
	if err != nil {
		t.Fatal("release source")
	}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "Close" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Lock" {
					t.Error("guard cleanup contains an unbounded mutex wait")
				}
			}
			return true
		})
	}
}

func TestP0CancelHistoryDeadline(t *testing.T) {
	parent, m, r := p0MetadataFixture(t)
	m.readOnly = false
	r.State = "running"
	if m.save(&r) != nil {
		t.Fatal("running fixture")
	}
	ctx, cancel := context.WithTimeout(parent, 40*time.Millisecond)
	defer cancel()
	m.metadata.mu.Lock()
	done := make(chan error, 1)
	go func() { done <- m.CancelContext(ctx, r.Lane) }()
	<-ctx.Done()
	var result error
	finished := false
	select {
	case result = <-done:
		finished = true
	case <-time.After(100 * time.Millisecond):
		t.Error("Cancel dropped its deadline in history")
	}
	m.metadata.mu.Unlock()
	if !finished {
		select {
		case result = <-done:
		case <-parent.Done():
			t.Fatal("Cancel worker did not join")
		}
	}
	if laneControlCode(result) != "locked" {
		t.Error("pre-effect Cancel did not return locked")
	}
}

func TestP0OwningContextControlWaits(t *testing.T) {
	for _, operation := range []string{"native", "pool"} {
		t.Run(operation, func(t *testing.T) {
			parent, m, _ := p0MetadataFixture(t)
			guard, err := m.admissionControlLockWithin(controlLockWait, parent)
			if err != nil {
				t.Fatal("outer guard")
			}
			defer guard.Close()
			ctx, cancel := context.WithCancel(parent)
			cancel()
			m.life = ctx
			began := time.Now()
			var got *controlFileLock
			if operation == "native" {
				got, err = m.nativeDispatchTransactionLock()
			} else {
				got, err = m.controlLock()
			}
			if got != nil {
				got.Close()
				t.Error("cancelled acquisition succeeded")
			}
			if laneControlCode(err) != "locked" || time.Since(began) > 150*time.Millisecond {
				t.Error("owning cancellation was discarded")
			}
		})
	}
}

func TestP0TouchedIntakeNames(t *testing.T) {
	ctx, m, r := p0MetadataFixture(t)
	for _, field := range []string{"lane", "goal"} {
		bad := r
		bad.Integrity = ""
		if field == "lane" {
			bad.Lane = "a..b"
		} else {
			bad.GoalID = "A..B"
		}
		b, _ := json.Marshal(bad)
		if decodeRunMetadata(b, bad.ID) != nil {
			t.Error("metadata accepted dot-dot", field)
		}
	}
	if _, err := m.DispatchContext(ctx, r.Lane, "A..B", "fixture"); err == nil {
		t.Error("dispatch accepted dot-dot goal")
	}
	c := m.config
	duplicate := c.Lanes[0]
	duplicate.Name = strings.ToUpper(duplicate.Name)
	c.Lanes = append(append([]Lane(nil), c.Lanes...), duplicate)
	if _, err := c.Validate(); err == nil {
		t.Error("case-colliding configured lanes accepted")
	}
}

func TestP0DisplayRetainedBudget(t *testing.T) {
	setupStarted := time.Now()
	ctx, m, r := p0MetadataFixture(t)
	r.Brief = strings.Repeat("x", 12000)
	for i := range 750 {
		if ctx.Err() != nil {
			t.Fatal("cache fixture deadline")
		}
		r.ID = fmt.Sprintf("%032x", i+1)
		r.Integrity = ""
		if privateDir(m.store+"/runs/"+r.ID) != nil || m.save(&r) != nil {
			t.Fatal("cache fixture write")
		}
	}
	parentDeadline, _ := ctx.Deadline()
	started := time.Now()
	opens, revalidations := m.metadata.opens.Load(), m.metadata.revalidations.Load()
	view, err := m.displaySnapshot(ctx)
	elapsed, parentErr := time.Since(started), ctx.Err()
	expired := "none"
	if errors.Is(err, context.DeadlineExceeded) {
		expired = "display"
		if parentDeadline.Before(started.Add(time.Second)) {
			expired = "parent"
		}
	}
	t.Logf("retained_display setup_ms=%.3f snapshot_ms=%.3f parent_remaining_before_ms=%.3f parent_remaining_after_ms=%.3f parent_error=%v expired=%s metadata_opens=%d revalidations=%d", float64(started.Sub(setupStarted))/float64(time.Millisecond), float64(elapsed)/float64(time.Millisecond), float64(parentDeadline.Sub(started))/float64(time.Millisecond), float64(time.Until(parentDeadline))/float64(time.Millisecond), parentErr, expired, m.metadata.opens.Load()-opens, m.metadata.revalidations.Load()-revalidations)
	if err != nil || len(view.runs) != 751 {
		t.Fatal("oversized display lost observations", err)
	}
	if len(m.display.value.runs) != 0 {
		t.Fatal("oversized private display retained without a byte budget")
	}
}

func TestP0StartupRetryNoMutexWait(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "mcp.go", nil, 0)
	if err != nil {
		t.Fatal("startup source")
	}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "Serve" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Lock" {
					t.Error("startup request has an unbounded mutex wait")
				}
			}
			return true
		})
	}
}

func TestP0LauncherOwningContext(t *testing.T) {
	for _, operation := range []string{"watcher", "recovery"} {
		t.Run(operation, func(t *testing.T) {
			ctx, m, r := p0MetadataFixture(t)
			m.readOnly = false
			r.State, r.ProcessStart, r.LaunchdLabel = "running", "", "nuntius.run."+r.ID
			if m.save(&r) != nil {
				t.Fatal("pending launcher fixture")
			}
			observed := false
			m.launcher = &jobLauncher{domain: "fixture", call: func(...string) ([]byte, error) {
				observed = m.launcher.ctx == ctx
				return nil, errors.New("offline pending inspection")
			}}
			if operation == "watcher" {
				_, _ = m.statusTransaction(ctx, false, false, false)
			} else {
				_ = m.recoverContext(ctx, ctx, false)
			}
			if !observed {
				t.Fatal("launcher lost the transaction's owning context")
			}
		})
	}
}

func TestP0LaunchctlCancelledIdentity(t *testing.T) {
	// A cancelled command cannot start. Empty PATH additionally excludes host tools.
	t.Setenv("PATH", t.TempDir())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := launchctlContext(ctx, "print", "offline")
	if !errors.Is(err, context.Canceled) {
		t.Fatal("launchctl erased cancellation identity")
	}
}

func TestP0PendingCancellationCode(t *testing.T) {
	parent, m, r := p0MetadataFixture(t)
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	m.readOnly = false
	r.State, r.ProcessStart, r.LaunchdLabel = "running", "", "nuntius.run."+r.ID
	if m.save(&r) != nil {
		t.Fatal("pending launcher fixture")
	}
	m.launcher = &jobLauncher{domain: "fixture", call: func(...string) ([]byte, error) {
		cancel()
		return nil, errors.New("offline inspection cancelled")
	}}
	_, err := m.DispatchContext(ctx, "codex", "Q12-CANCEL", "fixture")
	if laneControlCode(err) != "locked" {
		t.Fatal("pre-effect pending cancellation lost its locked code")
	}
}

func TestP0MCPWaitsExcludeDecision(t *testing.T) {
	parent, m, _ := p0MetadataFixture(t)
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	m.readOnly = false
	decision := func(context.Context) error {
		// Stand in for OS-bounded recovery work after its acquisition waits.
		// D_req is an acquisition budget, never a whole-decision deadline.
		if !p0MonitorPause(ctx, 2100*time.Millisecond) {
			return ctx.Err()
		}
		return nil
	}
	_, err := callContext(ctx, m, "nuntius_dispatch", json.RawMessage(`{"lane":"codex","goal_id":"Q12-WAIT","text":"ack-goal"}`), decision)
	if err != nil {
		t.Fatal("startup decision consumed the acquisition-only request budget", err)
	}
}

func TestP0ReleaseBorrowedGuard(t *testing.T) {
	ctx, m, _ := p0MetadataFixture(t)
	guard, err := m.admissionControlLockWithin(controlLockWait, ctx)
	if err != nil {
		t.Fatal("borrow fixture guard")
	}
	defer guard.Close()
	if !guard.use.CompareAndSwap(0, 1) {
		t.Fatal("borrow fixture lease")
	}
	// Model consume's synchronous, OS-bounded inspection. Closing must hand
	// cleanup to that borrower rather than wait or release its inode early.
	began := time.Now()
	if guard.Close() != nil || time.Since(began) > 50*time.Millisecond {
		t.Error("borrowed cleanup waited")
	}
	select {
	case <-guard.owner.done:
		t.Error("borrowed owner released before inspection finished")
	default:
	}
	guard.releaseUse()
	select {
	case <-guard.owner.done:
	default:
		t.Error("borrower lost release notification")
	}
	if guard.owner.slot.Load() == guard.owner {
		t.Error("released owner retained")
	}
	next, err := m.admissionControlLockWithin(controlLockWait, ctx)
	if err != nil {
		t.Fatal("released inode could not be acquired")
	}
	defer next.Close()
}

func TestP0IndependentControlIdentities(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	for _, id := range []controlLockIdentity{{device: 1, inode: 7}, {device: 1, inode: 4103}} {
		owner, held, err := reserveControlOwner(ctx, id)
		if err != nil || owner == nil || held != nil {
			t.Fatal("unrelated inode was refused as a local owner")
		}
		defer func() { owner.slot.CompareAndSwap(owner, nil); close(owner.done) }()
	}
}

func TestP0FixtureNoUnboundedCalls(t *testing.T) {
	for path, functions := range map[string]map[string]bool{
		"lane_control_upgrade_test.go": {"oldControlHolder": true, "TestLaneUpgradeStartupContention": true},
		"control_lock_owner_test.go":   {"TestControlOwnerMonitoredCrossProcessDispatch": true},
	} {
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal("fixture source")
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !functions[fn.Name.Name] {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "Sleep" || sel.Sel.Name == "Command") {
						t.Error("fixture call lacks owning context", path, fn.Name.Name)
					}
				}
				return true
			})
		}
	}
}
