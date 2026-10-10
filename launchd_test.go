package nuntius

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Fake launchctl exercises the actual plist/exec bootstrap with offline agents.
type fakeLaunchctl struct {
	mu       sync.Mutex
	jobs     map[string]string
	calls    []string
	fail     string
	printPID int
}

func (f *fakeLaunchctl) call(args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.Join(args, " "))
	if args[0] == f.fail || f.fail == "all" {
		return nil, errors.New("fixture launchctl failure")
	}
	switch args[0] {
	case "bootstrap":
		b, err := os.ReadFile(args[2])
		if err != nil {
			return nil, err
		}
		text := string(b)
		for _, required := range []string{"<key>LaunchOnlyOnce</key><true/>", "<key>KeepAlive</key><false/>", "<key>RunAtLoad</key><false/>", "StandardInPath", "StandardOutPath", "StandardErrorPath"} {
			if !strings.Contains(text, required) {
				return nil, errors.New("unsafe fixture plist")
			}
		}
		dir := filepath.Dir(args[2])
		f.jobs["gui/fixture/nuntius.run."+filepath.Base(dir)] = dir
	case "kickstart":
		dir := f.jobs[args[2]]
		r, err := loadRun(dir, filepath.Base(dir))
		if err != nil || r == nil {
			return nil, errors.New("missing recorded run")
		}
		// launchd is replaced by a test-owned process running the real exec stage.
		b, err := os.ReadFile(filepath.Join(dir, "launch.json"))
		if err != nil {
			return nil, err
		}
		var spec launchSpec
		if strictJSON(b, &spec) != nil {
			return nil, errors.New("bad spec")
		}
		in, err := privateFile(filepath.Join(dir, "goal.txt"), os.O_RDONLY)
		if err != nil {
			return nil, err
		}
		defer in.Close()
		out, err := privateFile(filepath.Join(dir, "events.jsonl"), os.O_WRONLY)
		if err != nil {
			return nil, err
		}
		defer out.Close()
		stderr, err := privateFile(filepath.Join(dir, "stderr.log"), os.O_WRONLY)
		if err != nil {
			return nil, err
		}
		defer stderr.Close()
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		cmd := exec.Command(exe, "__nuntius_launchd_exec", dir)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, stderr
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err = cmd.Start(); err != nil {
			return nil, err
		}
		go func() { _ = cmd.Wait() }()
		return []byte(fmt.Sprint(cmd.Process.Pid)), nil
	case "print":
		if f.printPID != 0 {
			return []byte(fmt.Sprintf("service = {\n\tpid = %d\n}", f.printPID)), nil
		}
		dir, ok := f.jobs[args[1]]
		if !ok {
			return nil, errJobAbsent
		}
		r, _ := loadRun(dir, filepath.Base(dir))
		if r != nil && sameProcess(*r) {
			return []byte(fmt.Sprintf("service = {\n\tpid = %d\n}", r.PID)), nil
		}
		return []byte("service = {\n\tstate = not running\n}"), nil
	case "bootout":
		delete(f.jobs, args[1])
	}
	return nil, nil
}

func launchdManager(t *testing.T, fail string) (*Manager, *fakeLaunchctl, string) {
	t.Helper()
	m, _, store := manager(t)
	f := &fakeLaunchctl{jobs: map[string]string{}, fail: fail}
	m.mu.Lock()
	m.launcher = &jobLauncher{domain: "gui/fixture", call: f.call}
	m.mu.Unlock()
	return m, f, store
}

func TestLaunchdRunLifecycle(t *testing.T) {
	for _, mode := range []string{"end", "cancel", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			m, f, store := launchdManager(t, "")
			if mode == "timeout" {
				m.mu.Lock()
				m.runtime = 300 * time.Millisecond
				m.Grace = 50 * time.Millisecond
				m.mu.Unlock()
			}
			prompt := "slow-finish"
			if mode != "end" {
				prompt = "wait"
			}
			id, err := m.Dispatch("codex", "LAUNCHD-LIFECYCLE", prompt)
			if err != nil {
				t.Fatal(err)
			}
			r, err := loadRun(filepath.Join(store, "runs", id), id)
			if err != nil || r == nil || r.LaunchdLabel != "nuntius.run."+id || r.ProcessStart == "" {
				t.Fatal("missing job identity")
			}
			plist, err := os.ReadFile(filepath.Join(store, "runs", id, "job.plist"))
			if err != nil || !strings.Contains(string(plist), fmt.Sprintf("<key>Umask</key><integer>%d</integer>", dispatchUmask)) {
				t.Fatal("launchd plist did not inherit dispatch umask")
			}
			if mode == "cancel" {
				if err = m.Cancel("codex"); err != nil {
					t.Fatal(err)
				}
			}
			waitState(t, m, "codex", "ended")
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.jobs) != 0 {
				t.Fatal("job leaked")
			}
			if !strings.Contains(strings.Join(f.calls, "\n"), "bootout gui/fixture/nuntius.run."+id) {
				t.Fatal("missing job removal")
			}
		})
	}
}

