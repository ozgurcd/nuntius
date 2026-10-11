package nuntius

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWebEnsureMCPContract(t *testing.T) {
	found := false
	for _, tool := range tools() {
		if tool.Name == "nuntius_web_ensure" {
			found = true
			if tool.Annotations["readOnlyHint"] != false || tool.Annotations["idempotentHint"] != true || tool.InputSchema["additionalProperties"] != false {
				t.Fatal("ensure must be state-changing, idempotent and closed-schema")
			}
		}
	}
	if !found {
		t.Fatal("judge-only ensure tool missing")
	}
	m, _, _ := manager(t)
	for _, input := range []string{`null`, ` null `, `[]`, `{"path":"/tmp"}`, `{"host":"foreign.ts.net"}`, `{"recipient":"synthetic@example.test"}`, `{"pid":99}`, `{"token":"synthetic-grant"}`, `{"funnel":true}`, strings.Repeat(" ", 4097) + `{}`} {
		if _, err := call(m, "nuntius_web_ensure", json.RawMessage(input)); err == nil {
			t.Fatal("unsafe request accepted")
		}
	}
	if _, err := call(m, "nuntius_web_unknown", json.RawMessage(`{}`)); err == nil {
		t.Fatal("unknown tool accepted")
	}
}

func TestWebEnsureMappingCleanupRefusesForeign(t *testing.T) {
	dir := fakeTailscale(t, fixtureTailnetStatus)
	exe, err := findTailscale()
	if err != nil {
		t.Fatal(err)
	}
	tn := &tailnet{exe: exe, host: "mac.tail123.ts.net"}
	if _, err := tn.ensure("8123"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "serve.json"), []byte(`{"TCP":{"443":{"TCPForward":"127.0.0.1:9999"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if tn.release() == nil {
		t.Fatal("foreign mapping cleanup must refuse")
	}
	if strings.Contains(strings.Join(fakeCalls(t, dir, "calls.log"), "\n"), "serve --https=443 off") {
		t.Fatal("foreign mapping removed")
	}
}

func TestWebEnsureServeValidation(t *testing.T) {
	for _, data := range []string{`{"TCP":{"443":{"HTTPS":true,"Unknown":true}},"Web":{"mac.tail123.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:8123"}}}}}`, `{"TCP":null,"TCP":{}}`, `{"Web":{"foreign.ts.net:0443":{"Handlers":{}}}}`, `{"AllowFunnel":null,"Unexpected":true}`} {
		if _, err := servePlan([]byte(data), "mac.tail123.ts.net", "8123"); err == nil {
			t.Fatal("ambiguous Serve config accepted")
		}
	}
}

const fixtureEnsureGrant = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type ensureFixture struct {
	m                            *Manager
	d                            webEnsureRuntime
	state                        webState
	status, mapping              string
	processes                    map[int]string
	starts, stops, writes, sends int
	probeOK, portOK              bool
	writeErr, notifyErr          bool
	readback                     string
	startedArgs                  []string
	newGrant                     string
}

func newEnsureFixture(t *testing.T, live bool) *ensureFixture {
	t.Helper()
	m, _, _ := manager(t)
	f := &ensureFixture{m: m, status: fixtureTailnetStatus, mapping: fixtureEnsureMapping("8123"), processes: map[int]string{}, probeOK: true, portOK: true, newGrant: strings.Repeat("b", 64)}
	f.state = webState{PID: 42, ProcessStart: "fixture-start", Address: ensureAddress, Host: "mac.tail123.ts.net", Started: time.Unix(100, 0)}
	previous := webPlatform
	webPlatform = "linux"
	t.Cleanup(func() { webPlatform = previous })
	write := func(s webState, grant string) {
		if m.saveWebState(s) != nil || saveWebFile(filepath.Join(m.store, "web-access.json"), webAccess{PID: s.PID, ProcessStart: s.ProcessStart, Token: grant}) != nil {
			t.Fatal("fixture state write")
		}
	}
	if live {
		f.processes[42] = f.state.ProcessStart
		write(f.state, fixtureEnsureGrant)
	}
	f.d = webEnsureRuntime{
		find: func() (string, error) { return "fake-tailscale", nil },
		run: func(ctx context.Context, exe string, args ...string) ([]byte, error) {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			switch strings.Join(args, " ") {
			case "status --json":
				return []byte(f.status), nil
			case "serve status --json":
				return []byte(f.mapping), nil
			case "serve --bg --https=443 localhost:8123":
				f.writes++
				f.mapping = fixtureEnsureMapping("8123")
				if f.readback != "" {
					f.mapping = f.readback
				}
				if f.writeErr {
					return nil, errors.New("synthetic-grant synthetic@example.test")
				}
				return nil, nil
			case "serve --https=443 off":
				f.writes++
				f.mapping = "null"
				return nil, nil
			default:
				t.Fatal("non-fixed Tailscale argv")
				return nil, errors.New("unexpected argv")
			}
		},
		process: func(pid int) (string, error) {
			if s, ok := f.processes[pid]; ok {
				return s, nil
			}
			return "", errWebProcessAbsent
		},
		launch: func(context.Context, ...string) ([]byte, error) { return nil, errJobAbsent },
		signal: func(pid int) error { f.stops++; delete(f.processes, pid); return nil },
		probe: func(ctx context.Context, s webState, grant string) bool {
			return ctx.Err() == nil && f.probeOK && (grant == fixtureEnsureGrant || grant == f.newGrant)
		},
		port: func(string) bool { return f.portOK },
		start: func(ctx context.Context, args []string) error {
			f.starts++
			f.startedArgs = append([]string{}, args...)
			f.state.PID++
			f.state.ProcessStart = "new-start"
			f.state.Host = "mac.tail123.ts.net"
			f.processes[f.state.PID] = f.state.ProcessStart
			write(f.state, f.newGrant)
			f.probeOK = true
			f.mapping = fixtureEnsureMapping("8123")
			return nil
		},
		notify: func(ctx context.Context, handle, text string) error {
			f.sends++
			if handle != "synthetic@example.test" || !strings.Contains(text, "?token="+f.newGrant) {
				t.Fatal("incorrect private notification")
			}
			if f.notifyErr {
				return errors.New("synthetic private notification failure")
			}
			return nil
		},
	}
	m.webEnsure = &f.d
	return f
}

func fixtureEnsureMapping(port string) string {
	return `{"TCP":{"443":{"HTTPS":true}},"Web":{"mac.tail123.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:` + port + `"}}}}}`
}

func ensureCall(t *testing.T, m *Manager) webEnsureResult {
	t.Helper()
	v, err := call(m, "nuntius_web_ensure", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal("ensure call refused before typed result")
	}
	r, ok := v.(webEnsureResult)
	if !ok {
		t.Fatal("missing state result")
	}
	b, _ := json.Marshal(r)
	for _, private := range []string{fixtureEnsureGrant, strings.Repeat("b", 64), "synthetic@example.test", "?token=", "http", m.store} {
		if strings.Contains(string(b), private) {
			t.Fatal("private material in result")
		}
	}
	return r
}

func TestWebEnsureRecovery(t *testing.T) {
	for _, tc := range []struct {
		name          string
		live          bool
		setup         func(*ensureFixture)
		operation     string
		starts, stops int
	}{
		{"healthy", true, func(f *ensureFixture) {}, "none", 0, 0},
		{"mapping repair", true, func(f *ensureFixture) { f.mapping = "null" }, "serve_repaired", 0, 0},
		{"absent", false, func(f *ensureFixture) { f.mapping = "null" }, "started", 1, 0},
		{"stale", true, func(f *ensureFixture) { delete(f.processes, 42) }, "started", 1, 0},
		{"unhealthy", true, func(f *ensureFixture) { f.probeOK = false }, "restarted", 1, 1},
		{"wrong host policy", true, func(f *ensureFixture) { f.state.Host = ""; _ = f.m.saveWebState(f.state) }, "restarted", 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEnsureFixture(t, tc.live)
			tc.setup(f)
			r := ensureCall(t, f.m)
			if r.Reason != "ready" || r.Local != "ready" || r.Tailnet != "ready" || r.Operation != tc.operation || r.RemoteClient != "unknown" || f.starts != tc.starts || f.stops != tc.stops {
				t.Fatal("recovery state or operation mismatch")
			}
			if f.sends != 0 {
				t.Fatal("disabled notifications sent")
			}
			if tc.operation == "none" && f.writes != 0 {
				t.Fatal("healthy reuse wrote mapping")
			}
			if f.starts > 0 && (!strings.Contains(strings.Join(f.startedArgs, " "), "--tailnet --allow-host mac.tail123.ts.net --ensure") || strings.Contains(strings.Join(f.startedArgs, " "), "optional")) {
				t.Fatal("start did not require exact tailnet policy")
			}
			var a webAccess
			if readWebFile(filepath.Join(f.m.store, "web-access.json"), &a) != nil {
				t.Fatal("access disappeared")
			}
			if tc.starts == 0 && a.Token != fixtureEnsureGrant {
				t.Fatal("repair rotated grant")
			}
			if tc.starts > 0 && a.Token == fixtureEnsureGrant {
				t.Fatal("restart reused grant")
			}
			next := ensureCall(t, f.m)
			if next.Operation != "none" || f.starts != tc.starts || f.stops != tc.stops {
				t.Fatal("ensure is not idempotent")
			}
		})
	}
}

func TestWebEnsureRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(*testing.T, *ensureFixture)
		reason string
	}{
		{"reused pid", func(t *testing.T, f *ensureFixture) { f.processes[42] = "foreign-start" }, "identity_refused"},
		{"unknown job", func(t *testing.T, f *ensureFixture) { f.state.Job = "gui/1/foreign"; _ = f.m.saveWebState(f.state) }, "identity_refused"},
		{"malformed", func(t *testing.T, f *ensureFixture) {
			_ = os.WriteFile(filepath.Join(f.m.store, "web.json"), []byte("not json"), 0600)
		}, "state_unavailable"},
		{"linked", func(t *testing.T, f *ensureFixture) {
			p := filepath.Join(f.m.store, "web.json")
			_ = os.Rename(p, p+".original")
			_ = os.Symlink(p+".original", p)
		}, "state_unavailable"},
		{"unreadable", func(t *testing.T, f *ensureFixture) { _ = os.Chmod(filepath.Join(f.m.store, "web.json"), 0000) }, "state_unavailable"},
		{"unavailable identity", func(t *testing.T, f *ensureFixture) {
			f.d.process = func(int) (string, error) { return "", errors.New("permission error") }
		}, "identity_unavailable"},
		{"bad access identity", func(t *testing.T, f *ensureFixture) {
			_ = saveWebFile(filepath.Join(f.m.store, "web-access.json"), webAccess{PID: 99, ProcessStart: "other", Token: fixtureEnsureGrant})
		}, "identity_refused"},
		{"missing access", func(t *testing.T, f *ensureFixture) { _ = os.Remove(filepath.Join(f.m.store, "web-access.json")) }, "access_unavailable"},
		{"foreign 443", func(t *testing.T, f *ensureFixture) { f.mapping = fixtureEnsureMapping("8124") }, "serve_refused"},
		{"Funnel another port", func(t *testing.T, f *ensureFixture) { f.mapping = `{"AllowFunnel":{"mac.tail123.ts.net:8443":true}}` }, "serve_refused"},
		{"bad serve JSON", func(t *testing.T, f *ensureFixture) { f.mapping = "startup failed" }, "serve_refused"},
		{"exit zero text status", func(t *testing.T, f *ensureFixture) { f.status = "startup failed" }, "tailnet_unavailable"},
		{"inactive", func(t *testing.T, f *ensureFixture) { f.status = `{"BackendState":"Stopped"}` }, "tailnet_inactive"},
		{"no name", func(t *testing.T, f *ensureFixture) { f.status = `{"BackendState":"Running","Self":{}}` }, "tailnet_name_unavailable"},
		{"no certificate", func(t *testing.T, f *ensureFixture) {
			f.status = `{"BackendState":"Running","Self":{"DNSName":"mac.tail123.ts.net."}}`
		}, "tailnet_certificate_unavailable"},
		{"certificate differs", func(t *testing.T, f *ensureFixture) {
			f.status = `{"BackendState":"Running","Self":{"DNSName":"mac.tail123.ts.net."},"CertDomains":["foreign.tail123.ts.net"]}`
		}, "tailnet_certificate_unavailable"},
		{"port occupied", func(t *testing.T, f *ensureFixture) { delete(f.processes, 42); f.portOK = false }, "port_occupied"},
		{"start pending", func(t *testing.T, f *ensureFixture) {
			_ = privateDir(filepath.Join(f.m.store, "web-launch"))
			s := f.state
			s.PID = 43
			f.processes[43] = s.ProcessStart
			_ = saveWebFile(filepath.Join(f.m.store, "web-launch", "process.json"), s)
		}, "start_pending"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEnsureFixture(t, true)
			tc.setup(t, f)
			r := ensureCall(t, f.m)
			if r.Reason != tc.reason || f.starts != 0 || f.stops != 0 || f.writes != 0 || f.sends != 0 {
				t.Fatal("refusal changed lifecycle or wrong reason")
			}
		})
	}
}

func TestWebEnsureNotification(t *testing.T) {
	for _, mode := range []string{"disabled", "sent", "failed"} {
		t.Run(mode, func(t *testing.T) {
			f := newEnsureFixture(t, false)
			f.m.mu.Lock()
			f.m.config.NotifyIMessage = "synthetic@example.test"
			enabled := mode != "disabled"
			f.m.config.NotifyIMessageEnabled = &enabled
			f.m.mu.Unlock()
			f.notifyErr = mode == "failed"
			r := ensureCall(t, f.m)
			if r.Reason != "ready" || r.Notification != mode || !r.SignIn {
				t.Fatal("notification changed healthy readiness")
			}
			if enabled && f.sends != 1 || !enabled && f.sends != 0 {
				t.Fatal("notification policy mismatch")
			}
			_ = ensureCall(t, f.m)
			if f.sends > 1 {
				t.Fatal("reused grant resent")
			}
		})
	}
}

func TestWebEnsurePartialMappingCleanup(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(strconv.FormatBool(foreign), func(t *testing.T) {
			f := newEnsureFixture(t, true)
			f.mapping = "null"
			f.writeErr = true
			if foreign {
				f.readback = fixtureEnsureMapping("9999")
			}
			r := ensureCall(t, f.m)
			if r.Reason != "serve_readback_failed" || f.starts != 0 || f.stops != 0 {
				t.Fatal("partial mapping incorrectly succeeded")
			}
			if foreign && f.writes != 1 || !foreign && f.writes != 2 {
				t.Fatal("cleanup did not respect current mapping")
			}
		})
	}
}

func TestWebEnsureLateRepairFailureCleanup(t *testing.T) {
	f := newEnsureFixture(t, true)
	f.mapping = "null"
	run := f.d.run
	f.d.run = func(ctx context.Context, exe string, args ...string) ([]byte, error) {
		b, err := run(ctx, exe, args...)
		if strings.Join(args, " ") == "serve --bg --https=443 localhost:8123" {
			delete(f.processes, 42)
		}
		return b, err
	}
	r := ensureCall(t, f.m)
	if r.Reason != "readiness_unavailable" || f.writes != 2 || f.starts != 0 || f.stops != 0 {
		t.Fatal("late readiness failure retained created mapping")
	}
}

func TestWebEnsureConcurrencyAndCancellation(t *testing.T) {
	f := newEnsureFixture(t, false)
	entered, release := make(chan struct{}), make(chan struct{})
	start := f.d.start
	f.d.start = func(ctx context.Context, args []string) error {
		close(entered)
		select {
		case <-release:
			return start(ctx, args)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	var wg sync.WaitGroup
	wg.Add(1)
	var first webEnsureResult
	go func() { defer wg.Done(); first = f.m.ensureWeb(context.Background()) }()
	<-entered
	busy := f.m.ensureWeb(context.Background())
	if busy.Reason != "busy" {
		t.Fatal("concurrent ensure did not return bounded busy")
	}
	// The same lifecycle lock gates CLI and helper-driven CLI operations.
	if guard, err := f.m.lock("web-control"); err == nil {
		guard.Close()
		t.Fatal("CLI lifecycle lock not shared")
	}
	close(release)
	wg.Wait()
	if first.Reason != "ready" || f.starts != 1 {
		t.Fatal("concurrent ensure duplicated start")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if f.m.ensureWeb(ctx).Reason != "cancelled" {
		t.Fatal("cancelled operation proceeded")
	}
	ctx, cancel = context.WithDeadline(context.Background(), time.Unix(1, 0))
	defer cancel()
	if f.m.ensureWeb(ctx).Reason != "cancelled" {
		t.Fatal("expired operation proceeded")
	}
	// A stop that cannot be proved complete must never spawn a replacement.
	f.probeOK = false
	f.d.signal = func(int) error { f.stops++; cancel(); return nil }
	ctx, cancel = context.WithCancel(context.Background())
	r := f.m.ensureWeb(ctx)
	cancel()
	if r.Reason != "stop_pending" || f.starts != 1 {
		t.Fatal("uncertain stop spawned duplicate")
	}
}

func TestWebEnsureAuthenticatedProbe(t *testing.T) {
	var requests int
	var redirect atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if redirect.Load() {
			http.Redirect(w, r, "http://127.0.0.1:1/", http.StatusFound)
			return
		}
		requests++
		if r.Host != "mac.tail123.ts.net" || r.URL.Path != "/data" || r.URL.RawQuery != "" || r.Header.Get("X-Nuntius-Token") != fixtureEnsureGrant {
			t.Error("probe is not exact-host header-only")
		}
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(200)
	}))
	defer server.Close()
	state := webState{Address: strings.TrimPrefix(server.URL, "http://"), Host: "mac.tail123.ts.net"}
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	if !ensureProbe(context.Background(), state, fixtureEnsureGrant) || requests != 1 {
		t.Fatal("authenticated loopback readiness unavailable")
	}
	redirect.Store(true)
	if ensureProbe(context.Background(), state, fixtureEnsureGrant) {
		t.Fatal("redirect readiness accepted")
	}
}

func TestWebEnsureStdioAndFrozen(t *testing.T) {
	f := newEnsureFixture(t, true)
	if f.m.Freeze("fixture hold") != nil {
		t.Fatal("freeze fixture")
	}
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"nuntius_web_ensure","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"nuntius_web_ensure","arguments":{"recipient":"synthetic@example.test","token":"` + fixtureEnsureGrant + `"}}}`,
	}, "\n") + "\n"
	var out bytes.Buffer
	if Serve(context.Background(), f.m, strings.NewReader(input), &out) != nil {
		t.Fatal("stdio ensure failed")
	}
	audit, err := os.ReadFile(filepath.Join(f.m.store, "audit.jsonl"))
	if err != nil {
		t.Fatal("audit absent")
	}
	for _, s := range []string{fixtureEnsureGrant, "synthetic@example.test", f.m.store, "?token="} {
		if strings.Contains(out.String(), s) || bytes.Contains(audit, []byte(s)) {
			t.Fatal("private content in stdio/audit")
		}
	}
	if !strings.Contains(out.String(), `\"operation\":\"none\"`) || !strings.Contains(out.String(), `"isError":true`) {
		t.Fatal("stdio state or refusal missing")
	}
	view, err := f.m.StatusView()
	if err != nil || view.(map[string]any)["frozen"] != true || f.starts != 0 || f.stops != 0 {
		t.Fatal("ensure thawed or dispatched agents")
	}
	v, err := call(nil, "nuntius_web_ensure", json.RawMessage(`{}`))
	if err != nil || v.(webEnsureResult).Reason != "unavailable" {
		t.Fatal("unavailable owner MCP not typed")
	}
}

