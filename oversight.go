package nuntius

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type FreezeState struct {
	Since  time.Time `json:"since"`
	Reason string    `json:"reason"`
}

func (m *Manager) StatusView() (any, error) {
	var controls laneControlSnapshot
	rows, e := m.status(true, &controls)
	if e != nil {
		return nil, e
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	f, e := m.freezeState()
	if e != nil {
		return nil, e
	}
	return map[string]any{"frozen": f != nil, "freeze": f, "lanes": rows, "lane_control_version": 1, "lane_control": controls}, nil
}

func (m *Manager) freezeState() (*FreezeState, error) {
	p := filepath.Join(m.store, "freeze.json")
	if _, e := os.Lstat(p); os.IsNotExist(e) {
		return nil, nil
	}
	f, e := privateFile(p, os.O_RDONLY)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, 16385))
	var v FreezeState
	if e != nil || len(b) > 16384 || strictJSON(b, &v) != nil || v.Since.IsZero() {
		return nil, errors.New("invalid freeze marker")
	}
	v.Reason = bounded(Redact(v.Reason), 12000)
	return &v, nil
}

func (m *Manager) checkFrozen() error {
	f, e := m.freezeState()
	if e != nil {
		return e
	}
	if f != nil {
		return errors.New("frozen by owner since " + f.Since.Format(time.RFC3339))
	}
	return nil
}

func (m *Manager) Freeze(reason string) error {
	if len(reason) < 1 || len(reason) > 12000 {
		return errors.New("reason must contain 1..12000 bytes")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	guard, e := m.controlLock()
	if e != nil {
		return e
	}
	defer guard.Close()
	state := FreezeState{time.Now().UTC(), Redact(reason)}
	b, _ := json.Marshal(state)
	if e = atomicPrivate(filepath.Join(m.store, "freeze.json"), b); e != nil {
		return e
	}
	runs, e := m.history()
	if e != nil {
		return e
	}
	var stopErr error
	for _, r := range runs {
		if r.State == "running" && processExists(r.PID) {
			if e = m.requestStop(&r, "cancelled"); e != nil {
				stopErr = e
			}
		}
	}
	if e = m.audit("nuntius_freeze", "", "", "", "frozen"); e != nil {
		return e
	}
	return stopErr
}

func (m *Manager) Unfreeze() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	guard, e := m.controlLock()
	if e != nil {
		return e
	}
	defer guard.Close()
	if e = os.Remove(filepath.Join(m.store, "freeze.json")); e != nil && !os.IsNotExist(e) {
		return errors.New("cannot remove freeze marker")
	}
	if e = syncDir(m.store); e != nil {
		return e
	}
	return m.audit("nuntius_unfreeze", "", "", "", "unfrozen")
}

type OwnerNote struct {
	Lane   string    `json:"lane,omitempty"`
	ID     string    `json:"id"`
	Time   time.Time `json:"time"`
	Text   string    `json:"text,omitempty"`
	Read   bool      `json:"read"`
	AckIDs []string  `json:"ack_ids,omitempty"`
}

func (m *Manager) appendNote(v OwnerNote) error {
	f, e := privateFile(filepath.Join(m.store, "notes.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_APPEND)
	if e != nil {
		return e
	}
	defer f.Close()
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	_, e = f.Write(append(b, '\n'))
	if e != nil {
		return e
	}
	return f.Sync()
}

func (m *Manager) AddOwnerNote(text string, lanes ...string) error {
	if len(text) < 1 || len(text) > 12000 {
		return errors.New("note must contain 1..12000 bytes")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	lane := ""
	if len(lanes) > 0 && lanes[0] != "" {
		lane = lanes[0]
		if _, err := m.lane(lane); err != nil {
			return err
		}
	}
	guard, e := m.controlLock()
	if e != nil {
		return e
	}
	defer guard.Close()
	id := make([]byte, 16)
	if _, e = rand.Read(id); e != nil {
		return e
	}
	return m.appendNote(OwnerNote{ID: hex.EncodeToString(id), Time: time.Now().UTC(), Text: Redact(text), Lane: lane})
}

func (m *Manager) readNotes() ([]OwnerNote, error) {
	p := filepath.Join(m.store, "notes.jsonl")
	if _, e := os.Lstat(p); os.IsNotExist(e) {
		return []OwnerNote{}, nil
	}
	f, e := privateFile(p, os.O_RDONLY)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	if fi, e := f.Stat(); e != nil || fi.Size() > 16<<20 {
		return nil, errors.New("notes exceed 16 MiB; archive read notes")
	}
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 16384), 65536)
	var notes []OwnerNote
	read := map[string]bool{}
	for scan.Scan() {
		var v OwnerNote
		if strictJSON(scan.Bytes(), &v) != nil {
			return nil, errors.New("invalid owner note record")
		}
		if len(v.AckIDs) > 0 {
			for _, id := range v.AckIDs {
				read[id] = true
			}
		} else {
			v.Text = bounded(Redact(v.Text), 12000)
			notes = append(notes, v)
		}
	}
	if e = scan.Err(); e != nil {
		return nil, e
	}
	for i := range notes {
		notes[i].Read = read[notes[i].ID]
	}
	return notes, nil
}

func (m *Manager) OwnerNotes(since time.Time) ([]OwnerNote, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	guard, e := m.controlLock()
	if e != nil {
		return nil, e
	}
	defer guard.Close()
	notes, e := m.readNotes()
	if e != nil {
		return nil, e
	}
	out := []OwnerNote{}
	for _, n := range notes {
		if n.Time.After(since) {
			out = append(out, n)
			if len(out) == 50 {
				break
			}
		}
	}
	return out, nil
}

func (m *Manager) AckOwnerNotes(ids []string) error {
	if len(ids) < 1 || len(ids) > 50 {
		return errors.New("ack requires 1..50 note ids")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	guard, e := m.controlLock()
	if e != nil {
		return e
	}
	defer guard.Close()
	notes, e := m.readNotes()
	if e != nil {
		return e
	}
	known := map[string]bool{}
	for _, n := range notes {
		known[n.ID] = true
	}
	for _, id := range ids {
		if !known[id] {
			return errors.New("unknown note id")
		}
	}
	return m.appendNote(OwnerNote{Time: time.Now().UTC(), AckIDs: ids})
}

func (m *Manager) history() ([]Run, error) {
	entries, e := os.ReadDir(filepath.Join(m.store, "runs"))
	if e != nil {
		return nil, e
	}
	if len(entries) > 10000 {
		return nil, errors.New("run store exceeds 10000 entries")
	}
	out := []Run{}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".pending-") && runPattern.MatchString(strings.TrimPrefix(entry.Name(), ".pending-")) {
			continue
		}
		if !runPattern.MatchString(entry.Name()) || !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil, errors.New("invalid run directory")
		}
		r, e := loadRun(filepath.Join(m.store, "runs", entry.Name()), entry.Name())
		if e != nil {
			return nil, e
		}
		if r != nil {
			out = append(out, *r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.After(out[j].Started) })
	return out, nil
}

