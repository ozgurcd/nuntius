package nuntius

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
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
	Name       string
	Configured bool
	laneControlRecord
}

type laneControlSnapshot struct {
	Version  int
	Revision uint64
	Lanes    []laneControlView
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
	s, err := readLaneControlsAt(root)
	if err != nil {
		return laneControlSnapshot{}, err
	}
	views := map[string]laneControlView{}
	for name, r := range s.Lanes {
		views[name] = laneControlView{Name: name, laneControlRecord: r}
	}
	for _, lane := range m.config.Lanes {
		if !namePattern.MatchString(lane.Name) {
			return laneControlSnapshot{}, errLaneControlUnavailable
		}
		r := views[lane.Name]
		r.Name = lane.Name
		r.Configured = true
		views[lane.Name] = r
	}
	out := laneControlSnapshot{Version: 1, Revision: s.Revision, Lanes: []laneControlView{}}
	for _, r := range views {
		out.Lanes = append(out.Lanes, r)
	}
	sort.Slice(out.Lanes, func(i, j int) bool { return out.Lanes[i].Name < out.Lanes[j].Name })
	return out, nil
}

// NewManager creates locks/. Use the same lock inode as existing mutations,
// opened relative to the verified root; never call the legacy path opener.
// Creating this private coordination file does not create control state.
func laneControlReadLock(root *os.File) (*os.File, error) {
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
	lfd, err := unix.Openat(fd, "control.lock", unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, errLaneControlUnavailable
	}
	lock := os.NewFile(uintptr(lfd), "lane-control-lock")
	if err = unix.Fstat(lfd, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&07777 != 0600 || st.Nlink != 1 || st.Uid != uint32(os.Geteuid()) {
		lock.Close()
		return nil, errLaneControlUnavailable
	}
	if err = unix.Flock(lfd, unix.LOCK_SH|unix.LOCK_NB); err != nil {
		lock.Close()
		return nil, errLaneControlUnavailable
	}
	return lock, nil
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
	// CreateTemp's path is not used for I/O. Create a unique rooted file with
	// O_EXCL instead; the name is independent of caller input and store paths.
	name := ".lane-control-" + rand.Text()
	fd, err := unix.Openat(int(root.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return errLaneControlUnavailable
	}
	f := os.NewFile(uintptr(fd), "lane-control-temporary")
	defer unix.Unlinkat(int(root.Fd()), name, 0)
	// Set exact private permissions even under a restrictive inherited umask.
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err == nil {
		err = ce
	}
	if err != nil {
		return errLaneControlUnavailable
	}
	if err = unix.Renameat(int(root.Fd()), name, int(root.Fd()), "lane-control.json"); err != nil {
		return errLaneControlUnavailable
	}
	if err = root.Sync(); err != nil {
		return errLaneControlUnavailable
	}
	return nil
}
