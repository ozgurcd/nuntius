package nuntius

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func goalCommitFixture(t *testing.T) (*Manager, goalCommitRequest) {
	t.Helper()
	dir := goalReplayDir(t)
	if os.Mkdir(filepath.Join(dir, "locks"), 0700) != nil {
		t.Fatal("fixture locks failed")
	}
	return &Manager{store: dir}, goalCommitRequest{OperationID: "operation-1", Spec: nativeSchemaBytes(t, nativeSchemaFixture())}
}

func TestGoalCommitCAS(t *testing.T) {
	m, req := goalCommitFixture(t)
	result, p, err := m.commitGoal(req, goalCommitIO{})
	if err != nil || p == nil || result.Revision != 1 || result.Sequence != 1 || result.GoalID != "EXAMPLE-DOCS" || result.OperationID != req.OperationID {
		t.Fatal("first change was not durably committed")
	}
	before := goalCommitTree(t, m.store)
	again, _, err := (&Manager{store: m.store}).commitGoal(req, goalCommitIO{})
	if err != nil || again != result || !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
		t.Fatal("same operation did not return original durable result without writes")
	}
	req.Spec = append(req.Spec, ' ')
	if _, _, err = m.commitGoal(req, goalCommitIO{}); !errors.Is(err, errGoalOperation) {
		t.Fatal("different input reused operation ID")
	}
	req.OperationID = "operation-2"
	if _, _, err = m.commitGoal(req, goalCommitIO{}); !errors.Is(err, errGoalConflict) {
		t.Fatal("stale expected revision committed")
	}
	req.ExpectedRevision = 1
	second, next, err := m.commitGoal(req, goalCommitIO{})
	if err != nil || second.Revision != 2 || second.Sequence != 2 || next.sequence != 2 || p.sequence != 1 {
		t.Fatal("revision update or immutable old projection failed")
	}
	before = goalCommitTree(t, m.store)
	got, found, err := (&Manager{store: m.store}).lookupGoalOperation("operation-1")
	if err != nil || !found || got != result || !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
		t.Fatal("read-only restart lookup lost original result or wrote files")
	}
	t.Run("cooperating-processes", testGoalCommitProcesses)
}

