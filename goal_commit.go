package nuntius

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

type goalCommitRequest struct {
	OperationID      string
	ExpectedRevision uint64
	Spec             []byte
	Seal             []byte
	Record           []byte
	MutationSHA256   string
}

type goalCommitResult struct {
	OperationID string
	GoalID      string
	Revision    uint64
	Sequence    uint64
}

type goalCommitIO struct{ checkpoint func(string) error }

// Keep public refusal text fixed. Tests can inspect only a fixed operation
// label and numeric errno, never an OS error's path or private contents.
type goalPersistenceFailure struct {
	refusal error
	stage   string
	errno   int
}

func (e *goalPersistenceFailure) Error() string                        { return e.refusal.Error() }
func (e *goalPersistenceFailure) Unwrap() error                        { return e.refusal }
func (e *goalPersistenceFailure) PersistenceDiagnostic() (string, int) { return e.stage, e.errno }

func goalPersistenceError(refusal error, stage string, cause error) error {
	if cause == nil {
		return nil
	}
	var prior *goalPersistenceFailure
	if errors.As(cause, &prior) {
		return cause
	}
	var errno unix.Errno
	_ = errors.As(cause, &errno)
	var control *laneControlFault
	if errors.As(cause, &control) && control.Code == "locked" {
		errno = unix.EWOULDBLOCK
	}
	return &goalPersistenceFailure{refusal: refusal, stage: stage, errno: int(errno)}
}

type goalOperationReceipt struct {
	input    string
	result   goalCommitResult
	mutation string
}

func (fault goalCommitIO) check(stage string) error {
	if fault.checkpoint != nil {
		return fault.checkpoint(stage)
	}
	return nil
}

func validGoalOperationID(id string) bool {
	if len(id) < 1 || len(id) > 64 {
		return false
	}
	for i, c := range []byte(id) {
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			continue
		}
		if i == 0 || (c != '.' && c != '-') {
			return false
		}
	}
	return true
}

func goalBlobName(prefix string, data []byte) string {
	sum := sha256.Sum256(data)
	return prefix + hex.EncodeToString(sum[:])
}

