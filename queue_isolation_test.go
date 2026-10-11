package nuntius

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func configuredQueueFixture(t *testing.T) (*Manager, *Web, string) {
	t.Helper()
	m, w, path := settingsFixture(t)
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal("fixture config load")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal("fixture queue root")
	}
	queue := filepath.Join(root, "queue.json")
	if err := os.WriteFile(queue, []byte(queueSample), 0600); err != nil {
		t.Fatal("fixture queue write")
	}
	c.QueueFile = &queue
	c.Lanes = nil
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal("fixture config encoding")
	}
	m.mu.Lock()
	err = os.WriteFile(path, b, 0600)
	m.mu.Unlock()
	if err != nil {
		t.Fatal("fixture config write")
	}
	return m, w, queue
}

// Ported from the independent v0.3.3 review, with replacement-file controls.
func TestReviewQueueFailureIsolation(t *testing.T) {
	for _, failure := range []string{"unreadable", "directory", "fifo", "symlink"} {
		t.Run(failure, func(t *testing.T) {
			m, w, queue := configuredQueueFixture(t)
			get := func(route string) int {
				r := httptest.NewRequest("GET", "http://localhost:8123"+route, nil)
				r.Header.Set("X-Nuntius-Token", w.token)
				out := httptest.NewRecorder()
				w.ServeHTTP(out, r)
				return out.Code
			}
			if get("/data") != 200 || get("/queue") != 200 {
				t.Fatal("operational and queue controls failed")
			}
			if failure == "unreadable" {
				if os.Chmod(queue, 0000) != nil {
					t.Fatal("fixture permissions")
				}
				t.Cleanup(func() { _ = os.Chmod(queue, 0600) })
			} else {
				if os.Remove(queue) != nil {
					t.Fatal("fixture replacement")
				}
				var err error
				switch failure {
				case "directory":
					err = os.Mkdir(queue, 0700)
				case "fifo":
					err = syscall.Mkfifo(queue, 0600)
				case "symlink":
					target := filepath.Join(filepath.Dir(queue), "target.json")
					if os.WriteFile(target, []byte(queueSample), 0600) != nil {
						t.Fatal("fixture target")
					}
					err = os.Symlink(target, queue)
				}
				if err != nil {
					t.Fatal("fixture special file")
				}
			}
			if m.queue().Status != "unavailable" || get("/queue") != 503 {
				t.Fatal("queue negative control failed")
			}
			if code := get("/data"); code != 200 {
				t.Fatalf("queue read failure broke operational data: HTTP %d", code)
			}
		})
	}
}

func TestQueueFailureDispatchReplyIsolation(t *testing.T) {
	m, w, queue := configuredQueueFixture(t)
	// Load the persisted override while it is readable, then fail only Queue I/O.
	r := httptest.NewRequest("GET", "http://localhost:8123/data", nil)
	r.Header.Set("X-Nuntius-Token", w.token)
	out := httptest.NewRecorder()
	w.ServeHTTP(out, r)
	if out.Code != 200 || m.queue().Status != "ok" {
		t.Fatal("operational and queue controls failed")
	}
	if os.Chmod(queue, 0000) != nil {
		t.Fatal("fixture permissions")
	}
	t.Cleanup(func() { _ = os.Chmod(queue, 0600) })
	if m.queue().Status != "unavailable" {
		t.Fatal("queue negative control failed")
	}
	if _, err := m.Dispatch("codex-1", "QUEUE-ISOLATION", "fixture dispatch"); err != nil {
		t.Fatal("queue read failure broke dispatch")
	}
	waitState(t, m, "codex-1", "ended")
	if _, err := m.Reply("codex-1", "fixture reply"); err != nil {
		t.Fatal("queue read failure broke reply")
	}
	waitState(t, m, "codex-1", "ended")
	if m.queue().Status != "unavailable" {
		t.Fatal("dispatch or reply changed queue failure state")
	}
}
