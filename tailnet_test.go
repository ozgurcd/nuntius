package nuntius

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"net"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"rsc.io/qr"
)

const fixtureTailnetStatus = `{"BackendState":"Running","Self":{"HostName":"mac","DNSName":"mac.tail123.ts.net."},"CertDomains":["mac.tail123.ts.net"],"MagicDNSSuffix":"tail123.ts.net"}`

// fakeTailscale puts fixture tailscale and osascript programs first on PATH and
// points the app fallback at an absent file, so no real CLI can be reached.
func fakeTailscale(t *testing.T, status string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"tailscale", "osascript"} {
		if err := os.Symlink(fakeBinary, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("NUNTIUS_FAKE_TAILSCALE", dir)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	previous := tailscaleAppPath
	tailscaleAppPath = filepath.Join(dir, "absent-app")
	t.Cleanup(func() { tailscaleAppPath = previous })
	if status != "" {
		if err := os.WriteFile(filepath.Join(dir, "status.json"), []byte(status), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func fakeCalls(t *testing.T, dir, name string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// webCLI starts nuntius web in-process and returns once the summary line is
// printed; stop cancels the context and returns the exit code.
func webCLI(t *testing.T, args ...string) (out, errOut *syncBuffer, stop func() int) {
	t.Helper()
	out, errOut = &syncBuffer{}, &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- CLI(ctx, append([]string{"web"}, args...), strings.NewReader(""), out, errOut) }()
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(out.String(), "summary:"); {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("web did not print its summary: %s", errOut.String())
		}
		select {
		case code := <-done:
			t.Fatalf("web exited early with %d: %s", code, errOut.String())
		case <-time.After(5 * time.Millisecond):
		}
	}
	return out, errOut, func() int {
		cancel()
		select {
		case code := <-done:
			return code
		case <-time.After(10 * time.Second):
			t.Fatal("web did not stop")
			return -1
		}
	}
}

func summaryLine(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "summary:") {
			return line
		}
	}
	return ""
}

func TestTailnetDetection(t *testing.T) {
	dir := fakeTailscale(t, fixtureTailnetStatus)
	exe, err := findTailscale()
	if err != nil || exe != filepath.Join(dir, "tailscale") {
		t.Fatal("fixture tailscale not selected", err)
	}
	host, err := detectTailnet(exe)
	if err != nil || host != "mac.tail123.ts.net" {
		t.Fatal("detection failed", host, err)
	}
	for _, tc := range []struct{ status, want string }{
		{`{"BackendState":"NeedsLogin","Self":{"DNSName":"mac.tail123.ts.net."},"CertDomains":["mac.tail123.ts.net"]}`, "log in"},
		{`{"BackendState":"Stopped","Self":{"DNSName":"mac.tail123.ts.net."},"CertDomains":["mac.tail123.ts.net"]}`, "log in"},
		{`{"BackendState":"Running","Self":{"DNSName":""},"CertDomains":["mac.tail123.ts.net"]}`, "MagicDNS"},
		{`{"BackendState":"Running","Self":{"DNSName":"mac.tail123.ts.net."},"CertDomains":[]}`, "HTTPS certificates"},
		{`{"BackendState":"Running","Self":{"DNSName":"mac.tail123.ts.net."}}`, "HTTPS certificates"},
		{`{"BackendState":"Running","Self":{"DNSName":"mac.example."},"CertDomains":["mac.example"]}`, "MagicDNS"},
		{`not json`, "status"},
	} {
		if err := os.WriteFile(filepath.Join(dir, "status.json"), []byte(tc.status), 0600); err != nil {
			t.Fatal(err)
		}
		host, err := detectTailnet(exe)
		if err == nil || host != "" || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "\n") {
			t.Fatalf("status %s: host=%q err=%v", tc.status, host, err)
		}
	}
	for _, call := range fakeCalls(t, dir, "calls.log") {
		if call != "status --json" {
			t.Fatal("detection ran another tailscale command")
		}
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := findTailscale(); err == nil || !strings.Contains(err.Error(), "install") {
		t.Fatal("absent CLI not reported", err)
	}
}

func TestTailnetServePlan(t *testing.T) {
	const host = "mac.tail123.ts.net"
	ours := func(target string) string {
		return `{"TCP":{"443":{"HTTPS":true}},"Web":{"` + host + `:443":{"Handlers":{"/":{"Proxy":"` + target + `"}}}}}`
	}
	for _, tc := range []struct{ name, config, state, want string }{
		{"absent", "null", "absent", ""},
		{"empty", "{}", "absent", ""},
		{"ours", ours("http://127.0.0.1:8123"), "reused", ""},
		{"ours localhost", ours("http://localhost:8123"), "reused", ""},
		{"other port", ours("http://127.0.0.1:8124"), "", "tailscale serve --https=443 off"},
		{"other host", ours("http://10.0.0.5:8123"), "", "tailscale serve --https=443 off"},
		{"other kind", `{"TCP":{"443":{"TCPForward":"127.0.0.1:8123"}}}`, "", "tailscale serve --https=443 off"},
		{"other path", `{"TCP":{"443":{"HTTPS":true}},"Web":{"` + host + `:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:8123"},"/x":{"Text":"hi"}}}}}`, "", "tailscale serve --https=443 off"},
		{"other name", `{"TCP":{"443":{"HTTPS":true}},"Web":{"other.tail123.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:8123"}}}}}`, "", "tailscale serve --https=443 off"},
		{"funnel", `{"TCP":{"443":{"HTTPS":true}},"Web":{"` + host + `:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:8123"}}}},"AllowFunnel":{"` + host + `:443":true}}`, "", "tailscale funnel --https=443 off"},
		{"funnel other port", `{"AllowFunnel":{"` + host + `:8443":true}}`, "", "tailscale funnel --https=8443 off"},
		{"funnel off flag", ours("http://127.0.0.1:8123")[:len(ours("http://127.0.0.1:8123"))-1] + `,"AllowFunnel":{"` + host + `:443":false}}`, "reused", ""},
		{"invalid", "nonsense", "", "serve status"},
	} {
		state, err := servePlan([]byte(tc.config), host, "8123")
		if tc.want == "" && (err != nil || state != tc.state) {
			t.Fatalf("%s: state=%q err=%v", tc.name, state, err)
		}
		if tc.want != "" && (err == nil || state != "" || !strings.Contains(err.Error(), tc.want)) {
			t.Fatalf("%s: state=%q err=%v", tc.name, state, err)
		}
	}
}

func TestIMessageHandles(t *testing.T) {
	for _, ok := range []string{"owner@icloud.com", "first.last+tag@example.co.uk", "+15551234567", "+447911123456"} {
		if !validIMessageHandle(ok) {
			t.Fatal("valid handle refused", ok)
		}
	}
	for _, bad := range []string{"", "owner", "owner@", "@icloud.com", "owner@icloud", "15551234567", "+1", "+0123456789", "+1555123456789012", "-e", "owner@icloud.com\n", "owner @icloud.com", "tel:+15551234567", strings.Repeat("a", 250) + "@example.com"} {
		if validIMessageHandle(bad) {
			t.Fatal("invalid handle accepted", bad)
		}
	}
	if !strings.HasPrefix(imessageScript, "on run argv") || strings.Contains(imessageScript, "%s") || !strings.Contains(imessageScript, "item 1 of argv") || !strings.Contains(imessageScript, "item 2 of argv") {
		t.Fatal("AppleScript must take handle and text from argv")
	}
}

func TestTailnetWebLifecycle(t *testing.T) {
	_, c, store := manager(t)
	cliFixture(t, c, store)
	dir := fakeTailscale(t, fixtureTailnetStatus)
	out, errOut, stop := webCLI(t, "--bind", "127.0.0.1:0", "--tailnet", "--notify-imessage", "owner@icloud.com")
	lines := strings.Split(out.String(), "\n")
	loopback, err := url.Parse(lines[0])
	if err != nil {
		t.Fatal(err)
	}
	port, token := loopback.Port(), loopback.Query().Get("token")
	if len(token) != 64 || port == "" || lines[1] != "https://mac.tail123.ts.net/?token="+token {
		t.Fatal("sign-in links missing")
	}
	if summaryLine(out.String()) != "summary: tailnet=mac.tail123.ts.net serve=created imessage=sent" {
		t.Fatalf("summary: %q", summaryLine(out.String()))
	}
	if strings.Contains(summaryLine(out.String()), token) || strings.Contains(errOut.String(), token) {
		t.Fatal("credential leaked")
	}
	want := []string{"status --json", "serve status --json", "serve --bg --https=443 localhost:" + port, "serve status --json"}
	if calls := fakeCalls(t, dir, "calls.log"); strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("tailscale calls: %q", calls)
	}
	var argv []string
	sends := fakeCalls(t, dir, "osascript.log")
	if len(sends) != 1 || json.Unmarshal([]byte(sends[0]), &argv) != nil {
		t.Fatalf("osascript sends: %q", sends)
	}
	if len(argv) != 4 || argv[0] != "-e" || argv[1] != imessageScript || argv[2] != "owner@icloud.com" || argv[3] != "nuntius dashboard (mac): https://mac.tail123.ts.net/?token="+token {
		t.Fatal("osascript argv differs from the fixed script with handle and text")
	}
	var status bytes.Buffer
	if CLI(context.Background(), []string{"status", "--long"}, strings.NewReader(""), &status, &status) != 0 || !strings.Contains(status.String(), "tailnet dashboard: active (mac.tail123.ts.net, serve created)") {
		t.Fatalf("status --long while active: %s", status.String())
	}
	status.Reset()
	if CLI(context.Background(), []string{"status"}, strings.NewReader(""), &status, &status) != 0 || strings.Contains(status.String(), "tailnet dashboard") {
		t.Fatal("short status should not carry the dashboard line")
	}
	leaked := false
	_ = filepath.WalkDir(store, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if b, e := os.ReadFile(path); e == nil && strings.Contains(string(b), token) {
				leaked = true
			}
		}
		return nil
	})
	if leaked {
		t.Fatal("credential written under the store")
	}
	if stop() != 0 {
		t.Fatal("web exit code", errOut.String())
	}
	if calls := fakeCalls(t, dir, "calls.log"); calls[len(calls)-1] != "serve --https=443 off" || len(calls) != 5 {
		t.Fatalf("serve mapping not removed on exit: %q", calls)
	}
	status.Reset()
	if CLI(context.Background(), []string{"status", "--long"}, strings.NewReader(""), &status, &status) != 0 || !strings.Contains(status.String(), "tailnet dashboard: inactive") {
		t.Fatalf("status --long after exit: %s", status.String())
	}
}

