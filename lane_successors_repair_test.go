package nuntius

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func successorsRepairFixture(t *testing.T) (*Manager, string) {
	t.Helper()
	m, _ := laneControlCommitFixture(t)
	r := seedCrashRun(t, m, strings.Repeat("f", 32), "REPAIR-HOLD", "completed")
	r.State, r.SessionID = "ended", "repair-session"
	r.SHA256 = laneControlDigest([]byte("repair brief"))
	if err := m.save(&r); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(m.store, "runs", r.ID)
	if err := atomicPrivate(filepath.Join(dir, "message.json"), []byte(`{"text":"repair accepted message"}`)); err != nil {
		t.Fatal(err)
	}
	return m, dir
}

func TestLaneSuccessorsRepairDurability(t *testing.T) {
	for _, first := range []string{"held.renamed", "held.dirsync"} {
		for _, repair := range []string{"held.fsync", "held.dirsync"} {
			t.Run(first+"/"+repair, func(t *testing.T) {
				m, dir := successorsRepairFixture(t)
				fail := func(want string) *laneControlIO {
					return &laneControlIO{checkpoint: func(stage string) error {
						if stage == want {
							return errors.New("injected hold persistence failure")
						}
						return nil
					}}
				}
				if _, err := successorControl(m, "codex", true, fail(first)); err == nil {
					t.Fatal("initial hold persistence failure accepted")
				}
				original, err := os.ReadFile(filepath.Join(dir, "message-hold.json"))
				if err != nil {
					t.Fatal("visible reservation missing")
				}
				if _, err := successorControl(m, "codex", true, fail(repair)); err == nil {
					t.Fatal("repair accepted an unconfirmed reservation")
				}
				if _, err := os.Stat(filepath.Join(m.store, "lane-control-intent.json")); err != nil {
					t.Fatal("repair lost unresolved intent")
				}
				if _, err := m.Dispatch("codex", "DENIED-REPAIR", "must not launch"); err == nil {
					t.Fatal("uncertain repair allowed delivery")
				}
				var stages []string
				if _, err := successorControl(m, "codex", true, &laneControlIO{checkpoint: func(stage string) error {
					stages = append(stages, stage)
					return nil
				}}); err != nil {
					t.Fatal(err)
				}
				fsync, dirsync, cleanup := -1, -1, -1
				for i, stage := range stages {
					switch stage {
					case "held.fsync":
						fsync = i
					case "held.dirsync":
						dirsync = i
					case "intent.cleanup":
						cleanup = i
					}
				}
				if fsync < 0 || dirsync <= fsync || cleanup <= dirsync {
					t.Fatal("intent cleanup did not follow hold file and directory sync")
				}
				if _, err := os.Stat(filepath.Join(m.store, "lane-control-intent.json")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("successful repair retained intent")
				}
				after, err := os.ReadFile(filepath.Join(dir, "message-hold.json"))
				if err != nil || !bytes.Equal(original, after) {
					t.Fatal("repair replaced original reservation bytes or revision")
				}
			})
		}
	}
}

func TestLaneSuccessorsUnsafeEvents(t *testing.T) {
	for _, kind := range []string{"fifo", "directory", "hardlink", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			m, dir := successorsRepairFixture(t)
			r := successorRun(t, m, filepath.Base(dir))
			r.SessionID = ""
			if err := m.save(r); err != nil {
				t.Fatal(err)
			}
			events := filepath.Join(dir, "events.jsonl")
			var err error
			switch kind {
			case "fifo":
				err = unix.Mkfifo(events, 0600)
			case "directory":
				err = os.Mkdir(events, 0700)
			case "hardlink", "symlink":
				original := filepath.Join(dir, "unsafe-events")
				if err = atomicPrivate(original, nil); err == nil {
					if kind == "hardlink" {
						err = os.Link(original, events)
					} else {
						err = os.Symlink(original, events)
					}
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { _, e := successorControl(m, "codex", true, nil); done <- e }()
			select {
			case err = <-done:
				if err == nil {
					t.Fatal("unsafe event file accepted")
				}
			case <-time.After(200 * time.Millisecond):
				// Release only this test's FIFO on the broken parent, so red
				// evidence leaves no blocked reader or manager cleanup.
				if kind == "fifo" {
					fd, e := unix.Open(events, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
					if e != nil {
						t.Fatal("could not release synthetic FIFO")
					}
					unix.Close(fd)
					select {
					case <-done:
					case <-time.After(time.Second):
						t.Fatal("synthetic FIFO reader did not exit")
					}
				}
				t.Fatal("pause blocked on unsafe original events")
			}
			if _, err := os.Stat(filepath.Join(m.store, "lane-control-intent.json")); err != nil {
				t.Fatal("unsafe events lost unresolved intent")
			}
			safe := make(chan struct{})
			go func() {
				m.Status()
				_ = m.Cancel("codex")
				close(safe)
			}()
			select {
			case <-safe:
			case <-time.After(time.Second):
				t.Fatal("independent status/cancel paths blocked after refusal")
			}
		})
	}
}

func TestLaneSuccessorsRepairIdentity(t *testing.T) {
	for _, change := range []string{"session", "inode"} {
		t.Run(change, func(t *testing.T) {
			m, dir := successorsRepairFixture(t)
			if _, err := successorControl(m, "codex", true, &laneControlIO{checkpoint: func(stage string) error {
				if stage == "held.renamed" {
					return errors.New("injected rename uncertainty")
				}
				return nil
			}}); err == nil {
				t.Fatal("rename uncertainty accepted")
			}
			path := filepath.Join(dir, "message-hold.json")
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if change == "session" {
				r := successorRun(t, m, filepath.Base(dir))
				r.SessionID = "changed-session"
				if err := m.save(r); err != nil {
					t.Fatal(err)
				}
			}
			replaced := false
			_, err = successorControl(m, "codex", true, &laneControlIO{checkpoint: func(stage string) error {
				if change == "inode" && stage == "held.fsync" {
					replaced = true
					return atomicPrivate(path, original)
				}
				return nil
			}})
			if err == nil {
				t.Fatal("repair accepted changed reservation identity")
			}
			if change == "inode" && !replaced {
				t.Fatal("did not exercise replacement of the opened hold inode")
			}
			if _, err := os.Stat(filepath.Join(m.store, "lane-control-intent.json")); err != nil {
				t.Fatal("identity failure lost intent")
			}
		})
	}
}
