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
	"strings"
	"syscall"
	"testing"
)

func TestPhoneAccessSettingsRoundTrip(t *testing.T) {
	m, _, _ := manager(t)
	var change settingsChanges
	if json.Unmarshal([]byte(`{"phone_access":true}`), &change) != nil {
		t.Fatal("decode")
	}
	next, _, err := m.settingsCandidate(m.config, change, true)
	if err != nil || settingsSnapshot(next)["tailnet"] != true {
		t.Fatal("saved phone access unavailable")
	}
}

func TestPhoneConfigBoolean(t *testing.T) {
	_, c, _ := manager(t)
	b, _ := json.Marshal(c)
	var fields map[string]any
	_ = json.Unmarshal(b, &fields)
	path := filepath.Join(t.TempDir(), "config.json")
	for _, value := range []any{nil, "on", 1, true, false} {
		fields["phone_access"] = value
		b, _ = json.Marshal(fields)
		if os.WriteFile(path, b, 0600) != nil {
			t.Fatal("config fixture")
		}
		_, err := LoadConfig(path)
		_, valid := value.(bool)
		if (err == nil) != valid {
			t.Fatal("phone_access did not require a JSON boolean")
		}
	}
}

func TestMenubarPhoneActionsRefusalReasons(t *testing.T) {
	m, _, _ := manager(t)
	w, _ := NewWeb(m, "127.0.0.1:0")
	for _, action := range []string{"qr", "phone"} {
		out := httptest.NewRecorder()
		w.menubarHandler().ServeHTTP(out, httptest.NewRequest("POST", "http://unix/"+action, nil))
		if out.Code != 409 || !strings.Contains(out.Body.String(), "phone_access_off") {
			t.Fatal("phone-off reason missing")
		}
		if strings.Contains(out.Body.String(), w.token) {
			t.Fatal("private grant exposed")
		}
	}
}

func TestPhoneSettingsAuthenticatedSave(t *testing.T) {
	m, c, store := manager(t)
	cliFixture(t, c, store)
	m.config.SourcePath = os.Getenv("NUNTIUS_CONFIG")
	w, _ := NewWeb(m, "127.0.0.1:9911")
	request := func(method, body string, auth bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://127.0.0.1:9911/settings", strings.NewReader(body))
		if auth {
			r.Header.Set("X-Nuntius-Token", w.token)
		}
		r.Header.Set("Origin", "http://127.0.0.1:9911")
		r.Header.Set("Content-Type", "application/json")
		out := httptest.NewRecorder()
		w.ServeHTTP(out, r)
		return out
	}
	var state struct {
		Revision    string
		PhoneAccess bool `json:"phone_access"`
	}
	get := request("GET", "", true)
	if json.Unmarshal(get.Body.Bytes(), &state) != nil || state.PhoneAccess {
		t.Fatal("default not off")
	}
	body := `{"schema_version":1,"expected_revision":"` + state.Revision + `","changes":{"phone_access":true}}`
	if request("POST", body, false).Code != 403 {
		t.Fatal("unsigned phone save accepted")
	}
	if response := request("POST", body, true); response.Code != 200 {
		var fault struct{ Code string }
		_ = json.Unmarshal(response.Body.Bytes(), &fault)
		t.Fatalf("phone-only save refused: %d %s", response.Code, fault.Code)
	}
	if request("POST", body, true).Code != 409 {
		t.Fatal("stale save accepted")
	}
	actual, err := LoadConfig(m.config.SourcePath)
	if err != nil || !actual.PhoneAccess || actual.NotifyIMessage != c.NotifyIMessage || actual.ReportsDir != c.ReportsDir {
		t.Fatal("settings preservation")
	}
	if m.webDashboard() != nil {
		t.Fatal("save started service")
	}
	var out bytes.Buffer
	if CLI(context.Background(), []string{"set", "phone-access", "off"}, strings.NewReader(""), &out, &out) != 0 {
		t.Fatal("CLI phone toggle")
	}
	actual, _ = LoadConfig(m.config.SourcePath)
	if actual.PhoneAccess {
		t.Fatal("CLI failed to turn off")
	}
	for _, invalid := range []string{`{"phone_access":"on"}`, `{"phone_access":1}`, `{"phone_access":null}`} {
		var change settingsRequest
		if settingsJSON([]byte(`{"schema_version":1,"expected_revision":"fixture","changes":`+invalid+`}`), &change) == nil {
			t.Fatal("nonboolean setting accepted")
		}
	}
}

