package nuntius

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const laneControlVersion = 1
const laneControlMaxBytes = 64 * 1024
const laneControlMaxNames = 256

// Revisions cross JSON transports in later steps. Keep integers exact in JS.
const laneControlMaxRevision uint64 = 1<<53 - 1

var errLaneControlUnavailable = errors.New("lane_control_unavailable")
var errLaneControlCapacity = errors.New("lane_control_capacity")

type laneControlRecord struct {
	Paused                bool   `json:"paused"`
	Since                 string `json:"since"`
	Source                string `json:"source"`
	RequestedAfterCurrent bool   `json:"requested_after_current"`
	Revision              uint64 `json:"revision"`
}

type laneControlStore struct {
	Version  int                          `json:"lane_control_version"`
	Revision uint64                       `json:"revision"`
	Lanes    map[string]laneControlRecord `json:"lanes"`
}

type laneControlView struct {
	Name       string `json:"name"`
	Configured bool   `json:"configured"`
	laneControlRecord
	Phase            string   `json:"phase"`
	DispatchEligible bool     `json:"dispatch_eligible"`
	Blockers         []string `json:"blockers"`
	Explanation      string   `json:"explanation"`
}

type laneControlSnapshot struct {
	Version  int               `json:"lane_control_version"`
	Revision uint64            `json:"revision"`
	Lanes    []laneControlView `json:"lanes"`
}

func laneControlUint(value any) (uint64, bool) {
	n, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	i, err := n.Int64()
	return uint64(i), err == nil && i >= 0 && uint64(i) <= laneControlMaxRevision
}

func validLaneControlRecord(r laneControlRecord, revision uint64) bool {
	t, err := time.Parse(time.RFC3339Nano, r.Since)
	return err == nil && !t.IsZero() && t.UTC().Format(time.RFC3339Nano) == r.Since &&
		(r.Source == "cli" || r.Source == "web" || r.Source == "mcp") &&
		r.Revision > 0 && r.Revision <= revision && (!r.RequestedAfterCurrent || r.Paused)
}

// Decode from exact object keys rather than encoding/json's case-insensitive
// struct fields. queueJSON supplies bounded depth and duplicate-key rejection;
// lane controls add a closed schema at every level, with no coercion or nulls.
func decodeLaneControls(b []byte) (laneControlStore, error) {
	fail := func() (laneControlStore, error) { return laneControlStore{}, errLaneControlUnavailable }
	if len(b) > laneControlMaxBytes || !utf8.Valid(b) {
		return fail()
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	raw, err := queueJSON(d, 1)
	if err != nil {
		return fail()
	}
	if _, err = d.Token(); err != io.EOF {
		return fail()
	}
	doc, ok := raw.(map[string]any)
	if !ok || len(doc) != 3 {
		return fail()
	}
	v, ok := doc["lane_control_version"].(json.Number)
	if !ok || v.String() != "1" {
		return fail()
	}
	revision, ok := laneControlUint(doc["revision"])
	if !ok {
		return fail()
	}
	lanes, ok := doc["lanes"].(map[string]any)
	if !ok || len(lanes) > laneControlMaxNames || (len(lanes) > 0 && revision == 0) || (len(lanes) == 0 && revision != 0) {
		return fail()
	}
	s := laneControlStore{Version: laneControlVersion, Revision: revision, Lanes: map[string]laneControlRecord{}}
	latest := uint64(0)
	for name, value := range lanes {
		if !namePattern.MatchString(name) {
			return fail()
		}
		fields, ok := value.(map[string]any)
		if !ok || len(fields) != 5 {
			return fail()
		}
		paused, pok := fields["paused"].(bool)
		since, tok := fields["since"].(string)
		source, sok := fields["source"].(string)
		after, aok := fields["requested_after_current"].(bool)
		rev, rok := laneControlUint(fields["revision"])
		r := laneControlRecord{Paused: paused, Since: since, Source: source, RequestedAfterCurrent: after, Revision: rev}
		if !pok || !tok || !sok || !aok || !rok || !validLaneControlRecord(r, revision) {
			return fail()
		}
		s.Lanes[name] = r
		if rev > latest {
			latest = rev
		}
	}
	if latest != revision {
		return fail()
	}
	return s, nil
}

func encodeLaneControls(s laneControlStore) ([]byte, error) {
	if len(s.Lanes) > laneControlMaxNames {
		return nil, errLaneControlCapacity
	}
	b, err := json.Marshal(s)
	if err != nil {
		return nil, errLaneControlUnavailable
	}
	if _, err = decodeLaneControls(b); err != nil {
		return nil, err
	}
	return b, nil
}

// Open each directory relative to the preceding descriptor. O_NOFOLLOW on
// every component prevents both linked ancestors and check/open races. Only
// the selected store itself must be private; its parents may be shared.
func openLaneControlRoot(path string) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return nil, errLaneControlUnavailable
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errLaneControlUnavailable
	}
	for _, component := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		next, e := unix.Openat(fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if e != nil {
			return nil, errLaneControlUnavailable
		}
		fd = next
	}
	var st unix.Stat_t
	if err = unix.Fstat(fd, &st); err != nil || st.Mode&07777 != 0700 || st.Uid != uint32(os.Geteuid()) {
		unix.Close(fd)
		return nil, errLaneControlUnavailable
	}
	return os.NewFile(uintptr(fd), "lane-control-root"), nil
}

