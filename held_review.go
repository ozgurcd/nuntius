package nuntius

import (
	"bufio"
	"crypto/rand"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// A reservation is written BEFORE any process may start. Only positive child
// identity can resolve it. Absence of a process is never permission to resend.
// Terminal receipts precede cleanup, so restored/unremoved message files cannot
// replay. Receipts and the parent/child links are deliberately retained.
type messageReceipt struct {
	Version   int         `json:"version"`
	Binding   heldMessage `json:"binding"`
	State     string      `json:"state"`
	ChildID   string      `json:"child_id"`
	Discarded bool        `json:"discarded"`
}

func validMessageBinding(h heldMessage, r Run) bool {
	a, ea := hex.DecodeString(h.RunSHA256)
	b, eb := hex.DecodeString(h.ContentSHA256)
	return h.Version == 1 && h.RunID == r.ID && h.Lane == r.Lane && h.GoalID == r.GoalID && h.RunSHA256 == r.SHA256 && ea == nil && eb == nil && len(a) == 32 && len(b) == 32 && h.Revision > 0 && validLaneControlOrigin(h.Origin) && (h.SessionID == "" || r.SessionID == "" || h.SessionID == r.SessionID)
}

func readMessageReceipt(dir *os.File, r Run) (*messageReceipt, error) {
	b, err := readLaneControlPrivate(dir, "message-receipt.json", 8192)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	var receipt messageReceipt
	if err != nil || decodeLaneControlObject(b, &receipt) != nil || receipt.Version != 1 || !validMessageBinding(receipt.Binding, r) {
		return nil, errLaneControlUnavailable
	}
	switch receipt.State {
	case "reserved", "consumed":
		if !runPattern.MatchString(receipt.ChildID) || receipt.ChildID == r.ID {
			return nil, errLaneControlUnavailable
		}
	case "discarded":
		if receipt.ChildID != "" {
			return nil, errLaneControlUnavailable
		}
	default:
		return nil, errLaneControlUnavailable
	}
	// Residual files after a crash must still be exactly the reviewed message.
	// A terminal receipt must never hide or delete a later replacement.
	if receipt.State != "reserved" || receipt.Discarded {
		b, err := readLaneControlPrivate(dir, "message.json", 16384)
		if err == nil {
			text, e := pendingText(b)
			if e != nil || laneControlDigest([]byte(text)) != receipt.Binding.ContentSHA256 {
				return nil, errLaneControlUnavailable
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, errLaneControlUnavailable
		}
		b, err = readLaneControlPrivate(dir, "message-hold.json", 4096)
		if err == nil {
			var h heldMessage
			if decodeLaneControlObject(b, &h) != nil || h != receipt.Binding {
				return nil, errLaneControlUnavailable
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, errLaneControlUnavailable
		}
	}
	return &receipt, nil
}

func writeMessageReceipt(dir *os.File, receipt messageReceipt, ops *laneControlIO) error {
	b, err := json.Marshal(receipt)
	if err != nil {
		return errLaneControlUnavailable
	}
	return replaceLaneControl(dir, "message-receipt.json", b, ops, "receipt-"+receipt.State)
}

func cleanReviewedMessage(dir *os.File) error {
	for _, name := range []string{"message.json", "message-hold.json"} {
		// Never follow/clear an unsafe replacement, even after terminal commit.
		_, err := readLaneControlPrivate(dir, name, 16384)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return errLaneControlUnavailable
		}
		if err = unix.Unlinkat(int(dir.Fd()), name, 0); err != nil {
			return errLaneControlUnavailable
		}
	}
	return dir.Sync()
}

// The CLI selector is the exact original run plus content revision/digest. Load
// the private binding locally; never return its session or accept one from argv.
func (m *Manager) heldCLISelection(lane, run string) (heldMessage, string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		return heldMessage{}, "", "", err
	}
	defer root.Close()
	guard, err := laneControlReadLock(root)
	if err != nil {
		return heldMessage{}, "", "", err
	}
	defer guard.Close()
	return m.heldSelectionLocked(lane, run)
}

// heldSelectionLocked uses the caller's manager and shared control locks.
func (m *Manager) heldSelectionLocked(lane, run string) (heldMessage, string, string, error) {
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		return heldMessage{}, "", "", err
	}
	defer root.Close()
	if _, err = readLaneControlsAt(root); err != nil {
		return heldMessage{}, "", "", err
	}
	var binding heldMessage
	state := "held_for_review"
	err = visitLaneMessages(root, lane, func(dir *os.File, r Run, _ []byte) error {
		if r.ID != run {
			return nil
		}
		receipt, e := readMessageReceipt(dir, r)
		if e != nil {
			return e
		}
		if receipt != nil {
			binding = receipt.Binding
			state = receipt.State
			return nil
		}
		b, e := readLaneControlPrivate(dir, "message-hold.json", 4096)
		if e != nil || decodeLaneControlObject(b, &binding) != nil || !validMessageBinding(binding, r) {
			return errLaneControlUnavailable
		}
		return nil
	})
	if err != nil || binding.RunID == "" {
		return heldMessage{}, "", "", errLaneControlUnavailable
	}
	dir, _, text, _, err := m.reviewedMessage(binding)
	if err != nil {
		return heldMessage{}, "", "", err
	}
	dir.Close()
	return binding, text, state, nil
}

