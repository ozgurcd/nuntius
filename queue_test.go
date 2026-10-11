package nuntius

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

const queueSample = `{"version":1,"updated":"2026-10-10T22:32:00Z","goals":[{"id":"EXAMPLE","title":"Example goal","size":"M","lane_kinds":["codex"],"order":4,"depends_on":[],"needs_owner":false,"state":"ready","created":"2026-10-10T22:28:00Z","updated":"2026-10-10T22:28:00Z"}]}`

func queueFixture(t *testing.T) (*Web, string) {
	t.Helper()
	root, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal("fixture root")
	}
	return &Web{manager: &Manager{store: root}, address: "127.0.0.1:8123", token: "fixture-auth", allowHost: "example.ts.net"}, filepath.Join(root, "queue.json")
}
func queueRequest(w *Web, method, target, host, auth, origin string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	r.Host = host
	r.Header.Set("X-Nuntius-Token", auth)
	r.Header.Set("Origin", origin)
	out := httptest.NewRecorder()
	w.ServeHTTP(out, r)
	return out
}
func TestQueueRouteGuards(t *testing.T) {
	w, path := queueFixture(t)
	if os.WriteFile(path, []byte(queueSample), 0600) != nil {
		t.Fatal("fixture write")
	}
	for _, host := range []string{"127.0.0.1:8123", "localhost:8123", "example.ts.net", "example.ts.net:443"} {
		out := queueRequest(w, "GET", "/queue", host, "fixture-auth", "")
		if out.Code != 200 {
			t.Fatalf("guarded queue control: %d", out.Code)
		}
		if out.Header().Get("Cache-Control") != "no-store" || out.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(out.Header().Get("Content-Security-Policy"), "script-src 'self'") || out.Header().Get("Content-Type") != "application/json" {
			t.Fatal("queue headers")
		}
	}
	for _, tc := range []struct {
		method, target, host, auth, origin string
		code                               int
	}{
		{"GET", "/queue", "evil.test:8123", "fixture-auth", "", 403}, {"GET", "/queue", "localhost:8124", "fixture-auth", "", 403},
		{"GET", "/queue", "localhost:8123", "", "", 403}, {"GET", "/queue?token=fixture-auth", "localhost:8123", "", "", 403},
		{"GET", "/queue?file=other", "localhost:8123", "fixture-auth", "", 400},
		{"POST", "/queue", "localhost:8123", "fixture-auth", "http://localhost:8123", 405}, {"POST", "/queue", "localhost:8123", "fixture-auth", "http://evil.test", 403},
		{"PUT", "/queue", "localhost:8123", "fixture-auth", "", 405}, {"DELETE", "/queue", "localhost:8123", "fixture-auth", "", 405}, {"PATCH", "/queue", "localhost:8123", "fixture-auth", "", 405}, {"OPTIONS", "/queue", "localhost:8123", "fixture-auth", "", 405},
	} {
		out := queueRequest(w, tc.method, tc.target, tc.host, tc.auth, tc.origin)
		if out.Code != tc.code {
			t.Fatalf("queue guard %s: %d", tc.method, out.Code)
		}
		if out.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatal("CORS")
		}
	}
	b, _ := os.ReadFile(path)
	if string(b) != queueSample {
		t.Fatal("queue mutated")
	}
	out := queueRequest(w, "GET", "/", "localhost:8123", "", "")
	if strings.Contains(out.Body.String(), "Example goal") {
		t.Fatal("public shell leak")
	}
}
func TestQueueSchema(t *testing.T) {
	w, path := queueFixture(t)
	run := func(b string, want int) {
		t.Helper()
		if os.WriteFile(path, []byte(b), 0600) != nil {
			t.Fatal("fixture write")
		}
		out := queueRequest(w, "GET", "/queue", "localhost:8123", "fixture-auth", "")
		if out.Code != want {
			t.Fatalf("schema outcome: %d, want %d", out.Code, want)
		}
		if want == 503 {
			var v map[string]any
			if json.Unmarshal(out.Body.Bytes(), &v) != nil || len(v) != 3 || v["status"] != "invalid" || v["reason"] != "invalid_snapshot" || len(v["goals"].([]any)) != 0 {
				t.Fatal("fixed schema error")
			}
		}
	}
	run(queueSample, 200)
	run(`{"version":1,"updated":"2026-10-10T22:32:00Z","goals":[]}`, 200)
	var base map[string]any
	_ = json.Unmarshal([]byte(queueSample), &base)
	for _, key := range []string{"version", "updated", "goals"} {
		for _, value := range []any{nil, "wrong", true} {
			var v map[string]any
			_ = json.Unmarshal([]byte(queueSample), &v)
			v[key] = value
			b, _ := json.Marshal(v)
			run(string(b), 503)
		}
	}
	goal := base["goals"].([]any)[0].(map[string]any)
	for key := range goal {
		for _, value := range []any{nil, map[string]any{}} {
			var v map[string]any
			_ = json.Unmarshal([]byte(queueSample), &v)
			v["goals"].([]any)[0].(map[string]any)[key] = value
			b, _ := json.Marshal(v)
			run(string(b), 503)
		}
		var v map[string]any
		_ = json.Unmarshal([]byte(queueSample), &v)
		delete(v["goals"].([]any)[0].(map[string]any), key)
		b, _ := json.Marshal(v)
		run(string(b), 503)
	}
	mutations := []string{
		strings.Replace(queueSample, `"version":1`, `"version":2`, 1), strings.Replace(queueSample, `"version":1`, `"version":1.0`, 1),
		strings.Replace(queueSample, `"version":1`, `"version":1,"version":1`, 1), strings.Replace(queueSample, `"version":1`, `"unknown":0,"unknown":1,"version":1`, 1),
		strings.Replace(queueSample, `"order":4`, `"order":2147483648`, 1), strings.Replace(queueSample, `"order":4`, `"order":-1`, 1),
		strings.Replace(queueSample, `"depends_on":[]`, `"depends_on":["EXAMPLE"]`, 1), strings.Replace(queueSample, `"depends_on":[]`, `"depends_on":["OTHER","OTHER"]`, 1),
		strings.Replace(queueSample, `"needs_owner":false`, `"needs_owner":true,"owner_reason":"Decide"`, 1), strings.Replace(queueSample, `"needs_owner":false`, `"needs_owner":false,"owner_reason":"Decide"`, 1),
		strings.Replace(queueSample, `"state":"ready"`, `"state":"blocked"`, 1), strings.Replace(queueSample, `"state":"ready"`, `"state":"parked","reason":" "`, 1),
		strings.Replace(queueSample, `"title":"Example goal"`, `"title":"x\n"`, 1), strings.Replace(queueSample, `"title":"Example goal"`, `"title":"x\u202e"`, 1),
		strings.Replace(queueSample, `"title":"Example goal"`, `"title":"`+strings.Repeat("x", 241)+`"`, 1),
		strings.Replace(queueSample, `"id":"EXAMPLE"`, `"id":"`+strings.Repeat("X", 65)+`"`, 1),
		strings.Replace(queueSample, `"lane_kinds":["codex"]`, `"lane_kinds":["codex","codex"]`, 1),
		strings.Replace(queueSample, `"created":"2026-10-10T22:28:00Z"`, `"created":"2026-10-10T22:33:00Z"`, 1),
		strings.Replace(queueSample, `"version":1`, `"ignored":[[[[[[[[0]]]]]]]],"version":1`, 1),
		queueSample + ` {}`, "\xef\xbb\xbf" + queueSample,
	}
	for _, b := range mutations {
		run(b, 503)
	}
	for _, link := range []string{"http://example.com/brief", "https://localhost/x", "https://127.0.0.1/x", "https://[::1]/x", "https://2130706433/x", "https://0x7f000001/x", "https://127.1/x", "https://example.local/x", "https://user@example.com/x", "https://example.com/x?q=y", "https://example.com/x#frag", "javascript:alert(1)"} {
		run(strings.Replace(queueSample, `"size":"M"`, `"brief_link":"`+link+`","size":"M"`, 1), 503)
	}
	run(strings.Replace(queueSample, `"size":"M"`, `"brief_link":"https://example.com/brief","size":"M"`, 1), 200)
	for _, key := range []string{"lane", "reason", "owner_reason", "brief_sha256", "brief_link"} {
		run(strings.Replace(queueSample, `"size":"M"`, `"`+key+`":null,"size":"M"`, 1), 503)
	}
	run(strings.Replace(queueSample, `"version":1`, `"ignored":[[[[[[[0]]]]]]],"version":1`, 1), 200)
	run(strings.Replace(queueSample, `"title":"Example goal"`, `"title":"`+strings.Repeat(" ", 250)+`Trimmed"`, 1), 200)
	for _, n := range []int{512, 513} {
		want := 200
		if n == 513 {
			want = 503
		}
		run(strings.Replace(queueSample, `"size":"M"`, `"reason":"`+strings.Repeat("x", n)+`","size":"M"`, 1), want)
	}
	g := mustGoalJSON(t, goal)
	run(`{"version":1,"updated":"2026-10-10T22:32:00Z","goals":[`+g+`,`+g+`]}`, 503)
	a := strings.Replace(g, `"depends_on":[]`, `"depends_on":["OTHER"]`, 1)
	a = strings.Replace(a, `"state":"ready"`, `"state":"blocked","reason":"Waiting"`, 1)
	b := strings.Replace(a, `"id":"EXAMPLE"`, `"id":"OTHER"`, 1)
	b = strings.Replace(b, `"depends_on":["OTHER"]`, `"depends_on":["EXAMPLE"]`, 1)
	run(`{"version":1,"updated":"2026-10-10T22:32:00Z","goals":[`+a+`,`+b+`]}`, 503)
	for _, n := range []int{500, 501} {
		g := []string{}
		for i := 0; i < n; i++ {
			g = append(g, strings.Replace(mustGoalJSON(t, goal), "EXAMPLE", fmt.Sprintf("G%d", i), 1))
		}
		want := 200
		if n == 501 {
			want = 503
		}
		run(`{"version":1,"updated":"2026-10-10T22:32:00Z","goals":[`+strings.Join(g, ",")+`]}`, want)
	}
	for _, n := range []int{32, 33} {
		deps := []string{}
		for i := 0; i < n; i++ {
			deps = append(deps, fmt.Sprintf("D%d", i))
		}
		b, _ := json.Marshal(deps)
		s := strings.Replace(queueSample, `"depends_on":[]`, `"depends_on":`+string(b), 1)
		s = strings.Replace(s, `"state":"ready"`, `"state":"blocked","reason":"Waiting"`, 1)
		want := 200
		if n == 33 {
			want = 503
		}
		run(s, want)
	}
	run(strings.Replace(queueSample, `"version":1`, `"ignored":{"private":"never echo"},"version":1`, 1), 200)
	out := queueRequest(w, "GET", "/queue", "localhost:8123", "fixture-auth", "")
	if strings.Contains(out.Body.String(), "private") || strings.Contains(out.Body.String(), path) {
		t.Fatal("DTO allowlist")
	}
	for _, n := range []int{1048576, 1048577} {
		s := queueSample + strings.Repeat(" ", n-len(queueSample))
		want := 200
		if n > 1048576 {
			want = 503
		}
		run(s, want)
	}
}
func mustGoalJSON(t *testing.T, v any) string {
	t.Helper()
	b, e := json.Marshal(v)
	if e != nil {
		t.Fatal("fixture encoding")
	}
	return string(b)
}
func TestQueueFiles(t *testing.T) {
	w, path := queueFixture(t)
	status := func(want string) {
		t.Helper()
		out := queueRequest(w, "GET", "/queue", "localhost:8123", "fixture-auth", "")
		var v struct{ Status string }
		if json.Unmarshal(out.Body.Bytes(), &v) != nil || v.Status != want {
			t.Fatal("filesystem status")
		}
	}
	status("not_configured")
	explicit := path
	w.manager.config.QueueFile = &explicit
	status("missing")
	empty := ""
	w.manager.config.QueueFile = &empty
	status("disabled")
	w.manager.config.QueueFile = nil
	// Unix sockaddr paths are capped on Darwin. Use a short private directory
	// directly under the test TMPDIR rather than its long test-name hierarchy.
	socketDir, err := os.MkdirTemp("", "q")
	if err != nil {
		t.Fatal("socket directory")
	}
	defer os.RemoveAll(socketDir)
	socketDir, err = filepath.EvalSymlinks(socketDir)
	if err != nil {
		t.Fatal("socket directory canonicalization")
	}
	socket := filepath.Join(socketDir, "s")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal("socket fixture")
	}
	defer listener.Close()
	w.manager.config.QueueFile = &socket
	status("unavailable")
	w.manager.config.QueueFile = nil
	if os.Mkdir(path, 0700) != nil {
		t.Fatal("fixture directory")
	}
	status("unavailable")
	_ = os.Remove(path)
	if syscall.Mkfifo(path, 0600) != nil {
		t.Fatal("fixture FIFO")
	}
	status("unavailable")
	_ = os.Remove(path)
	target := filepath.Join(filepath.Dir(path), "target")
	_ = os.WriteFile(target, []byte(queueSample), 0600)
	if os.Symlink(target, path) != nil {
		t.Fatal("fixture link")
	}
	status("unavailable")
	_ = os.Remove(path)
	dir := filepath.Join(filepath.Dir(path), "linked")
	if os.Symlink(filepath.Dir(path), dir) != nil {
		t.Fatal("ancestor fixture")
	}
	linked := filepath.Join(dir, "target")
	w.manager.config.QueueFile = &linked
	status("unavailable")
	w.manager.config.QueueFile = nil
	for i := 0; i < 8; i++ {
		tmp := filepath.Join(filepath.Dir(path), "new")
		_ = os.WriteFile(tmp, []byte(queueSample), 0600)
		if os.Rename(tmp, path) != nil {
			t.Fatal("atomic fixture")
		}
		status("ok")
	}
	w.manager.config.QueueFile = &path
	status("ok")
	if os.Chmod(path, 0000) != nil {
		t.Fatal("permission fixture")
	}
	status("unavailable")
	_ = os.Chmod(path, 0600)
}

