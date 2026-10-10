package nuntius

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// These regressions preserve the guards exposed by the independent ensure
// review. All processes and HTTP endpoints are owned offline fixtures.
func TestWebEnsureGracefulSignal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, fakeBinary)
	cmd.Stdin = strings.NewReader("wait")
	stdout, err := cmd.StdoutPipe()
	if err != nil || cmd.Start() != nil {
		t.Fatal("fake process startup")
	}
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Signal(os.Interrupt)
			_ = cmd.Wait()
		}
	}()
	scanner := bufio.NewScanner(stdout)
	ready := false
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), "ready") {
			ready = true
			break
		}
	}
	if !ready {
		t.Fatal("fake process not ready")
	}
	m, _, _ := manager(t)
	if m.ensureAdapters().signal(cmd.Process.Pid) != nil {
		t.Fatal("graceful fake signal unavailable")
	}
	if cmd.Wait() != nil {
		t.Fatal("fake process was force killed")
	}
}

func TestWebEnsureRecordedNonLoopbackRefusal(t *testing.T) {
	f := newEnsureFixture(t, true)
	f.state.Address = "192.0.2.1:8123"
	if f.m.saveWebState(f.state) != nil {
		t.Fatal("fake state setup")
	}
	if r := ensureCall(t, f.m); r.Reason != "identity_refused" || f.starts != 0 || f.stops != 0 || f.writes != 0 {
		t.Fatal("nonloopback state accepted")
	}
}

func TestWebEnsureIdentityChangesBeforeSignal(t *testing.T) {
	f := newEnsureFixture(t, true)
	calls := 0
	f.d.process = func(int) (string, error) {
		calls++
		if calls == 1 {
			return f.state.ProcessStart, nil
		}
		return "reused-fixture-start", nil
	}
	if f.m.ensureStop(context.Background(), f.d, f.state) == nil || f.stops != 0 {
		t.Fatal("changed process identity was signalled")
	}
}

func TestWebEnsureFinalTailnetIdentity(t *testing.T) {
	f := newEnsureFixture(t, true)
	run := f.d.run
	calls := 0
	f.d.run = func(ctx context.Context, exe string, args ...string) ([]byte, error) {
		if strings.Join(args, " ") == "status --json" {
			calls++
			if calls > 1 {
				return []byte(strings.ReplaceAll(f.status, "mac.tail123", "other.tail123")), nil
			}
		}
		return run(ctx, exe, args...)
	}
	if ensureCall(t, f.m).Reason != "tailnet_identity_changed" {
		t.Fatal("changed final tailnet identity accepted")
	}
}

func TestWebEnsureFinalServeReadback(t *testing.T) {
	f := newEnsureFixture(t, true)
	run := f.d.run
	calls := 0
	f.d.run = func(ctx context.Context, exe string, args ...string) ([]byte, error) {
		if strings.Join(args, " ") == "serve status --json" {
			calls++
			if calls == 3 {
				return []byte(fixtureEnsureMapping("9999")), nil
			}
		}
		return run(ctx, exe, args...)
	}
	if ensureCall(t, f.m).Reason != "serve_readback_failed" {
		t.Fatal("changed final mapping accepted")
	}
}

func TestWebEnsureProbeDoesNotFollowWorkingRedirect(t *testing.T) {
	var received atomic.Bool
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Store(true)
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(200)
	}))
	defer sink.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, sink.URL, http.StatusFound) }))
	defer source.Close()
	state := webState{Address: strings.TrimPrefix(source.URL, "http://"), Host: "mac.tail123.ts.net"}
	if ensureProbe(context.Background(), state, fixtureEnsureGrant) || received.Load() {
		t.Fatal("redirect destination received readiness request")
	}
}

func TestWebEnsureProbeRequiresNoStore(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer server.Close()
	state := webState{Address: strings.TrimPrefix(server.URL, "http://"), Host: "mac.tail123.ts.net"}
	if ensureProbe(context.Background(), state, fixtureEnsureGrant) {
		t.Fatal("cacheable response certified readiness")
	}
}

func TestWebEnsurePrivateStateMode(t *testing.T) {
	f := newEnsureFixture(t, true)
	if os.Chmod(filepath.Join(f.m.store, "web.json"), 0400) != nil {
		t.Fatal("fake state chmod")
	}
	if ensureCall(t, f.m).Reason != "state_unavailable" {
		t.Fatal("noncanonical private mode accepted")
	}
}