func (m *Manager) pendingMessage(id string) (string, error) {
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		return "", err
	}
	defer root.Close()
	runs, err := heldDirectory(root, "runs")
	if err != nil {
		return "", err
	}
	defer runs.Close()
	dir, err := heldDirectory(runs, id)
	if err != nil {
		return "", err
	}
	defer dir.Close()
	r, err := loadRun(filepath.Join(m.store, "runs", id), id)
	if err != nil || r == nil {
		return "", errLaneControlUnavailable
	}
	receipt, err := readMessageReceipt(dir, *r)
	if err != nil {
		return "", err
	}
	if receipt != nil {
		if receipt.State == "reserved" {
			return "", errors.New("pending_message_requires_review")
		}
		return "", nil
	}
	if _, err := os.Lstat(filepath.Join(m.store, "runs", id, "message-hold.json")); !os.IsNotExist(err) {
		return "", errors.New("pending_message_requires_review")
	}
	p := filepath.Join(m.store, "runs", id, "message.json")
	if _, e := os.Lstat(p); os.IsNotExist(e) {
		return "", nil
	}
	f, e := privateFile(p, os.O_RDONLY)
	if e != nil {
		return "", e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, 16385))
	var v struct {
		Text string `json:"text"`
	}
	if e != nil || len(b) > 16384 || strictJSON(b, &v) != nil {
		return "", errors.New("invalid pending message")
	}
	return v.Text, nil
}

func (m *Manager) SendMessage(lane, text string, stop bool) error {
	if len(text) < 1 || len(text) > 12000 {
		return errors.New("text must contain 1..12000 bytes")
	}
	m.mu.Lock()
	guard, e := m.admissionControlLock()
	if e != nil {
		m.mu.Unlock()
		return e
	}
	if e = m.reload(); e != nil {
		_ = guard.Close()
		m.mu.Unlock()
		return e
	}
	_, e = m.lane(lane)
	if e != nil {
		_ = guard.Close()
		m.mu.Unlock()
		return e
	}
	if e = m.admitLaneLocked(lane); e == nil {
		e = m.checkFrozen()
	}
	if e != nil {
		_ = guard.Close()
		m.mu.Unlock()
		return e
	}
	r, e := m.latest(lane)
	if e == nil && r != nil && r.State == "running" {
		pending, err := m.pendingMessage(r.ID)
		// A reviewed original run cannot accept another message under its
		// terminal receipt. A later child has its own independent message slot.
		_, receiptErr := os.Lstat(filepath.Join(m.store, "runs", r.ID, "message-receipt.json"))
		if err != nil || pending != "" || !os.IsNotExist(receiptErr) {
			e = errors.New("an owner message is already queued")
		} else {
			b, _ := json.Marshal(map[string]string{"text": text})
			e = atomicPrivate(filepath.Join(m.store, "runs", r.ID, "message.json"), b)
			if e == nil && stop {
				e = m.requestStop(r, "cancelled")
			}
		}
		_ = guard.Close()
		m.mu.Unlock()
		return e
	}
	_ = guard.Close()
	m.mu.Unlock()
	if e != nil {
		return e
	}
	_, e = m.Reply(lane, text)
	return e
}