func TestMenubarPhonePrerequisitesAndStaleClick(t *testing.T) {
	m, _, _ := manager(t)
	w, _ := NewWeb(m, "127.0.0.1:0")
	w.allowHost = "fixture.ts.net"
	w.phoneCheck = func(context.Context) string { return "" }
	oldSend, oldAvailable, oldOpen := menubarSend, phoneMessagesAvailable, menubarOpenFile
	defer func() { menubarSend, phoneMessagesAvailable, menubarOpenFile = oldSend, oldAvailable, oldOpen }()
	phoneMessagesAvailable = func() bool { return true }
	sent, opened := 0, 0
	menubarSend = func(context.Context, string, string) error { sent++; return nil }
	menubarOpenFile = func(context.Context, string) error { opened++; return nil }
	request := func(a string) *httptest.ResponseRecorder {
		out := httptest.NewRecorder()
		w.menubarHandler().ServeHTTP(out, httptest.NewRequest("POST", "http://unix/"+a, nil))
		return out
	}
	for _, test := range []struct {
		code     string
		notify   string
		enabled  bool
		ready    string
		messages bool
	}{
		{"recipient_missing", "", true, "", true},
		{"notifications_off", "synthetic@example.test", false, "", true},
		{"messages_unavailable", "synthetic@example.test", true, "", false},
		{"tailnet_not_running", "synthetic@example.test", true, "tailnet_not_running", true},
		{"readiness_unavailable", "synthetic@example.test", true, "readiness_unavailable", true},
	} {
		t.Run(test.code, func(t *testing.T) {
			m.config.NotifyIMessage = test.notify
			m.config.NotifyIMessageEnabled = &test.enabled
			w.phoneCheck = func(context.Context) string { return test.ready }
			phoneMessagesAvailable = func() bool { return test.messages }
			actions, _ := w.phoneCapabilities(context.Background())
			if actions["phone"].Code != test.code || actions["phone"].Enabled {
				t.Fatal("wrong capability")
			}
			out := request("phone")
			if !strings.Contains(out.Body.String(), test.code) || sent != 0 {
				t.Fatal("refusal sent link")
			}
			if test.ready == "" && request("qr").Code != 200 {
				t.Fatal("send prerequisite disabled QR")
			}
		})
	}
	m.config.NotifyIMessage = "latest@example.test"
	m.config.NotifyIMessageEnabled = nil
	w.phoneCheck = func(context.Context) string { return "" }
	phoneMessagesAvailable = func() bool { return true }
	actions, _ := w.phoneCapabilities(context.Background())
	if !actions["phone"].Enabled {
		t.Fatal("positive capability")
	}
	beforeOpen := opened
	w.phoneCheck = func(context.Context) string { return "tailnet_not_running" }
	if request("phone").Code != 409 || request("qr").Code != 409 || sent != 0 || opened != beforeOpen {
		t.Fatal("stale enabled click performed side effect")
	}
}

func TestMenubarStartUsesSavedPhoneAccess(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(map[bool]string{true: "on", false: "off"}[on], func(t *testing.T) {
			_, c, store := manager(t)
			c.PhoneAccess = on
			cliFixture(t, c, store)
			dir := fakeTailscale(t, fixtureTailnetStatus)
			if os.Symlink(fakeBinary, filepath.Join(dir, "launchctl")) != nil {
				t.Fatal("fake launchctl")
			}
			t.Setenv("NUNTIUS_FAKE_LAUNCHCTL", dir)
			prevOS, prevExe := webPlatform, webExecutable
			webPlatform = "darwin"
			webExecutable = func() (string, error) { return fakeBinary, nil }
			call := func(args ...string) int {
				var out bytes.Buffer
				return CLI(context.Background(), append([]string{"web"}, args...), strings.NewReader(""), &out, &out)
			}
			t.Cleanup(func() { _ = call("stop"); webPlatform, webExecutable = prevOS, prevExe })
			if call("start", "--no-menubar", "--bind", "127.0.0.1:0") != 0 {
				t.Fatal("menu start failed")
			}
			var request webRequest
			if readWebFile(filepath.Join(store, "web-launch", "request.json"), &request) != nil {
				t.Fatal("canonical argv missing")
			}
			joined := strings.Join(request.Args, " ")
			if strings.Contains(joined, "--tailnet") != on || strings.Contains(joined, "?token=") || strings.Contains(joined, "--notify-imessage") {
				t.Fatal("saved phone argv or privacy")
			}
			state := (&Manager{store: store}).webDashboard()
			if state == nil || (state.Host != "") != on {
				t.Fatal("wrong effective phone mode")
			}
			if on {
				if call("start", "--replace", "--no-menubar", "--bind", "127.0.0.1:0", "--tailnet=false") != 0 {
					t.Fatal("explicit loopback override refused")
				}
				if state := (&Manager{store: store}).webDashboard(); state == nil || state.Host != "" {
					t.Fatal("saved setting overrode explicit flag")
				}
				if call("start", "--no-menubar") != 1 {
					t.Fatal("loopback mismatch silently accepted")
				}
			}
		})
	}
}