func TestWebEnsureCompiledTransports(t *testing.T) {
	dir := fakeTailscale(t, fixtureTailnetStatus)
	if os.Symlink(fakeBinary, filepath.Join(dir, "launchctl")) != nil {
		t.Fatal("fake launchctl setup")
	}
	t.Setenv("NUNTIUS_FAKE_LAUNCHCTL", dir)
	m, _, _ := manager(t)
	d := m.ensureAdapters()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	exe, err := d.find()
	if err != nil {
		t.Fatal("fake CLI discovery")
	}
	if host, reason := ensureTailnet(ctx, d, exe); host != "mac.tail123.ts.net" || reason != "" {
		t.Fatal("compiled status adapter")
	}
	if _, err := d.launch(ctx, "print", "gui/"+strconv.Itoa(os.Getuid())+"/nuntius.web"); !errors.Is(err, errJobAbsent) {
		t.Fatal("compiled launchd absence")
	}
	if sendIMessage(ctx, "synthetic@example.test", "https://mac.tail123.ts.net/?token="+fixtureEnsureGrant) != nil {
		t.Fatal("fixed Messages stdin transport")
	}
	if logs := strings.Join(fakeCalls(t, dir, "osascript.log"), "\n"); strings.Contains(logs, fixtureEnsureGrant) || strings.Contains(logs, "synthetic@example.test") || !strings.Contains(logs, `["-"]`) {
		t.Fatal("Messages data entered argv/log")
	}
	if os.WriteFile(filepath.Join(dir, "status.json"), []byte(strings.Repeat("x", 65537)), 0600) != nil {
		t.Fatal("bounded output fixture")
	}
	if _, err := d.run(ctx, exe, "status", "--json"); err == nil {
		t.Fatal("oversized command output accepted")
	}
	cancel()
	if _, err := d.run(ctx, exe, "status", "--json"); err == nil {
		t.Fatal("cancelled command adapter ignored deadline")
	}
}

