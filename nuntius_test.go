package nuntius

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/ozgurcd/nuntius/internal/fixturelife"
)

var fakeBinary string
var cliBinary string

func TestMain(m *testing.M) {
	// Darwin's default /tmp is a symlink. Fixtures need one physical spelling
	// for no-follow store opens and workdir-keyed child lifetime records.
	tempRoot, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil || os.Setenv("TMPDIR", tempRoot) != nil {
		fmt.Println("offline temporary root unavailable")
		os.Exit(1)
	}
	lifetimeHelper := os.Args[len(os.Args)-1] == "fixture-lifetime-helper"
	if lifetimeHelper {
		fmt.Println("fixture-stage=1")
	}
	if os.Args[len(os.Args)-1] == "fixture-web-server" {
		fixtureWebServer()
		os.Exit(0)
	}
	if err := testAgentStartBootstrap(os.Args[1:]); err != nil {
		os.Exit(47)
	}
	if code, handled := launchEntry(os.Args[1:]); handled {
		os.Exit(code)
	}
	newJobLauncher = func() *jobLauncher { return nil }
	dir, err := os.MkdirTemp("", "nuntius-fixture-")
	if err != nil {
		os.Exit(1)
	}
	var owner *fixturelife.Owner
	if lifetimeHelper {
		fmt.Println("fixture-stage=2")
	}
	if os.Getenv(fixturelife.Environment) == "" {
		owner, err = fixturelife.New(filepath.Join(dir, "children"))
		if err != nil {
			os.Exit(1)
		}
		_ = os.Setenv(fixturelife.Environment, owner.Dir)
		if lifetimeHelper {
			fmt.Println("fixture-stage=3")
		}
		interrupt := make(chan os.Signal, 1)
		signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM, syscall.SIGQUIT)
		go func() {
			<-interrupt
			if owner.Close() != nil {
				fmt.Println("offline interrupted child cleanup failed")
			}
			os.Exit(130)
		}()
	}
	binary := os.Getenv("NUNTIUS_LIFETIME_FAKE_BINARY")
	if lifetimeHelper {
		fmt.Println("fixture-stage=4")
	}
	if binary == "" {
		binary = os.Getenv("NUNTIUS_TEST_FAKE_BINARY")
	}
	if binary != "" {
		// Nested test helpers share the outer suite's already compiled programs
		// and inherited offline PATH. They retain their own leases and stores.
		fakeBinary = binary
		cliBinary = os.Getenv("NUNTIUS_TEST_CLI_BINARY")
		var child *exec.Cmd
		if os.Args[len(os.Args)-1] == "fixture-lifetime-helper" {
			child, err = fixtureLifetimeReady()
			if err != nil {
				fmt.Println("offline lifetime preparation failed")
				if owner != nil {
					_ = owner.Close()
				}
				_ = os.RemoveAll(dir)
				os.Exit(1)
			}
		}
		code := m.Run()
		if owner != nil && owner.Close() != nil {
			fmt.Println("offline final child cleanup failed")
			code = 1
		}
		if child != nil {
			_ = child.Wait()
		}
		code = testAgentStartSuiteResult(code)
		_ = os.RemoveAll(dir)
		os.Exit(code)
	}
	fakeBinary = filepath.Join(dir, "fake-agent")
	cmd := exec.Command("go", "build", "-o", fakeBinary, "./internal/testagent")
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Println(string(out))
		_ = os.RemoveAll(dir)
		os.Exit(1)
	}
	cliBinary = filepath.Join(dir, "nuntius")
	if out, err := exec.Command("go", "build", "-o", cliBinary, "./cmd/nuntius").CombinedOutput(); err != nil {
		fmt.Println(string(out))
		_ = os.RemoveAll(dir)
		os.Exit(1)
	}
	// Every CLI subprocess resolves launchctl to an offline fake. With no
	// fixture launchd directory it refuses, exercising the detached fallback.
	if os.Symlink(fakeBinary, filepath.Join(dir, "launchctl")) != nil {
		os.Exit(1)
	}
	_ = os.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	// All external dashboard transports resolve to compiled offline fakes,
	// including when a regression removes a request or lifecycle guard.
	for _, name := range []string{"tailscale", "osascript", "open", "xdg-open", "nuntius-menubar"} {
		if os.Symlink(fakeBinary, filepath.Join(dir, name)) != nil {
			os.Exit(1)
		}
	}
	tailscaleAppPath = filepath.Join(dir, "absent-app")
	_ = os.Setenv("NUNTIUS_TEST_FAKE_BINARY", fakeBinary)
	_ = os.Setenv("NUNTIUS_TEST_CLI_BINARY", cliBinary)
	code := m.Run()
	if owner != nil && owner.Close() != nil {
		fmt.Println("offline final child cleanup failed")
		code = 1
	}
	code = testAgentStartSuiteResult(code)
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func fixture(t *testing.T) (Config, string) {
	t.Helper()
	d := t.TempDir()
	off := false
	store := filepath.Join(d, "store")
	// Cleanup is LIFO: observe after the existing cleanup below, while this
	// test's records still exist. Never infer teardown from a removed directory.
	t.Cleanup(func() {
		entries, err := os.ReadDir(filepath.Join(store, "runs"))
		if err != nil && !os.IsNotExist(err) {
			t.Error("offline fixture teardown could not read run records")
			return
		}
		observed := false
		for _, entry := range entries {
			if !runPattern.MatchString(entry.Name()) || !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				continue
			}
			dir := filepath.Join(store, "runs", entry.Name())
			_, err := os.Lstat(filepath.Join(dir, "meta.json"))
			if os.IsNotExist(err) {
				continue // ETA fixtures contain goal metadata without a child record.
			}
			r, err := loadRun(dir, entry.Name())
			if err != nil {
				t.Error("offline fixture teardown could not validate a child record")
				continue
			}
			if r == nil {
				t.Log("synthetic corrupt fixture metadata supplies no child identity")
				continue
			}
			observed = true
			if sameProcess(*r) {
				t.Error("offline fixture child still exists after test cleanup")
				return
			}
		}
		if observed {
			t.Log("offline fixture teardown verified before temporary record removal")
		}
	})
	t.Cleanup(func() {
		// This directory was created by this test. Never scan the owner's store.
		m := &Manager{store: store}
		runs, _ := m.history()
		var killed []Run
		for _, r := range runs {
			if sameProcess(r) {
				_ = signalRun(r, syscall.SIGKILL)
				killed = append(killed, r)
			}
		}
		if err := fixturelife.Stop(os.Getenv(fixturelife.Environment), d); err != nil {
			t.Error("offline fixture process group cleanup failed")
		}
		// SIGKILL is asynchronous, and Stop waits only for children that already
		// registered with fixturelife. A run killed while still bootstrapping is
		// identified but unregistered; wait until its identity is gone.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := awaitRunsGone(ctx, killed); err != nil {
			t.Error("offline fixture child did not exit after SIGKILL")
		}
	})
	return Config{Lanes: []Lane{{Name: "claude", Kind: "claude", Workdir: d, Binary: fakeBinary, ExtraArgs: []string{"--permission-mode=auto"}}, {Name: "codex", Kind: "codex", Workdir: d, Binary: fakeBinary, ExtraArgs: []string{"--sandbox=workspace-write"}}}, MaxRuntime: "10s", ReportsDir: d, Notifications: &off}, store
}

