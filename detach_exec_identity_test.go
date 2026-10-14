package nuntius

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Native reconcile holds a launch for review when sameProcess is false while
// the PID still exists. The helper publishes identity just before it replaces
// itself, so that check can run while execve is completing. On Linux, an
// execve from a thread other than the thread-group leader shows the leader as
// a zombie until the kernel swaps thread IDs. A live child must keep its
// identity through that window.
func TestDetachedIdentityContinuousThroughExec(t *testing.T) {
	c, store := fixture(t)
	lane := c.Lanes[1]
	if err := os.MkdirAll(filepath.Join(store, "runs"), 0700); err != nil {
		t.Fatal(err)
	}
	lock, err := privateFile(filepath.Join(t.TempDir(), "lane.lock"), os.O_RDWR|os.O_CREATE)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	const launches = 30
	misread := 0
	for i := 0; i < launches; i++ {
		random := make([]byte, 16)
		if _, err := rand.Read(random); err != nil {
			t.Fatal(err)
		}
		id := hex.EncodeToString(random)
		dir := filepath.Join(store, "runs", id)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		r := Run{ID: id, Lane: lane.Name, Kind: lane.Kind, GoalID: "EXEC-IDENTITY", Started: time.Now().UTC(), State: "running"}
		if err := saveAt(dir, &r); err != nil {
			t.Fatal(err)
		}
		if err := putPrivate(filepath.Join(dir, "goal.txt"), []byte("wait")); err != nil {
			t.Fatal(err)
		}
		input, err := privateFile(filepath.Join(dir, "goal.txt"), os.O_RDONLY)
		if err != nil {
			t.Fatal(err)
		}
		stdout, err := privateFile(filepath.Join(dir, "events.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_EXCL)
		if err != nil {
			t.Fatal(err)
		}
		stderr, err := privateFile(filepath.Join(dir, "stderr.log"), os.O_WRONLY|os.O_CREATE|os.O_EXCL)
		if err != nil {
			t.Fatal(err)
		}
		launched := make(chan error, 1)
		go func() { launched <- detachAgent(dir, lane, "", input, stdout, stderr, lock) }()
		// Observe from the moment the helper publishes identity, as a racing
		// reconcile would, rather than after the launcher's own polling interval.
		var child *Run
		for deadline := time.Now().Add(5 * time.Second); child == nil && time.Now().Before(deadline); {
			if r, err := loadRun(dir, id); err == nil && r != nil && r.PID > 1 && r.ProcessStart != "" {
				child = r
			}
		}
		ready, ended := false, false
		for polls, deadline := 0, time.Now().Add(4*time.Second); child != nil && !ready && !ended && time.Now().Before(deadline); polls++ {
			ended = !sameProcess(*child) && syscall.Kill(child.PID, 0) == nil
			if polls%64 == 0 {
				b, _ := os.ReadFile(filepath.Join(dir, "events.jsonl"))
				ready = strings.Contains(string(b), "ready")
			}
		}
		err = <-launched
		input.Close()
		stdout.Close()
		stderr.Close()
		if err != nil || child == nil {
			t.Fatal("detached identity unavailable", err)
		}
		if ended {
			// The fixture's lifetime cleanup stops this child after its exec.
			misread++
			continue
		}
		if !ready {
			t.Fatal("live detached child did not become ready")
		}
		if err := signalRun(*child, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
	}
	if misread > 0 {
		t.Fatalf("live detached child lost its identity during exec in %d of %d launches", misread, launches)
	}
}
