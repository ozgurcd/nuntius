package nuntius

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"
)

var runPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)
var errBusy = errors.New("lane busy")

func privateDir(p string) error {
	if e := os.MkdirAll(p, 0700); e != nil {
		return errors.New("cannot create private run store")
	}
	st, e := os.Lstat(p)
	if e != nil || !st.IsDir() || st.Mode().Perm()&0077 != 0 {
		return errors.New("run store must be a real private directory (0700)")
	}
	return nil
}
func privateFile(p string, flags int) (*os.File, error) {
	f, e := os.OpenFile(p, flags|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return nil, errors.New("cannot open private run file")
	}
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		f.Close()
		return nil, errors.New("run file must be regular and private (0600)")
	}
	return f, nil
}
func putPrivate(p string, b []byte) error {
	f, e := privateFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if e != nil {
		return e
	}
	defer f.Close()
	if _, e = f.Write(b); e != nil {
		return errors.New("cannot persist run data")
	}
	if e = f.Sync(); e != nil {
		return errors.New("cannot sync run data")
	}
	return nil
}
func (m *Manager) lock(lane string) (*os.File, error) {
	f, e := privateFile(filepath.Join(m.store, "locks", lane+".lock"), os.O_RDWR|os.O_CREATE)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		f.Close()
		if errors.Is(e, syscall.EWOULDBLOCK) {
			return nil, errBusy
		}
		return nil, e
	}
	return f, nil
}
func (m *Manager) busy(lane string) (bool, error) {
	f, e := m.lock(lane)
	if e != nil {
		return errors.Is(e, errBusy), e
	}
	f.Close()
	return false, nil
}
func (m *Manager) save(r *Run) error {
	dir := filepath.Join(m.store, "runs", r.ID)
	return saveAt(dir, r)
}
func saveAt(dir string, r *Run) error {
	r.Integrity = ""
	b, e := json.Marshal(r)
	if e != nil {
		return e
	}
	sum := sha256.Sum256(b)
	r.Integrity = hex.EncodeToString(sum[:])
	b, e = json.Marshal(r)
	if e != nil {
		return e
	}
	return atomicPrivate(filepath.Join(dir, "meta.json"), b)
}

func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func readBrief(f *os.File) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(f, 12001))
	if err != nil || len(b) == 0 || len(b) > 12000 {
		return nil, errors.New("original brief unavailable")
	}
	return b, nil
}

// A corrupt or torn record does not hide the intact goals beside it. Linked
// paths and I/O failures remain errors; malformed bytes are ignored, not repaired.
func loadRun(dir, id string) (*Run, error) {
	f, err := privateFile(filepath.Join(dir, "meta.json"), os.O_RDONLY)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(f, 131073))
	_ = f.Close()
	var r Run
	if err != nil {
		return nil, err
	}
	if len(b) > 131072 || strictJSON(b, &r) != nil || r.ID != id || !namePattern.MatchString(r.Lane) || !goalPattern.MatchString(r.GoalID) || r.Started.IsZero() || (r.State != "running" && r.State != "ended" && r.State != "timed_out" && r.State != "interrupted") {
		return nil, nil
	}
	if r.ResumeCount < 0 || r.ResumeCount > 2 || len(r.Brief) > 12000 || (r.ParentRunID != "" && !runPattern.MatchString(r.ParentRunID)) || (r.ContinueRunID != "" && !runPattern.MatchString(r.ContinueRunID)) {
		return nil, nil
	}
	if r.Integrity != "" {
		want := r.Integrity
		r.Integrity = ""
		canonical, _ := json.Marshal(r)
		sum := sha256.Sum256(canonical)
		r.Integrity = want
		if want != hex.EncodeToString(sum[:]) {
			return nil, nil
		}
	}
	return &r, nil
}
func (m *Manager) latest(lane string) (*Run, error) {
	runs, err := m.history()
	if err != nil {
		return nil, err
	}
	for _, r := range runs {
		if r.Lane == lane {
			return &r, nil
		}
	}
	return nil, nil
}
func processExists(pid int) bool {
	if pid <= 1 {
		return false
	}
	e := syscall.Kill(pid, 0)
	return e == nil || errors.Is(e, syscall.EPERM)
}