func (m *Manager) reviewedMessage(h heldMessage) (*os.File, Run, string, *messageReceipt, error) {
	if !runPattern.MatchString(h.RunID) || !namePattern.MatchString(h.Lane) {
		return nil, Run{}, "", nil, errLaneControlUnavailable
	}
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		return nil, Run{}, "", nil, err
	}
	defer root.Close()
	var selected *os.File
	var original Run
	var text string
	var receipt *messageReceipt
	err = visitLaneMessages(root, h.Lane, func(dir *os.File, r Run, b []byte) error {
		if r.ID != h.RunID {
			return nil
		}
		if !validMessageBinding(h, r) {
			return errLaneControlUnavailable
		}
		var e error
		receipt, e = readMessageReceipt(dir, r)
		if e != nil {
			return e
		}
		if receipt != nil && receipt.Binding != h {
			return errLaneControlUnavailable
		}
		if receipt == nil || (receipt.State == "reserved" && !receipt.Discarded) {
			holdBytes, e := readLaneControlPrivate(dir, "message-hold.json", 4096)
			var stored heldMessage
			if e != nil || decodeLaneControlObject(holdBytes, &stored) != nil || stored != h {
				return errLaneControlUnavailable
			}
			text, e = pendingText(b)
			if e != nil || laneControlDigest([]byte(text)) != h.ContentSHA256 {
				return errLaneControlUnavailable
			}
		}
		fd, e := unix.Dup(int(dir.Fd()))
		if e != nil {
			return errLaneControlUnavailable
		}
		unix.CloseOnExec(fd)
		selected, original = os.NewFile(uintptr(fd), "reviewed-message"), r
		return nil
	})
	if err != nil || selected == nil {
		if selected != nil {
			selected.Close()
		}
		return nil, Run{}, "", nil, errLaneControlUnavailable
	}
	return selected, original, text, receipt, nil
}