func TestQueueReadChanges(t *testing.T) {
	_, path := queueFixture(t)
	for _, change := range []string{"replace_content", "grow", "read_failure"} {
		if os.WriteFile(path, []byte(queueSample), 0600) != nil {
			t.Fatal("fixture write")
		}
		file, e := openQueueFile(path)
		if e != nil {
			t.Fatal("fixture open")
		}
		result := readQueue(file, func(reader io.Reader) ([]byte, error) {
			b, e := io.ReadAll(reader)
			if change == "read_failure" {
				return nil, io.ErrUnexpectedEOF
			}
			data := []byte("{}")
			if change == "grow" {
				data = []byte(queueSample + strings.Repeat(" ", queueMaxBytes))
			}
			if os.WriteFile(path, data, 0600) != nil {
				t.Fatal("fixture change")
			}
			return b, e
		})
		_ = file.Close()
		if result.Status != "unavailable" || len(result.Goals) != 0 {
			t.Fatal("changed read must fail closed")
		}
	}
}

func TestQueueConfigPath(t *testing.T) {
	c, _ := fixture(t)
	for _, path := range []string{"relative", "/", "/a/../queue.json", "/a//queue.json", "/a/queue\n.json"} {
		c.QueueFile = &path
		if _, e := c.Validate(); e == nil {
			t.Fatal("unsafe queue path accepted")
		}
	}
	_, path := queueFixture(t)
	c.QueueFile = &path
	if _, e := c.Validate(); e != nil {
		t.Fatal("missing absolute queue accepted control")
	}
	if os.WriteFile(path, []byte(queueSample), 0600) != nil {
		t.Fatal("fixture write")
	}
	if _, e := c.Validate(); e != nil {
		t.Fatal("regular queue control")
	}
	linked := filepath.Join(filepath.Dir(path), "link")
	_ = os.Symlink(path, linked)
	c.QueueFile = &linked
	if _, e := c.Validate(); e != nil {
		t.Fatal("queue I/O belongs to reader, not config validation")
	}
	empty := ""
	c.QueueFile = &empty
	if _, e := c.Validate(); e != nil {
		t.Fatal("disabled config control")
	}
	for _, mode := range []string{"disabled", "default", "configured"} {
		if mode == "default" {
			c.QueueFile = nil
		}
		if mode == "configured" {
			c.QueueFile = &path
		}
		v := settingsView(c, "fixture-revision")
		b, _ := json.Marshal(v)
		if v["queue_file_mode"] != mode || strings.Contains(string(b), path) {
			t.Fatal("settings queue mode allowlist")
		}
	}
}
