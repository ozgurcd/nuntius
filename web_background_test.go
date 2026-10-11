package nuntius

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRedispatchedReportIsNotCurrent(t *testing.T) {
	m, c, _ := manager(t)
	r := Run{ID: strings.Repeat("a", 32), Lane: "codex", GoalID: "REDISPATCH-1", State: "running", Started: time.Now().Add(-time.Minute)}
	dir := filepath.Join(m.store, "runs", r.ID)
	if err := privateDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := m.save(&r); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(c.ReportsDir, r.GoalID+".txt")
	if err := os.WriteFile(path, []byte("old report"), 0600); err != nil {
		t.Fatal(err)
	}
	old := r.Started.Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if stuck, err := m.checkStuck(&r, time.Now()); err != nil || stuck {
		t.Fatal("old report marked active redispatch stuck")
	}
	if m.reportExists(r) {
		t.Fatal("old report credited to redispatch")
	}
	fresh := r.Started.Add(time.Second)
	if err := os.Chtimes(path, fresh, fresh); err != nil {
		t.Fatal(err)
	}
	if !m.reportExists(r) {
		t.Fatal("current report missing")
	}
	if stuck, err := m.checkStuck(&r, fresh.Add(6*time.Minute)); err != nil || !stuck {
		t.Fatal("current report did not trigger stuck detection")
	}
}

func TestSetQueries(t *testing.T) {
	_, c, store := manager(t)
	c.Lanes[0].ExtraArgs = []string{"--model", "private-model-fixture"}
	cliFixture(t, c, store)
	for _, args := range [][]string{{"set"}, {"set", "codex-agent-pool"}, {"set", "opencode-agent-pool"}} {
		var out, errs bytes.Buffer
		if CLI(context.Background(), args, strings.NewReader(""), &out, &errs) != 0 {
			t.Fatal("set query failed")
		}
		if !strings.Contains(out.String(), "codex-agent-pool = 1") && len(args) == 1 {
			t.Fatal("missing pool value")
		}
		if len(args) == 2 && !strings.Contains(out.String(), args[1]+" = ") {
			t.Fatal("key query missing")
		}
		if len(args) == 1 && !strings.Contains(out.String(), "read-only config:") {
			t.Fatal("read-only keys not documented")
		}
		if strings.Contains(out.String(), "private-model-fixture") {
			t.Fatal("set query exposed template data")
		}
		if len(args) == 2 && args[1] == "opencode-agent-pool" && out.String() != "opencode-agent-pool = 0 (default)\n" {
			t.Fatal("default setting provenance missing")
		}
		if len(args) == 2 && args[1] == "codex-agent-pool" && !strings.Contains(out.String(), "("+cell(os.Getenv("NUNTIUS_CONFIG"))+")") {
			t.Fatal("config setting provenance missing")
		}
	}
}

func TestBrandAssets(t *testing.T) {
	m, _, _ := manager(t)
	w, _ := NewWeb(m, "127.0.0.1:8123")
	for _, path := range []string{"/favicon.svg", "/assets/brand/nuntius-logo.svg", "/assets/brand/nuntius-logo-dark.svg"} {
		r := httptest.NewRequest("GET", "http://127.0.0.1:8123"+path, nil)
		out := httptest.NewRecorder()
		w.ServeHTTP(out, r)
		if out.Code != 200 || !strings.Contains(out.Header().Get("Content-Type"), "image/svg+xml") {
			t.Fatal("embedded brand asset unavailable")
		}
		r.Host = "foreign.example"
		out = httptest.NewRecorder()
		w.ServeHTTP(out, r)
		if out.Code != 403 {
			t.Fatal("brand asset bypassed host guard")
		}
	}
	page, _ := dashboardAssets.ReadFile("index.html")
	if !strings.Contains(string(page), "rel=\"icon\"") || !strings.Contains(string(page), "nuntius-logo-dark.svg") || strings.Contains(string(page), ">N</span>") {
		t.Fatal("dashboard brand missing")
	}
}