// A missing state file is distinct from a missing root or a dangling link.
// O_NONBLOCK ensures a FIFO cannot hang before the regular-file check.
func readLaneControlFile(root *os.File) (laneControlStore, error) {
	fd, err := unix.Openat(int(root.Fd()), "lane-control.json", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return laneControlStore{Version: 1, Lanes: map[string]laneControlRecord{}}, nil
	}
	if err != nil {
		return laneControlStore{}, errLaneControlUnavailable
	}
	f := os.NewFile(uintptr(fd), "lane-control-state")
	defer f.Close()
	var st unix.Stat_t
	if err = unix.Fstat(fd, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&07777 != 0600 || st.Nlink != 1 || st.Uid != uint32(os.Geteuid()) || st.Size > laneControlMaxBytes {
		return laneControlStore{}, errLaneControlUnavailable
	}
	b, err := io.ReadAll(io.LimitReader(f, laneControlMaxBytes+1))
	if err != nil {
		return laneControlStore{}, errLaneControlUnavailable
	}
	return decodeLaneControls(b)
}

// Caller already holds m.mu and the shared control lock, in that order.
// This method does not reacquire either. A02 will reconcile intent on startup
// or mutation; reads only detect it and keep admission closed, even if the
// state file is missing or records an enabled lane.
func (m *Manager) readLaneControlsLocked() (laneControlStore, error) {
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		return laneControlStore{}, err
	}
	defer root.Close()
	return readLaneControlsAt(root)
}

func readLaneControlsAt(root *os.File) (laneControlStore, error) {
	var st unix.Stat_t
	err := unix.Fstatat(int(root.Fd()), "lane-control-intent.json", &st, unix.AT_SYMLINK_NOFOLLOW)
	if !errors.Is(err, unix.ENOENT) {
		return laneControlStore{}, errLaneControlUnavailable
	}
	return readLaneControlFile(root)
}

// Internal, read-only configured-plus-retained projection. No GET or public
// surface calls this until the later admission/surface goals are complete.
func (m *Manager) laneControls() (laneControlSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Validate the root before the legacy lock helper can follow an ancestor.
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		return laneControlSnapshot{}, err
	}
	defer root.Close()
	lock, err := laneControlReadLock(root)
	if err != nil {
		return laneControlSnapshot{}, err
	}
	defer lock.Close()
	if err := m.reload(); err != nil {
		return laneControlSnapshot{}, err
	}
	for _, lane := range m.config.Lanes {
		if !namePattern.MatchString(lane.Name) {
			return laneControlSnapshot{}, errLaneControlUnavailable
		}
	}
	s, err := readLaneControlsAt(root)
	if err != nil {
		return laneControlSnapshot{}, err
	}
	runs, err := m.history()
	if os.IsNotExist(err) {
		runs, err = []Run{}, nil
	}
	if err != nil {
		return laneControlSnapshot{}, err
	}
	return m.projectLaneControls(s, root, runs, true), nil
}

// Called under the manager/common control locks; intent is observed, never repaired.
func (m *Manager) laneControlSnapshotLocked(runs []Run, locked bool) laneControlSnapshot {
	if locked {
		if root, err := openLaneControlRoot(m.store); err == nil {
			defer root.Close()
			if s, err := readLaneControlsAt(root); err == nil {
				return m.projectLaneControls(s, root, runs, true)
			}
		}
	}
	return m.projectLaneControls(laneControlStore{Version: 1}, nil, runs, false)
}