// Exact-byte references bind all input, including the expected revision.
func goalInputFingerprint(id string, revision uint64, spec, seal string) string {
	b, _ := json.Marshal(struct {
		ID       string
		Revision uint64
		Spec     string
		Seal     string
	}{id, revision, spec, seal})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

var errGoalCommit = errors.New("native goal change could not be saved; preserve history and review before retrying")
var errGoalConflict = errors.New("native goal changed; read its current revision before saving")
var errGoalOperation = errors.New("this operation ID already belongs to different input; look up the original change")
var errGoalUnconfirmed = errors.New("commit_unconfirmed: the native goal change may be saved; look up the operation ID before retrying")

func (m *Manager) commitGoal(req goalCommitRequest, fault goalCommitIO) (goalCommitResult, *goalProjection, error) {
	// Own bytes before validation; no caller-owned slice enters the projection.
	req.Spec = bytes.Clone(req.Spec)
	req.Seal = bytes.Clone(req.Seal)
	req.Record = bytes.Clone(req.Record)
	spec, err := decodeNativeGoalSpec(req.Spec)
	if err != nil || !validGoalOperationID(req.OperationID) || req.ExpectedRevision >= goalJournalMaxEvents || len(req.Seal) > goalSealMaxBytes {
		return goalCommitResult{}, nil, errGoalCommit
	}
	specRef := goalBlobName("goal-spec-", req.Spec)
	sealRef := ""
	if req.Seal != nil {
		sealRef = goalBlobName("goal-seal-", req.Seal)
	}
	input := goalInputFingerprint(spec.ID, req.ExpectedRevision, specRef, sealRef)
	if req.Record != nil {
		if _, err := decodeGoalRecord(req.Record); err != nil || !validGoalDigest(req.MutationSHA256) {
			return goalCommitResult{}, nil, errGoalRecord
		}
		input = goalRecordFingerprint(input, goalBlobName("goal-record-", req.Record), req.MutationSHA256)
	} else if req.MutationSHA256 != "" {
		return goalCommitResult{}, nil, errGoalRecord
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.readOnly || m.closed {
		return goalCommitResult{}, nil, errGoalCommit
	}
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		return goalCommitResult{}, nil, errGoalCommit
	}
	defer root.Close()
	lock, err := laneControlLock(root, unix.LOCK_EX)
	if err != nil {
		return goalCommitResult{}, nil, errGoalCommit
	}
	defer lock.Close()
	return commitGoalLocked(root, req, spec.ID, specRef, sealRef, input, fault)
}

func (m *Manager) lookupGoalOperation(id string) (goalCommitResult, bool, error) {
	if !validGoalOperationID(id) {
		return goalCommitResult{}, false, errGoalCommit
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	p, err := replayGoalJournal(m.store)
	if err != nil {
		return goalCommitResult{}, false, err
	}
	receipt, exists := p.operations[id]
	return receipt.result, exists, nil
}

// Caller owns manager -> control. No nested control/audit lock is acquired.
func commitGoalLocked(root *os.File, req goalCommitRequest, id, specRef, sealRef, input string, fault goalCommitIO) (goalCommitResult, *goalProjection, error) {
	p, err := replayGoalJournalRoot(root)
	if err != nil {
		return goalCommitResult{}, nil, err
	}
	if receipt, exists := p.operations[req.OperationID]; exists {
		if receipt.input != input {
			return goalCommitResult{}, nil, errGoalOperation
		}
		return confirmGoalOperation(root, p, receipt, fault)
	}
	if p.records[id].revision != req.ExpectedRevision {
		return goalCommitResult{}, nil, errGoalConflict
	}
	if p.sequence >= goalJournalMaxEvents || (p.records[id].revision == 0 && len(p.records) >= goalJournalMaxGoals) {
		return goalCommitResult{}, nil, errGoalCommit
	}
	if !goalRecordHistoryValid(p.records[id], goalReplayRecord{revision: req.ExpectedRevision + 1, spec: string(req.Spec), sealRef: sealRef, record: string(req.Record)}, req.Seal) {
		return goalCommitResult{}, nil, errGoalRecord
	}
	changes, err := goalDependencyChanges(p, id, goalReplayRecord{revision: req.ExpectedRevision + 1, spec: string(req.Spec), sealRef: sealRef, record: string(req.Record)})
	if err != nil {
		return goalCommitResult{}, nil, err
	}
	if err := stageGoalBlob(root, specRef, req.Spec, fault); err != nil {
		return goalCommitResult{}, nil, err
	}
	if sealRef != "" {
		if err := stageGoalBlob(root, sealRef, req.Seal, fault); err != nil {
			return goalCommitResult{}, nil, err
		}
	}
	if fault.check("blobs_sync") != nil {
		return goalCommitResult{}, nil, errGoalCommit
	}
	if err := goalPersistenceError(errGoalCommit, "blobs.directory_sync", root.Sync()); err != nil {
		return goalCommitResult{}, nil, err
	}
	e := goalJournalEvent{Kind: "goal.spec", GoalID: id, Revision: req.ExpectedRevision + 1, SpecRef: specRef, SealRef: sealRef, OperationID: req.OperationID, InputSHA256: input}
	if req.Record != nil {
		e.RecordRef = goalBlobName("goal-record-", req.Record)
		e.MutationSHA256 = req.MutationSHA256
		if err := stageGoalBlob(root, e.RecordRef, req.Record, fault); err != nil {
			return goalCommitResult{}, nil, err
		}
		if err := goalPersistenceError(errGoalCommit, "record.directory_sync", root.Sync()); err != nil {
			return goalCommitResult{}, nil, err
		}
	}
	if len(changes) > 0 {
		b, err := encodeGoalChanges(root, changes, fault)
		if err != nil {
			return goalCommitResult{}, nil, errGoalCommit
		}
		e.ChangesRef = goalBlobName("goal-changes-", b)
		if stageGoalBlob(root, e.ChangesRef, b, fault) != nil || root.Sync() != nil {
			return goalCommitResult{}, nil, errGoalCommit
		}
	}
	payload, err := json.Marshal(e)
	if err != nil || len(payload) > goalJournalMaxFrameBytes {
		return goalCommitResult{}, nil, errGoalCommit
	}
	frame := encodeGoalFrame(p.sequence+1, payload)
	data, err := readGoalPrivate(root, goalJournalName, goalJournalMaxBytes)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return goalCommitResult{}, nil, errGoalReplay
	}
	// Validate the entire candidate and cumulative historical blob budget first.
	next, err := replayGoalJournalData(root, append(data[:p.committedBytes:p.committedBytes], frame...))
	if err != nil {
		return goalCommitResult{}, nil, err
	}
	if fault.check("before_append") != nil {
		return goalCommitResult{}, nil, errGoalCommit
	}
	journal, err := openGoalJournal(root, true)
	if err != nil {
		return goalCommitResult{}, nil, errGoalCommit
	}
	defer journal.Close()
	if p.tornTail {
		// Only the writer repairs a validated incomplete final frame, under lock.
		if journal.Truncate(p.committedBytes) != nil || journal.Sync() != nil {
			return goalCommitResult{}, nil, errGoalCommit
		}
	}
	if _, err := journal.Seek(p.committedBytes, 0); err != nil {
		return goalCommitResult{}, nil, errGoalCommit
	}
	if fault.check("partial_write") != nil {
		_, _ = journal.Write(frame[:len(frame)/2])
		return goalCommitResult{}, nil, errGoalCommit
	}
	n, err := journal.Write(frame)
	if n != len(frame) {
		return goalCommitResult{}, nil, errGoalCommit
	}
	result := next.operations[req.OperationID].result
	if err != nil || syncGoalJournal(root, journal, fault) != nil || fault.check("publication") != nil {
		return result, nil, errGoalUnconfirmed
	}
	return result, next, nil
}

// Both storage and typed-record retries confirm durability through this seam.
func confirmGoalOperation(root *os.File, p *goalProjection, receipt goalOperationReceipt, fault goalCommitIO) (goalCommitResult, *goalProjection, error) {
	journal, err := openGoalJournal(root, false)
	if err != nil {
		return receipt.result, nil, errGoalUnconfirmed
	}
	defer journal.Close()
	if syncGoalJournal(root, journal, fault) != nil || fault.check("publication") != nil {
		return receipt.result, nil, errGoalUnconfirmed
	}
	return receipt.result, p, nil
}

func encodeGoalFrame(sequence uint64, payload []byte) []byte {
	h := make([]byte, goalJournalHeaderBytes)
	copy(h, "NGJF")
	binary.BigEndian.PutUint32(h[4:8], 1)
	binary.BigEndian.PutUint64(h[8:16], sequence)
	binary.BigEndian.PutUint32(h[16:20], uint32(len(payload)))
	header := sha256.Sum256(h[:20])
	copy(h[20:52], header[:])
	sum := sha256.Sum256(payload)
	copy(h[52:84], sum[:])
	return append(h, payload...)
}

func openGoalJournal(root *os.File, create bool) (*os.File, error) {
	flags := unix.O_RDWR | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC
	if create {
		flags |= unix.O_CREAT
	}
	fd, err := unix.Openat(int(root.Fd()), goalJournalName, flags, 0600)
	if err != nil {
		return nil, errGoalCommit
	}
	f := os.NewFile(uintptr(fd), "native-goal-journal")
	if checkLaneControlFile(fd) != nil {
		f.Close()
		return nil, errGoalCommit
	}
	return f, nil
}

func syncGoalJournal(root, journal *os.File, fault goalCommitIO) error {
	if err := fault.check("journal_sync"); err != nil {
		return err
	}
	if err := journal.Sync(); err != nil {
		return err
	}
	if err := fault.check("journal_directory_sync"); err != nil {
		return err
	}
	return root.Sync()
}

// Publish by link, never replace an existing immutable content address.
func stageGoalBlob(root *os.File, ref string, data []byte, fault goalCommitIO) error {
	old, err := readGoalPrivate(root, ref, int64(len(data)))
	if err == nil {
		if !bytes.Equal(old, data) {
			return errGoalCommit
		}
		fd, err := unix.Openat(int(root.Fd()), ref, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err != nil {
			return goalPersistenceError(errGoalCommit, "blob.reopen", err)
		}
		f := os.NewFile(uintptr(fd), "native-goal-blob")
		defer f.Close()
		if checkLaneControlFile(fd) != nil {
			return errGoalCommit
		}
		return goalPersistenceError(errGoalCommit, "blob.existing_sync", f.Sync())
	}
	if !errors.Is(err, os.ErrNotExist) {
		return errGoalCommit
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return errGoalCommit
	}
	name := ".goal-stage-" + hex.EncodeToString(nonce[:])
	fd, err := unix.Openat(int(root.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return goalPersistenceError(errGoalCommit, "blob.open", err)
	}
	f := os.NewFile(uintptr(fd), "native-goal-stage")
	defer f.Close()
	defer unix.Unlinkat(int(root.Fd()), name, 0)
	if fault.check("blob_write") != nil {
		return errGoalCommit
	}
	if n, err := f.Write(data); err != nil || n != len(data) {
		if err == nil {
			err = io.ErrShortWrite
		}
		return goalPersistenceError(errGoalCommit, "blob.write", err)
	}
	if fault.check("blob_sync") != nil {
		return errGoalCommit
	}
	if err := goalPersistenceError(errGoalCommit, "blob.sync", f.Sync()); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return goalPersistenceError(errGoalCommit, "blob.close", err)
	}
	if fault.check("blob_publish") != nil {
		return errGoalCommit
	}
	if err := unix.Linkat(int(root.Fd()), name, int(root.Fd()), ref, 0); err != nil {
		return goalPersistenceError(errGoalCommit, "blob.publish", err)
	}
	if err := unix.Unlinkat(int(root.Fd()), name, 0); err != nil {
		return goalPersistenceError(errGoalCommit, "blob.unlink", err)
	}
	return nil
}