func (m *Manager) identifiedMessageChild(r Run, receipt messageReceipt) bool {
	child, err := loadRun(filepath.Join(m.store, "runs", receipt.ChildID), receipt.ChildID)
	if err != nil || child == nil || r.ContinueRunID != receipt.ChildID || child.ParentRunID != r.ID || child.Lane != r.Lane || child.GoalID != r.GoalID || child.Kind != r.Kind || child.SessionID != receipt.Binding.SessionID || child.SHA256 != receipt.Binding.ContentSHA256 || !child.Resumed || child.PID <= 1 || child.ProcessStart == "" {
		return false
	}
	if known, e := m.nativeGoalLocked(r.GoalID); e != nil || known && !m.nativeSupervisionAllowed(*child) {
		return false
	}
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		return false
	}
	defer root.Close()
	runs, err := heldDirectory(root, "runs")
	if err != nil {
		return false
	}
	defer runs.Close()
	dir, err := heldDirectory(runs, child.ID)
	if err != nil {
		return false
	}
	defer dir.Close()
	b, err := readLaneControlPrivate(dir, "goal.txt", 12000)
	return err == nil && laneControlDigest(b) == receipt.Binding.ContentSHA256
}

func (m *Manager) consumeReviewedMessage(dir *os.File, receipt messageReceipt, ops *laneControlIO) error {
	// Audits can repeat during reconciliation; they contain only fixed verbs,
	// original goal/lane, a digest and an identified child, never message text.
	h := receipt.Binding
	if err := m.auditReviewedMessage(h, "held_message_send", "intent", receipt.ChildID, ops); err != nil {
		return err
	}
	receipt.State = "consumed"
	if err := writeMessageReceipt(dir, receipt, ops); err != nil {
		return err
	}
	if err := m.finishReviewedMessage(dir, receipt, ops); err != nil {
		return err
	}
	return laneControlCheckpoint(ops, "review-after-consume")
}

func (m *Manager) auditReviewedMessage(h heldMessage, tool, result, childID string, ops *laneControlIO) error {
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		return err
	}
	defer root.Close()
	if result != "intent" {
		found, err := recordedReviewedMessageAudit(root, h, tool, result, childID, ops)
		if err != nil || found {
			return err
		}
	}
	return appendLaneControlRow(root, map[string]any{"time": time.Now().UTC(), "tool": tool, "lane": h.Lane, "goal_id": h.GoalID, "sha256": h.ContentSHA256, "result": result, "reason": childID, "run_id": h.RunID, "origin": h.Origin, "message_revision": h.Revision}, ops, "review-audit-"+result)
}

// The common control lock serializes reviewed-message writers. The audit lock
// also excludes other audit appends while inspecting and confirming an outcome.
// A visible row after a failed sync is reused only after both syncs succeed.
func recordedReviewedMessageAudit(root *os.File, h heldMessage, tool, result, childID string, ops *laneControlIO) (bool, error) {
	fd, err := unix.Openat(int(root.Fd()), "audit.jsonl", unix.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return false, errLaneControlUnavailable
	}
	f := os.NewFile(uintptr(fd), "reviewed-message-audit")
	defer f.Close()
	if err = checkLaneControlFile(fd); err != nil {
		return false, err
	}
	if err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return false, errLaneControlUnavailable
	}
	defer unix.Flock(fd, unix.LOCK_UN)
	var st unix.Stat_t
	if err = unix.Fstat(fd, &st); err != nil {
		return false, errLaneControlUnavailable
	}
	if st.Size > 0 {
		var last [1]byte
		if n, e := f.ReadAt(last[:], st.Size-1); e != nil || n != 1 || last[0] != '\n' {
			return false, errLaneControlUnavailable
		}
	}
	found := false
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 65536)
	for scanner.Scan() {
		var row struct {
			Tool, Result, Lane, Reason, Origin string
			RunID                              string `json:"run_id"`
			GoalID                             string `json:"goal_id"`
			SHA256                             string `json:"sha256"`
			Revision                           uint64 `json:"message_revision"`
		}
		if json.Unmarshal(scanner.Bytes(), &row) != nil {
			return false, errLaneControlUnavailable
		}
		if row.Tool == tool && row.Result == result && row.Lane == h.Lane && row.RunID == h.RunID && row.GoalID == h.GoalID && row.SHA256 == h.ContentSHA256 && row.Revision == h.Revision && row.Origin == h.Origin && row.Reason == childID {
			found = true
		}
	}
	if scanner.Err() != nil {
		return false, errLaneControlUnavailable
	}
	if !found {
		return false, nil
	}
	stage := "review-audit-" + result
	if err = laneControlCheckpoint(ops, stage+".fsync"); err != nil {
		return false, err
	}
	if err = f.Sync(); err != nil {
		return false, err
	}
	if err = laneControlCheckpoint(ops, stage+".dirsync"); err != nil {
		return false, err
	}
	return true, root.Sync()
}