func TestGoalCommitFaults(t *testing.T) {
	for _, stage := range []string{"blob_write", "blob_sync", "blob_publish", "blobs_sync", "before_append", "partial_write", "journal_sync", "journal_directory_sync", "publication"} {
		t.Run(stage, func(t *testing.T) {
			m, req := goalCommitFixture(t)
			hit := false
			fault := goalCommitIO{checkpoint: func(at string) error {
				if at == stage {
					hit = true
					return errors.New("injected storage failure")
				}
				return nil
			}}
			_, p, err := m.commitGoal(req, fault)
			if !hit || err == nil || p != nil {
				t.Fatal("fault did not prevent a successful projection publication")
			}
			complete := stage == "journal_sync" || stage == "journal_directory_sync" || stage == "publication"
			if complete && !errors.Is(err, errGoalUnconfirmed) {
				t.Fatal("complete append falsely claimed rollback")
			}
			before := goalCommitTree(t, m.store)
			fresh := &Manager{store: m.store}
			got, found, e := fresh.lookupGoalOperation(req.OperationID)
			if e != nil || found != complete || !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
				t.Fatal("restart lookup wrote history or invented/lost a committed operation")
			}
			if complete && (got.Sequence != 1 || got.Revision != 1) {
				t.Fatal("restart result changed")
			}
			result, replayed, e := fresh.commitGoal(req, goalCommitIO{})
			if e != nil || result.Sequence != 1 || replayed.sequence != 1 || replayed.tornTail {
				t.Fatal("retry did not resolve exactly one complete event")
			}
			data, _ := os.ReadFile(filepath.Join(m.store, goalJournalName))
			if bytes.Count(data, []byte("NGJF")) != 1 {
				t.Fatal("retry duplicated a journal event")
			}
		})
	}
	t.Run("preserve-prefix", func(t *testing.T) {
		m, req := goalCommitFixture(t)
		first, original, err := m.commitGoal(req, goalCommitIO{})
		if err != nil {
			t.Fatal("first commit failed")
		}
		prefix, _ := os.ReadFile(filepath.Join(m.store, goalJournalName))
		req.OperationID = "operation-2"
		req.ExpectedRevision = 1
		req.Seal = []byte("opaque fixture seal")
		_, _, err = m.commitGoal(req, goalCommitIO{checkpoint: func(at string) error {
			if at == "partial_write" {
				return errors.New("partial failure")
			}
			return nil
		}})
		if err == nil {
			t.Fatal("partial append claimed success")
		}
		before := goalCommitTree(t, m.store)
		p, err := replayGoalJournal(m.store)
		if err != nil || p.sequence != 1 || !p.tornTail || original.sequence != 1 || !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
			t.Fatal("partial write changed published prefix or read repaired history")
		}
		second, p, err := (&Manager{store: m.store}).commitGoal(req, goalCommitIO{})
		data, _ := os.ReadFile(filepath.Join(m.store, goalJournalName))
		if err != nil || second.Sequence != 2 || p.tornTail || !bytes.HasPrefix(data, prefix) || p.records["EXAMPLE-DOCS"].sealRef != goalBlobName("goal-seal-", req.Seal) {
			t.Fatal("locked retry lost prefix or immutable seal")
		}
		old, found, err := m.lookupGoalOperation(first.OperationID)
		if err != nil || !found || old != first {
			t.Fatal("retry lost first receipt")
		}
	})
	t.Run("repeated-sync-uncertainty", func(t *testing.T) {
		m, req := goalCommitFixture(t)
		result, _, err := m.commitGoal(req, goalCommitIO{})
		if err != nil {
			t.Fatal("first commit failed")
		}
		before := goalCommitTree(t, m.store)
		got, p, err := m.commitGoal(req, goalCommitIO{checkpoint: func(at string) error {
			if at == "journal_sync" {
				return errors.New("sync failure")
			}
			return nil
		}})
		if !errors.Is(err, errGoalUnconfirmed) || got != result || p != nil || !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
			t.Fatal("repeat sync failure duplicated or falsely rolled back receipt")
		}
	})
	t.Run("process-crash-after-append", testGoalCommitCrash)
}

func goalCommitTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		result[path] = fmt.Sprintf("%x:%o:%d", sha256.Sum256(b), info.Mode(), info.ModTime().UnixNano())
		return nil
	})
	if err != nil {
		t.Fatal("fixture tree read failed")
	}
	return result
}

// The test binary is an offline cooperating process, never an agent or server.
func TestGoalCommitProcessHelper(t *testing.T) {
	if len(os.Args) < 5 || os.Args[len(os.Args)-3] != "b03-fixture" {
		return
	}
	root, role := os.Args[len(os.Args)-2], os.Args[len(os.Args)-1]
	id := "process-" + role
	req := goalCommitRequest{OperationID: id, Spec: nativeSchemaBytes(t, nativeSchemaFixture())}
	fault := goalCommitIO{}
	if role == "holder" || role == "crash" {
		fault.checkpoint = func(stage string) error {
			if role == "holder" && stage == "before_append" || role == "crash" && stage == "journal_sync" {
				fmt.Println("ready")
				var signal [1]byte
				if _, err := io.ReadFull(os.Stdin, signal[:]); err != nil {
					return err
				}
			}
			return nil
		}
	}
	_, _, err := (&Manager{store: root}).commitGoal(req, fault)
	if err != nil {
		fmt.Println("refused")
	} else {
		fmt.Println("committed")
	}
}

