package nuntius

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const (
	goalJournalName          = "goal-journal.bin"
	goalJournalHeaderBytes   = 84
	goalJournalMaxFrameBytes = 4096
	goalJournalMaxBytes      = 16 << 20
	goalJournalMaxEvents     = 16384
	goalJournalMaxGoals      = 4096
	goalJournalMaxBlobBytes  = 64 << 20
	goalSealMaxBytes         = 256 << 10
)

var errGoalReplay = errors.New("native goal history is unavailable or invalid; preserve the files and review the journal and private references before using native goals")

type goalJournalEvent struct {
	Kind           string `json:"kind"`
	GoalID         string `json:"goal_id"`
	Revision       uint64 `json:"revision"`
	SpecRef        string `json:"spec_ref"`
	SealRef        string `json:"seal_ref,omitempty"`
	OperationID    string `json:"operation_id,omitempty"`
	InputSHA256    string `json:"input_sha256,omitempty"`
	RecordRef      string `json:"record_ref,omitempty"`
	MutationSHA256 string `json:"mutation_sha256,omitempty"`
}

type goalReplayRecord struct {
	revision uint64
	spec     string
	sealRef  string
	record   string
}

// goalProjection owns immutable strings; reads decode fresh detached values.
type goalProjection struct {
	sequence       uint64
	committedBytes int64
	tornTail       bool
	records        map[string]goalReplayRecord
	operations     map[string]goalOperationReceipt
}

func replayGoalJournal(path string) (*goalProjection, error) {
	root, err := openLaneControlRoot(path)
	if err != nil {
		return nil, errGoalReplay
	}
	defer root.Close()
	return replayGoalJournalRoot(root)
}

func replayGoalJournalRoot(root *os.File) (*goalProjection, error) {
	data, err := readGoalPrivate(root, goalJournalName, goalJournalMaxBytes)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, errGoalReplay
	}
	return replayGoalJournalData(root, data)
}

func replayGoalJournalData(root *os.File, data []byte) (*goalProjection, error) {
	if len(data) > goalJournalMaxBytes {
		return nil, errGoalReplay
	}
	p := &goalProjection{records: map[string]goalReplayRecord{}, operations: map[string]goalOperationReceipt{}}
	var blobBytes int64
	for len(data) != 0 {
		if p.sequence >= goalJournalMaxEvents || !validGoalFrameHeader(data, p.sequence+1) {
			return nil, errGoalReplay
		}
		if len(data) < goalJournalHeaderBytes {
			p.tornTail = true
			break
		}
		size := int(binary.BigEndian.Uint32(data[16:20]))
		if len(data)-goalJournalHeaderBytes < size {
			p.tornTail = true
			break
		}
		payload := data[goalJournalHeaderBytes : goalJournalHeaderBytes+size]
		sum := sha256.Sum256(payload)
		if !bytes.Equal(sum[:], data[52:84]) {
			return nil, errGoalReplay
		}
		e, err := decodeGoalJournalEvent(payload)
		if err != nil {
			return nil, errGoalReplay
		}
		old, exists := p.records[e.GoalID]
		if e.Revision != old.revision+1 || (!exists && len(p.records) >= goalJournalMaxGoals) {
			return nil, errGoalReplay
		}
		spec, err := readGoalBlob(root, e.SpecRef, "goal-spec-", nativeGoalMaxBytes, &blobBytes)
		if err != nil {
			return nil, errGoalReplay
		}
		s, err := decodeNativeGoalSpec(spec)
		if err != nil || s.ID != e.GoalID {
			return nil, errGoalReplay
		}
		if e.SealRef != "" {
			if _, err := readGoalBlob(root, e.SealRef, "goal-seal-", goalSealMaxBytes, &blobBytes); err != nil {
				return nil, errGoalReplay
			}
		}
		var record []byte
		input := goalInputFingerprint(e.GoalID, e.Revision-1, e.SpecRef, e.SealRef)
		if e.RecordRef != "" {
			record, err = readGoalBlob(root, e.RecordRef, "goal-record-", goalRecordMaxBytes, &blobBytes)
			if err != nil {
				return nil, errGoalReplay
			}
			metadata, err := decodeGoalRecord(record)
			if err != nil || metadata.ApprovalRevision > e.Revision {
				return nil, errGoalReplay
			}
			input = goalRecordFingerprint(input, e.RecordRef, e.MutationSHA256)
		}
		nextRecord := goalReplayRecord{revision: e.Revision, spec: string(spec), sealRef: e.SealRef, record: string(record)}
		if !goalRecordHistoryValid(old, nextRecord) {
			return nil, errGoalReplay
		}
		if e.OperationID != "" {
			if _, duplicate := p.operations[e.OperationID]; duplicate || e.InputSHA256 != input {
				return nil, errGoalReplay
			}
			p.operations[e.OperationID] = goalOperationReceipt{input: e.InputSHA256, mutation: e.MutationSHA256, result: goalCommitResult{OperationID: e.OperationID, GoalID: e.GoalID, Revision: e.Revision, Sequence: p.sequence + 1}}
		}
		p.records[e.GoalID] = nextRecord
		p.sequence++
		p.committedBytes += int64(goalJournalHeaderBytes + size)
		data = data[goalJournalHeaderBytes+size:]
	}
	return p, nil
}

