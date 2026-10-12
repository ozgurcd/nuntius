// Package fixturelife owns only compiled offline test children. It is never
// used by the installed server or by real agents.
package fixturelife

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

const Environment = "NUNTIUS_TEST_LEASE"

type Child struct {
	PID     int
	Start   string
	Workdir string
}

type Owner struct {
	Dir   string
	lease *os.File
}

func New(dir string) (*Owner, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "lease"), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	return &Owner{Dir: dir, lease: f}, nil
}

// Guard runs before a fake accepts input. Detached bootstraps already provide
// a group; direct fixture commands establish their own group here too.
// The lease is held by the outer test binary, not an intentionally crashed
// manager helper. Kernel lock release covers panic, timeout and SIGKILL, where
// neither t.Cleanup nor a signal handler can run.
func Guard() error {
	if os.Getenv(Environment) == "" {
		return nil
	}
	if syscall.Getpgrp() != os.Getpid() {
		if err := syscall.Setpgid(0, 0); err != nil {
			return err
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	_, err = guard(os.Getpid(), cwd, true)
	return err
}

// GuardChild owns a separate-session offline child whose executable has no
// Guard of its own. Its caller remains alive to wait and reap that child.
func GuardChild(pid int, cwd string) (func(), error) { return guard(pid, cwd, false) }

func guard(pid int, cwd string, self bool) (stop func(), err error) {
	dir := os.Getenv(Environment)
	if dir == "" {
		return func() {}, nil
	}
	start, err := processStart(pid)
	if err != nil {
		return nil, err
	}
	if group, e := syscall.Getpgid(pid); e != nil || group != pid {
		return nil, errors.New("offline child is not a group leader")
	}
	stop = func() {
		birth, e := processStart(pid)
		if group, groupErr := syscall.Getpgid(pid); e == nil && birth == start && groupErr == nil && group == pid {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
		if self {
			os.Exit(1)
		}
	}
	defer func() {
		if err != nil {
			stop()
		}
	}()
	b, err := json.Marshal(Child{PID: pid, Start: start, Workdir: cwd})
	if err != nil {
		return stop, err
	}
	name := strconv.Itoa(pid)
	children := bucket(dir, cwd)
	if err = os.MkdirAll(children, 0700); err != nil {
		return stop, err
	}
	if _, err := os.Stat(filepath.Join(children, "closed")); !os.IsNotExist(err) {
		return stop, errors.New("offline fixture already closed")
	}
	staged := filepath.Join(children, ".register-"+name)
	if err = os.WriteFile(staged, b, 0600); err != nil {
		return stop, err
	}
	if err = os.Rename(staged, filepath.Join(children, name)); err != nil {
		return stop, err
	}
	lease, err := os.OpenFile(filepath.Join(dir, "lease"), os.O_RDWR, 0)
	if err != nil {
		return stop, err
	}
	go func() {
		defer lease.Close()
		for {
			if !self {
				birth, e := processStart(pid)
				if e != nil || birth != start {
					return
				}
			}
			err := syscall.Flock(int(lease.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
			stopMarker, _ := os.ReadFile(filepath.Join(children, "stop-"+name))
			_, closed := os.Stat(filepath.Join(children, "closed"))
			if err == nil || !errors.Is(err, syscall.EWOULDBLOCK) || string(stopMarker) == start || !os.IsNotExist(closed) {
				stop()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	return stop, nil
}

func Children(dir, workdir string) ([]Child, error) {
	if workdir == "" {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		var all []Child
		for _, entry := range entries {
			if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				continue
			}
			children, err := readChildren(filepath.Join(dir, entry.Name()), "")
			if err != nil {
				return nil, err
			}
			all = append(all, children...)
		}
		return all, nil
	}
	return readChildren(bucket(dir, workdir), workdir)
}

func bucket(dir, workdir string) string {
	return filepath.Join(dir, fmt.Sprintf("%x", sha256.Sum256([]byte(workdir))))
}

func readChildren(dir, workdir string) ([]Child, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var children []Child
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 1 || entry.Type() != 0 {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		var child Child
		if json.Unmarshal(b, &child) != nil || child.PID != pid || child.Start == "" {
			return nil, errors.New("invalid offline child identity")
		}
		if workdir != "" && child.Workdir != workdir {
			continue
		}
		if start, err := processStart(pid); err == nil && start == child.Start {
			children = append(children, child)
		}
	}
	return children, nil
}

// Stop requests self group termination as well as signalling the verified
// group. The request works even when a sandbox refuses signalling a detached
// descendant. Records remain until identity absence is observed.
func Stop(dir, workdir string) error {
	// Publish closure before the first census. A child still bootstrapping can
	// register after that census; it must observe the same closed fixture.
	if workdir != "" {
		children := bucket(dir, workdir)
		if err := os.MkdirAll(children, 0700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(children, "closed"), nil, 0600); err != nil {
			return err
		}
	}
	for deadline := time.Now().Add(5 * time.Second); ; {
		children, err := Children(dir, workdir)
		if err != nil {
			return err
		}
		if len(children) == 0 {
			return nil
		}
		for _, child := range children {
			if err := os.WriteFile(filepath.Join(bucket(dir, child.Workdir), "stop-"+strconv.Itoa(child.PID)), []byte(child.Start), 0600); err != nil {
				return err
			}
			if group, err := syscall.Getpgid(child.PID); err == nil && group == child.PID {
				_ = syscall.Kill(-group, syscall.SIGKILL)
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("offline child group teardown deadline")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (o *Owner) Close() error {
	_ = o.lease.Close()
	return Stop(o.Dir, "")
}