func (m *Manager) finishReviewedMessage(dir *os.File, receipt messageReceipt, ops *laneControlIO) error {
	// A prior rename may be visible while its directory sync is still uncertain.
	if err := dir.Sync(); err != nil {
		return err
	}
	if receipt.Discarded || receipt.State == "discarded" {
		if err := m.auditReviewedMessage(receipt.Binding, "held_message_discard", "discarded", receipt.ChildID, ops); err != nil {
			return err
		}
	}
	if receipt.State == "consumed" {
		if err := m.auditReviewedMessage(receipt.Binding, "held_message_send", "consumed", receipt.ChildID, ops); err != nil {
			return err
		}
	}
	return cleanReviewedMessage(dir)
}

// Internal only: adapters must pass the exact reviewed reservation, never text.
func (m *Manager) sendHeld(h heldMessage, ops *laneControlIO) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	guard, err := m.admissionControlLock()
	if err != nil {
		return "", err
	}
	defer guard.Close()
	return m.sendHeldLocked(h, ops)
}

// sendHeldLocked preserves A05 admission and receipts under caller-owned locks.
func (m *Manager) sendHeldLocked(h heldMessage, ops *laneControlIO) (string, error) {
	dir, r, text, receipt, err := m.reviewedMessage(h)
	if err != nil {
		return "", err
	}
	defer dir.Close()
	if receipt != nil {
		if receipt.State == "discarded" || receipt.Discarded {
			return "", errors.New("held_message_discarded")
		}
		if !m.identifiedMessageChild(r, *receipt) {
			return "", errors.New("held_message_launch_uncertain")
		}
		if receipt.State == "reserved" {
			if err = m.consumeReviewedMessage(dir, *receipt, ops); err != nil {
				return "", err
			}
		} else if err = m.finishReviewedMessage(dir, *receipt, ops); err != nil {
			return "", err
		}
		return receipt.ChildID, nil
	}
	if !sessionPattern.MatchString(h.SessionID) || r.SessionID != h.SessionID {
		return "", errors.New("held_message_session_unavailable")
	}
	if err = m.reload(); err != nil {
		return "", err
	}
	lane, err := m.lane(h.Lane)
	if err != nil {
		return "", err
	}
	if lane.Kind != r.Kind {
		return "", errors.New("held_message_session_unavailable")
	}
	if err = m.admitLaneExceptLocked(h.Lane, h.RunID); err != nil {
		return "", err
	}
	if err = m.checkFrozen(); err != nil {
		return "", err
	}
	if m.closed {
		return "", errors.New("manager is closing")
	}
	lock, err := m.lock(h.Lane)
	if err != nil {
		return "", err
	}
	owned := false
	defer func() {
		if !owned {
			lock.Close()
		}
	}()
	if sameProcess(r) {
		return "", errors.New("lane is busy")
	}
	if err = laneControlCheckpoint(ops, "review-validated"); err != nil {
		return "", err
	}
	known, e := m.nativeGoalLocked(r.GoalID)
	if e != nil {
		return "", e
	}
	if known {
		id, e := m.launchGoalContinuationLocked(lane, text, h.SessionID, lock, &r, &h, ops, &owned, goalCommitIO{}, "message")
		owned = owned || m.active[h.Lane] != nil
		return id, e
	}
	id, err := m.launchMessage(lane, r.GoalID, text, h.SessionID, lock, &r, &h, ops, &owned)
	return id, err
}