// Header integrity is independent of the payload. A damaged length cannot
// disguise an interior corrupt event as an incomplete final append.
func validGoalFrameHeader(b []byte, sequence uint64) bool {
	expected := make([]byte, 16)
	copy(expected, "NGJF")
	binary.BigEndian.PutUint32(expected[4:8], 1)
	binary.BigEndian.PutUint64(expected[8:16], sequence)
	if !bytes.Equal(b[:min(len(b), 16)], expected[:min(len(b), 16)]) {
		return false
	}
	if len(b) >= 20 {
		size := binary.BigEndian.Uint32(b[16:20])
		if size == 0 || size > goalJournalMaxFrameBytes {
			return false
		}
		sum := sha256.Sum256(b[:20])
		n := min(len(b)-20, 32)
		if !bytes.Equal(b[20:20+n], sum[:n]) {
			return false
		}
	}
	return true
}

func decodeGoalJournalEvent(b []byte) (goalJournalEvent, error) {
	if len(b) > goalJournalMaxFrameBytes || !utf8.Valid(b) || !nativeJSONScalars(b) {
		return goalJournalEvent{}, errGoalReplay
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	raw, err := queueJSON(d, 1)
	if err != nil {
		return goalJournalEvent{}, errGoalReplay
	}
	if _, err := d.Token(); err != io.EOF {
		return goalJournalEvent{}, errGoalReplay
	}
	m, ok := raw.(map[string]any)
	if !ok || !nativeClosedFields(m, []string{"kind", "goal_id", "revision", "spec_ref"}, []string{"seal_ref", "operation_id", "input_sha256", "record_ref", "mutation_sha256"}) {
		return goalJournalEvent{}, errGoalReplay
	}
	rev, ok := m["revision"].(json.Number)
	if !ok {
		return goalJournalEvent{}, errGoalReplay
	}
	n, err := rev.Int64()
	if err != nil || n < 1 || n > goalJournalMaxEvents {
		return goalJournalEvent{}, errGoalReplay
	}
	var e goalJournalEvent
	if json.Unmarshal(b, &e) != nil || e.Kind != "goal.spec" || !goalPattern.MatchString(e.GoalID) || !validGoalBlobRef(e.SpecRef, "goal-spec-") {
		return goalJournalEvent{}, errGoalReplay
	}
	if _, exists := m["seal_ref"]; exists && !validGoalBlobRef(e.SealRef, "goal-seal-") {
		return goalJournalEvent{}, errGoalReplay
	}
	_, operation := m["operation_id"]
	_, fingerprint := m["input_sha256"]
	if operation != fingerprint || (operation && (!validGoalOperationID(e.OperationID) || !validGoalBlobRef("goal-spec-"+e.InputSHA256, "goal-spec-"))) {
		return goalJournalEvent{}, errGoalReplay
	}
	_, record := m["record_ref"]
	_, mutation := m["mutation_sha256"]
	if record != mutation || (record && (!operation || !validGoalBlobRef(e.RecordRef, "goal-record-") || !validGoalDigest(e.MutationSHA256))) {
		return goalJournalEvent{}, errGoalReplay
	}
	return e, nil
}

func validGoalBlobRef(ref, prefix string) bool {
	if !strings.HasPrefix(ref, prefix) || len(ref) != len(prefix)+64 {
		return false
	}
	for _, c := range ref[len(prefix):] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func readGoalBlob(root *os.File, ref, prefix string, limit int64, total *int64) ([]byte, error) {
	if !validGoalBlobRef(ref, prefix) {
		return nil, errGoalReplay
	}
	// Bound cumulative work, including repeated historical references. Preserve
	// all history on overflow; only a separate owner maintenance step may archive.
	b, err := readGoalPrivate(root, ref, min(limit, goalJournalMaxBlobBytes-*total))
	if err != nil {
		return nil, errGoalReplay
	}
	*total += int64(len(b))
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != ref[len(prefix):] {
		return nil, errGoalReplay
	}
	return b, nil
}

// Reuse A-CONTROL's rooted opening and private-file predicates, adding a size
// preflight and change detection. No read creates a lock or repairs a file.
func readGoalPrivate(root *os.File, name string, limit int64) ([]byte, error) {
	fd, err := unix.Openat(int(root.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, errGoalReplay
	}
	f := os.NewFile(uintptr(fd), "native-goal-private")
	defer f.Close()
	if checkLaneControlFile(fd) != nil {
		return nil, errGoalReplay
	}
	before, err := f.Stat()
	if err != nil || before.Size() > limit {
		return nil, errGoalReplay
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, errGoalReplay
	}
	after, err := f.Stat()
	if err != nil || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || int64(len(b)) != after.Size() || checkLaneControlFile(fd) != nil {
		return nil, errGoalReplay
	}
	return b, nil
}

func (p *goalProjection) spec(id string) (nativeGoalSpec, bool) {
	r, ok := p.records[id]
	if !ok {
		return nativeGoalSpec{}, false
	}
	s, err := decodeNativeGoalSpec([]byte(r.spec))
	return s, err == nil
}