func TestBackgroundWebLifecycle(t *testing.T) {
	for _, platform := range []string{"darwin", "linux"} {
		t.Run(platform, func(t *testing.T) {
			_, c, store := manager(t)
			cliFixture(t, c, store)
			dir := fakeTailscale(t, fixtureTailnetStatus)
			if err := os.Symlink(fakeBinary, filepath.Join(dir, "launchctl")); err != nil {
				t.Fatal(err)
			}
			t.Setenv("NUNTIUS_FAKE_LAUNCHCTL", dir)
			prevOS, prevExe := webPlatform, webExecutable
			webPlatform = platform
			webExecutable = func() (string, error) { return fakeBinary, nil }
			t.Cleanup(func() { webPlatform, webExecutable = prevOS, prevExe })
			call := func(args ...string) (int, string) {
				var out, errs bytes.Buffer
				code := CLI(context.Background(), append([]string{"web"}, args...), strings.NewReader(""), &out, &errs)
				return code, out.String()
			}
			t.Cleanup(func() { _, _ = call("stop") })
			code, out := call("start", "--bind", "127.0.0.1:0", "--tailnet", "--notify-imessage", "owner@icloud.com")
			if code != 0 || strings.Count(out, "summary:") != 1 || strings.Count(out, "Scan to sign in") != 1 {
				t.Fatal("background start failed or output repeated")
			}
			state := (&Manager{store: store}).webDashboard()
			if state == nil || state.PID == os.Getpid() {
				t.Fatal("no independent background process")
			}
			if pgid, err := syscall.Getpgid(state.PID); err != nil || pgid != state.PID {
				t.Fatal("dashboard retained the terminal process group")
			}
			resp, err := (&http.Client{Timeout: time.Second}).Get("http://" + state.Address + "/")
			if err != nil {
				t.Fatal("background dashboard is not ready")
			}
			resp.Body.Close()
			code, status := call("status")
			if code != 0 || !strings.Contains(status, "running") || !strings.Contains(status, "imessage=sent") || strings.Contains(status, "?token=") {
				t.Fatal("unsafe or incomplete status")
			}
			var long bytes.Buffer
			if CLI(context.Background(), []string{"status", "--long"}, strings.NewReader(""), &long, &long) != 0 || !strings.Contains(long.String(), strings.TrimSpace(status)) {
				t.Fatal("long status differs from web status")
			}
			code, again := call("start")
			if code != 0 || !strings.Contains(again, "already running") || strings.Contains(again, "?token=") {
				t.Fatal("duplicate start not refused")
			}
			code, link := call("link", "--notify")
			if code != 0 || !strings.Contains(link, "?token=") || len(fakeCalls(t, dir, "osascript.log")) != 2 {
				t.Fatal("link or resend failed")
			}
			if os.WriteFile(filepath.Join(dir, "osascript-fail"), nil, 0600) != nil {
				t.Fatal("fixture failure setup")
			}
			code, _ = call("link", "--notify")
			if code != 0 {
				t.Fatal("send failure stopped link output")
			}
			_, status = call("status")
			if !strings.Contains(status, "imessage=failed") {
				t.Fatal("last send failure missing")
			}
			var access struct {
				Token string `json:"token"`
			}
			p := filepath.Join(store, "web-access.json")
			b, err := os.ReadFile(p)
			if err != nil || json.Unmarshal(b, &access) != nil || access.Token == "" {
				t.Fatal("owner access file missing")
			}
			// The environment-resolved config must survive start -> wrapper ->
			// __background even when the owner supplied no explicit --config.
			req, err := http.NewRequest("GET", "http://"+state.Address+"/settings", nil)
			if err != nil {
				t.Fatal("settings request")
			}
			req.Header.Set("X-Nuntius-Token", access.Token)
			resp, err = (&http.Client{Timeout: 3 * time.Second}).Do(req)
			if err != nil {
				t.Fatal("background Settings unavailable")
			}
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("background Settings HTTP %d", resp.StatusCode)
			}
			fi, _ := os.Stat(p)
			if fi.Mode().Perm() != 0600 {
				t.Fatal("access file not private")
			}
			for _, p := range []string{filepath.Join(store, "web.log"), filepath.Join(store, "web.json"), filepath.Join(dir, "osascript.log")} {
				b, _ := os.ReadFile(p)
				if strings.Contains(string(b), access.Token) || strings.Contains(string(b), "?token=") {
					t.Fatal("credential leaked to state or log")
				}
			}
			if platform == "darwin" {
				b, _ := os.ReadFile(filepath.Join(store, "web-launch", "job.plist"))
				if !strings.Contains(string(b), "<key>RunAtLoad</key><false/>") || !strings.Contains(string(b), "<key>Umask</key>") {
					t.Fatal("launchd policy missing")
				}
			}
			code, _ = call("start", "--replace", "--bind", "127.0.0.1:0")
			if code != 0 {
				t.Fatal("replace failed")
			}
			if next := (&Manager{store: store}).webDashboard(); next == nil || next.PID == state.PID {
				t.Fatal("replace did not restart")
			}
			code, _ = call("stop")
			if code != 0 {
				t.Fatal("stop failed")
			}
			if _, err := os.Stat(p); !os.IsNotExist(err) {
				t.Fatal("stop retained credential")
			}
			if s := (&Manager{store: store}).webDashboard(); s != nil {
				t.Fatal("dashboard remained alive")
			}
			code, status = call("stop")
			if code != 0 || !strings.Contains(status, "not running") {
				t.Fatal("stop not idempotent")
			}
			if calls := fakeCalls(t, dir, "calls.log"); !strings.Contains(strings.Join(calls, "\n"), "serve --https=443 off") {
				t.Fatal("owned mapping not removed")
			}
		})
	}
}

