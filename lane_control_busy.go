package nuntius

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Projection never reads mutable control/intent/held-message records without
// the shared lock. Busy is a current observation, not a stale permission grant.
func (m *Manager) laneControlFailureSnapshot(runs []Run, err error) laneControlSnapshot {
	s := m.projectLaneControls(laneControlStore{Version: 1}, nil, runs, false)
	var fault *laneControlFault
	if !errors.As(err, &fault) || fault.Code != "locked" {
		return s
	}
	reason := controlLockHolderExplanation(m.store)
	for i := range s.Lanes {
		lane := &s.Lanes[i]
		lane.Phase = "busy"
		for j, code := range lane.Blockers {
			if code == "lane_control_unavailable" {
				lane.Blockers[j] = "lane_control_busy"
			}
		}
		lane.Explanation = reason + "; lane hold is unknown; new work is blocked"
	}
	return s
}

// Diagnostic only. Neither lsof nor executable provenance grants authority.
// A single deadline bounds both commands. No argv, paths or raw output escape.
func controlLockHolderExplanation(store string) string {
	unknown := "Controls busy: held by another process; holder version is unknown; restart older Nuntius MCP processes if this persists"
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	tool, err := exec.LookPath("lsof")
	if err != nil {
		return unknown
	}
	b, err := controlLockDiagnostic(ctx, tool, "-nP", "-Fpcfl", "--", filepath.Join(store, "locks", "control.lock"))
	if err != nil {
		return unknown
	}
	pid := controlLockHolderPID(b)
	if pid == 0 {
		return unknown
	}
	b, err = controlLockDiagnostic(ctx, tool, "-nP", "-a", "-p", strconv.Itoa(pid), "-d", "txt", "-Fn")
	version := ""
	if err == nil {
		version = controlLockHomebrewVersion(b)
	}
	return describeControlLockHolder(pid, version)
}

func describeControlLockHolder(pid int, version string) string {
	prefix := "Controls busy: held by another Nuntius process (PID " + strconv.Itoa(pid) + ")"
	if olderControlLockVersion(version) {
		return "Controls busy: an older Nuntius process (PID " + strconv.Itoa(pid) + ", " + version + ") holds the control lock; restart it after the upgrade"
	}
	if version != "" && version != Version {
		return prefix + " running a different Nuntius version (" + version + "); restart it after the upgrade"
	}
	if version == Version {
		return prefix + "; retry when its operation finishes"
	}
	return prefix + "; version is unknown; restart older Nuntius MCP processes if this persists"
}

func olderControlLockVersion(version string) bool {
	a, b := strings.Split(strings.TrimPrefix(version, "v"), "."), strings.Split(strings.TrimPrefix(Version, "v"), ".")
	if len(a) != 3 || len(b) != 3 {
		return false
	}
	for i := range a {
		x, err := strconv.ParseUint(a[i], 10, 32)
		if err != nil {
			return false
		}
		y, err := strconv.ParseUint(b[i], 10, 32)
		if err != nil {
			return false
		}
		if x != y {
			return x < y
		}
	}
	return false
}

func controlLockDiagnostic(ctx context.Context, tool string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, tool, args...)
	cmd.WaitDelay = 20 * time.Millisecond
	cmd.Stderr = io.Discard
	out := &controlDiagnosticBuffer{}
	cmd.Stdout = out
	err := cmd.Run()
	if out.overflow {
		return nil, errors.New("lock diagnostic exceeded bound")
	}
	return out.data, err
}

type controlDiagnosticBuffer struct {
	data     []byte
	overflow bool
}

func (b *controlDiagnosticBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if len(b.data)+n > 16*1024 {
		b.overflow = true
		return n, nil
	}
	b.data = append(b.data, p...)
	return n, nil
}

func controlLockHolderPID(b []byte) int {
	pid := 0
	nuntius := false
	for _, line := range strings.Split(string(b), "\n") {
		if len(line) < 2 {
			continue
		}
		switch line[0] {
		case 'p':
			pid, _ = strconv.Atoi(line[1:])
			nuntius = false
		case 'c':
			nuntius = line[1:] == "nuntius"
		case 'l':
			if nuntius && pid > 0 && pid <= 1<<31-1 && pid != os.Getpid() && (line[1:] == "W" || line[1:] == "R") {
				return pid
			}
		}
	}
	return 0
}

func controlLockHomebrewVersion(b []byte) string {
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "n/") {
			continue
		}
		parts := strings.Split(strings.TrimPrefix(line, "n"), "/")
		for i := 0; i+3 < len(parts); i++ {
			if (parts[i] == "Caskroom" || parts[i] == "Cellar") && parts[i+1] == "nuntius" && filepath.Base(line) == "nuntius" {
				v := strings.TrimPrefix(parts[i+2], "v")
				if len(v) <= 64 && webVersionPattern.MatchString("v"+v) {
					return "v" + v
				}
			}
		}
	}
	return ""
}
