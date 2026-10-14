package nuntius

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

const idleGuardJournal = "idle-guard.journal"
const idleGuardMaxBytes = 16 << 20
const idleGuardMaxFrames = 20160 // Bounded; never evict monitoring history to turn health green.
const idleGuardMaxFrame = 256 << 10

var errIdleGuard = errors.New("idle monitoring unavailable; observation coverage is unknown")

type idleGuardFrame struct {
	Version    int
	Sequence   uint64
	At         time.Time
	Epoch      string
	GapUnknown bool
	Fault      string
	Lanes      []idleGuardPoint
}

type idleGuardStore struct {
	path                 string
	root, fence, journal *os.File
	size                 int64
}

func idleGuardOpenAt(root *os.File, name string, flags int) (*os.File, error) {
	fd, err := unix.Openat(int(root.Fd()), name, flags|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "idle-monitoring")
	if checkLaneControlFile(fd) != nil {
		f.Close()
		return nil, errIdleGuard
	}
	return f, nil
}

func newIdleGuard(path string, notifier idleGuardNotifier, ops idleGuardIO) (*idleGuardObserver, error) {
	root, err := openLaneControlRoot(path)
	if err != nil {
		return nil, errIdleGuard
	}
	s := &idleGuardStore{path: path, root: root}
	fail := func() (*idleGuardObserver, error) { _ = s.close(); return nil, errIdleGuard }
	s.fence, err = idleGuardOpenAt(root, "idle-guard.lock", unix.O_RDWR|unix.O_CREAT)
	if err != nil {
		return fail()
	}
	if unix.Flock(int(s.fence.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
		s.fence.Close()
		s.fence = nil
		return fail()
	}
	s.journal, err = idleGuardOpenAt(root, idleGuardJournal, unix.O_RDWR|unix.O_APPEND|unix.O_CREAT)
	if err != nil {
		return fail()
	}
	frames, err := idleGuardReadFile(s.journal)
	if err != nil {
		return fail()
	}
	st, err := s.journal.Stat()
	if err != nil || st.Size() >= idleGuardMaxBytes || len(frames) >= idleGuardMaxFrames {
		return fail()
	}
	s.size = st.Size()
	// Reconfirm a complete prior append after a process crash/uncertain Sync.
	if s.journal.Sync() != nil || root.Sync() != nil {
		return fail()
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return fail()
	}
	o := &idleGuardObserver{store: s, notifier: notifier, ops: ops, epoch: "boot-" + hex.EncodeToString(nonce[:])}
	if len(frames) > 0 {
		o.frame = frames[len(frames)-1]
	}
	return o, nil
}

func (s *idleGuardStore) close() error {
	var err error
	if s.journal != nil {
		err = errors.Join(err, s.journal.Close())
		s.journal = nil
	}
	if s.fence != nil {
		err = errors.Join(err, unix.Flock(int(s.fence.Fd()), unix.LOCK_UN), s.fence.Close())
		s.fence = nil
	}
	if s.root != nil {
		err = errors.Join(err, s.root.Close())
		s.root = nil
	}
	return err
}

func (s *idleGuardStore) current() bool {
	root, err := openLaneControlRoot(s.path)
	if err != nil {
		return false
	}
	defer root.Close()
	a, ea := s.root.Stat()
	b, eb := root.Stat()
	if ea != nil || eb != nil || !os.SameFile(a, b) {
		return false
	}
	for _, entry := range []struct {
		name string
		file *os.File
	}{{"idle-guard.lock", s.fence}, {idleGuardJournal, s.journal}} {
		f, e := idleGuardOpenAt(root, entry.name, unix.O_RDONLY)
		if e != nil {
			return false
		}
		a, ea := entry.file.Stat()
		b, eb := f.Stat()
		f.Close()
		if ea != nil || eb != nil || !os.SameFile(a, b) {
			return false
		}
	}
	return true
}

func (s *idleGuardStore) append(frame idleGuardFrame, stage string, ops idleGuardIO) error {
	if !s.current() || frame.Sequence > idleGuardMaxFrames {
		return errIdleGuard
	}
	if ops.fault != nil && ops.fault(stage+"_commit") != nil {
		return errIdleGuard
	}
	b, err := json.Marshal(frame)
	if err != nil || len(b) > idleGuardMaxFrame {
		return errIdleGuard
	}
	digest := sha256.Sum256(b)
	buf := make([]byte, 36, 36+len(b))
	binary.BigEndian.PutUint32(buf, uint32(len(b)))
	copy(buf[4:], digest[:])
	buf = append(buf, b...)
	if s.size+int64(len(buf)) > idleGuardMaxBytes {
		return errIdleGuard
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if settingsReadLock(ctx, func() (bool, error) {
		err := unix.Flock(int(s.journal.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if errors.Is(err, unix.EWOULDBLOCK) {
			return false, nil
		}
		return err == nil, err
	}) != nil {
		return errIdleGuard
	}
	defer unix.Flock(int(s.journal.Fd()), unix.LOCK_UN)
	st, err := s.journal.Stat()
	if err != nil || st.Size() != s.size {
		return errIdleGuard
	}
	n, err := s.journal.Write(buf)
	if err != nil || n != len(buf) || s.journal.Sync() != nil {
		return errIdleGuard
	}
	s.size += int64(n)
	if ops.fault != nil && ops.fault(stage+"_synced") != nil {
		return errIdleGuard
	}
	return nil
}

func idleGuardReadFile(f *os.File) ([]idleGuardFrame, error) {
	if unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB) != nil {
		return nil, errIdleGuard
	}
	defer unix.Flock(int(f.Fd()), unix.LOCK_UN)
	st, err := f.Stat()
	if err != nil || st.Size() > idleGuardMaxBytes {
		return nil, errIdleGuard
	}
	b, err := io.ReadAll(io.NewSectionReader(f, 0, idleGuardMaxBytes+1))
	after, e := f.Stat()
	if err != nil || e != nil || st.Size() != after.Size() || !st.ModTime().Equal(after.ModTime()) || int64(len(b)) != after.Size() {
		return nil, errIdleGuard
	}
	var out []idleGuardFrame
	for len(b) > 0 {
		if len(b) < 36 || len(out) >= idleGuardMaxFrames {
			return nil, errIdleGuard
		}
		n := int(binary.BigEndian.Uint32(b[:4]))
		if n < 2 || n > idleGuardMaxFrame || len(b) < 36+n {
			return nil, errIdleGuard
		}
		payload := b[36 : 36+n]
		digest := sha256.Sum256(payload)
		if !bytes.Equal(digest[:], b[4:36]) {
			return nil, errIdleGuard
		}
		var frame idleGuardFrame
		if strictJSON(payload, &frame) != nil || !idleGuardFrameValid(frame, uint64(len(out)+1)) {
			return nil, errIdleGuard
		}
		canonical, err := json.Marshal(frame)
		if err != nil || !bytes.Equal(payload, canonical) {
			return nil, errIdleGuard
		}
		if len(out) > 0 && frame.At.Before(out[len(out)-1].At) {
			return nil, errIdleGuard
		}
		out = append(out, frame)
		b = b[36+n:]
	}
	return out, nil
}

func idleGuardFrameValid(f idleGuardFrame, seq uint64) bool {
	if f.Version != 1 || f.Sequence != seq || f.At.IsZero() || len(f.Epoch) != 37 || len(f.Lanes) > 32 {
		return false
	}
	if f.Fault != "" && f.Fault != "clock_rollback" && f.Fault != "sampling_gap" {
		return false
	}
	seen := map[string]bool{}
	for _, p := range f.Lanes {
		if !namePattern.MatchString(p.Lane) || seen[p.Lane] || p.Candidates < 0 || p.Candidates > 500 || p.IdleNanos < 0 || p.OpenedAt.After(f.At) || len(p.Coverage) > 2000 {
			return false
		}
		seen[p.Lane] = true
		if p.IntervalID != "" && (len(p.IntervalID) != 67 || p.IntervalID[:3] != "ig-" || !queueDigest.MatchString(p.IntervalID[3:])) {
			return false
		}
		if p.IncidentID != "" && p.IncidentID != p.IntervalID {
			return false
		}
		switch p.Category {
		case idleGuardRunning, idleGuardUnexplained, idleGuardBlocked:
			if p.Reason != "" {
				return false
			}
		case idleGuardPermitted:
			if !slicesIdleGuardReason(p.Reason) {
				return false
			}
		case idleGuardUnknown:
			if p.Reason != "capacity_unknown" && p.Reason != "input_unavailable" && p.Reason != "proof_unavailable" && p.Reason != "clock_rollback" {
				return false
			}
		default:
			return false
		}
		for _, c := range p.Coverage {
			if !queueDigest.MatchString(c.Candidate) || !queueDigest.MatchString(c.Evidence) || !slicesIdleGuardReason(c.Code) {
				return false
			}
		}
		if len(p.UnknownCandidates) > 500 {
			return false
		}
		for _, id := range p.UnknownCandidates {
			if !queueDigest.MatchString(id) {
				return false
			}
		}
	}
	return true
}

func slicesIdleGuardReason(code string) bool {
	return code == "OWNER_HOLD_ALL" || code == "LOAD_ABOVE_LIMIT" || code == "CLAIM_HELD" || code == "FILE_CONFLICT"
}

func readIdleGuard(store string, lanes []string, from, until time.Time) idleGuardReport {
	from, until = from.UTC(), until.UTC()
	r := idleGuardReport{Mode: "alert_only", Coverage: "known", From: from.UTC(), Until: until.UTC(), Lanes: []idleGuardLaneReport{}}
	frames, err := idleGuardReadStore(store)
	if err != nil {
		r.Fault = "storage_unavailable"
	}
	if !until.After(from) || until.Sub(from) > 31*24*time.Hour {
		r.Coverage, r.Fault = "unknown", "invalid_window"
		return r
	}
	for _, lane := range lanes {
		v := idleGuardLaneReport{Lane: lane, RatioDisplay: "N/A"}
		var known [4]time.Duration
		if err == nil {
			for i := 0; i+1 < len(frames); i++ {
				a, b := frames[i], frames[i+1]
				start, end := a.At, b.At
				if start.Before(from) {
					start = from
				}
				if end.After(until) {
					end = until
				}
				if !end.After(start) || b.GapUnknown {
					continue
				}
				for _, p := range a.Lanes {
					if p.Lane != lane {
						continue
					}
					if len(p.UnknownCandidates) > 0 {
						r.Coverage = "unknown"
					}
					d := end.Sub(start)
					switch p.Category {
					case idleGuardRunning:
						known[0] += d
					case idleGuardPermitted:
						known[1] += d
					case idleGuardUnexplained:
						known[2] += d
					case idleGuardBlocked:
						known[3] += d
					default:
						continue
					}
				}
			}
		}
		v.RunningSeconds, v.PermittedIdleSeconds = known[0].Seconds(), known[1].Seconds()
		v.UnexplainedIdleSeconds, v.BlockedNoDemandSeconds = known[2].Seconds(), known[3].Seconds()
		v.UnknownSeconds = (until.Sub(from) - known[0] - known[1] - known[2] - known[3]).Seconds()
		opportunity := known[0] + known[2]
		v.OpportunitySeconds, v.UnexplainedSeconds = opportunity.Seconds(), known[2].Seconds()
		if opportunity > 0 {
			ratio := float64(known[0]) / float64(opportunity)
			v.ObservationRatio = &ratio
			v.RatioDisplay = fmt.Sprintf("%.2f%% observed", ratio*100)
		}
		if v.UnknownSeconds > 0 {
			r.Coverage = "unknown"
		}
		r.Lanes = append(r.Lanes, v)
	}
	if err != nil || len(frames) == 0 {
		r.Coverage = "unknown"
	}
	return r
}

func idleGuardReadStore(path string) ([]idleGuardFrame, error) {
	root, err := openLaneControlRoot(path)
	if err != nil {
		return nil, errIdleGuard
	}
	defer root.Close()
	f, err := idleGuardOpenAt(root, idleGuardJournal, unix.O_RDONLY)
	if err != nil {
		return nil, errIdleGuard
	}
	defer f.Close()
	return idleGuardReadFile(f)
}