// Already-accepted, unpaused messages use the same receipt protocol. This is
// not a review bypass: callers first pass ordinary admission with no exemption.
func (m *Manager) bindPendingMessage(r Run, text string) (*heldMessage, error) {
	controls, err := m.readLaneControlsLocked()
	if err != nil {
		return nil, err
	}
	random := make([]byte, 16)
	if _, err = rand.Read(random); err != nil {
		return nil, errLaneControlUnavailable
	}
	revision := controls.Revision
	if revision == 0 {
		revision = 1
	}
	h := heldMessage{Version: 1, Lane: r.Lane, RunID: r.ID, GoalID: r.GoalID, SessionID: r.SessionID, RunSHA256: r.SHA256, ContentSHA256: laneControlDigest([]byte(text)), Revision: revision, Origin: "lane-control:" + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(random)}
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	runs, err := heldDirectory(root, "runs")
	if err != nil {
		return nil, err
	}
	defer runs.Close()
	dir, err := heldDirectory(runs, r.ID)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	b, err := json.Marshal(h)
	if err != nil {
		return nil, err
	}
	if err = replaceLaneControl(dir, "message-hold.json", b, nil, "held"); err != nil {
		return nil, err
	}
	return &h, nil
}

func (m *Manager) discardHeld(h heldMessage, ops *laneControlIO) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	guard, err := m.admissionControlLock()
	if err != nil {
		return err
	}
	defer guard.Close()
	return m.discardHeldLocked(h, ops)
}

func (m *Manager) discardHeldLocked(h heldMessage, ops *laneControlIO) error {
	dir, run, _, receipt, err := m.reviewedMessage(h)
	if err != nil {
		return err
	}
	defer dir.Close()
	if receipt != nil {
		if receipt.Discarded || receipt.State == "discarded" {
			if receipt.ChildID == "" {
				if e := m.settleGoalMessageLocked(run, h); e != nil {
					return e
				}
			}
			return m.finishReviewedMessage(dir, *receipt, ops)
		}
		// Discard removes only the reviewed content. A possible child remains
		// reserved until positive launch reconciliation; never infer cancellation.
		if err = m.auditReviewedMessage(h, "held_message_discard", "intent", receipt.ChildID, ops); err != nil {
			return err
		}
		receipt.Discarded = true
		if err = writeMessageReceipt(dir, *receipt, ops); err != nil {
			return err
		}
		return m.finishReviewedMessage(dir, *receipt, ops)
	}
	if err = m.auditReviewedMessage(h, "held_message_discard", "intent", "", ops); err != nil {
		return err
	}
	if err = writeMessageReceipt(dir, messageReceipt{Version: 1, Binding: h, State: "discarded", Discarded: true}, ops); err != nil {
		return err
	}
	if err = m.settleGoalMessageLocked(run, h); err != nil {
		return err
	}
	return m.finishReviewedMessage(dir, messageReceipt{Version: 1, Binding: h, State: "discarded", Discarded: true}, ops)
}

// B and future adapters may reconcile evidence, never launch from this method.
// A discarded uncertain reservation remains blocking when identity is absent.
func (m *Manager) reconcileHeld(h heldMessage, ops *laneControlIO) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	guard, err := m.admissionControlLock()
	if err != nil {
		return err
	}
	defer guard.Close()
	dir, r, _, receipt, err := m.reviewedMessage(h)
	if err != nil {
		return err
	}
	defer dir.Close()
	if receipt == nil {
		return errors.New("held_message_not_launched")
	}
	if receipt.State != "reserved" {
		return m.finishReviewedMessage(dir, *receipt, ops)
	}
	if !m.identifiedMessageChild(r, *receipt) {
		return errors.New("held_message_launch_uncertain")
	}
	return m.consumeReviewedMessage(dir, *receipt, ops)
}
