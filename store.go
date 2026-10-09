package nuntius

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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
	f, e := os.CreateTemp(dir, ".meta-")
	if e != nil {
		return errors.New("cannot persist metadata")
	}
	name := f.Name()
	defer os.Remove(name)
	b, e := json.Marshal(r)
	if e == nil {
		_, e = f.Write(b)
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e == nil {
		e = os.Rename(name, filepath.Join(dir, "meta.json"))
	}
	if e != nil {
		return errors.New("cannot persist metadata")
	}
	return nil
}
func (m *Manager) latest(lane string) (*Run, error) {
	entries, e := os.ReadDir(filepath.Join(m.store, "runs"))
	if e != nil {
		return nil, errors.New("cannot read run store")
	}
	if len(entries) > 10000 {
		return nil, errors.New("run store exceeds 10000 entries; archive ended runs")
	}
	var latest *Run
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".pending-") && runPattern.MatchString(strings.TrimPrefix(entry.Name(), ".pending-")) {
			continue
		}
		if !runPattern.MatchString(entry.Name()) {
			return nil, errors.New("invalid run store entry")
		}
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil, errors.New("run directory must not be linked")
		}
		f, e := privateFile(filepath.Join(m.store, "runs", entry.Name(), "meta.json"), os.O_RDONLY)
		if e != nil {
			return nil, e
		}
		b, e := io.ReadAll(io.LimitReader(f, 16385))
		f.Close()
		if e != nil || len(b) > 16384 {
			return nil, errors.New("invalid run metadata")
		}
		var r Run
		if strictJSON(b, &r) != nil || r.ID != entry.Name() || !namePattern.MatchString(r.Lane) || !goalPattern.MatchString(r.GoalID) || (r.State != "running" && r.State != "ended" && r.State != "timed_out") || r.Started.IsZero() {
			return nil, errors.New("invalid run metadata")
		}
		if r.Lane == lane && (latest == nil || r.Started.After(latest.Started)) {
			latest = &r
		}
	}
	return latest, nil
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
		r.StopReason = "unknown_exit"
	}
	r.SessionID = m.session(r.ID)
	return m.endRun(r)
}
func (m *Manager) audit(tool, lane, goal, digest, result string) error {
	f, e := privateFile(filepath.Join(m.store, "audit.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_APPEND)
	if e != nil {
		return e
	}
	defer f.Close()
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); e != nil {
		return errors.New("cannot lock audit")
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	if !namePattern.MatchString(lane) {
		lane = ""
	}
	if !goalPattern.MatchString(goal) {
		goal = ""
	}
	b, _ := json.Marshal(map[string]any{"time": time.Now().UTC(), "tool": tool, "lane": lane, "goal_id": goal, "sha256": digest, "result": result})
	if _, e = f.Write(append(b, '\n')); e != nil {
		return errors.New("cannot append audit")
	}
	if e = f.Sync(); e != nil {
		return errors.New("cannot sync audit")
	}
	return nil
}
func readWindow(p string, tail bool) ([]byte, error) {
	f, e := privateFile(p, os.O_RDONLY)
	if e != nil {
		return nil, e
	}
	defer f.Close()
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
			b = b[i+1:]
		} else {
			return nil, nil
		}
	}
	if i := bytes.LastIndexByte(b, '\n'); i >= 0 {
		b = b[:i+1]
	} else {
		b = nil
	}
	return b, nil
}