func TestWebEnsureLaunchOwnership(t *testing.T) {
	for _, mode := range []string{"unknown", "wrong_pid", "unavailable", "owned"} {
		t.Run(mode, func(t *testing.T) {
			f := newEnsureFixture(t, true)
			webPlatform = "darwin"
			f.state.Job = "gui/" + strconv.Itoa(os.Getuid()) + "/nuntius.web"
			_ = f.m.saveWebState(f.state)
			f.d.launch = func(context.Context, ...string) ([]byte, error) {
				switch mode {
				case "unknown":
					return []byte("state = waiting"), nil
				case "wrong_pid":
					return []byte("pid = 99"), nil
				case "unavailable":
					return nil, errors.New("private launch error")
				default:
					return []byte("pid = 42"), nil
				}
			}
			r := ensureCall(t, f.m)
			if mode == "owned" && r.Reason != "ready" || mode != "owned" && r.Reason == "ready" || f.starts != 0 || f.stops != 0 {
				t.Fatal("launchd ownership not enforced")
			}
		})
	}
}

func TestWebEnsureStateModesAndRequestGuards(t *testing.T) {
	f := newEnsureFixture(t, true)
	_ = ensureCall(t, f.m)
	for _, name := range []string{"web.json", "web-access.json", "locks/web-control.lock"} {
		st, err := os.Stat(filepath.Join(f.m.store, name))
		if err != nil || st.Mode().Perm() != 0600 {
			t.Fatal("private file mode changed")
		}
	}
	for _, name := range []string{"", "locks"} {
		st, err := os.Stat(filepath.Join(f.m.store, name))
		if err != nil || st.Mode().Perm() != 0700 {
			t.Fatal("private directory mode changed")
		}
	}
	w, err := NewWeb(f.m, ensureAddress, "mac.tail123.ts.net")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, host, origin, path string
		header                     bool
		want                       int
	}{
		{"GET", "foreign.ts.net", "", "/", false, 403},
		{"POST", "mac.tail123.ts.net", "http://mac.tail123.ts.net", "/action", true, 403},
		{"POST", "mac.tail123.ts.net", "https://foreign.ts.net", "/action", true, 403},
		{"GET", "mac.tail123.ts.net", "", "/data?token=" + w.token, false, 403},
		{"GET", "mac.tail123.ts.net", "", "/action", true, 405},
		{"GET", "mac.tail123.ts.net", "", "/ensure", false, 403},
		{"GET", "mac.tail123.ts.net", "", "/", false, 200},
	} {
		r := httptest.NewRequest(tc.method, "http://"+tc.host+tc.path, nil)
		r.Header.Set("Origin", tc.origin)
		if tc.header {
			r.Header.Set("X-Nuntius-Token", w.token)
		}
		out := httptest.NewRecorder()
		w.ServeHTTP(out, r)
		if out.Code != tc.want || !strings.Contains(out.Header().Get("Content-Security-Policy"), "default-src 'none'") {
			t.Fatal("request guard weakened")
		}
	}
	if _, err := NewWeb(f.m, "0.0.0.0:8123", "mac.tail123.ts.net"); err == nil {
		t.Fatal("nonloopback bind accepted")
	}
}