func (m *Manager) recoverEnded(r *Run) error {
	r.State = "ended"
	r.Exit = nil
	now := time.Now().UTC()
	r.Ended = &now
	if r.StopReason == "" {
		r.StopReason = "ended_no_report"
		if m.reportExists(*r) {
			r.StopReason = "completed"
		}
	}
	r.SessionID = m.session(r.ID)
	return m.endRun(r)
}
func (m *Manager) audit(tool, lane, goal, digest, result string, reasons ...string) error {
	return m.appendAudit(false, tool, lane, goal, digest, result, reasons...)
}

func (m *Manager) appendAudit(nonblocking bool, tool, lane, goal, digest, result string, reasons ...string) error {
	// Native outcomes belong only to the journal mirror, never generic MCP
	// success/failure auditing. The generic seam cannot fabricate their IDs.
	if tool == nativeAuditTool {
		return errNativeAudit
	}
	f, e := privateFile(filepath.Join(m.store, "audit.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_APPEND)
	if e != nil {
		return e
	}
	defer f.Close()
	lockMode := syscall.LOCK_EX
	if nonblocking {
		lockMode |= syscall.LOCK_NB
	}
	if e = syscall.Flock(int(f.Fd()), lockMode); e != nil {
		return errors.New("cannot lock audit")
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	if !namePattern.MatchString(lane) {
		lane = ""
	}
	if !goalPattern.MatchString(goal) {
		goal = ""
	}
	reason := ""
	if len(reasons) > 0 {
		reason = cell(bounded(Redact(reasons[0]), 512))
	}
	b, _ := json.Marshal(map[string]any{"time": time.Now().UTC(), "tool": tool, "lane": lane, "goal_id": goal, "sha256": digest, "result": result, "reason": reason})
	if _, e = f.Write(append(b, '\n')); e != nil {
		return errors.New("cannot append audit")
	}
	if e = f.Sync(); e != nil {
		return errors.New("cannot sync audit")
	}
	return nil
}

// The native mirror uses the existing audit file and cooperating audit lock,
// after manager/control. Reuse A's durable append without its control intent.
func appendNativeAudit(root *os.File, row nativeAuditRow, fault goalCommitIO) error {
	if !validNativeAuditRow(row) {
		return errNativeAudit
	}
	b, _ := json.Marshal(row)
	var fields map[string]any
	if json.Unmarshal(b, &fields) != nil {
		return errNativeAudit
	}
	return appendLaneControlRow(root, fields, &laneControlIO{checkpoint: fault.checkpoint}, "native_mirror")
}
func readWindow(p string, tail bool) ([]byte, error) {
	f, e := privateFile(p, os.O_RDONLY|syscall.O_NONBLOCK)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	if checkLaneControlFile(int(f.Fd())) != nil {
		return nil, errors.New("events must be owned, private and single-link")
	}
	st, e := f.Stat()
	if e != nil {
		return nil, errors.New("cannot stat events")
	}
	const limit = 1 << 20
	offset := int64(0)
	if tail && st.Size() > limit {
		offset = st.Size() - limit
		if _, e = f.Seek(offset, io.SeekStart); e != nil {
			return nil, errors.New("cannot seek events")
		}
	}
	b, e := io.ReadAll(io.LimitReader(f, limit))
	if e != nil {
		return nil, errors.New("cannot read events")
	}
	if offset > 0 {
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			if _, e = f.Seek(0, io.SeekStart); e != nil {
				return nil, errors.New("cannot seek events")
			}
			// Scan through the retained suffix too: a dash-only fragment at the
			// cutoff is public unless later text supplies private marker intent.
			// A marker anywhere still makes this displaced window fail closed.
			if !publicEventPrefix(f, st.Size()) {
				return []byte("[REDACTED]\n"), nil
			}
			after, err := f.Stat()
			if err != nil || after.Size() != st.Size() || !after.ModTime().Equal(st.ModTime()) {
				return []byte("[REDACTED]\n"), nil
			}
			b = b[i+1:]
		} else {
			return nil, nil
		}
	}
	// An unterminated event still carries privacy evidence. Classify it with
	// the complete events before dropping it from the parsing window: an
	// orphan END or fragment can make earlier, otherwise public bodies private.
	// An open block also stays private when its last body event has no newline.
	if len(b) > 0 && b[len(b)-1] != '\n' {
		lines := bytes.Split(b, []byte("\n"))
		if private := privateEventLines(b, lines); private != nil && private[len(private)-1] {
			return []byte("[REDACTED]\n"), nil
		}
	}
	if i := bytes.LastIndexByte(b, '\n'); i >= 0 {
		b = b[:i+1]
	} else {
		b = nil
	}
	return b, nil
}
