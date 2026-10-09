package nuntius

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Models the released direct-parent topology without consulting any owner's store.
func TestLegacyParentHelper(t *testing.T) {
	if os.Getenv("NUNTIUS_LEGACY_HELPER") != "1" {
		return
	}
	dir := os.Getenv("NUNTIUS_FIXTURE_STORE")
	f, e := os.Create(filepath.Join(dir, "events.jsonl"))
	if e != nil {
		os.Exit(2)
	}
	cmd := exec.Command(fakeBinary, "-p")
	// TestMain builds a new fake in this subprocess; it stays available until exit.
	cmd.Stdin = strings.NewReader(os.Getenv("NUNTIUS_FIXTURE_MODE"))
	cmd.Stdout = f
	cmd.Stderr = f
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if cmd.Start() != nil {
		os.Exit(3)
	}
	start, _ := processStart(cmd.Process.Pid)
	b, _ := json.Marshal(Run{PID: cmd.Process.Pid, ProcessStart: start})
	fmt.Println(string(b))
	_ = cmd.Wait()
	select {}
}

func TestLegacyParentLossHypotheses(t *testing.T) {
	for _, mode := range []string{"slow-finish", "orphan-detect"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			executable, _ := os.Executable()
			child := exec.Command(executable, "-test.run=^TestLegacyParentHelper$")
			child.Env = append(os.Environ(), "NUNTIUS_LEGACY_HELPER=1", "NUNTIUS_FIXTURE_STORE="+dir, "NUNTIUS_FIXTURE_MODE="+mode)
			pipe, _ := child.StdoutPipe()
			if e := child.Start(); e != nil {
				t.Fatal(e)
			}
			t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
			line, e := bufio.NewReader(pipe).ReadString('\n')
			if e != nil {
				t.Fatal(e)
			}
			var r Run
			if json.Unmarshal([]byte(line), &r) != nil {
				t.Fatal("bad fixture identity")
			}
			t.Cleanup(func() { _ = signalRun(r, syscall.SIGKILL) })
			ready := false
			for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
				b, _ := os.ReadFile(filepath.Join(dir, "events.jsonl"))
				if strings.Contains(string(b), "ready") {
					ready = true
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !ready {
				t.Fatal("fake not ready")
			}
			if e := child.Process.Kill(); e != nil {
				t.Fatal(e)
			}
			_ = child.Wait()
			for deadline := time.Now().Add(3 * time.Second); sameProcess(r) && time.Now().Before(deadline); {
				time.Sleep(10 * time.Millisecond)
			}
			b, _ := os.ReadFile(filepath.Join(dir, "events.jsonl"))
			finished := strings.Contains(string(b), "finished after client exit")
			if mode == "slow-finish" && !finished {
				t.Fatal("detached fake failed after parent loss")
			}
			if mode == "orphan-detect" && sameProcess(r) {
				t.Fatal("orphan fake survived parent loss")
			}
			t.Logf("%s: parent killed; normal completion=%v; no stop requested; file-backed stdout", mode, finished)
		})
	}
}