func TestTailnetServeReuseKeepAndNotifyFailure(t *testing.T) {
	_, c, store := manager(t)
	cliFixture(t, c, store)
	dir := fakeTailscale(t, fixtureTailnetStatus)
	probe, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	bind := probe.Addr().String()
	_ = probe.Close()
	_, port, _ := net.SplitHostPort(bind)
	ours := `{"TCP":{"443":{"HTTPS":true}},"Web":{"mac.tail123.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:` + port + `"}}}}}`
	if err := os.WriteFile(filepath.Join(dir, "serve.json"), []byte(ours), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "osascript-fail"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	out, errOut, stop := webCLI(t, "--bind", bind, "--tailnet", "--notify-imessage", "+15551234567")
	if summaryLine(out.String()) != "summary: tailnet=mac.tail123.ts.net serve=reused imessage=failed" || !strings.Contains(errOut.String(), "warning: iMessage") {
		t.Fatalf("summary: %q stderr: %q", summaryLine(out.String()), errOut.String())
	}
	if !strings.Contains(out.String(), "https://mac.tail123.ts.net/?token=") || !strings.Contains(out.String(), "Scan to sign in") {
		t.Fatal("links and QR must still print after a failed send")
	}
	if stop() != 0 {
		t.Fatal("web exit code", errOut.String())
	}
	if calls := fakeCalls(t, dir, "calls.log"); strings.Join(calls, "\n") != "status --json\nserve status --json" {
		t.Fatalf("a reused mapping must never be changed: %q", calls)
	}
	// A mapping nuntius created stays when --tailnet-keep is given.
	_ = os.Remove(filepath.Join(dir, "serve.json"))
	_ = os.Remove(filepath.Join(dir, "calls.log"))
	out, errOut, stop = webCLI(t, "--bind", bind, "--tailnet", "--tailnet-keep")
	if summaryLine(out.String()) != "summary: tailnet=mac.tail123.ts.net serve=created imessage=off" {
		t.Fatalf("summary: %q", summaryLine(out.String()))
	}
	if stop() != 0 {
		t.Fatal("web exit code", errOut.String())
	}
	if calls := fakeCalls(t, dir, "calls.log"); len(calls) != 4 || strings.Contains(strings.Join(calls, "\n"), " off") {
		t.Fatalf("--tailnet-keep removed the mapping: %q", calls)
	}
	if sends := fakeCalls(t, dir, "osascript.log"); len(sends) != 1 {
		t.Fatalf("iMessage sent without a handle: %q", sends)
	}
}

func TestTailnetOptionalAndConfigHandle(t *testing.T) {
	_, c, store := manager(t)
	c.NotifyIMessage = "owner@icloud.com"
	cliFixture(t, c, store)
	dir := fakeTailscale(t, `{"BackendState":"NeedsLogin","Self":{"DNSName":""}}`)
	out, errOut, stop := webCLI(t, "--bind", "127.0.0.1:0", "--tailnet", "--tailnet-optional")
	if summaryLine(out.String()) != "summary: tailnet=off serve=none imessage=off" || !strings.HasPrefix(errOut.String(), "warning: ") || strings.Contains(out.String(), "https://") {
		t.Fatalf("optional fallback: %q %q", out.String(), errOut.String())
	}
	if stop() != 0 || len(fakeCalls(t, dir, "calls.log")) != 1 || fakeCalls(t, dir, "osascript.log") != nil {
		t.Fatal("fallback must not touch serve or Messages")
	}
	// The configured handle is used when the flag is absent.
	if err := os.WriteFile(filepath.Join(dir, "status.json"), []byte(fixtureTailnetStatus), 0600); err != nil {
		t.Fatal(err)
	}
	out, errOut, stop = webCLI(t, "--bind", "127.0.0.1:0", "--tailnet")
	if summaryLine(out.String()) != "summary: tailnet=mac.tail123.ts.net serve=created imessage=sent" {
		t.Fatalf("summary: %q %q", summaryLine(out.String()), errOut.String())
	}
	if stop() != 0 {
		t.Fatal("web exit code", errOut.String())
	}
	var argv []string
	sends := fakeCalls(t, dir, "osascript.log")
	if len(sends) != 1 || json.Unmarshal([]byte(sends[0]), &argv) != nil || len(argv) != 4 || argv[2] != "owner@icloud.com" {
		t.Fatalf("configured handle not used: %q", sends)
	}
	c.NotifyIMessage = "not-a-handle"
	if _, err := c.Validate(); err == nil || !strings.Contains(err.Error(), "notify_imessage") {
		t.Fatal("invalid notify_imessage accepted", err)
	}
}

func TestTailnetSetupRefusals(t *testing.T) {
	_, c, store := manager(t)
	cliFixture(t, c, store)
	dir := fakeTailscale(t, fixtureTailnetStatus)
	ours := `{"TCP":{"443":{"HTTPS":true}},"Web":{"mac.tail123.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:1"}}}}`
	run := func(args ...string) (int, string) {
		var out, errOut bytes.Buffer
		code := CLI(context.Background(), append([]string{"web", "--bind", "127.0.0.1:0"}, args...), strings.NewReader(""), &out, &errOut)
		if out.Len() != 0 {
			t.Fatalf("refusal printed output: %q", out.String())
		}
		return code, errOut.String()
	}
	for _, tc := range []struct {
		name, serve string
		args        []string
		code        int
		want        string
	}{
		{"funnel", ours + `,"AllowFunnel":{"mac.tail123.ts.net:443":true}}`, []string{"--tailnet"}, 1, "tailscale funnel --https=443 off"},
		{"other", ours + `}`, []string{"--tailnet"}, 1, "tailscale serve --https=443 off"},
		{"mismatch", "null", []string{"--tailnet", "--allow-host", "other.tail123.ts.net"}, 1, "does not match"},
		{"handle", "null", []string{"--tailnet", "--notify-imessage", "owner"}, 2, "notify-imessage"},
		{"keep alone", "null", []string{"--tailnet-keep"}, 2, "invalid web flags"},
		{"optional alone", "null", []string{"--tailnet-optional"}, 2, "invalid web flags"},
		{"notify alone", "null", []string{"--notify-imessage", "owner@icloud.com"}, 2, "invalid web flags"},
	} {
		_ = os.Remove(filepath.Join(dir, "calls.log"))
		if err := os.WriteFile(filepath.Join(dir, "serve.json"), []byte(tc.serve), 0600); err != nil {
			t.Fatal(err)
		}
		code, diagnostic := run(tc.args...)
		if code != tc.code || !strings.Contains(diagnostic, tc.want) {
			t.Fatalf("%s: code=%d stderr=%q", tc.name, code, diagnostic)
		}
		for _, call := range fakeCalls(t, dir, "calls.log") {
			if call != "status --json" && call != "serve status --json" {
				t.Fatalf("%s: refusal changed tailscale state: %q", tc.name, call)
			}
		}
		if fakeCalls(t, dir, "osascript.log") != nil {
			t.Fatal("refusal sent a message")
		}
	}
	// A matching --allow-host with --tailnet is accepted.
	_ = os.Remove(filepath.Join(dir, "serve.json"))
	out, errOut, stop := webCLI(t, "--bind", "127.0.0.1:0", "--tailnet", "--allow-host", "mac.tail123.ts.net")
	if summaryLine(out.String()) != "summary: tailnet=mac.tail123.ts.net serve=created imessage=off" || stop() != 0 {
		t.Fatal("matching allow-host refused", errOut.String())
	}
}

func TestTailnetGuards(t *testing.T) {
	m, _, _ := manager(t)
	const host = "mac.tail123.ts.net"
	w, err := NewWeb(m, "127.0.0.1:8123", host)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, host, origin, path string
		auth                     bool
		want                     int
	}{
		{"host", host, "", "/", false, 200},
		{"443", host + ":443", "", "/", false, 200},
		{"data", host, "", "/data", true, 200},
		{"no header", host, "", "/data", false, 403},
		{"query", host, "", "/data?token=" + w.token, false, 403},
		{"https post", host, "https://" + host, "/action", true, 200},
		{"443 post", host + ":443", "https://" + host, "/action", true, 200},
		{"http", host, "http://" + host, "/action", true, 403},
		{"origin port", host, "https://" + host + ":443", "/action", true, 403},
		{"origin suffix", host, "https://" + host + ".evil.test", "/action", true, 403},
		{"missing origin", host, "POST", "/action", true, 403},
		{"other tailnet", "mac.other.ts.net", "", "/", true, 403},
		{"suffix", host + ".evil.test", "", "/", true, 403},
		{"subdomain", "evil." + host, "", "/", true, 403},
		{"wrong port", host + ":8123", "", "/", true, 403},
		{"forwarded spoof", "evil.test", "https://" + host, "/action", true, 403},
		{"loopback", "127.0.0.1:8123", "http://127.0.0.1:8123", "/action", true, 200},
		{"cross local", "127.0.0.1:8123", "https://" + host, "/action", true, 403},
		{"cross tailnet", host, "http://127.0.0.1:8123", "/action", true, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			method := "GET"
			if tc.origin != "" {
				method = "POST"
			}
			r := httptest.NewRequest(method, "http://127.0.0.1:8123"+tc.path, strings.NewReader(`{"action":"unfreeze"}`))
			r.Host = tc.host
			if tc.origin != "POST" {
				r.Header.Set("Origin", tc.origin)
			}
			r.Header.Set("X-Forwarded-Host", host)
			r.Header.Set("X-Forwarded-Proto", "https")
			r.Header.Set("Forwarded", "host="+host+";proto=https")
			if tc.auth {
				r.Header.Set("X-Nuntius-Token", w.token)
			}
			out := httptest.NewRecorder()
			w.ServeHTTP(out, r)
			if out.Code != tc.want {
				t.Fatalf("status=%d, want=%d", out.Code, tc.want)
			}
			if out.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatal("CORS enabled")
			}
		})
	}
	local, _ := NewWeb(m, "127.0.0.1:8123")
	r := httptest.NewRequest("GET", "http://"+host+"/", nil)
	out := httptest.NewRecorder()
	local.ServeHTTP(out, r)
	if out.Code != 403 {
		t.Fatal("tailnet enabled without opt-in")
	}
	for _, invalid := range []string{"", "ts.net", ".ts.net", "*.tail.ts.net", "mac.tail.ts.net:443", "https://mac.tail.ts.net", "mac.tail.ts.net/", "mac.tail.ts.net.", "mac..ts.net", "-mac.tail.ts.net", "mac_.tail.ts.net", "mac.tail.example", strings.Repeat("x", 64) + ".ts.net", strings.Repeat("x.", 125) + "ts.net"} {
		if _, err := NewWeb(m, "127.0.0.1:8123", invalid); err == nil {
			t.Fatal("invalid allow-host accepted")
		}
	}
	if _, err := NewWeb(m, "0.0.0.0:8123", host); err == nil {
		t.Fatal("remote bind allowed")
	}
}