func (m *Manager) projectLaneControls(s laneControlStore, root *os.File, runs []Run, available bool) laneControlSnapshot {
	views := map[string]laneControlView{}
	for name, r := range s.Lanes {
		views[name] = laneControlView{Name: name, laneControlRecord: r}
	}
	for _, lane := range m.config.Lanes {
		r := views[lane.Name]
		r.Name = lane.Name
		r.Configured = true
		views[lane.Name] = r
	}
	out := laneControlSnapshot{Version: 1, Revision: s.Revision, Lanes: []laneControlView{}}
	frozen, freezeErr := m.freezeState()
	for _, r := range views {
		r.Phase = "enabled"
		r.Blockers = []string{}
		if !available {
			r.Phase = "unavailable"
			r.Blockers = append(r.Blockers, "lane_control_unavailable")
		}
		if !r.Configured {
			r.Phase = "unavailable"
			r.Blockers = append(r.Blockers, "not_configured")
		}
		if r.Paused {
			r.Blockers = append(r.Blockers, "lane_paused")
			if available && r.Configured {
				r.Phase = "paused"
			}
		}
		if freezeErr != nil {
			r.Blockers = append(r.Blockers, "freeze_unavailable")
		} else if frozen != nil {
			r.Blockers = append(r.Blockers, "frozen")
		}
		for _, run := range runs {
			if run.Lane != r.Name {
				continue
			}
			if run.State == "running" {
				r.Blockers = append(r.Blockers, "running")
				if r.Paused && available && r.Configured {
					r.Phase = "draining"
				}
			}
			if run.WaitingToResume {
				r.Blockers = append(r.Blockers, "waiting_to_resume")
			}
			break
		}
		if root != nil {
			held, err := laneHasHeldMessage(root, r.Name)
			if err != nil {
				r.Blockers = append(r.Blockers, "held_message_unavailable")
			} else if held {
				r.Blockers = append(r.Blockers, "held_message")
			}
		}
		r.DispatchEligible = len(r.Blockers) == 0
		r.Explanation = laneControlExplanation(r)
		out.Lanes = append(out.Lanes, r)
	}
	sort.Slice(out.Lanes, func(i, j int) bool { return out.Lanes[i].Name < out.Lanes[j].Name })
	return out
}

func laneControlExplanation(v laneControlView) string {
	parts := []string{}
	if v.Paused {
		when := "an unknown time"
		if t, err := time.Parse(time.RFC3339Nano, v.Since); err == nil {
			when = t.Local().Format("15:04")
		}
		source := map[string]string{"cli": "CLI", "web": "dashboard", "mcp": "judge MCP"}[v.Source]
		parts = append(parts, "Paused by you at "+when+" via "+source)
		if v.Phase == "draining" {
			parts = append(parts, "current run may finish")
		}
	} else if slices.Contains(v.Blockers, "lane_control_unavailable") {
		parts = append(parts, "Lane hold is unknown")
	} else {
		parts = append(parts, "Lane hold is clear")
	}
	for _, b := range v.Blockers {
		switch b {
		case "lane_control_unavailable":
			parts = append(parts, "lane controls cannot be read; new work is blocked")
		case "not_configured":
			parts = append(parts, "retained name is no longer configured; resume does not recreate it")
		case "frozen":
			parts = append(parts, "all lanes are still frozen")
		case "freeze_unavailable":
			parts = append(parts, "freeze state cannot be read; new work is blocked")
		case "running":
			if v.Phase != "draining" {
				parts = append(parts, "current run is still running")
			}
		case "waiting_to_resume":
			parts = append(parts, "interrupted work is waiting for a separate recovery check")
		case "held_message":
			parts = append(parts, "an owner message is held for explicit review")
		case "held_message_unavailable":
			parts = append(parts, "message reservations cannot be read; new work is blocked")
		}
	}
	return strings.Join(parts, "; ")
}

// NewManager creates locks/. Use the same lock inode as existing mutations,
// opened relative to the verified root; never call the legacy path opener.
// Creating this private coordination file does not create control state.
func laneControlReadLock(root *os.File) (*os.File, error) {
	lock, err := openLaneControlLock(root, unix.LOCK_SH, false)
	if err != nil {
		return nil, errLaneControlUnavailable
	}
	return lock, nil
}

func laneControlLock(root *os.File, mode int) (*os.File, error) {
	return openLaneControlLock(root, mode, true)
}

// Admission opens the permanent coordination inode, never creates it. Startup
// and control mutations prepare it using laneControlLock. The caller holds the
// manager mutex first. No lane lifetime lock is acquired here.
func (m *Manager) admissionControlLock() (*os.File, error) {
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		return nil, errLaneControlUnavailable
	}
	defer root.Close()
	lock, err := openLaneControlLock(root, unix.LOCK_EX, false)
	if err != nil {
		return nil, errLaneControlUnavailable
	}
	return lock, nil
}

func openLaneControlLock(root *os.File, mode int, create bool) (*os.File, error) {
	lock, err := openLaneControlLockFile(root, create)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(lock.Fd()), mode|unix.LOCK_NB); err != nil {
		lock.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, &laneControlFault{Code: "locked"}
		}
		return nil, errLaneControlUnavailable
	}
	return lock, nil
}