func TestWebEnsureNativeBackground(t *testing.T) {
	m, c, store := manager(t)
	cliFixture(t, c, store)
	m.mu.Lock()
	m.config.SourcePath = os.Getenv("NUNTIUS_CONFIG")
	m.mu.Unlock()
	dir := fakeTailscale(t, fixtureTailnetStatus)
	if os.Symlink(fakeBinary, filepath.Join(dir, "launchctl")) != nil {
		t.Fatal("fake launchctl setup")
	}
	t.Setenv("NUNTIUS_FAKE_LAUNCHCTL", dir)
	previousOS, previousExe := webPlatform, webExecutable
	webPlatform = "darwin"
	webExecutable = func() (string, error) { return fakeBinary, nil }
	t.Cleanup(func() {
		var output bytes.Buffer
		_ = m.webCommand(context.Background(), []string{"stop"}, &output, &output)
		webPlatform, webExecutable = previousOS, previousExe
	})
	// Preserve an owner-selected ephemeral fixture address. Never use the owner's port.
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := l.Addr().String()
	_ = l.Close()
	if m.saveWebState(webState{PID: 99999999, ProcessStart: "stale-fixture", Address: address, Host: "mac.tail123.ts.net"}) != nil {
		t.Fatal("stale state fixture")
	}
	r := ensureCall(t, m)
	if r.Reason != "ready" || r.Operation != "started" || !r.SignIn {
		t.Fatalf("native fake-only start unavailable: %s", r.Reason)
	}
	state := m.webDashboard()
	if state == nil {
		t.Fatal("native process identity absent")
	}
	var original webAccess
	if readWebFile(filepath.Join(store, "web-access.json"), &original) != nil {
		t.Fatal("private fixture access absent")
	}
	if r := ensureCall(t, m); r.Reason != "ready" || r.Operation != "none" {
		t.Fatal("native idempotence")
	}
	if os.WriteFile(filepath.Join(dir, "serve.json"), []byte("null"), 0600) != nil {
		t.Fatal("missing mapping fixture")
	}
	if r := ensureCall(t, m); r.Reason != "ready" || r.Operation != "serve_repaired" {
		t.Fatal("native mapping-only repair")
	}
	var after webAccess
	if readWebFile(filepath.Join(store, "web-access.json"), &after) != nil || after.Token != original.Token || after.PID != original.PID {
		t.Fatal("native repair rotated access")
	}
	for _, name := range []string{"web.log", "web-launch/job.plist", "web-launch/request.json", "web.json", "web-serve.json"} {
		b, err := os.ReadFile(filepath.Join(store, name))
		if err != nil || bytes.Contains(b, []byte(after.Token)) || bytes.Contains(b, []byte("?token=")) {
			t.Fatal("native grant leak")
		}
	}
	for _, name := range []string{"calls.log", "launchctl.log", "osascript.log"} {
		b, _ := os.ReadFile(filepath.Join(dir, name))
		if bytes.Contains(b, []byte(after.Token)) {
			t.Fatal("credential in fake transport argv")
		}
	}
}
