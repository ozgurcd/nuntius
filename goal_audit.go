package nuntius

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const nativeAuditTool = "native_goal_spec"
const nativeAuditIntentName = "goal-audit-intent.json"
const nativeAuditReceiptName = "goal-audit-receipt.json"

// Readiness examines the full delivery copy, with a fixed I/O and row budget.
// It refuses larger histories rather than certifying an unchecked suffix.
const nativeAuditLogMaxBytes = 64 << 20
const nativeAuditLogMaxRowBytes = 16384

var errNativeAudit = errors.New("native audit is pending or unavailable; preserve the history and retry audit reconciliation before starting native work")
var errNativeAuditMissing = errors.New("required native audit rows are missing or damaged; native work remains held; preserve the history and retry audit reconciliation")
var errNativeAuditLog = errors.New("native audit history cannot be checked safely: it is unreadable, incomplete or exceeds the inspection limit; preserve it for review before starting native work")

// These are mirror delivery records, never goal or lane-control authority.
type nativeAuditReceipt struct {
	Version  int    `json:"version"`
	Sequence uint64 `json:"sequence"`
	EventID  string `json:"event_id"`
}

type nativeAuditRow struct {
	Version  int       `json:"native_audit_version"`
	Time     time.Time `json:"time"`
	Tool     string    `json:"tool"`
	EventID  string    `json:"event_id"`
	Sequence uint64    `json:"sequence"`
	GoalID   string    `json:"goal_id"`
	Revision uint64    `json:"goal_revision"`
	Source   string    `json:"source"`
	Result   string    `json:"result"`
}

type nativeAuditState struct {
	Sequence    uint64 `json:"sequence"`
	Mirrored    uint64 `json:"mirrored"`
	Pending     uint64 `json:"pending"`
	Available   bool   `json:"available"`
	Explanation string `json:"explanation"`
}

func (m *Manager) nativeAuditStatus() (nativeAuditState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		return nativeAuditUnavailable(), errNativeAudit
	}
	defer root.Close()
	lock, err := laneControlReadLock(root)
	if err != nil {
		return nativeAuditUnavailable(), errNativeAudit
	}
	defer lock.Close()
	state, _, _, _, err := readNativeAudit(root)
	return state, err
}

// source is supplied by the trusted adapter invoking this component. It is
// not decoded from a request, spec, seal, actor field or operation ID.
func (m *Manager) mirrorNativeAudit(source string, fault goalCommitIO) (nativeAuditState, error) {
	if !validNativeAuditSource(source) {
		return nativeAuditUnavailable(), errNativeAudit
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.readOnly {
		return nativeAuditUnavailable(), errNativeAudit
	}
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		return nativeAuditUnavailable(), errNativeAudit
	}
	defer root.Close()
	lock, err := laneControlLock(root, unix.LOCK_EX)
	if err != nil {
		return nativeAuditUnavailable(), errNativeAudit
	}
	defer lock.Close()
	state, rows, receipt, intent, err := readNativeAuditRecords(root)
	if err != nil {
		return state, err
	}
	// Receipts are cursors, not proof that earlier delivery bytes still exist.
	// Inspect once, then repair only absent acknowledged events. Keep the
	// latest intent/receipt intact while restoring earlier rows.
	delivered, err := observeNativeAuditLog(root, rows[:receipt.Sequence], intent, false)
	if err != nil {
		state.Available, state.Explanation = false, err.Error()
		return state, err
	}
	fail := func() (nativeAuditState, error) {
		state.Available = false
		state.Explanation = errNativeAudit.Error()
		return state, errNativeAudit
	}
	// Reconfirm journal sync first, including a prior B03 uncertain save. A
	// replayed complete event alone cannot certify a failed filesystem sync.
	if state.Sequence > 0 {
		journal, e := openGoalJournal(root, false)
		if e != nil {
			return fail()
		}
		e = syncGoalJournal(root, journal, fault)
		journal.Close()
		if e != nil {
			return fail()
		}
	}
	for i, present := range delivered {
		if present {
			continue
		}
		row := rows[i]
		row.Time, row.Source = time.Now().UTC(), source
		if intent != nil && intent.Sequence == row.Sequence {
			row = *intent
		}
		if appendNativeAudit(root, row, fault) != nil {
			return fail()
		}
	}
	for _, row := range rows[receipt.Sequence:] {
		row.Time, row.Source = time.Now().UTC(), source
		if intent != nil && intent.Sequence == row.Sequence {
			row = *intent // Preserve the first adapter and timestamp on redelivery.
		} else {
			b, _ := json.Marshal(row)
			if writeNativeAuditState(root, nativeAuditIntentName, b, fault, "native_intent") != nil {
				return fail()
			}
		}
		if appendNativeAudit(root, row, fault) != nil {
			return fail()
		}
		next := nativeAuditReceipt{Version: 1, Sequence: row.Sequence, EventID: row.EventID}
		b, _ := json.Marshal(next)
		if writeNativeAuditState(root, nativeAuditReceiptName, b, fault, "native_receipt") != nil {
			return fail()
		}
		receipt = next
		state.Mirrored, state.Pending = next.Sequence, state.Sequence-next.Sequence
	}
	if requireNativeAuditLocked(root, fault) != nil {
		return fail()
	}
	state.Available = true
	state.Explanation = "Native journal events are mirrored to the audit log. This operation started no work."
	return state, nil
}