func openLaneControlLockFile(root *os.File, create bool) (*os.File, error) {
	fd, err := unix.Openat(int(root.Fd()), "locks", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errLaneControlUnavailable
	}
	dir := os.NewFile(uintptr(fd), "lane-control-locks")
	defer dir.Close()
	var st unix.Stat_t
	if err = unix.Fstat(fd, &st); err != nil || st.Mode&07777 != 0700 || st.Uid != uint32(os.Geteuid()) {
		return nil, errLaneControlUnavailable
	}
	flags := unix.O_RDWR | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC
	if create {
		flags |= unix.O_CREAT
	}
	lfd, err := unix.Openat(fd, "control.lock", flags, 0600)
	if err != nil {
		return nil, errLaneControlUnavailable
	}
	lock := os.NewFile(uintptr(lfd), "lane-control-lock")
	if err = unix.Fstat(lfd, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&07777 != 0600 || st.Nlink != 1 || st.Uid != uint32(os.Geteuid()) {
		lock.Close()
		return nil, errLaneControlUnavailable
	}
	return lock, nil
}

// laneAdmissionFault stays internal until the later adapter steps. It carries
// only fixed safe codes; no prompt, session or store path belongs in a refusal.
type laneAdmissionFault struct {
	Code      string   `json:"code"`
	Lane      string   `json:"lane"`
	Paused    bool     `json:"paused"`
	Retryable bool     `json:"retryable"`
	Blockers  []string `json:"blockers"`
}

func (e *laneAdmissionFault) Error() string { return e.Code }

// admitLaneLocked is the final H2 boundary. Both the manager mutex and common
// control lock must already be held; never call a lock-acquiring snapshot or
// intent reconciliation here. Every launch (including follow/Recover) uses it
// before allocating an ID, prompt, run directory or child.
func (m *Manager) admitLaneLocked(name string) error {
	return m.admitLaneExceptLocked(name, "")
}

// The sole exemption is an original reservation validated under these same
// locks by the internal message protocol. Ordinary callers pass no exemption.
func (m *Manager) admitLaneExceptLocked(name, reviewedRun string) error {
	if _, err := m.lane(name); err != nil {
		return err
	}
	// Some established successor callers acquired the common inode through
	// the legacy helper. Validate coordination health without another flock,
	// even when their held descriptor outlives a missing or unsafe path.
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		return errLaneControlUnavailable
	}
	coordination, err := openLaneControlLockFile(root, false)
	root.Close()
	if err != nil {
		return errLaneControlUnavailable
	}
	coordination.Close()
	controls, err := m.readLaneControlsLocked()
	if err != nil {
		return errLaneControlUnavailable
	}
	if controls.Lanes[name].Paused {
		blockers := []string{"lane_paused"}
		frozen, err := m.freezeState()
		if err != nil {
			blockers = append(blockers, "freeze_unavailable")
		} else if frozen != nil {
			blockers = append(blockers, "frozen")
		}
		return &laneAdmissionFault{Code: "lane_paused", Lane: name, Paused: true, Blockers: blockers}
	}
	root, err = openLaneControlRoot(m.store)
	if err != nil {
		return errLaneControlUnavailable
	}
	var held bool
	if reviewedRun == "" {
		held, err = laneHasHeldMessage(root, name)
	} else {
		held, err = laneHasHeldMessageExcept(root, name, reviewedRun)
	}
	root.Close()
	if err != nil {
		return errLaneControlUnavailable
	}
	if held {
		return errors.New("pending_message_requires_review")
	}
	return nil
}

// Internal adapter contract. Source is supplied by the trusted transport,
// separately from decoded request arguments. CLI reads its revision in-lock.
type laneControlChange struct {
	Lane             string
	Paused           bool
	AfterCurrent     bool
	ExpectedRevision *uint64
}

type laneControlResult struct {
	Code     string
	Revision uint64
	Paused   bool
	Changed  bool
	Origin   string
	// False on every error: visible state under an unresolved intent is not
	// an admission snapshot. Adapters must report unavailable, never enabled.
	Available bool
}

type laneControlFault struct{ Code string }

func (e *laneControlFault) Error() string { return e.Code }

// checkpoint injects an error before or after a real filesystem operation.
// It is per invocation, never global; production always passes nil.
type laneControlIO struct{ checkpoint func(string) error }

func (m *Manager) setLaneControl(change laneControlChange, source string) (laneControlResult, error) {
	return m.setLaneControlIO(change, source, nil)
}

func (m *Manager) setLaneControlIO(change laneControlChange, source string, ops *laneControlIO) (laneControlResult, error) {
	if !validLaneControlSource(source) || !namePattern.MatchString(change.Lane) ||
		(!change.Paused && change.AfterCurrent) ||
		(source != "cli" && (change.ExpectedRevision == nil || *change.ExpectedRevision > laneControlMaxRevision)) {
		return laneControlResult{}, &laneControlFault{Code: "invalid_request"}
	}
	if !m.mu.TryLock() {
		return laneControlResult{}, &laneControlFault{Code: "locked"}
	}
	defer m.mu.Unlock()
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		return laneControlResult{}, &laneControlFault{Code: "lane_control_unavailable"}
	}
	defer root.Close()
	guard, err := laneControlLock(root, unix.LOCK_EX)
	if err != nil {
		return laneControlResult{}, laneControlError(err)
	}
	defer guard.Close()
	return m.setLaneControlLocked(root, change, source, ops)
}