// awaitRunsGone polls until no run keeps its verified process identity, or ctx ends.
func awaitRunsGone(ctx context.Context, runs []Run) error {
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		alive := false
		for _, r := range runs {
			alive = alive || sameProcess(r)
		}
		if !alive {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}
func manager(t *testing.T) (*Manager, Config, string) {
	t.Helper()
	c, s := fixture(t)
	m, e := NewManager(c, s)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		for _, l := range c.Lanes {
			if m.Cancel(l.Name) == nil {
				waitState(t, m, l.Name, "ended")
			}
		}
		_ = m.Close()
	})
	return m, c, s
}
func waitState(t testing.TB, m *Manager, lane, state string) Status {
	t.Helper()
	return waitStateContext(context.Background(), t, m, lane, state)
}

// waitStateContext polls like waitState, but its reads and sleeps observe ctx:
// cancellation ends the poll with a Fatal instead of letting it run on.
func waitStateContext(ctx context.Context, t testing.TB, m *Manager, lane, state string) Status {
	t.Helper()
	pause := func() {
		timer := time.NewTimer(5 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			t.Fatalf("waiting for %s: %v", state, ctx.Err())
		}
	}
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		// Native Status repairs and replays the entire journal. Observe the
		// small run record first while a verified live child is still running;
		// always use the real Status for the requested state and dead/adopted runs.
		r, err := m.latestContext(ctx, lane)
		if err == nil && r != nil && r.State == "running" && state == "ended" && sameProcess(*r) {
			pause()
			continue
		}
		// statusContext with ctx is Status() with the caller's cancellation.
		s, e := m.statusContext(ctx, true)
		if statusBusyRefusal(e) {
			// Only statusTransaction's bounded m.mu wait refused while another
			// owner (e.g. end publication) held it. That is a busy observation;
			// keep polling within this deadline. Every other error stays fatal.
			if o, ok := t.(statusRefusalObserver); ok {
				o.observeStatusRefusal()
			}
			pause()
			continue
		}
		if e != nil {
			t.Fatal(e)
		}
		for _, v := range s {
			if v.Lane == lane && (v.State == state || state == "ended" && v.State == "ended_no_report") {
				return v
			}
		}
		pause()
	}
	t.Fatalf("lane did not reach %s", state)
	return Status{}
}

// statusBusyRefusal is true only for statusTransaction's bounded m.mu wait
// expiring. Other deadline origins (metadata waits, control-lock fallback)
// and cancellation are not busy observations.
func statusBusyRefusal(err error) bool {
	var busy statusMutexBusy
	return errors.As(err, &busy) && errors.Is(busy.err, context.DeadlineExceeded)
}

// latestRunWhen polls m.latestContext until done accepts the lane's newest run.
// errLaneControlUnavailable is the store's bounded refusal (meta.json replaced
// across every bounded re-read, or not yet openable); like a busy Status it is
// retried within ctx. done never sees a nil run, every other error returns at
// once, and expiry returns the context error, never a nil run as success.
func latestRunWhen(ctx context.Context, m *Manager, lane string, done func(*Run) bool) (*Run, error) {
	last, refusal := "no read", error(nil)
	for {
		r, err := m.latestContext(ctx, lane)
		switch {
		case err == nil && r != nil && done(r):
			return r, nil
		case err == nil && r != nil:
			last, refusal = fmt.Sprintf("run %s state %s resume %d", r.ID, r.State, r.ResumeCount), nil
		case err == nil:
			last, refusal = "no run", nil
		case errors.Is(err, errLaneControlUnavailable) && ctx.Err() == nil:
			last, refusal = "refused", err
		case ctx.Err() == nil:
			return nil, err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			if refusal != nil {
				return nil, fmt.Errorf("lane %s: %w; last: %w", lane, ctx.Err(), refusal)
			}
			return nil, fmt.Errorf("lane %s: %w; last: %s", lane, ctx.Err(), last)
		}
	}
}

