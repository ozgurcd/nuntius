package nuntius

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
)

// An exec bootstrap must replace itself from its startup thread. On Linux, an
// execve from any other thread leaves the thread-group leader a zombie until
// the kernel swaps thread IDs, so a concurrent identity check reads the live
// child as ended. LockOSThread in init keeps main on the startup thread.
func init() {
	if len(os.Args) > 1 && (os.Args[1] == "__nuntius_exec" || os.Args[1] == "__nuntius_launchd_exec") {
		runtime.LockOSThread()
	}
}

// These two bootstrap stages exist only during launch. The intermediate exits;
// the orphaned, session-leading grandchild becomes the agent with exec. No
// wrapper, waiter, supervisor or nuntius process remains beside the agent.
type launchSpec struct {
	Binary          string   `json:"binary"`
	Args            []string `json:"args"`
	Workdir         string   `json:"workdir"`
	EnvironmentPipe bool     `json:"environment_pipe,omitempty"`
}

func detachAgent(dir string, lane Lane, session string, input, stdout, stderr *os.File, lock *os.File) error {
	b, err := json.Marshal(launchSpec{Binary: lane.Binary, Args: agentArgs(lane, session), Workdir: lane.Workdir})
	if err != nil {
		return err
	}
	if err = atomicPrivate(filepath.Join(dir, "launch.json"), b); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "__nuntius_fork", dir)
	cmd.Dir = lane.Workdir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = input, stdout, stderr
	cmd.ExtraFiles = []*os.File{lock}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err = cmd.Run(); err != nil {
		return errors.New("detached bootstrap failed")
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		r, err := loadRun(dir, filepath.Base(dir))
		if err != nil {
			return err
		}
		if r != nil && r.PID > 1 && r.ProcessStart != "" {
			return nil
		}
		time.Sleep(time.Millisecond)
	}
	return errors.New("detached bootstrap identity unavailable; goal remains recorded")
}

func launchEntry(args []string) (int, bool) {
	if len(args) == 0 || (args[0] != "__nuntius_fork" && args[0] != "__nuntius_exec" && args[0] != "__nuntius_launchd_exec") {
		return 0, false
	}
	if len(args) != 2 || !filepath.IsAbs(args[1]) || !runPattern.MatchString(filepath.Base(args[1])) {
		return 2, true
	}
	dir := args[1]
	if args[0] == "__nuntius_fork" {
		exe, err := os.Executable()
		if err != nil {
			return 1, true
		}
		cmd := exec.Command(exe, "__nuntius_exec", dir)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		cmd.ExtraFiles = []*os.File{os.NewFile(3, "lane-lock")}
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err = cmd.Start(); err != nil {
			return 1, true
		}
		_ = cmd.Process.Release()
		return 0, true
	}
	// Waiting is bounded bootstrap coordination, not runtime supervision.
	for deadline := time.Now().Add(5 * time.Second); args[0] != "__nuntius_launchd_exec" && os.Getppid() != 1; {
		if time.Now().After(deadline) {
			return 1, true
		}
		time.Sleep(time.Millisecond)
	}
	f, err := privateFile(filepath.Join(dir, "launch.json"), os.O_RDONLY)
	if err != nil {
		return 1, true
	}
	b, err := io.ReadAll(io.LimitReader(f, 65537))
	_ = f.Close()
	var spec launchSpec
	if err != nil || len(b) > 65536 || strictJSON(b, &spec) != nil || !filepath.IsAbs(spec.Binary) || !filepath.IsAbs(spec.Workdir) {
		return 1, true
	}
	r, err := loadRun(dir, filepath.Base(dir))
	if err != nil || r == nil {
		return 1, true
	}
	env := os.Environ()
	if spec.EnvironmentPipe {
		env, err = readLaunchEnvironment(dir)
		if err != nil {
			return 1, true
		}
	}
	if args[0] == "__nuntius_launchd_exec" && syscall.Getpgrp() != os.Getpid() {
		if _, err = syscall.Setsid(); err != nil {
			return 1, true
		}
	}
	r.PID = os.Getpid()
	r.ProcessStart, err = processStart(r.PID)
	if err != nil || saveAt(dir, r) != nil {
		return 1, true
	}
	if os.Chdir(spec.Workdir) != nil {
		return 1, true
	}
	// The agent owns only file-backed standard descriptors, never a manager lock.
	_ = syscall.Close(3)
	if syscall.Exec(spec.Binary, append([]string{spec.Binary}, spec.Args...), env) != nil {
		return 127, true
	}
	return 0, true
}