func (m *Manager) recoverLaneControls(ops *laneControlIO) error {
	if !m.mu.TryLock() {
		return &laneControlFault{Code: "locked"}
	}
	defer m.mu.Unlock()
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		return laneControlError(err)
	}
	defer root.Close()
	guard, err := laneControlLock(root, unix.LOCK_EX)
	if err != nil {
		return laneControlError(err)
	}
	defer guard.Close()
	if err = reconcileLaneControl(root, ops); err != nil {
		return &laneControlFault{Code: "audit_unavailable"}
	}
	return nil
}

func validLaneControlSource(source string) bool {
	return source == "cli" || source == "web" || source == "mcp"
}

func laneControlError(err error) error {
	var fault *laneControlFault
	if errors.As(err, &fault) {
		return fault
	}
	return &laneControlFault{Code: "lane_control_unavailable"}
}

func laneControlCheckpoint(ops *laneControlIO, stage string) error {
	if ops != nil && ops.checkpoint != nil {
		return ops.checkpoint(stage)
	}
	return nil
}

// The caller already owns manager -> shared control lock. This primitive
// never acquires either lock and never dispatches, recovers or signals a run.
func (m *Manager) setLaneControlLocked(root *os.File, change laneControlChange, source string, ops *laneControlIO) (laneControlResult, error) {
	if !validLaneControlSource(source) || !namePattern.MatchString(change.Lane) ||
		(!change.Paused && change.AfterCurrent) ||
		(source != "cli" && (change.ExpectedRevision == nil || *change.ExpectedRevision > laneControlMaxRevision)) {
		return laneControlResult{}, &laneControlFault{Code: "invalid_request"}
	}
	if err := reconcileLaneControl(root, ops); err != nil {
		return laneControlResult{}, &laneControlFault{Code: "audit_unavailable"}
	}
	if err := m.reload(); err != nil {
		return laneControlResult{}, &laneControlFault{Code: "lane_control_unavailable"}
	}
	configured := false
	for _, lane := range m.config.Lanes {
		if lane.Name == change.Lane {
			configured = true
			break
		}
	}
	previous, err := readLaneControlFile(root)
	if err != nil {
		return laneControlResult{}, laneControlError(err)
	}
	old, exists := previous.Lanes[change.Lane]
	// A retained hold can be cleared without growing a pool or recreating a
	// configured lane. New holds require a current configured exact name.
	if !configured {
		code := "lane_not_found"
		if exists && change.Paused {
			code = "lane_not_configured"
		}
		if !exists || change.Paused {
			return laneControlResult{}, &laneControlFault{Code: code}
		}
	}
	result := laneControlResult{Revision: previous.Revision, Paused: old.Paused}
	refuse := func(code string) (laneControlResult, error) {
		// Refusal audit is best effort; its failure cannot replace the primary
		// refusal or open admission. No caller text enters this row.
		_ = appendLaneControlRow(root, map[string]any{"time": time.Now().UTC(), "tool": "lane_control", "lane": change.Lane, "source": source, "revision": previous.Revision, "result": code}, nil, "audit.refusal")
		return result, &laneControlFault{Code: code}
	}
	if source != "cli" && *change.ExpectedRevision != previous.Revision {
		return refuse("revision_conflict")
	}
	if old.Paused == change.Paused {
		result.Code = "unchanged"
		result.Available = true
		return result, nil
	}
	if previous.Revision == laneControlMaxRevision {
		return refuse("lane_control_unavailable")
	}
	if !exists && len(previous.Lanes) == laneControlMaxNames {
		return refuse("lane_control_capacity")
	}
	next := laneControlStore{Version: 1, Revision: previous.Revision + 1, Lanes: make(map[string]laneControlRecord, len(previous.Lanes)+1)}
	for name, record := range previous.Lanes {
		next.Lanes[name] = record
	}
	record := laneControlRecord{Paused: change.Paused, RequestedAfterCurrent: change.AfterCurrent, Source: source, Since: time.Now().UTC().Format(time.RFC3339Nano), Revision: next.Revision}
	next.Lanes[change.Lane] = record
	beforeBytes, _ := encodeLaneControls(previous)
	afterBytes, err := encodeLaneControls(next)
	if err != nil {
		return refuse("lane_control_capacity")
	}
	intent := laneControlIntent{Version: 1, Time: record.Since, Origin: "lane-control:" + rand.Text(), Lane: change.Lane, Source: source, Before: old.Paused, After: record, BeforeRevision: previous.Revision, Revision: next.Revision, BeforeDigest: laneControlDigest(beforeBytes), AfterDigest: laneControlDigest(afterBytes), PrepareHeldMessage: change.Paused}
	b, err := json.Marshal(intent)
	if err != nil {
		return result, &laneControlFault{Code: "audit_unavailable"}
	}
	if err = replaceLaneControl(root, "lane-control-intent.json", b, ops, "intent"); err != nil {
		return result, &laneControlFault{Code: "audit_unavailable"}
	}
	if err = appendLaneControlAudit(root, intent, "intent", ops); err != nil {
		return result, &laneControlFault{Code: "audit_unavailable"}
	}
	if err = replaceLaneControl(root, "lane-control.json", afterBytes, ops, "state"); err != nil {
		// The old or the new state may be visible. In both cases the intent
		// remains; readLaneControlsLocked refuses until durable readback.
		return result, &laneControlFault{Code: "save_unconfirmed"}
	}
	result = laneControlResult{Revision: next.Revision, Paused: record.Paused, Origin: intent.Origin}
	if err = reconcileLaneControl(root, ops); err != nil {
		return result, &laneControlFault{Code: "committed_audit_failed"}
	}
	result.Code, result.Changed = "committed", true
	result.Available = true
	return result, nil
}