func testGoalCommitCrash(t *testing.T) {
	m, req := goalCommitFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestGoalCommitProcessHelper$", "b03-fixture", m.store, "crash")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal("fixture output failed")
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal("fixture input failed")
	}
	start := time.Now().UTC()
	if cmd.Start() != nil {
		t.Fatal("fixture start failed")
	}
	waited := false
	t.Cleanup(func() {
		stdin.Close()
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		if !goalTestProcessEnded(cmd) {
			t.Error("offline crashed child was not reaped inside test")
		}
		t.Logf("offline crash fixture start=%s end=%s teardown verified", start.Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano))
	})
	if line, _ := bufio.NewReader(stdout).ReadString('\n'); line != "ready\n" {
		t.Fatal("child did not reach complete append crash barrier")
	}
	if cmd.Process.Kill() != nil {
		t.Fatal("fixture crash failed")
	}
	_ = cmd.Wait()
	waited = true
	stdin.Close()
	req.OperationID = "process-crash"
	fresh := &Manager{store: m.store}
	result, found, err := fresh.lookupGoalOperation(req.OperationID)
	if err != nil || !found || result.Sequence != 1 {
		t.Fatal("restart lost fully appended operation")
	}
	again, p, err := fresh.commitGoal(req, goalCommitIO{})
	if err != nil || again != result || p.sequence != 1 || len(p.operations) != 1 {
		t.Fatal("restart duplicated operation or invented rollback")
	}
}

func TestGoalCommitReceipts(t *testing.T) {
	m, req := goalCommitFixture(t)
	_, _, err := m.commitGoal(req, goalCommitIO{})
	if err != nil {
		t.Fatal("fixture commit failed")
	}
	data, _ := os.ReadFile(filepath.Join(m.store, goalJournalName))
	base := string(data[goalJournalHeaderBytes:])
	for _, payload := range []string{
		strings.Replace(base, `"operation_id":"operation-1"`, `"operation_id":null`, 1),
		strings.Replace(base, `"operation_id":"operation-1",`, "", 1),
		strings.Replace(base, `"operation_id":"operation-1"`, `"operation_id":"../bad"`, 1),
		strings.TrimSuffix(base, "}") + `,"operation_id":"operation-1"}`,
		strings.Replace(base, `"input_sha256":"`, `"input_sha256":"x`, 1),
	} {
		goalReplayWrite(t, m.store, goalJournalName, goalReplayFrame(1, []byte(payload)))
		before := goalCommitTree(t, m.store)
		if _, _, err := m.lookupGoalOperation(req.OperationID); !errors.Is(err, errGoalReplay) || !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
			t.Fatal("invalid receipt was accepted or repaired by lookup")
		}
	}
	var event goalJournalEvent
	if json.Unmarshal([]byte(base), &event) != nil {
		t.Fatal("fixture event failed")
	}
	event.Revision = 2
	event.InputSHA256 = goalInputFingerprint(event.GoalID, 1, event.SpecRef, event.SealRef)
	second, _ := json.Marshal(event)
	goalReplayWrite(t, m.store, goalJournalName, append(data, goalReplayFrame(2, second)...))
	if _, _, err := m.lookupGoalOperation(req.OperationID); !errors.Is(err, errGoalReplay) {
		t.Fatal("duplicate operation in complete history accepted")
	}
}

func testGoalCommitProcesses(t *testing.T) {
	m, _ := goalCommitFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now().UTC()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestGoalCommitProcessHelper$", "b03-fixture", m.store, "holder")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal("fixture output failed")
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal("fixture input failed")
	}
	if cmd.Start() != nil {
		t.Fatal("fixture start failed")
	}
	waited := false
	t.Cleanup(func() {
		stdin.Close()
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		if !goalTestProcessEnded(cmd) {
			t.Error("offline child did not end inside test")
		}
		t.Logf("offline child start=%s end=%s teardown verified", start.Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano))
	})
	r := bufio.NewReader(stdout)
	if line, _ := r.ReadString('\n'); line != "ready\n" {
		t.Fatal("holder did not reach locked append barrier")
	}
	before := goalCommitTree(t, m.store)
	rival := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestGoalCommitProcessHelper$", "b03-fixture", m.store, "rival")
	rivalStart := time.Now().UTC()
	out, err := rival.Output()
	if err != nil || !bytes.HasPrefix(out, []byte("refused\n")) || rival.ProcessState == nil || !rival.ProcessState.Exited() {
		t.Fatal("second offline process won a held revision or failed teardown")
	}
	t.Logf("offline rival start=%s end=%s teardown verified", rivalStart.Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano))
	if !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
		t.Fatal("refused process created artifacts")
	}
	if _, err := stdin.Write([]byte{'x'}); err != nil {
		t.Fatal("fixture release failed")
	}
	stdin.Close()
	if line, _ := r.ReadString('\n'); line != "committed\n" {
		t.Fatal("holder failed durable commit")
	}
	err = cmd.Wait()
	waited = true
	if err != nil {
		t.Fatal("fixture wait failed")
	}
	// A restarted independent process still cannot win that revision.
	rival = exec.CommandContext(ctx, os.Args[0], "-test.run=^TestGoalCommitProcessHelper$", "b03-fixture", m.store, "rival")
	rivalStart = time.Now().UTC()
	out, err = rival.Output()
	if err != nil || !bytes.HasPrefix(out, []byte("refused\n")) || rival.ProcessState == nil || !rival.ProcessState.Exited() {
		t.Fatal("stale independent process committed or did not end")
	}
	t.Logf("offline restarted rival start=%s end=%s teardown verified", rivalStart.Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano))
	p, err := replayGoalJournal(m.store)
	if err != nil || p.sequence != 1 || len(p.operations) != 1 {
		t.Fatal("process serialization duplicated or lost the event")
	}
}