func TestCaptureUmask(t *testing.T) {
	for _, mask := range []int{0, 0022, 0077} {
		current := mask
		got := captureUmask(func(next int) (int, error) {
			old := current
			current = next
			return old, nil
		})
		if got != mask || current != mask {
			t.Fatal("umask not captured and restored")
		}
	}
	if captureUmask(func(int) (int, error) { return 0, errors.New("unavailable") }) != 0022 {
		t.Fatal("missing fallback umask")
	}
}

func TestLaunchdFallback(t *testing.T) {
	for _, step := range []string{"bootstrap", "kickstart", "all"} {
		t.Run(step, func(t *testing.T) {
			m, f, store := launchdManager(t, step)
			id, err := m.Dispatch("codex", "LAUNCHD-FALLBACK", "wait")
			if err != nil {
				t.Fatal(err)
			}
			r, _ := loadRun(filepath.Join(store, "runs", id), id)
			if r == nil || !r.LaunchFallback || (step != "all" && r.LaunchdLabel != "") || !sameProcess(*r) {
				t.Fatal("fallback unavailable")
			}
			rows, err := m.Status()
			if err != nil || !rows[1].LaunchFallback {
				t.Fatal("fallback not in status")
			}
			b, err := os.ReadFile(filepath.Join(store, "runs", id, "events.jsonl"))
			if err != nil || !strings.Contains(string(b), "launchd_fallback") {
				t.Fatal("fallback not in events")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.jobs) != 0 {
				t.Fatal("failed job leaked")
			}
		})
	}
}

func TestLaunchdRecoveryAndLabelSafety(t *testing.T) {
	m, f, store := launchdManager(t, "")
	id, err := m.Dispatch("codex", "LAUNCHD-RECOVER", "wait")
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Cancel("codex"); err != nil {
		t.Fatal(err)
	}
	waitState(t, m, "codex", "ended")
	r, _ := loadRun(filepath.Join(store, "runs", id), id)
	r.LaunchdCleaned = false
	if err = m.save(r); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.jobs["gui/fixture/owner.other"] = "untouched"
	f.jobs["gui/fixture/nuntius.run."+id] = filepath.Join(store, "runs", id)
	f.mu.Unlock()
	if err = m.Recover(); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	_, stale := f.jobs["gui/fixture/nuntius.run."+id]
	_, unrelated := f.jobs["gui/fixture/owner.other"]
	f.mu.Unlock()
	if stale || !unrelated {
		t.Fatal("recovery label boundary broken")
	}
	r.LaunchdLabel = "owner.other"
	if err = m.cleanupJob(r); err == nil {
		t.Fatal("unrelated label accepted")
	}
	r.LaunchdLabel = "nuntius.run." + strings.Repeat("a", 32)
	if err = m.cleanupJob(r); err == nil {
		t.Fatal("unrecorded label accepted")
	}
}