func TestMenubarOptionalFallbackReasonSurvives(t *testing.T) {
	m, _, _ := manager(t)
	w, _ := NewWeb(m, "127.0.0.1:0")
	for _, startup := range []string{"stopped", "missing", "dns", "https", "serve_conflict", "funnel", "serve_readback", "status"} {
		w.phoneFallback = phoneStartupReason(startup)
		out := httptest.NewRecorder()
		w.menubarHandler().ServeHTTP(out, httptest.NewRequest("GET", "http://unix/status", nil))
		var state struct{ Actions map[string]phoneAction }
		if json.Unmarshal(out.Body.Bytes(), &state) != nil || state.Actions["qr"].Code != w.phoneFallback || state.Actions["phone"].Reason == "" {
			t.Fatal("fallback reason lost from status")
		}
	}
}

func TestMenubarPresentationPrivateAndUnavailable(t *testing.T) {
	m, _, _ := manager(t)
	w, _ := NewWeb(m, "127.0.0.1:0", "fixture.ts.net")
	w.phoneCheck = func(context.Context) string { return "" }
	oldOpen := menubarOpenFile
	defer func() { menubarOpenFile = oldOpen }()
	opened := ""
	menubarOpenFile = func(_ context.Context, path string) error { opened = path; return nil }
	if w.menubarPresentation(context.Background(), true) != nil {
		t.Fatal("private presentation")
	}
	st, err := os.Lstat(opened)
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0600 {
		t.Fatal("presentation mode")
	}
	if owner, ok := st.Sys().(*syscall.Stat_t); !ok || owner.Uid != uint32(os.Getuid()) {
		t.Fatal("presentation owner")
	}
	if strings.Contains(opened, "?token=") || strings.Contains(opened, w.token) {
		t.Fatal("opener argv contains grant")
	}
	dir := filepath.Dir(opened)
	st, err = os.Lstat(dir)
	if err != nil || !st.IsDir() || st.Mode().Perm() != 0700 {
		t.Fatal("presentation directory")
	}
	menubarOpenFile = func(_ context.Context, path string) error {
		opened = path
		return errors.New("private failure must not cross socket")
	}
	if w.menubarPresentation(context.Background(), true) == nil {
		t.Fatal("opener failure hidden")
	}
	if _, err := os.Lstat(opened); !os.IsNotExist(err) {
		t.Fatal("failed presentation retained")
	}
	if os.Chmod(dir, 0755) != nil {
		t.Fatal("fixture mode")
	}
	actions, _ := w.phoneCapabilities(context.Background())
	if actions["qr"].Enabled || actions["qr"].Code != "presentation_unavailable" {
		t.Fatal("unsafe presentation directory enabled")
	}
}

func TestPhoneReadinessRechecksRuntime(t *testing.T) {
	m, _, _ := manager(t)
	w, _ := NewWeb(m, "127.0.0.1:0", "mac.tail123.ts.net")
	dir := fakeTailscale(t, fixtureTailnetStatus)
	w.tailnet = &tailnet{exe: filepath.Join(dir, "tailscale"), host: w.allowHost}
	if w.phoneReadiness(context.Background()) != "setup_pending" {
		t.Fatal("early readiness")
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal("fixture listener")
	}
	w.address = listener.Addr().String()
	w.phoneReady = true
	server := &http.Server{Handler: w}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	_, port, _ := net.SplitHostPort(w.address)
	_, err = w.tailnet.ensure(port)
	if err != nil {
		t.Fatal("fake mapping setup")
	}
	if w.phoneReadiness(context.Background()) != "" {
		t.Fatal("verified fake readiness")
	}
	if os.WriteFile(filepath.Join(dir, "serve.json"), []byte(`{"AllowFunnel":{"mac.tail123.ts.net:443":true}}`), 0600) != nil {
		t.Fatal("fake Funnel")
	}
	if w.phoneReadiness(context.Background()) != "funnel_enabled" {
		t.Fatal("stale Funnel accepted")
	}
	before, _ := os.ReadFile(filepath.Join(dir, "serve.json"))
	_ = w.phoneReadiness(context.Background())
	after, _ := os.ReadFile(filepath.Join(dir, "serve.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("click repaired mapping")
	}
}
