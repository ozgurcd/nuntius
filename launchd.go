package nuntius

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

var errJobAbsent = errors.New("launchd job absent")
var errBeforeKickstart = errors.New("launchd start was not attempted")

type jobLauncher struct {
	domain string
	call   func(...string) ([]byte, error)
}

// Selected once at construction; tests inject a fake, never the host launchctl.
var newJobLauncher = platformJobLauncher

func platformJobLauncher() *jobLauncher {
	if runtime.GOOS != "darwin" {
		return nil
	}
	return &jobLauncher{domain: fmt.Sprintf("gui/%d", os.Getuid()), call: func(args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "/bin/launchctl", args...).Output()
		var exit *exec.ExitError
		if (args[0] == "print" || args[0] == "bootout") && errors.As(err, &exit) && exit.ExitCode() == 113 {
			return nil, errJobAbsent
		}
		// Never return launchctl stderr: paths or inherited job details are private.
		if err != nil {
			return nil, errors.New("launchctl command failed")
		}
		return out, nil
	}}
}

func plistString(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return "<string>" + b.String() + "</string>"
}

func (j *jobLauncher) start(dir string, lane Lane, session string) (err error) {
	kickstarted := false
	defer func() {
		if err != nil && !kickstarted {
			err = errors.Join(errBeforeKickstart, err)
		}
	}()
	r, err := loadRun(dir, filepath.Base(dir))
	if err != nil || r == nil {
		return errors.New("launchd run unavailable")
	}
	r.LaunchdLabel = "nuntius.run." + r.ID
	if err = saveAt(dir, r); err != nil {
		return err
	}
	// Restore this exact environment in the exec bootstrap. launchd otherwise
	// supplies its own environment. Command.Environ applies the existing PWD rule.
	cmd := exec.Command(lane.Binary)
	cmd.Dir = lane.Workdir
	stop, err := environmentPipe(dir, cmd.Environ())
	if err != nil {
		return err
	}
	defer stop()
	spec := launchSpec{Binary: lane.Binary, Args: agentArgs(lane, session), Workdir: lane.Workdir, EnvironmentPipe: true}
	b, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	if len(b) > 65536 {
		return errors.New("launch environment too large")
	}
	if err = atomicPrivate(filepath.Join(dir, "launch.json"), b); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	text := `<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict><key>Label</key>` + plistString(r.LaunchdLabel) +
		`<key>ProgramArguments</key><array>` + plistString(exe) + plistString("__nuntius_launchd_exec") + plistString(dir) + `</array>` +
		`<key>WorkingDirectory</key>` + plistString(lane.Workdir) +
		`<key>StandardInPath</key>` + plistString(filepath.Join(dir, "goal.txt")) +
		`<key>StandardOutPath</key>` + plistString(filepath.Join(dir, "events.jsonl")) +
		`<key>StandardErrorPath</key>` + plistString(filepath.Join(dir, "stderr.log")) +
		`<key>RunAtLoad</key><false/><key>KeepAlive</key><false/><key>LaunchOnlyOnce</key><true/><key>Umask</key><integer>63</integer></dict></plist>`
	path := filepath.Join(dir, "job.plist")
	if err = atomicPrivate(path, []byte(text)); err != nil {
		return err
	}
	if _, err = j.call("bootstrap", j.domain, path); err != nil {
		return err
	}
	kickstarted = true
	if _, err = j.call("kickstart", "-p", j.domain+"/"+r.LaunchdLabel); err != nil {
		return err
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		r, err = loadRun(dir, filepath.Base(dir))
		if err != nil {
			return err
		}
		if r != nil && r.PID > 1 && r.ProcessStart != "" {
			return nil
		}
		time.Sleep(time.Millisecond)
	}
	return errors.New("launchd identity unavailable")
}