func TestLaunchdIdentityAndEnvironment(t *testing.T) {
	t.Setenv("NUNTIUS_FIXTURE_ENV", "inherited-fixture-marker")
	m, f, store := launchdManager(t, "")
	id, err := m.Dispatch("codex", "LAUNCHD-IDENTITY", "wait")
	if err != nil {
		t.Fatal(err)
	}
	r, _ := loadRun(filepath.Join(store, "runs", id), id)
	f.mu.Lock()
	f.printPID = r.PID
	before := len(f.calls)
	f.mu.Unlock()
	wrong := *r
	wrong.ProcessStart = "mismatch"
	if err = m.cleanupJob(&wrong); err == nil {
		t.Fatal("mismatched start time removed job")
	}
	f.mu.Lock()
	for _, call := range f.calls[before:] {
		if strings.HasPrefix(call, "bootout ") {
			t.Fatal("unsafe bootout")
		}
	}
	f.printPID = 0
	f.mu.Unlock()
	b, err := os.ReadFile(filepath.Join(store, "runs", id, "launch.json"))
	if err != nil {
		t.Fatal(err)
	}
	var spec launchSpec
	if strictJSON(b, &spec) != nil {
		t.Fatal("bad launch spec")
	}
	if strings.Contains(string(b), "inherited-fixture-marker") {
		t.Fatal("environment persisted to state")
	}
	for deadline := time.Now().Add(3 * time.Second); ; {
		output, err := os.ReadFile(filepath.Join(store, "runs", id, "events.jsonl"))
		if err == nil && strings.Contains(string(output), "env=inherited-fixture-marker") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("inherited environment not restored")
		}
		time.Sleep(time.Millisecond)
	}
	if spec.Workdir != m.config.Lanes[1].Workdir || strings.Join(spec.Args, "\x00") != strings.Join(agentArgs(m.config.Lanes[1], ""), "\x00") {
		t.Fatal("argv or cwd changed")
	}
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
	if !sameProcess(*r) {
		t.Fatal("close killed launchd agent")
	}
	m2, err := NewManager(m.config, store)
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	m2.mu.Lock()
	m2.launcher = m.launcher
	m2.mu.Unlock()
	if err = m2.Freeze("fixture"); err != nil {
		t.Fatal(err)
	}
	waitState(t, m2, "codex", "ended")
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.jobs) != 0 {
		t.Fatal("recovered frozen run leaked job")
	}
}

func TestLaunchdPendingIdentityRecovery(t *testing.T) {
	m, f, store := launchdManager(t, "")
	id, err := m.Dispatch("codex", "LAUNCHD-PENDING", "wait")
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
	r, _ := loadRun(filepath.Join(store, "runs", id), id)
	original := *r
	defer func() {
		_ = saveAt(filepath.Join(store, "runs", id), &original)
		_ = signalRun(original, syscall.SIGKILL)
	}()
	f.mu.Lock()
	f.printPID = r.PID
	f.mu.Unlock()
	r.PID, r.ProcessStart = 0, ""
	if err = saveAt(filepath.Join(store, "runs", id), r); err != nil {
		t.Fatal(err)
	}
	m2 := &Manager{store: store, config: m.config, launcher: m.launcher, active: map[string]*activeRun{}, closing: make(chan struct{})}
	defer m2.Close()
	if err = m2.Recover(); err != nil {
		t.Fatal(err)
	}
	r, _ = loadRun(filepath.Join(store, "runs", id), id)
	if r.State != "running" || r.PID != original.PID || r.ProcessStart != "" || r.ContinueRunID != "" {
		t.Fatal("pending launch was duplicated or guessed")
	}
	if err = m2.Cancel("codex"); err == nil {
		t.Fatal("pending PID signalled without start identity")
	}
}

func TestLaunchdResumeCreatesDistinctJob(t *testing.T) {
	m, f, store := launchdManager(t, "")
	first, err := m.Dispatch("codex", "LAUNCHD-RESUME", "slow-finish")
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, m, "codex", "ended")
	second, err := m.Reply("codex", "wait")
	if err != nil {
		t.Fatal(err)
	}
	r, _ := loadRun(filepath.Join(store, "runs", second), second)
	if first == second || r.LaunchdLabel != "nuntius.run."+second {
		t.Fatal("resume reused job")
	}
	b, err := os.ReadFile(filepath.Join(store, "runs", second, "launch.json"))
	if err != nil {
		t.Fatal(err)
	}
	var spec launchSpec
	if strictJSON(b, &spec) != nil || !strings.Contains(strings.Join(spec.Args, " "), "resume fixture-thread -") {
		t.Fatal("resume argv changed")
	}
	if err = m.Cancel("codex"); err != nil {
		t.Fatal(err)
	}
	waitState(t, m, "codex", "ended")
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.jobs) != 0 {
		t.Fatal("resume leaked job")
	}
}

func TestLaunchdFallbackContinuation(t *testing.T) {
	m, _, _ := launchdManager(t, "bootstrap")
	first, err := m.Dispatch("claude", "LAUNCHD-CONTINUE", "ack-goal")
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, m, "claude", "ended")
	runs, err := m.history()
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatal("fallback suppressed automatic continuation")
	}
	for _, r := range runs {
		if r.ID == first && r.ContinueRunID == "" {
			t.Fatal("missing continuation link")
		}
	}
}
