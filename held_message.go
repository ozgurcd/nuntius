package nuntius

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// This is a reservation, not a consumption receipt. A05 owns explicit review
// and a durable child/receipt protocol before it may remove this record.
type heldMessage struct {
	Version       int    `json:"version"`
	Lane          string `json:"lane"`
	RunID         string `json:"run_id"`
	GoalID        string `json:"goal_id"`
	SessionID     string `json:"session_id"`
	RunSHA256     string `json:"run_sha256"`
	ContentSHA256 string `json:"content_sha256"`
	Revision      uint64 `json:"revision"`
	Origin        string `json:"origin"`
}

func heldDirectory(parent *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errLaneControlUnavailable
	}
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Mode&07777 != 0700 || st.Uid != uint32(os.Geteuid()) {
		unix.Close(fd)
		return nil, errLaneControlUnavailable
	}
	return os.NewFile(uintptr(fd), name), nil
}

// All callers hold manager -> common control lock. Scan every run, including
// older ended/interrupted records; timestamps never select message identity.
func visitLaneMessages(root *os.File, lane string, visit func(*os.File, Run, []byte) error, scans ...runMetadataScan) error {
	if len(scans) > 1 || len(scans) == 1 && (scans[0].ctx == nil || scans[0].cache == nil) {
		return errLaneControlUnavailable
	}
	runs, err := heldDirectory(root, "runs")
	if err != nil {
		return err
	}
	defer runs.Close()
	names, err := runs.Readdirnames(10001)
	if err != nil && !errors.Is(err, io.EOF) {
		return errLaneControlUnavailable
	}
	if len(names) > 10000 {
		return errLaneControlUnavailable
	}
	for _, id := range names {
		if len(scans) == 1 && scans[0].ctx.Err() != nil {
			return scans[0].ctx.Err()
		}
		if strings.HasPrefix(id, ".pending-") && runPattern.MatchString(strings.TrimPrefix(id, ".pending-")) {
			continue
		}
		if !runPattern.MatchString(id) {
			return errLaneControlUnavailable
		}
		dir, e := heldDirectory(runs, id)
		if e != nil {
			return e
		}
		e = func() error {
			defer dir.Close()
			var b []byte
			var e error
			if len(scans) == 1 {
				b, e = scans[0].cache.read(scans[0].ctx, dir, id)
			} else {
				b, e = readLaneControlPrivate(dir, "meta.json", 131072)
			}
			if e != nil {
				return e
			}
			var r Run
			if strictJSON(b, &r) != nil || r.ID != id || !namePattern.MatchString(r.Lane) || !goalPattern.MatchString(r.GoalID) {
				return errLaneControlUnavailable
			}
			if r.Integrity != "" {
				want := r.Integrity
				r.Integrity = ""
				canonical, _ := json.Marshal(r)
				if laneControlDigest(canonical) != want {
					return errLaneControlUnavailable
				}
				r.Integrity = want
			}
			if len(scans) == 1 && scans[0].history != nil {
				if historical := decodeRunMetadata(b, id); historical != nil {
					*scans[0].history = append(*scans[0].history, *historical)
				}
			}
			if r.Lane != lane {
				return nil
			}
			var message []byte
			if len(scans) == 1 {
				message, e = scans[0].cache.readMessage(scans[0].ctx, dir, id)
			} else {
				message, e = readLaneControlPrivate(dir, "message.json", 16384)
			}
			if errors.Is(e, os.ErrNotExist) {
				message = nil
			} else if e != nil {
				return e
			}
			return visit(dir, r, message)
		}()
		if e != nil {
			return errLaneControlUnavailable
		}
	}
	return nil
}

func pendingText(b []byte) (string, error) {
	var v struct {
		Text string `json:"text"`
	}
	if strictJSON(b, &v) != nil || len(v.Text) == 0 || len(v.Text) > 12000 {
		return "", errLaneControlUnavailable
	}
	return v.Text, nil
}