func TestTailnetCLIRefusals(t *testing.T) {
	_, c, store := manager(t)
	cliFixture(t, c, store)
	for _, flags := range [][]string{{"--allow-host", "evil.test"}, {"--allow-host", ""}, {"--allow-host", "mac.tail.ts.net", "--allow-host", "other.tail.ts.net"}, {"--allow-host", "mac.tail.ts.net", "--bind", "0.0.0.0:8123"}} {
		var out, diagnostic bytes.Buffer
		if CLI(context.Background(), append([]string{"web"}, flags...), strings.NewReader(""), &out, &diagnostic) != 1 || out.Len() != 0 {
			t.Fatal("unsafe web CLI accepted")
		}
	}
}

func TestTailnetSignInQR(t *testing.T) {
	m, _, _ := manager(t)
	w, _ := NewWeb(m, "127.0.0.1:8123", "mac.tail123.ts.net")
	var out bytes.Buffer
	if err := w.writeLinks(&out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(out.String(), "\n")
	if lines[0] != "http://127.0.0.1:8123/?token="+w.token || lines[1] != "https://mac.tail123.ts.net/?token="+w.token {
		t.Fatal("incorrect sign-in links")
	}
	code, err := qr.Encode(lines[1], qr.M)
	if err != nil {
		t.Fatal("fixture QR encoding failed")
	}
	// Reconstruct every terminal module, including the four-module quiet zone.
	if len(lines) != code.Size+12 {
		t.Fatal("wrong QR height")
	}
	for y := -4; y < code.Size+4; y++ {
		line := strings.TrimPrefix(lines[y+7], "\x1b[30;47m")
		line = strings.TrimSuffix(line, "\x1b[0m")
		cells := []rune(line)
		if len(cells) != 2*(code.Size+8) {
			t.Fatal("wrong QR width")
		}
		for x := -4; x < code.Size+4; x++ {
			want := ' '
			if code.Black(x, y) {
				want = '█'
			}
			if cells[2*(x+4)] != want || cells[2*(x+4)+1] != want {
				t.Fatal("terminal QR changed encoded URL")
			}
		}
	}
}