// Only the exact label of an intact recorded run may be queried or removed.
// A job backing an unverifiable live PID is never booted out (bootout kills).
func (m *Manager) cleanupJob(r *Run) error {
	if r.LaunchdLabel == "" || m.launcher == nil {
		return nil
	}
	if !runPattern.MatchString(r.ID) || r.LaunchdLabel != "nuntius.run."+r.ID {
		return errors.New("unrecorded launchd label refused")
	}
	if r.LaunchdCleaned {
		return nil
	}
	target := m.launcher.domain + "/" + r.LaunchdLabel
	b, err := m.launcher.call("print", target)
	if errors.Is(err, errJobAbsent) {
		return m.jobCleaned(r)
	}
	if err != nil {
		return err
	}
	identified := false
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "state = not running" || line == "state = waiting" {
			identified = true
		}
		if strings.HasPrefix(line, "pid = ") {
			pid, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "pid = ")))
			if err != nil || pid <= 1 || pid != r.PID || !sameProcess(*r) {
				return errors.New("launchd process identity mismatch; no removal")
			}
			identified = true
		}
	}
	if !identified {
		return errors.New("launchd state unrecognized; no removal")
	}
	_, err = m.launcher.call("bootout", target)
	if errors.Is(err, errJobAbsent) {
		return m.jobCleaned(r)
	}
	if err != nil {
		return err
	}
	return m.jobCleaned(r)
}

func (m *Manager) jobCleaned(r *Run) error {
	for _, name := range []string{"job.plist", "launch.json", "environment.pipe"} {
		if err := os.Remove(filepath.Join(m.store, "runs", r.ID, name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	r.LaunchdCleaned = true
	return syncDir(filepath.Join(m.store, "runs", r.ID))
}

// A crash can precede the exec stage's identity write. Remember only the PID
// reported by this run's job, leaving it unsignalable until the stage records
// its own kernel start time. Do not resume a second agent in that gap.
func (m *Manager) pendingJob(r *Run) error {
	if m.launcher == nil || r.LaunchdLabel == "" || r.ProcessStart != "" {
		return nil
	}
	if !runPattern.MatchString(r.ID) || r.LaunchdLabel != "nuntius.run."+r.ID {
		return errors.New("unrecorded launchd label refused")
	}
	b, err := m.launcher.call("print", m.launcher.domain+"/"+r.LaunchdLabel)
	if errors.Is(err, errJobAbsent) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "pid = ") {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "pid = ")))
		if err != nil || pid <= 1 {
			return errors.New("invalid pending launchd PID")
		}
		r.PID = pid
		return m.save(r)
	}
	return nil
}

func (m *Manager) startAgent(dir string, lane Lane, session string, input, stdout, stderr, lock *os.File) error {
	if m.launcher == nil {
		return detachAgent(dir, lane, session, input, stdout, stderr, lock)
	}
	launchErr := m.launcher.start(dir, lane, session)
	if launchErr == nil {
		return nil
	}
	r, err := loadRun(dir, filepath.Base(dir))
	if err != nil || r == nil {
		return errors.New("failed launch record unavailable")
	}
	// If a failed start partially launched the job, remove it safely before
	// dispatching the fallback. Never duplicate an agent on an ambiguous failure.
	cleanupErr := m.cleanupJob(r)
	if cleanupErr != nil && !errors.Is(launchErr, errBeforeKickstart) {
		return errors.New("launchd fallback cleanup unavailable; goal remains recorded")
	}
	// No kickstart was attempted, so our job cannot have started an agent.
	// Keep its recorded label for later cleanup if launchctl itself is absent.
	if cleanupErr != nil {
		_ = m.audit("launchd_cleanup", r.Lane, r.GoalID, r.SHA256, "failed")
	}
	for deadline := time.Now().Add(5 * time.Second); sameProcess(*r); {
		if time.Now().After(deadline) {
			return errors.New("launchd process still running; no duplicate fallback")
		}
		time.Sleep(time.Millisecond)
	}
	r.PID, r.ProcessStart = 0, ""
	if cleanupErr == nil {
		r.LaunchdLabel = ""
	}
	r.LaunchdCleaned = false
	r.LaunchFallback = true
	if err = saveAt(dir, r); err != nil {
		return err
	}
	if err = os.Remove(filepath.Join(dir, "job.plist")); err != nil && !os.IsNotExist(err) {
		return err
	}
	// Fixed event text has no launchctl output, path, environment or secrets.
	if _, err = stdout.WriteString("{\"type\":\"system\",\"event\":\"launchd_fallback\",\"detail\":\"launchctl failed; using detached start; app-quit survival unavailable\"}\n"); err != nil {
		return err
	}
	if err = stdout.Sync(); err != nil {
		return err
	}
	return detachAgent(dir, lane, session, input, stdout, stderr, lock)
}