func TestBackgroundServeCleanupFailure(t *testing.T) {
	_, c, store := manager(t)
	cliFixture(t, c, store)
	dir := fakeTailscale(t, fixtureTailnetStatus)
	prevOS, prevExe := webPlatform, webExecutable
	webPlatform = "linux"
	webExecutable = func() (string, error) { return fakeBinary, nil }
	defer func() { webPlatform, webExecutable = prevOS, prevExe }()
	var out, errs bytes.Buffer
	if CLI(context.Background(), []string{"web", "start", "--bind", "127.0.0.1:0", "--tailnet"}, strings.NewReader(""), &out, &errs) != 0 {
		t.Fatal("fixture start failed")
	}
	if os.WriteFile(filepath.Join(dir, "serve-off-fail"), nil, 0600) != nil {
		t.Fatal("fixture failure setup")
	}
	out.Reset()
	errs.Reset()
	if CLI(context.Background(), []string{"web", "stop"}, strings.NewReader(""), &out, &errs) == 0 {
		t.Fatal("failed Serve cleanup reported success")
	}
	if (&Manager{store: store}).webDashboard() != nil {
		t.Fatal("failed Serve cleanup kept process alive")
	}
	if _, err := os.Stat(filepath.Join(store, "web-access.json")); !os.IsNotExist(err) {
		t.Fatal("failed Serve cleanup retained credential")
	}
}

func TestWebOwnershipRefusals(t *testing.T) {
	m, _, _ := manager(t)
	start, err := processStart(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	state := webState{PID: os.Getpid(), ProcessStart: start + "wrong", Address: "127.0.0.1:8123"}
	if saveWebFile(filepath.Join(m.store, "web-launch-process.json"), state) != nil {
		t.Fatal("fixture write")
	}
	if privateDir(filepath.Join(m.store, "web-launch")) != nil {
		t.Fatal("fixture directory")
	}
	if saveWebFile(filepath.Join(m.store, "web-launch", "process.json"), state) != nil {
		t.Fatal("fixture write")
	}
	if m.stopWeb(context.Background()) == nil {
		t.Fatal("mismatched live process was not refused")
	}
	state.ProcessStart = start
	state.Host = "mac.tail123.ts.net"
	state.Address = "" // v0.2.9 record
	if saveWebFile(filepath.Join(m.store, "web.json"), state) != nil {
		t.Fatal("legacy write")
	}
	if m.webDashboard() == nil {
		t.Fatal("legacy foreground dashboard not recognized")
	}
}

func TestBackgroundStartFailureCleanup(t *testing.T) {
	m, c, store := manager(t)
	cliFixture(t, c, store)
	dir := fakeTailscale(t, fixtureTailnetStatus)
	if os.Symlink(fakeBinary, filepath.Join(dir, "launchctl")) != nil {
		t.Fatal("fake launchctl setup")
	}
	t.Setenv("NUNTIUS_FAKE_LAUNCHCTL", dir)
	prevOS, prevExe := webPlatform, webExecutable
	webPlatform = "darwin"
	webExecutable = func() (string, error) { return fakeBinary, nil }
	defer func() { webPlatform, webExecutable = prevOS, prevExe }()
	if os.WriteFile(filepath.Join(dir, "kickstart-fail"), nil, 0600) != nil {
		t.Fatal("fixture setup")
	}
	var out, errs bytes.Buffer
	if CLI(context.Background(), []string{"web", "start", "--bind", "127.0.0.1:0"}, strings.NewReader(""), &out, &errs) == 0 {
		t.Fatal("failed launch reported success")
	}
	if _, err := os.Stat(filepath.Join(dir, "job.path")); !os.IsNotExist(err) {
		t.Fatal("failed launch retained its job")
	}
	if m.webDashboard() != nil || strings.Contains(out.String(), "?token=") {
		t.Fatal("failed launch left access")
	}
	cleanupFixtureWeb(t, filepath.Join(store, "web-fixture"))
}