// latestRun reads the lane's newest run within Status's bounded wait budget,
// retrying only the store's refusal, and fails instead of returning nil.
func latestRun(t testing.TB, m *Manager, lane string) *Run {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), dispatchControlLockWait)
	defer cancel()
	r, err := latestRunWhen(ctx, m, lane, func(*Run) bool { return true })
	if err != nil {
		t.Fatalf("reading newest %s run: %v", lane, err)
	}
	return r
}

// TestLatestRunWhenRetriesStoreRefusal pins the crash-recovery read helper to
// the store's bounded refusal: while meta.json cannot be read, m.latest returns
// (nil, errLaneControlUnavailable), which the old `r, _ := m.latest(lane)`
// call sites dereferenced.
func TestLatestRunWhenRetriesStoreRefusal(t *testing.T) {
	m, _, store := manager(t)
	id := strings.Repeat("e", 32)
	seedCrashRun(t, m, id, "REFUSE-23", "")
	meta := filepath.Join(store, "runs", id, "meta.json")
	running := func(r *Run) bool { return r.State == "running" }
	if err := os.Chmod(meta, 0o644); err != nil {
		t.Fatal(err)
	}
	if r, err := m.latest("codex"); r != nil || !errors.Is(err, errLaneControlUnavailable) {
		t.Fatalf("fault seam: m.latest = %v, %v; want nil, errLaneControlUnavailable", r, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	r, err := latestRunWhen(ctx, m, "codex", running)
	cancel()
	if r != nil || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, errLaneControlUnavailable) {
		t.Fatalf("persistent refusal: got %v, %v; want nil and the bounded refusal", r, err)
	}
	if err = os.Chmod(meta, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if r, err = latestRunWhen(ctx, m, "codex", running); err != nil || r == nil || r.ID != id {
		t.Fatalf("readable run: got %v, %v", r, err)
	}
	// Any other store error is not a refusal and returns before ctx ends.
	stray := filepath.Join(store, "runs", "not-a-run")
	if err = os.WriteFile(stray, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r, err = latestRunWhen(ctx, m, "codex", running)
	if rmErr := os.Remove(stray); rmErr != nil {
		t.Fatal(rmErr)
	}
	if r != nil || err == nil || errors.Is(err, errLaneControlUnavailable) || ctx.Err() != nil {
		t.Fatalf("non-refusal error: got %v, %v (ctx %v)", r, err, ctx.Err())
	}
}

// statusRefusalObserver lets a test learn that waitState saw a busy refusal.
type statusRefusalObserver interface{ observeStatusRefusal() }

// fatalRecorder ends only the polling goroutine on Fatal and reports each
// busy refusal, so a test can observe what waitState did.
type fatalRecorder struct {
	testing.TB
	refused chan struct{}
	fatal   atomic.Pointer[string]
}

func (f *fatalRecorder) Helper() {}
func (f *fatalRecorder) Fatal(args ...any) {
	text := fmt.Sprint(args...)
	f.fatal.Store(&text)
	runtime.Goexit()
}
func (f *fatalRecorder) Fatalf(format string, args ...any) { f.Fatal(fmt.Sprintf(format, args...)) }
func (f *fatalRecorder) fatalText() string {
	if p := f.fatal.Load(); p != nil {
		return *p
	}
	return ""
}
func (f *fatalRecorder) observeStatusRefusal() {
	select {
	case f.refused <- struct{}{}:
	default:
	}
}

// joinWithin waits a bounded time for an owned goroutine and fails loudly.
func joinWithin(t testing.TB, done <-chan struct{}, d time.Duration, what string) {
	t.Helper()
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		t.Errorf("%s did not exit within %v", what, d)
	}
}

func TestStatusBusyRefusalOrigin(t *testing.T) {
	if !statusBusyRefusal(statusMutexBusy{context.DeadlineExceeded}) {
		t.Fatal("the bounded m.mu refusal is not recognized")
	}
	for _, err := range []error{
		nil,
		context.DeadlineExceeded,
		fmt.Errorf("metadata wait: %w", context.DeadlineExceeded),
		&laneControlFault{Code: "locked"},
		statusMutexBusy{context.Canceled},
	} {
		if statusBusyRefusal(err) {
			t.Fatalf("%v from another origin would be retried", err)
		}
	}
}

// A run end publishes under m.mu; on a slow race host that can outlast the
// bounded Status mutex wait (CI run 38864108310). The bound must stay in
// force, and the poll must treat that refusal as busy, not as a failure.
func TestWaitStateRetriesBoundedStatusRefusal(t *testing.T) {
	m, _, _ := manager(t)
	lane := m.config.Lanes[0].Name
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	releaseHolder := holdManagerMutex(t, ctx, m)
	if _, err := m.Status(); !statusBusyRefusal(err) {
		t.Fatalf("Status mutex wait is no longer a bounded busy refusal: %v", err)
	}
	rec := &fatalRecorder{TB: t, refused: make(chan struct{}, 1)}
	got, done := make(chan Status, 1), make(chan struct{})
	go func() {
		defer close(done)
		got <- waitStateContext(ctx, rec, m, lane, "idle")
	}()
	t.Cleanup(func() { cancel(); joinWithin(t, done, 10*time.Second, "waitState") })
	// Release only after waitState has observed at least one refusal.
	select {
	case <-rec.refused:
	case <-done:
		t.Fatalf("waitState returned before any refusal: %s", rec.fatalText())
	case <-ctx.Done():
		t.Fatal("waitState observed no refusal")
	}
	releaseHolder()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("waitState did not finish after release")
	}
	if f := rec.fatalText(); f != "" {
		t.Fatalf("waitState failed on a bounded busy refusal: %s", f)
	}
	if s := <-got; s.Lane != lane || s.State != "idle" {
		t.Fatalf("unexpected status %s/%s", s.Lane, s.State)
	}
}

// holdManagerMutex holds m.mu in an owned goroutine until release or ctx ends.
// Cleanup releases it and joins the holder within a bound.
func holdManagerMutex(t *testing.T, ctx context.Context, m *Manager) (release func()) {
	t.Helper()
	held, stop, exited := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(stop) }) }
	go func() {
		defer close(exited)
		if lockContext(ctx, &m.mu) != nil {
			return
		}
		defer m.mu.Unlock()
		close(held)
		select {
		case <-stop:
		case <-ctx.Done():
		}
	}()
	t.Cleanup(func() { release(); joinWithin(t, exited, 5*time.Second, "mutex holder") })
	select {
	case <-held:
	case <-exited:
		t.Fatal("mutex holder did not acquire m.mu")
	}
	return release
}

