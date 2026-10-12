package nuntius

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ozgurcd/nuntius/internal/fixturelife"
)

func TestFixtureLifetimeHelper(t *testing.T) {
	if len(os.Args) < 2 || os.Args[len(os.Args)-1] != "fixture-lifetime-helper" {
		return
	}
	cmd := exec.Command(fakeBinary)
	cmd.Stdin = strings.NewReader("ignore-int wait")
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal("start offline lifetime child")
	}
	t.Cleanup(func() { _ = cmd.Wait() })
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		children, err := fixturelife.Children(os.Getenv(fixturelife.Environment), "")
		if err != nil {
			t.Fatal("read offline lifetime children")
		}
		for _, child := range children {
			if child.PID == cmd.Process.Pid {
				b, _ := json.Marshal(struct {
					Child fixturelife.Child
					Lease string
				}{child, os.Getenv(fixturelife.Environment)})
				fmt.Println("child=" + string(b))
				select {} // parent interrupts, kills or lets testing's timeout panic.
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("offline child registration deadline")
}

func TestFixtureLifetimeInterrupted(t *testing.T) {
	for _, mode := range []string{"interrupt", "terminate", "kill", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			timeout := "30s"
			if mode == "timeout" {
				timeout = "5s"
			}
			cmd := exec.Command(exe, "-test.run=^TestFixtureLifetimeHelper$", "-test.timeout="+timeout, "--", "fixture-lifetime-helper")
			cmd.Env = append([]string{}, os.Environ()...)
			for i, value := range cmd.Env {
				if strings.HasPrefix(value, fixturelife.Environment+"=") {
					cmd.Env[i] = fixturelife.Environment + "="
				}
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			cmd.Stderr = io.Discard
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			wait := make(chan error, 1)
			go func() { wait <- cmd.Wait() }()
			var child fixturelife.Child
			var leaseDir string
			waited := false
			t.Cleanup(func() {
				if child.PID > 1 && leaseDir != "" {
					if err := fixturelife.Stop(leaseDir, ""); err != nil {
						t.Error("interrupted fixture cleanup failed")
					}
				}
				_ = cmd.Process.Kill()
				if !waited {
					<-wait
				}
			})
			ready := make(chan bool, 1)
			go func() {
				scanner := bufio.NewScanner(stdout)
				for scanner.Scan() {
					if strings.HasPrefix(scanner.Text(), "child=") {
						var report struct {
							Child fixturelife.Child
							Lease string
						}
						err := json.Unmarshal([]byte(strings.TrimPrefix(scanner.Text(), "child=")), &report)
						child, leaseDir = report.Child, report.Lease
						ready <- err == nil
						return
					}
				}
				ready <- false
			}()
			select {
			case ok := <-ready:
				if !ok || child.PID <= 1 || child.Start == "" {
					t.Fatal("offline child ready identity missing")
				}
			case <-time.After(15 * time.Second):
				t.Fatal("offline helper ready deadline")
			}
			var sig os.Signal
			switch mode {
			case "interrupt":
				sig = os.Interrupt
			case "terminate":
				sig = syscall.SIGTERM
			case "kill":
				sig = syscall.SIGKILL
			}
			if sig != nil && cmd.Process.Signal(sig) != nil {
				t.Fatal("offline helper signal failed")
			}
			select {
			case err := <-wait:
				waited = true
				if err == nil {
					t.Fatal("interrupted helper succeeded")
				}
			case <-time.After(15 * time.Second):
				t.Fatal("interrupted helper wait deadline")
			}
			for deadline := time.Now().Add(5 * time.Second); ; {
				birth, err := processStart(child.PID)
				if err != nil || birth != child.Start {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("offline child survived interrupted owner")
				}
				time.Sleep(10 * time.Millisecond)
			}
			t.Log("interrupted fixture child identity absent; helper reaped")
		})
	}
}

func TestFixtureLifetimeCleanup(t *testing.T) {
	dir := t.TempDir()
	owner, err := fixturelife.New(filepath.Join(dir, "children"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if owner.Close() != nil {
			t.Error("offline owner cleanup")
		}
	})
	var child fixturelife.Child
	t.Run("child", func(t *testing.T) {
		cmd := exec.Command(fakeBinary)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), fixturelife.Environment+"="+owner.Dir)
		cmd.Stdin = strings.NewReader("ignore-int wait")
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if cmd.Start() != nil {
			t.Fatal("offline cleanup start")
		}
		t.Cleanup(func() {
			if fixturelife.Stop(owner.Dir, dir) != nil {
				t.Error("offline group cleanup")
			}
			_ = cmd.Wait()
		})
		for deadline := time.Now().Add(5 * time.Second); ; {
			children, err := fixturelife.Children(owner.Dir, dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(children) == 1 {
				child = children[0]
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("offline cleanup registration")
			}
			time.Sleep(10 * time.Millisecond)
		}
		if pgid, err := syscall.Getpgid(child.PID); err != nil || pgid != child.PID {
			t.Fatal("offline child has no owned process group")
		}
	})
	if birth, err := processStart(child.PID); err == nil && birth == child.Start {
		t.Fatal("offline child survived t.Cleanup")
	}
	late := exec.Command(fakeBinary)
	late.Dir = dir
	late.Env = append(os.Environ(), fixturelife.Environment+"="+owner.Dir)
	late.Stdin = strings.NewReader("normal")
	late.Stdout, late.Stderr = io.Discard, io.Discard
	if err := late.Run(); err == nil {
		t.Fatal("offline child started after fixture cleanup")
	}
}