func latchLaneMessages(root *os.File, intent laneControlIntent, ops *laneControlIO) error {
	return visitLaneMessages(root, intent.Lane, func(dir *os.File, r Run, message []byte) error {
		receipt, err := readMessageReceipt(dir, r)
		if err != nil {
			return err
		}
		if receipt != nil && (receipt.State != "reserved" || receipt.Discarded) {
			return nil
		}
		b, err := readLaneControlPrivate(dir, "message-hold.json", 4096)
		if err == nil {
			var old heldMessage
			text, e := pendingText(message)
			if e != nil || decodeLaneControlObject(b, &old) != nil || old.Version != 1 || old.RunID != r.ID || old.Lane != r.Lane || old.GoalID != r.GoalID || old.RunSHA256 != r.SHA256 || old.ContentSHA256 != laneControlDigest([]byte(text)) || old.Revision == 0 || old.Revision > intent.Revision || !validLaneControlOrigin(old.Origin) || (old.SessionID != "" && !sessionPattern.MatchString(old.SessionID)) || (old.SessionID != "" && r.SessionID != "" && old.SessionID != r.SessionID) {
				return errLaneControlUnavailable
			}
			// A previous rename can be visible without being durable. Preserve
			// the first bytes/revision, but prove persistence before intent cleanup.
			return syncHeldMessage(dir, b, ops)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if message == nil {
			return nil
		}
		text, err := pendingText(message)
		if err != nil {
			return err
		}
		// Capture the accepted run's session from its own rooted event file,
		// even if status has not yet persisted the process's session metadata.
		if r.SessionID == "" {
			fd, e := unix.Openat(int(dir.Fd()), "events.jsonl", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
			if e != nil {
				return errLaneControlUnavailable
			}
			if checkLaneControlFile(fd) != nil {
				unix.Close(fd)
				return errLaneControlUnavailable
			}
			f := os.NewFile(uintptr(fd), "held-events")
			events, e := io.ReadAll(io.LimitReader(f, 65536))
			f.Close()
			if e != nil {
				return errLaneControlUnavailable
			}
			for _, line := range bytes.Split(events, []byte("\n")) {
				var v event
				if json.Unmarshal(line, &v) != nil {
					continue
				}
				for _, session := range []string{v.SessionID, v.ThreadID, v.Session.ID, v.Thread.ID, v.OpenCodeSessionID} {
					if sessionPattern.MatchString(session) {
						r.SessionID = session
						break
					}
				}
				if r.SessionID != "" {
					break
				}
			}
		}
		hold := heldMessage{Version: 1, Lane: r.Lane, RunID: r.ID, GoalID: r.GoalID, SessionID: r.SessionID, RunSHA256: r.SHA256, ContentSHA256: laneControlDigest([]byte(text)), Revision: intent.Revision, Origin: intent.Origin}
		b, err = json.Marshal(hold)
		if err != nil {
			return err
		}
		return replaceLaneControl(dir, "message-hold.json", b, ops, "held")
	})
}

func syncHeldMessage(dir *os.File, expected []byte, ops *laneControlIO) error {
	fd, err := unix.Openat(int(dir.Fd()), "message-hold.json", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return errLaneControlUnavailable
	}
	f := os.NewFile(uintptr(fd), "held-message")
	defer f.Close()
	if err = checkLaneControlFile(fd); err != nil {
		return err
	}
	b, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || !bytes.Equal(b, expected) {
		return errLaneControlUnavailable
	}
	if err = laneControlCheckpoint(ops, "held.fsync"); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = laneControlCheckpoint(ops, "held.dirsync"); err != nil {
		return err
	}
	if err = dir.Sync(); err != nil {
		return err
	}
	// Confirm that the durable directory entry still names the validated,
	// synced inode, rather than a replacement or a newly linked file.
	var opened, named unix.Stat_t
	if checkLaneControlFile(fd) != nil || unix.Fstat(fd, &opened) != nil || unix.Fstatat(int(dir.Fd()), "message-hold.json", &named, unix.AT_SYMLINK_NOFOLLOW) != nil || opened.Dev != named.Dev || opened.Ino != named.Ino {
		return errLaneControlUnavailable
	}
	return nil
}

func laneHasHeldMessage(root *os.File, lane string) (bool, error) {
	return laneHasHeldMessageExcept(root, lane, "")
}

func laneHasHeldMessageExcept(root *os.File, lane, reviewedRun string, scans ...runMetadataScan) (bool, error) {
	held := false
	err := visitLaneMessages(root, lane, func(dir *os.File, r Run, _ []byte) error {
		receipt, err := readMessageReceipt(dir, r)
		if err != nil {
			return err
		}
		if receipt != nil {
			if receipt.State != "reserved" {
				return nil
			}
			if r.ID != reviewedRun {
				held = true
			}
		}
		_, err = readLaneControlPrivate(dir, "message-hold.json", 4096)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if r.ID != reviewedRun {
			held = true
		}
		return nil
	}, scans...)
	return held, err
}