// Cancelling the poll's context must end it while Status is blocked on m.mu,
// well inside Status's own 2s mutex bound and waitState's 4s loop.
func TestWaitStateStopsOnCancellation(t *testing.T) {
	m, _, _ := manager(t)
	lane := m.config.Lanes[0].Name
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	holdManagerMutex(t, ctx, m)
	work, stop := context.WithCancel(ctx)
	defer stop()
	rec := &fatalRecorder{TB: t, refused: make(chan struct{}, 1)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		waitStateContext(work, rec, m, lane, "idle")
	}()
	t.Cleanup(func() { stop(); joinWithin(t, done, 10*time.Second, "waitState") })
	select {
	case <-rec.refused:
	case <-done:
		t.Fatalf("waitState returned before any refusal: %s", rec.fatalText())
	case <-ctx.Done():
		t.Fatal("waitState observed no refusal")
	}
	stop()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		t.Fatal("waitState ignored cancellation while Status waited for m.mu")
	}
	if f := rec.fatalText(); !strings.Contains(f, context.Canceled.Error()) {
		t.Fatalf("waitState did not stop on cancellation: %q", f)
	}
}
func waitReady(t *testing.T, m *Manager, lane string) {
	t.Helper()
	for i := 0; i < 400; i++ {
		rows, e := m.Tail(lane, 200)
		if e == nil && strings.Contains(strings.Join(rows, "\n"), "ready") {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("fixture not ready")
}

func TestConfigAndBypassRefusal(t *testing.T) {
	for _, kind := range []string{"claude", "codex"} {
		cases := []struct {
			name        string
			args        []string
			allowedKind string
		}{{"skip-git-repo-check", []string{"--skip-git-repo-check"}, "codex"}}
		for _, mode := range []string{"auto", "acceptEdits", "plan", "manual", "bypassPermissions", "dontAsk", "unknown", ""} {
			allowed := ""
			if mode == "auto" || mode == "acceptEdits" || mode == "plan" || mode == "manual" {
				allowed = "claude"
			}
			cases = append(cases, struct {
				name        string
				args        []string
				allowedKind string
			}{"permission-" + mode, []string{"--permission-mode", mode}, allowed})
		}
		for _, mode := range []string{"read-only", "workspace-write", "danger-full-access", "unknown", ""} {
			allowed := ""
			if mode == "read-only" || mode == "workspace-write" {
				allowed = "codex"
			}
			cases = append(cases, struct {
				name        string
				args        []string
				allowedKind string
			}{"sandbox-" + mode, []string{"--sandbox", mode}, allowed})
		}
		cases = append(cases, struct {
			name        string
			args        []string
			allowedKind string
		}{"skip-value", []string{"--skip-git-repo-check", "true"}, ""})
		for _, tc := range cases {
			forms := [][]string{tc.args}
			if len(tc.args) == 2 {
				forms = append(forms, []string{tc.args[0] + "=" + tc.args[1]})
			}
			for form, args := range forms {
				t.Run(fmt.Sprintf("%s/%s/%d", kind, tc.name, form), func(t *testing.T) {
					c, _ := fixture(t)
					c.Lanes[0].Kind = kind
					if tc.name == "skip-git-repo-check" && kind == "codex" {
						args = append(append([]string(nil), args...), "--sandbox=workspace-write")
					}
					c.Lanes[0].ExtraArgs = args
					_, err := c.Validate()
					if (err == nil) != (tc.allowedKind == kind) {
						t.Fatalf("acceptance mismatch: error=%v", err)
					}
				})
			}
		}
		for _, flag := range []string{"--permission-mode", "--sandbox"} {
			t.Run(kind+"/missing/"+flag, func(t *testing.T) {
				c, _ := fixture(t)
				c.Lanes[0].Kind = kind
				c.Lanes[0].ExtraArgs = []string{flag}
				if _, err := c.Validate(); err == nil {
					t.Fatal("missing value accepted")
				}
			})
		}
	}
	bad := [][]string{{"--dangerously-skip-permissions"}, {"--permission-mode", "bypassPermissions"}, {"--permission-mode=bypassPermissions"}, {"--dangerously-bypass-approvals-and-sandbox"}, {"--yolo"}, {"--sandbox", "danger-full-access"}, {"--allow-dangerously-skip-permissions"}, {"--dangerously-bypass-hook-trust"}, {"--bare"}, {"--safe-mode"}, {"--ignore-rules"}, {"-c", "sandbox_mode=\"danger-full-access\""}, {"--settings", "{}"}, {"--cd", "/tmp"}, {"--output-format", "text"}}
	for i, args := range bad {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			c, _ := fixture(t)
			c.Lanes[0].ExtraArgs = args
			if _, e := c.Validate(); e == nil {
				t.Fatal("unsafe arguments accepted")
			}
		})
	}
	c, _ := fixture(t)
	c.Lanes[0].ExtraArgs = []string{"--model", "example", "--effort", "high", "--permission-mode=auto"}
	if _, e := c.Validate(); e != nil {
		t.Fatal(e)
	}
	for _, change := range []func(*Config){func(c *Config) { c.Lanes[0].Binary = "agent" }, func(c *Config) { c.Lanes[0].Name = "../bad" }, func(c *Config) { c.Lanes[1].Name = c.Lanes[0].Name }, func(c *Config) { c.MaxRuntime = "0s" }, func(c *Config) { c.ReportsDir = "relative" }, func(c *Config) { c.Lanes[0].Kind = "shell" }} {
		x, _ := fixture(t)
		change(&x)
		if _, e := x.Validate(); e == nil {
			t.Fatal("invalid config accepted")
		}
	}
	p := filepath.Join(t.TempDir(), "lanes.json")
	b, _ := json.Marshal(c)
	if e := os.WriteFile(p, b, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := LoadConfig(p); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(p, []byte(`{"lanes":[],"unknown":true}`), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := LoadConfig(p); e == nil {
		t.Fatal("unknown config field accepted")
	}
}

func TestGoalIDAndSize(t *testing.T) {
	m, _, _ := manager(t)
	for _, goal := range []string{"A", "a-b", "../AA", strings.Repeat("A", 65), "OK/NO"} {
		if _, e := m.Dispatch("claude", goal, "hello"); e == nil {
			t.Fatal("invalid goal accepted")
		}
	}
	for _, text := range []string{"", strings.Repeat("x", 12001)} {
		if _, e := m.Dispatch("claude", "AB", text); e == nil {
			t.Fatal("invalid text size accepted")
		}
	}
	if _, e := m.Dispatch("missing", "AB", "hello"); e == nil {
		t.Fatal("unknown lane accepted")
	}
	if _, e := m.Dispatch("claude", strings.Repeat("A", 64), strings.Repeat("x", 12000)); e != nil {
		t.Fatal(e)
	}
	waitState(t, m, "claude", "ended")
}

func TestDispatchReplyArgvAndStore(t *testing.T) {
	goalEndParallel(t)
	// Keep the fake command's complete cwd below the 120-rune summary cap,
	// independent of TMPDIR and testing's long per-test directory names.
	workdir, err := os.MkdirTemp(".", ".tail-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workdir) })
	workdir, err = filepath.Abs(workdir)
	if err != nil {
		t.Fatal(err)
	}
	m, c, store := manager(t)
	m.mu.Lock()
	for i := range c.Lanes {
		c.Lanes[i].Workdir = workdir
		m.config.Lanes[i].Workdir = workdir
	}
	m.mu.Unlock()
	for _, lane := range []string{"claude", "codex"} {
		id, e := m.Dispatch(lane, "DEMO-1", "hello slow-finish")
		if e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(filepath.Join(c.ReportsDir, "DEMO-1.txt"), []byte("done"), 0600); e != nil {
			t.Fatal(e)
		}
		s := waitState(t, m, lane, "ended")
		if s.Exit != nil || s.Outcome != "completed" {
			t.Fatal("detached outcome must come from report; exit is unknown")
		}
		dir := filepath.Join(store, "runs", id)
		for _, name := range []string{"goal.txt", "events.jsonl", "stderr.log", "meta.json"} {
			st, e := os.Stat(filepath.Join(dir, name))
			if e != nil || st.Mode().Perm() != 0600 {
				t.Fatal("private file mode", name, e)
			}
		}
		st, e := os.Stat(dir)
		if e != nil || st.Mode().Perm() != 0700 {
			t.Fatal("directory mode")
		}
		var r Run
		b, _ := os.ReadFile(filepath.Join(dir, "meta.json"))
		if e := json.Unmarshal(b, &r); e != nil {
			t.Fatal(e)
		}
		sum := sha256.Sum256([]byte("hello slow-finish"))
		if r.SHA256 != hex.EncodeToString(sum[:]) || r.SessionID == "" {
			t.Fatal("metadata mismatch")
		}
		rows, e := m.Tail(lane, 200)
		if e != nil {
			t.Fatal(e)
		}
		joined := strings.Join(rows, "\n")
		if !strings.Contains(joined, "cwd="+c.Lanes[0].Workdir) || !strings.Contains(joined, "fixture command") || strings.Contains(joined, "must-not-return") {
			t.Fatal("tail contract")
		}
		if lane == "claude" && !strings.Contains(joined, `["-p","--output-format","stream-json","--verbose","--permission-mode=auto"]`) {
			t.Fatal("claude argv")
		}
		if lane == "codex" && !strings.Contains(joined, `["exec","--json","--sandbox=workspace-write","-"]`) {
			t.Fatal("codex argv")
		}
		if _, e := m.Reply(lane, "follow up slow-finish"); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(filepath.Join(c.ReportsDir, "DEMO-1.txt"), []byte("done"), 0600); e != nil {
			t.Fatal(e)
		}
		waitState(t, m, lane, "ended")
		rows, e = m.Tail(lane, 200)
		if e != nil {
			t.Fatal(e)
		}
		if !strings.Contains(strings.Join(rows, "\n"), "resume") {
			t.Fatal("no resume argv")
		}
	}
	b, e := os.ReadFile(filepath.Join(store, "audit.jsonl"))
	if e != nil || bytes.Contains(b, []byte("follow up")) {
		t.Fatal("audit must omit prompt text")
	}
	for _, l := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
		if !json.Valid(l) {
			t.Fatal("invalid audit line")
		}
	}
}

func TestBusyLaneAndConcurrentDispatch(t *testing.T) {
	m, c, store := manager(t)
	if _, e := m.Dispatch("claude", "BUSY-1", "wait"); e != nil {
		t.Fatal(e)
	}
	waitReady(t, m, "claude")
	second, e := NewManager(c, store)
	if e != nil {
		t.Fatal(e)
	}
	defer second.Close()
	if _, e := second.Dispatch("claude", "BUSY-2", "hello"); e == nil {
		t.Fatal("cross-manager busy lane accepted")
	}
	if _, e := m.Reply("claude", "hello"); e == nil {
		t.Fatal("reply on running lane accepted")
	}
	if e := m.Cancel("claude"); e != nil {
		t.Fatal(e)
	}
	waitState(t, m, "claude", "ended")
	var wg sync.WaitGroup
	wins := make(chan bool, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, e := m.Dispatch("codex", "BUSY-3", "wait"); wins <- e == nil }()
	}
	wg.Wait()
	close(wins)
	n := 0
	for b := range wins {
		if b {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("successful dispatches=%d", n)
	}
}

func TestTimeoutKill(t *testing.T) {
	c, store := fixture(t)
	c.MaxRuntime = "150ms"
	m, e := NewManager(c, store)
	if e != nil {
		t.Fatal(e)
	}
	m.Grace = 25 * time.Millisecond
	defer m.Close()
	if _, e := m.Dispatch("claude", "TIME-1", "ignore-int"); e != nil {
		t.Fatal(e)
	}
	s := waitState(t, m, "claude", "ended")
	if s.Outcome != "timed_out" || s.Exit != nil {
		t.Fatal("timeout was not forced")
	}
}
func TestRestartRecovery(t *testing.T) {
	m, c, store := manager(t)
	id, e := m.Dispatch("claude", "RECOVER-1", "hello")
	if e != nil {
		t.Fatal(e)
	}
	waitState(t, m, "claude", "ended")
	if e := m.Close(); e != nil {
		t.Fatal(e)
	}
	p := filepath.Join(store, "runs", id, "meta.json")
	b, _ := os.ReadFile(p)
	var r Run
	_ = json.Unmarshal(b, &r)
	r.State = "running"
	r.Exit = nil
	r.Ended = nil
	r.PID = 99999999
	r.StopReason = ""
	if e := m.save(&r); e != nil {
		t.Fatal(e)
	}
	next, e := NewManager(c, store)
	if e != nil {
		t.Fatal(e)
	}
	defer next.Close()
	s := waitState(t, next, "claude", "ended")
	if s.Exit != nil || s.Outcome != "ended_no_report" {
		t.Fatal("recovered exit invented")
	}
	if _, e := next.Reply("claude", "hello"); e != nil {
		t.Fatal(e)
	}
	waitState(t, next, "claude", "ended")
}
func TestReplyNeedsSession(t *testing.T) {
	m, _, _ := manager(t)
	if _, e := m.Reply("claude", "hello"); e == nil {
		t.Fatal("reply without run accepted")
	}
	if _, e := m.Dispatch("claude", "NOSESSION-1", "no-session"); e != nil {
		t.Fatal(e)
	}
	waitState(t, m, "claude", "ended")
	if _, e := m.Reply("claude", "hello"); e == nil {
		t.Fatal("reply without session accepted")
	}
}
func TestRedaction(t *testing.T) {
	samples := []string{"ghp_" + strings.Repeat("a", 30), "gho_abc123", "ghs_abc123", "github_pat_abcdef", "sk-abcdef123", "sk-ant-abcdef123", "xoxb-123-456-abcdef", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.fixture", "-----BEGIN PRIVATE KEY-----\nfixture material\n-----END PRIVATE KEY-----", "password=synthetic-value", "secret=synthetic-value", "token=synthetic-value", "PASSWORD = synthetic-value", "-----BEGIN RSA PRIVATE KEY-----\nunfinished"}
	for i, s := range samples {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			got := Redact("prefix " + s + " suffix")
			if strings.Contains(got, s) || !strings.Contains(got, "[REDACTED]") {
				t.Fatal("secret class not redacted")
			}
		})
	}
	if Redact("ordinary data") != "ordinary data" {
		t.Fatal("ordinary text changed")
	}
}
func TestReportAndOutputBounds(t *testing.T) {
	m, c, _ := manager(t)
	if _, e := m.Report("../outside"); e == nil {
		t.Fatal("traversal accepted")
	}
	p := filepath.Join(c.ReportsDir, "DEMO-2.txt")
	if e := os.WriteFile(p, []byte("token=synthetic-value\n"+strings.Repeat("x", 70000)), 0600); e != nil {
		t.Fatal(e)
	}
	s, e := m.Report("DEMO-2")
	if e != nil || len(s) > 65536 || strings.Contains(s, "synthetic-value") {
		t.Fatal("report bound or redaction")
	}
	if _, e := m.Tail("claude", 201); e == nil {
		t.Fatal("tail limit accepted")
	}
	link := filepath.Join(c.ReportsDir, "LINK-1.txt")
	if e := os.Symlink(p, link); e != nil {
		t.Fatal(e)
	}
	if _, e := m.Report("LINK-1"); e == nil {
		t.Fatal("report symlink accepted")
	}
}

func TestMCPRoundTrip(t *testing.T) {
	m, _, _ := manager(t)
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"fixture","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"nuntius_status","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"nuntius_dispatch","arguments":{"lane":"claude","goal_id":"RPC-1","text":"hello"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"nuntius_dispatch","arguments":{"lane":"claude","goal_id":"RPC-1","text":"hello","binary":"bad"}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"unknown"}`}, "\n") + "\n"
	var out bytes.Buffer
	if e := Serve(context.Background(), m, strings.NewReader(input), &out); e != nil {
		t.Fatal(e)
	}
	lines := bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n"))
	if len(lines) != 6 {
		t.Fatalf("responses=%d", len(lines))
	}
	for _, l := range lines {
		if !json.Valid(l) {
			t.Fatal("non-JSON stdout")
		}
	}
	var list struct {
		Result struct {
			Tools []json.RawMessage `json:"tools"`
		} `json:"result"`
	}
	_ = json.Unmarshal(lines[1], &list)
	if len(list.Result.Tools) != 16 {
		t.Fatal("tool count")
	}
	if !bytes.Contains(lines[4], []byte(`"isError":true`)) || !bytes.Contains(lines[5], []byte(`-32601`)) {
		t.Fatal("error shape")
	}
	waitState(t, m, "claude", "ended")
}

func TestCLIVersion(t *testing.T) {
	var out, err bytes.Buffer
	if code := CLI(context.Background(), []string{"version"}, strings.NewReader(""), &out, &err); code != 0 || !strings.Contains(out.String(), Version) {
		t.Fatal("version CLI")
	}
}

func TestEndedQuestion(t *testing.T) {
	m, _, store := manager(t)
	id, e := m.Dispatch("codex", "QUESTION-1", "hello")
	if e != nil {
		t.Fatal(e)
	}
	waitState(t, m, "codex", "ended")
	question := strings.Repeat("Which option?\n", 80)
	events := "{\"type\":\"item.completed\",\"item\":{\"type\":\"mcp_tool_call\",\"tool\":\"request_user_input\",\"error\":\"unavailable\"}}\n"
	b, _ := json.Marshal(map[string]any{"type": "item.completed", "item": map[string]string{"type": "agent_message", "text": question}})
	if e := os.WriteFile(filepath.Join(store, "runs", id, "events.jsonl"), []byte(events+string(b)+"\n"), 0600); e != nil {
		t.Fatal(e)
	}
	s, e := m.Status()
	if e != nil || s[1].State != "ended_no_report" {
		t.Fatal("missing ended_no_report")
	}
	b, _ = json.Marshal(s[1])
	if !bytes.Contains(b, []byte(`"asked_interactively":true`)) {
		t.Fatal("missing interactive question flag")
	}
	rows, e := m.Tail("codex", 1)
	if e != nil || strings.Join(rows, "\n") != question {
		t.Fatal("final question truncated to requested lines")
	}
}

func TestClaudeGoalThenContinue(t *testing.T) {
	m, c, store := manager(t)
	first, e := m.Dispatch("claude", "CONTINUE-1", "ack-goal")
	if e != nil {
		t.Fatal(e)
	}
	s := waitState(t, m, "claude", "ended")
	if s.RunID == first {
		t.Fatal("acknowledgement did not trigger continuation")
	}
	if e := os.WriteFile(filepath.Join(c.ReportsDir, "CONTINUE-1.txt"), []byte("done"), 0600); e != nil {
		t.Fatal(e)
	}
	for _, id := range []string{first, s.RunID} {
		b, e := os.ReadFile(filepath.Join(store, "runs", id, "meta.json"))
		if e != nil {
			t.Fatal(e)
		}
		if !bytes.Contains(b, []byte(first)) || !bytes.Contains(b, []byte(s.RunID)) {
			t.Fatal("linked run ids missing")
		}
	}
	b, e := os.ReadFile(filepath.Join(store, "runs", s.RunID, "goal.txt"))
	if e != nil || string(b) != "continue" {
		t.Fatal("default continuation text")
	}
	rows, e := m.Tail("claude", 200)
	if e != nil || !strings.Contains(strings.Join(rows, "\n"), "--resume") {
		t.Fatal("continuation did not resume")
	}
	entries, _ := os.ReadDir(filepath.Join(store, "runs"))
	if len(entries) != 2 {
		t.Fatal("continuation repeated")
	}
}
func TestClaudeAlreadyWorkedNotContinued(t *testing.T) {
	m, _, store := manager(t)
	id, e := m.Dispatch("claude", "WORK-1", "hello")
	if e != nil {
		t.Fatal(e)
	}
	s := waitState(t, m, "claude", "ended")
	entries, _ := os.ReadDir(filepath.Join(store, "runs"))
	if s.RunID != id || len(entries) != 1 {
		t.Fatal("working Claude continued")
	}
}
func TestCodexNeverContinued(t *testing.T) {
	m, _, store := manager(t)
	id, e := m.Dispatch("codex", "CODEX-1", "ack-goal")
	if e != nil {
		t.Fatal(e)
	}
	s := waitState(t, m, "codex", "ended")
	entries, _ := os.ReadDir(filepath.Join(store, "runs"))
	if s.RunID != id || len(entries) != 1 {
		t.Fatal("Codex continued")
	}
}

func TestContinuationConfiguration(t *testing.T) {
	for _, mode := range []string{"as_is", "goal_then_continue"} {
		t.Run(mode, func(t *testing.T) {
			c, store := fixture(t)
			c.Lanes[0].StartMode = mode
			c.Lanes[0].ContinueText = "owner continuation"
			m, e := NewManager(c, store)
			if e != nil {
				t.Fatal(e)
			}
			defer m.Close()
			id, e := m.Dispatch("claude", "MODE-1", "ack-goal")
			if e != nil {
				t.Fatal(e)
			}
			s := waitState(t, m, "claude", "ended")
			if mode == "as_is" && s.RunID != id {
				t.Fatal("as_is continued")
			}
			if mode != "as_is" {
				b, e := os.ReadFile(filepath.Join(store, "runs", s.RunID, "goal.txt"))
				if e != nil || string(b) != "owner continuation" {
					t.Fatal("custom continue text")
				}
			}
		})
	}
}
func TestTailPhysicalLinesAndMultilineRedaction(t *testing.T) {
	m, c, store := manager(t)
	if e := os.WriteFile(filepath.Join(c.ReportsDir, "TAIL-1.txt"), []byte("done"), 0600); e != nil {
		t.Fatal(e)
	}
	id, e := m.Dispatch("codex", "TAIL-1", "hello")
	if e != nil {
		t.Fatal(e)
	}
	waitState(t, m, "codex", "ended")
	var data bytes.Buffer
	for _, s := range []string{"-----BEGIN PRIVATE KEY-----", "synthetic material", "-----END PRIVATE KEY-----", strings.Repeat("line\n", 300)} {
		v, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]string{"type": "text", "text": s}}}})
		data.Write(v)
		data.WriteByte('\n')
	}
	if e := os.WriteFile(filepath.Join(store, "runs", id, "events.jsonl"), data.Bytes(), 0600); e != nil {
		t.Fatal(e)
	}
	rows, e := m.Tail("codex", 200)
	if e != nil || len(rows) > 200 || strings.Contains(strings.Join(rows, "\n"), "synthetic material") {
		t.Fatal("tail physical lines/redaction")
	}
}
func TestCLICheckStatusWatchAndMCPUnavailable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c, store := fixture(t)
	path := filepath.Join(t.TempDir(), "lanes.json")
	b, _ := json.Marshal(c)
	if e := os.WriteFile(path, b, 0600); e != nil {
		t.Fatal(e)
	}
	for _, command := range [][]string{{"config", "check"}, {"status"}, {"watch", "claude"}} {
		var out, err bytes.Buffer
		args := append([]string{"--config", path, "--store", store}, command...)
		if CLI(context.Background(), args, strings.NewReader(""), &out, &err) != 0 {
			t.Fatal("CLI command failed")
		}
	}
	in := `{"jsonrpc":"2.0","id":1,"method":"initialize"}` + "\n" + `{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" + `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"nuntius_status","arguments":{}}}` + "\n"
	var out, err bytes.Buffer
	if CLI(context.Background(), []string{"--config", filepath.Join(t.TempDir(), "missing"), "mcp"}, strings.NewReader(in), &out, &err) != 0 || !strings.Contains(out.String(), "not serving") {
		t.Fatal("MCP startup fault was not exposed on live transport")
	}
}
func TestRecoveredLivePIDNotSignalled(t *testing.T) {
	m, c, store := manager(t)
	id, e := m.Dispatch("codex", "LIVE-1", "hello")
	if e != nil {
		t.Fatal(e)
	}
	waitState(t, m, "codex", "ended")
	_ = m.Close()
	p := filepath.Join(store, "runs", id, "meta.json")
	b, _ := os.ReadFile(p)
	var r Run
	_ = json.Unmarshal(b, &r)
	r.State = "running"
	r.PID = os.Getpid()
	// A v1 record has no identity; it must remain busy and never be signalled.
	r.ProcessStart = ""
	r.Ended = nil
	r.Exit = nil
	r.Started = time.Now().Add(-time.Hour)
	r.Integrity = "" // Deliberately synthesize a legacy v1 record.
	b, _ = json.Marshal(r)
	if e := os.WriteFile(p, b, 0600); e != nil {
		t.Fatal(e)
	}
	next, e := NewManager(c, store)
	if e != nil {
		t.Fatal(e)
	}
	defer next.Close()
	s, e := next.Status()
	if e != nil {
		t.Fatal(e)
	}
	if s[1].State != "running" {
		t.Fatal("live process considered gone")
	}
	if _, e = next.Dispatch("codex", "LIVE-2", "hello"); e == nil {
		t.Fatal("live PID lane reused")
	}
	if next.Cancel("codex") == nil {
		t.Fatal("foreign PID signalled")
	}
}