func goalTestProcessEnded(cmd *exec.Cmd) bool {
	if cmd.ProcessState == nil {
		return false
	}
	status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	return ok && (status.Exited() || status.Signaled())
}

func TestGoalCommitSafety(t *testing.T) {
	t.Run("read-without-lock-creation", func(t *testing.T) {
		m := &Manager{store: goalReplayDir(t)}
		before := goalCommitTree(t, m.store)
		_, found, err := m.lookupGoalOperation("missing")
		if err != nil || found || !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
			t.Fatal("empty lookup mutated the store")
		}
	})
	t.Run("shared-control-lock", func(t *testing.T) {
		m, req := goalCommitFixture(t)
		root, err := openLaneControlRoot(m.store)
		if err != nil {
			t.Fatal("fixture root failed")
		}
		defer root.Close()
		lock, err := laneControlLock(root, unix.LOCK_EX)
		if err != nil {
			t.Fatal("fixture lock failed")
		}
		defer lock.Close()
		before := goalCommitTree(t, m.store)
		_, p, err := m.commitGoal(req, goalCommitIO{})
		if err == nil || p != nil || !reflect.DeepEqual(before, goalCommitTree(t, m.store)) {
			t.Fatal("native commit bypassed shared lane control serialization")
		}
	})
	for _, kind := range []string{"journal-link", "journal-hardlink", "journal-mode", "blob-link", "blob-content", "locks-link", "read-only", "closed", "bad-operation", "bad-spec", "seal-overflow"} {
		t.Run(kind, func(t *testing.T) {
			m, req := goalCommitFixture(t)
			outside := goalReplayDir(t)
			target := filepath.Join(outside, "untouched")
			if os.WriteFile(target, []byte("fixture-original"), 0600) != nil {
				t.Fatal("fixture target failed")
			}
			journal := filepath.Join(m.store, goalJournalName)
			blob := filepath.Join(m.store, goalBlobName("goal-spec-", req.Spec))
			var setup error
			switch kind {
			case "journal-link":
				setup = os.Symlink(target, journal)
			case "journal-hardlink":
				setup = os.Link(target, journal)
			case "journal-mode":
				setup = os.WriteFile(journal, nil, 0644)
			case "blob-link":
				setup = os.Symlink(target, blob)
			case "blob-content":
				setup = os.WriteFile(blob, []byte("wrong immutable content"), 0600)
			case "locks-link":
				setup = os.Remove(filepath.Join(m.store, "locks"))
				if setup == nil {
					setup = os.Symlink(outside, filepath.Join(m.store, "locks"))
				}
			case "read-only":
				m.readOnly = true
			case "closed":
				m.closed = true
			case "bad-operation":
				req.OperationID = "../bad"
			case "bad-spec":
				req.Spec = []byte("{}")
			case "seal-overflow":
				req.Seal = make([]byte, goalSealMaxBytes+1)
			}
			if setup != nil {
				t.Fatal("unsafe fixture setup failed")
			}
			_, p, err := m.commitGoal(req, goalCommitIO{})
			b, e := os.ReadFile(target)
			if err == nil || p != nil || e != nil || string(b) != "fixture-original" {
				t.Fatal("unsafe commit succeeded or changed linked data")
			}
		})
	}
}