// Digests bind the entire canonical old/new snapshot, including unrelated
// lanes. A third state is uncertain, never inferred from only the lane value.
type laneControlIntent struct {
	Version            int               `json:"version"`
	Time               string            `json:"time"`
	Origin             string            `json:"origin"`
	Lane               string            `json:"lane"`
	Source             string            `json:"source"`
	Before             bool              `json:"before"`
	After              laneControlRecord `json:"after"`
	BeforeRevision     uint64            `json:"before_revision"`
	Revision           uint64            `json:"revision"`
	BeforeDigest       string            `json:"before_sha256"`
	AfterDigest        string            `json:"after_sha256"`
	PrepareHeldMessage bool              `json:"prepare_held_message"`
}

func laneControlDigest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func validLaneControlOrigin(origin string) bool {
	if !strings.HasPrefix(origin, "lane-control:") || len(origin) != len("lane-control:")+26 {
		return false
	}
	for _, c := range strings.TrimPrefix(origin, "lane-control:") {
		if !(c >= 'A' && c <= 'Z') && !(c >= '2' && c <= '7') {
			return false
		}
	}
	return true
}

// Decode via the same duplicate-key, case-sensitive, closed JSON contract as
// state. Re-marshalling a typed value must preserve every supplied field.
func decodeLaneControlObject(b []byte, target any) error {
	if len(b) > 4096 || !utf8.Valid(b) {
		return errLaneControlUnavailable
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	raw, err := queueJSON(d, 1)
	if err != nil {
		return errLaneControlUnavailable
	}
	if _, err = d.Token(); err != io.EOF {
		return errLaneControlUnavailable
	}
	if json.Unmarshal(b, target) != nil {
		return errLaneControlUnavailable
	}
	canonical, err := json.Marshal(target)
	if err != nil {
		return errLaneControlUnavailable
	}
	check := json.NewDecoder(bytes.NewReader(canonical))
	check.UseNumber()
	want, err := queueJSON(check, 1)
	actual, _ := json.Marshal(raw)
	expected, _ := json.Marshal(want)
	if err != nil || !bytes.Equal(actual, expected) {
		return errLaneControlUnavailable
	}
	return nil
}

func validLaneControlIntent(i laneControlIntent) bool {
	before, e1 := hex.DecodeString(i.BeforeDigest)
	after, e2 := hex.DecodeString(i.AfterDigest)
	return i.Version == 1 && namePattern.MatchString(i.Lane) && validLaneControlOrigin(i.Origin) &&
		validLaneControlSource(i.Source) && i.BeforeRevision < laneControlMaxRevision && i.Revision == i.BeforeRevision+1 &&
		i.After.Revision == i.Revision && validLaneControlRecord(i.After, i.Revision) &&
		i.Time == i.After.Since && i.Source == i.After.Source && i.Before != i.After.Paused &&
		i.PrepareHeldMessage == i.After.Paused && e1 == nil && e2 == nil && len(before) == 32 && len(after) == 32 &&
		i.BeforeDigest != i.AfterDigest && hex.EncodeToString(before) == i.BeforeDigest && hex.EncodeToString(after) == i.AfterDigest
}

func reconcileLaneControl(root *os.File, ops *laneControlIO) error {
	b, err := readLaneControlPrivate(root, "lane-control-intent.json", 4096)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var intent laneControlIntent
	if decodeLaneControlObject(b, &intent) != nil || !validLaneControlIntent(intent) {
		return errLaneControlUnavailable
	}
	s, err := readLaneControlFile(root)
	if err != nil {
		return err
	}
	state, err := encodeLaneControls(s)
	if err != nil {
		return err
	}
	result := "uncertain"
	switch laneControlDigest(state) {
	case intent.AfterDigest:
		if s.Revision != intent.Revision || s.Lanes[intent.Lane] != intent.After {
			return errLaneControlUnavailable
		}
		// A visible rename from a failed save is not yet a durability claim.
		// Re-sync both the read-back file and its directory before outcome.
		if err = syncLaneControlState(root, ops); err != nil {
			return err
		}
		if intent.PrepareHeldMessage {
			if err = prepareLaneControlHold(root, intent, ops); err != nil {
				return err
			}
		}
		result = "committed"
	case intent.BeforeDigest:
		if s.Revision != intent.BeforeRevision {
			return errLaneControlUnavailable
		}
		result = "not_committed"
	}
	if err = appendLaneControlAudit(root, intent, result, ops); err != nil {
		return err
	}
	if result == "uncertain" {
		return errLaneControlUnavailable
	}
	if err = laneControlCheckpoint(ops, "intent.cleanup"); err != nil {
		return err
	}
	if err = unix.Unlinkat(int(root.Fd()), "lane-control-intent.json", 0); err != nil {
		return errLaneControlUnavailable
	}
	if err = root.Sync(); err != nil {
		// Keep visible intent on cleanup uncertainty as well. An interrupted
		// removal may replay the same outcome with the same origin after boot.
		_ = replaceLaneControl(root, "lane-control-intent.json", b, nil, "intent")
		return errLaneControlUnavailable
	}
	return nil
}

// A04 consumes this durable repair obligation only after marking accepted
// messages held for review. Resume never removes it. Repeated pauses retain
// the earliest boundary and extend the revision high-water mark; no message
// text or run/session identity enters the control intent or public audit.
type laneControlHold struct {
	Version         int    `json:"version"`
	Lane            string `json:"lane"`
	Since           string `json:"since"`
	ThroughRevision uint64 `json:"through_revision"`
	Origin          string `json:"origin"`
}

func prepareLaneControlHold(root *os.File, intent laneControlIntent, ops *laneControlIO) error {
	name := "lane-control-hold-" + intent.Lane + ".json"
	hold := laneControlHold{Version: 1, Lane: intent.Lane, Since: intent.Time, ThroughRevision: intent.Revision, Origin: intent.Origin}
	b, err := readLaneControlPrivate(root, name, 4096)
	if err == nil {
		var old laneControlHold
		if decodeLaneControlObject(b, &old) != nil || old.Version != 1 || old.Lane != intent.Lane || !validLaneControlOrigin(old.Origin) || old.ThroughRevision == 0 || old.ThroughRevision > intent.Revision {
			return errLaneControlUnavailable
		}
		t, e := time.Parse(time.RFC3339Nano, old.Since)
		if e != nil || t.IsZero() || t.UTC().Format(time.RFC3339Nano) != old.Since {
			return errLaneControlUnavailable
		}
		hold.Since, hold.Origin = old.Since, old.Origin
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	b, err = json.Marshal(hold)
	if err != nil {
		return err
	}
	if err = replaceLaneControl(root, name, b, ops, "hold"); err != nil {
		return err
	}
	return latchLaneMessages(root, intent, ops)
}

func readLaneControlPrivate(root *os.File, name string, limit int64) ([]byte, error) {
	fd, err := unix.Openat(int(root.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, errLaneControlUnavailable
	}
	f := os.NewFile(uintptr(fd), "lane-control-private")
	defer f.Close()
	if err = checkLaneControlFile(fd); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, errLaneControlUnavailable
	}
	return b, nil
}

func checkLaneControlFile(fd int) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&07777 != 0600 || st.Nlink != 1 || st.Uid != uint32(os.Geteuid()) {
		return errLaneControlUnavailable
	}
	return nil
}

func syncLaneControlState(root *os.File, ops *laneControlIO) error {
	fd, err := unix.Openat(int(root.Fd()), "lane-control.json", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return errLaneControlUnavailable
	}
	f := os.NewFile(uintptr(fd), "lane-control-state")
	defer f.Close()
	if err = checkLaneControlFile(fd); err != nil {
		return err
	}
	if err = laneControlCheckpoint(ops, "reconcile.state.fsync"); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = laneControlCheckpoint(ops, "reconcile.state.dirsync"); err != nil {
		return err
	}
	return root.Sync()
}

func appendLaneControlAudit(root *os.File, intent laneControlIntent, result string, ops *laneControlIO) error {
	tool := "lane_resume"
	if intent.After.Paused {
		tool = "lane_pause"
	}
	stage := "audit.outcome"
	if result == "intent" {
		stage = "audit.intent"
	}
	return appendLaneControlRow(root, map[string]any{"time": intent.Time, "tool": tool, "lane": intent.Lane, "source": intent.Source, "before": intent.Before, "after": intent.After.Paused, "after_current": intent.After.RequestedAfterCurrent, "revision": intent.Revision, "origin": intent.Origin, "result": result, "reason": "lane_control"}, ops, stage)
}

func appendLaneControlRow(root *os.File, row map[string]any, ops *laneControlIO, stage string) error {
	fd, err := unix.Openat(int(root.Fd()), "audit.jsonl", unix.O_RDWR|unix.O_CREAT|unix.O_APPEND|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0600)
	if err != nil {
		return errLaneControlUnavailable
	}
	f := os.NewFile(uintptr(fd), "lane-control-audit")
	defer f.Close()
	if err = checkLaneControlFile(fd); err != nil {
		return err
	}
	if err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return errLaneControlUnavailable
	}
	defer unix.Flock(fd, unix.LOCK_UN)
	// An interrupted append can leave a prefix. Do not concatenate another
	// row onto it or rewrite historical bytes; preserve intent and fail closed.
	var st unix.Stat_t
	if err = unix.Fstat(fd, &st); err != nil {
		return errLaneControlUnavailable
	}
	if st.Size > 0 {
		var last [1]byte
		if n, e := f.ReadAt(last[:], st.Size-1); e != nil || n != 1 || last[0] != '\n' {
			return errLaneControlUnavailable
		}
	}
	b, err := json.Marshal(row)
	if err != nil {
		return err
	}
	if err = laneControlCheckpoint(ops, stage+".append"); err != nil {
		return err
	}
	b = append(b, '\n')
	n, err := f.Write(b)
	if err != nil || n != len(b) {
		return errLaneControlUnavailable
	}
	if err = laneControlCheckpoint(ops, stage+".fsync"); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = laneControlCheckpoint(ops, stage+".dirsync"); err != nil {
		return err
	}
	return root.Sync()
}