// Final native admission must call this while already holding manager/control
// locks. It never acquires them recursively, starts work or reconciles A's
// independent lane intent. Later launch steps combine this with A's guard.
func requireNativeAuditLocked(root *os.File, fault goalCommitIO) error {
	state, rows, _, intent, err := readNativeAudit(root)
	if err != nil || state.Pending != 0 {
		return errNativeAudit
	}
	if state.Sequence == 0 {
		return nil
	}
	if fault.check("native_confirm.audit.jsonl") != nil || checkNativeAuditLog(root, rows, intent, true) != nil {
		return errNativeAudit
	}
	// A visible receipt after a failed directory sync is still uncertain.
	// Reconfirm every required file and directory before allowing admission.
	for _, name := range []string{goalJournalName, nativeAuditIntentName, nativeAuditReceiptName} {
		if fault.check("native_confirm."+name) != nil {
			return errNativeAudit
		}
		fd, err := unix.Openat(int(root.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err != nil {
			return errNativeAudit
		}
		f := os.NewFile(uintptr(fd), "native-audit-confirm")
		err = checkLaneControlFile(fd)
		if err == nil {
			err = f.Sync()
		}
		f.Close()
		if err != nil {
			return errNativeAudit
		}
	}
	if fault.check("native_confirm.dirsync") != nil || root.Sync() != nil {
		return errNativeAudit
	}
	return nil
}

func nativeAuditUnavailable() nativeAuditState {
	return nativeAuditState{Explanation: errNativeAudit.Error()}
}

// Read only. The journal validates every complete frame and private reference
// before any delivery record can be used. Uncommitted tails produce no row.
func readNativeAudit(root *os.File) (nativeAuditState, []nativeAuditRow, nativeAuditReceipt, *nativeAuditRow, error) {
	state, rows, receipt, intent, err := readNativeAuditRecords(root)
	if err != nil || receipt.Sequence == 0 {
		return state, rows, receipt, intent, err
	}
	delivered, err := observeNativeAuditLog(root, rows[:receipt.Sequence], intent, false)
	if err == nil {
		state.Mirrored = 0
		for _, present := range delivered {
			if present {
				state.Mirrored++
			}
		}
		state.Pending = state.Sequence - state.Mirrored
		if state.Mirrored != receipt.Sequence {
			err = errNativeAuditMissing
		}
	}
	if err != nil {
		state.Available, state.Explanation = false, err.Error()
	}
	return state, rows, receipt, intent, err
}

// Journal and private delivery metadata only. Reconciliation uses this even
// when delivered rows are absent; observation/admission also inspect the log.
func readNativeAuditRecords(root *os.File) (nativeAuditState, []nativeAuditRow, nativeAuditReceipt, *nativeAuditRow, error) {
	bad := func() (nativeAuditState, []nativeAuditRow, nativeAuditReceipt, *nativeAuditRow, error) {
		return nativeAuditUnavailable(), nil, nativeAuditReceipt{}, nil, errNativeAudit
	}
	p, err := replayGoalJournalRoot(root)
	if err != nil {
		return bad()
	}
	data, err := readGoalPrivate(root, goalJournalName, goalJournalMaxBytes)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return bad()
	}
	data = data[:p.committedBytes]
	rows := make([]nativeAuditRow, 0, p.sequence)
	for sequence := uint64(1); len(data) != 0; sequence++ {
		size := int(binary.BigEndian.Uint32(data[16:20])) + goalJournalHeaderBytes
		event, err := decodeGoalJournalEvent(data[goalJournalHeaderBytes:size])
		if err != nil {
			return bad()
		}
		sum := sha256.Sum256(data[:size])
		rows = append(rows, nativeAuditRow{Version: 1, Tool: nativeAuditTool, EventID: "native-goal-" + hex.EncodeToString(sum[:]), Sequence: sequence, GoalID: event.GoalID, Revision: event.Revision, Result: "updated"})
		data = data[size:]
	}
	receipt := nativeAuditReceipt{}
	b, err := readGoalPrivate(root, nativeAuditReceiptName, 4096)
	if err == nil {
		if decodeLaneControlObject(b, &receipt) != nil || receipt.Version != 1 || receipt.Sequence == 0 || receipt.Sequence > p.sequence || receipt.EventID != rows[receipt.Sequence-1].EventID {
			return bad()
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return bad()
	}
	var intent *nativeAuditRow
	b, err = readGoalPrivate(root, nativeAuditIntentName, 4096)
	if err == nil {
		var row nativeAuditRow
		if decodeLaneControlObject(b, &row) != nil || !validNativeAuditRow(row) || row.Sequence > p.sequence || (row.Sequence != receipt.Sequence && row.Sequence != receipt.Sequence+1) {
			return bad()
		}
		want := rows[row.Sequence-1]
		want.Time, want.Source = row.Time, row.Source
		if row != want {
			return bad()
		}
		intent = &row
	} else if !errors.Is(err, os.ErrNotExist) {
		return bad()
	}
	if receipt.Sequence > 0 && intent == nil {
		return bad()
	}
	state := nativeAuditState{Sequence: p.sequence, Mirrored: receipt.Sequence, Pending: p.sequence - receipt.Sequence, Available: p.sequence == receipt.Sequence, Explanation: "Native audit delivery is up to date. No native work was started."}
	if state.Pending > 0 {
		state.Explanation = errNativeAudit.Error()
	}
	return state, rows, receipt, intent, nil
}

func checkNativeAuditLog(root *os.File, rows []nativeAuditRow, intent *nativeAuditRow, confirm bool) error {
	delivered, err := observeNativeAuditLog(root, rows, intent, confirm)
	if err != nil {
		return err
	}
	for _, present := range delivered {
		if !present {
			return errNativeAuditMissing
		}
	}
	return nil
}

func observeNativeAuditLog(root *os.File, rows []nativeAuditRow, intent *nativeAuditRow, confirm bool) ([]bool, error) {
	delivered := make([]bool, len(rows))
	fd, err := unix.Openat(int(root.Fd()), "audit.jsonl", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return delivered, nil
	}
	if err != nil {
		return nil, errNativeAuditLog
	}
	f := os.NewFile(uintptr(fd), "native-audit-observation")
	defer f.Close()
	if checkLaneControlFile(fd) != nil || unix.Flock(fd, unix.LOCK_SH|unix.LOCK_NB) != nil {
		return nil, errNativeAuditLog
	}
	defer unix.Flock(fd, unix.LOCK_UN)
	st, err := f.Stat()
	if err != nil || st.Size() > nativeAuditLogMaxBytes {
		return nil, errNativeAuditLog
	}
	if st.Size() > 0 {
		var last [1]byte
		if n, err := f.ReadAt(last[:], st.Size()-1); err != nil || n != 1 || last[0] != '\n' {
			return nil, errNativeAuditLog
		}
	}
	byID := make(map[string]int, len(rows))
	for i, row := range rows {
		byID[row.EventID] = i
	}
	scan := bufio.NewScanner(io.LimitReader(f, nativeAuditLogMaxBytes+1))
	scan.Buffer(make([]byte, 4096), nativeAuditLogMaxRowBytes+1)
	// Keep CR bytes in genuine legacy CRLF rows for exact byte accounting.
	scan.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			return i + 1, data[:i], nil
		}
		if atEOF && len(data) > 0 {
			return 0, nil, errNativeAuditLog
		}
		return 0, nil, nil
	})
	var read int64
	for scan.Scan() {
		line := scan.Bytes()
		read += int64(len(line)) + 1
		if read > nativeAuditLogMaxBytes {
			return nil, errNativeAuditLog
		}
		var row nativeAuditRow
		if decodeLaneControlObject(line, &row) != nil || !validNativeAuditRow(row) {
			continue // Keep malformed and genuine legacy history untouched.
		}
		i, required := byID[row.EventID]
		if !required {
			continue
		}
		want := rows[i]
		want.Time, want.Source = row.Time, row.Source
		if intent != nil && intent.Sequence == row.Sequence {
			want.Time, want.Source = intent.Time, intent.Source
		}
		if row == want {
			delivered[i] = true
		}
	}
	if scan.Err() != nil || read != st.Size() {
		return nil, errNativeAuditLog
	}
	if confirm && f.Sync() != nil {
		return nil, errNativeAuditLog
	}
	return delivered, nil
}