func replaceLaneControl(root *os.File, target string, b []byte, ops *laneControlIO, stage string) error {
	// Refuse unsafe existing targets before any replacement. All names here
	// are fixed or formed from validated exact lane names, never caller paths.
	fd, err := unix.Openat(int(root.Fd()), target, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err == nil {
		err = checkLaneControlFile(fd)
		unix.Close(fd)
		if err != nil {
			return err
		}
	} else if !errors.Is(err, unix.ENOENT) {
		return errLaneControlUnavailable
	}
	return replaceLaneControlBytes(root, target, b, ops, stage)
}

func replaceLaneControlBytes(root *os.File, target string, b []byte, ops *laneControlIO, stage string) error {
	name := ".lane-control-" + rand.Text()
	fd, err := unix.Openat(int(root.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return errLaneControlUnavailable
	}
	f := os.NewFile(uintptr(fd), "lane-control-temporary")
	defer unix.Unlinkat(int(root.Fd()), name, 0)
	if err = f.Chmod(0600); err == nil {
		err = laneControlCheckpoint(ops, stage+".write")
	}
	if err == nil {
		var n int
		n, err = f.Write(b)
		if err == nil && n != len(b) {
			err = io.ErrShortWrite
		}
	}
	if err == nil {
		err = laneControlCheckpoint(ops, stage+".fsync")
	}
	if err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err == nil {
		err = ce
	}
	if err != nil {
		return err
	}
	if err = laneControlCheckpoint(ops, stage+".rename"); err != nil {
		return err
	}
	if err = unix.Renameat(int(root.Fd()), name, int(root.Fd()), target); err != nil {
		return err
	}
	if err = laneControlCheckpoint(ops, stage+".renamed"); err != nil {
		return err
	}
	if err = laneControlCheckpoint(ops, stage+".dirsync"); err != nil {
		return err
	}
	return root.Sync()
}

// Storage-only primitive for A02's transaction, not a pause/resume API.
// Caller holds manager mutex -> control lock. Intent is deliberately not
// reconciled here: A02 must durably prepare it before calling this writer.
func (m *Manager) saveLaneControlsLocked(next laneControlStore) error {
	b, err := encodeLaneControls(next)
	if err != nil {
		return err
	}
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		return err
	}
	defer root.Close()
	previous, err := readLaneControlFile(root)
	if err != nil {
		return err
	}
	if previous.Revision == laneControlMaxRevision || next.Revision != previous.Revision+1 {
		return errLaneControlUnavailable
	}
	changed := false
	for name, old := range previous.Lanes {
		r, exists := next.Lanes[name]
		if !exists || (r != old && r.Revision != next.Revision) {
			return errLaneControlUnavailable
		}
		if r != old {
			changed = true
		}
	}
	for name, r := range next.Lanes {
		if _, exists := previous.Lanes[name]; !exists {
			if r.Revision != next.Revision {
				return errLaneControlUnavailable
			}
			changed = true
		}
	}
	if !changed {
		return errLaneControlUnavailable
	}
	return writeLaneControlFile(root, b)
}

func writeLaneControlFile(root *os.File, b []byte) error {
	if err := replaceLaneControl(root, "lane-control.json", b, nil, "state"); err != nil {
		return errLaneControlUnavailable
	}
	return nil
}