func validNativeAuditSource(source string) bool {
	return source == "cli" || source == "web" || source == "mcp" || source == "native"
}

func validNativeAuditEventID(id string) bool {
	if !strings.HasPrefix(id, "native-goal-") || len(id) != len("native-goal-")+64 {
		return false
	}
	for _, c := range id[len("native-goal-"):] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func validNativeAuditRow(row nativeAuditRow) bool {
	return row.Version == 1 && row.Tool == nativeAuditTool && validNativeAuditEventID(row.EventID) && row.Sequence > 0 && row.Sequence <= goalJournalMaxEvents && row.Revision > 0 && row.Revision <= row.Sequence && goalPattern.MatchString(row.GoalID) && validNativeAuditSource(row.Source) && !row.Time.IsZero() && row.Time.Location() == time.UTC && row.Result == "updated"
}

func writeNativeAuditState(root *os.File, name string, b []byte, fault goalCommitIO, stage string) error {
	// Validate any existing destination before replacement. Separate filenames
	// and checkpoints preserve A's control intent and revision authority.
	if _, err := readGoalPrivate(root, name, 4096); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errNativeAudit
	}
	ops := &laneControlIO{checkpoint: fault.checkpoint}
	return replaceLaneControlBytes(root, name, b, ops, stage)
}
