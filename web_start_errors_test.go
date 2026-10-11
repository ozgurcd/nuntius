package nuntius

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBackgroundStartupCauses(t *testing.T) {
	for _, tc := range []struct{ name, status, serve, want string }{
		{"stopped", `{"BackendState":"Stopped"}`, "", "state Stopped"},
		{"logged-out", `{"BackendState":"NeedsLogin"}`, "", "state NeedsLogin"},
		{"unavailable", "", "", "Tailscale status is unavailable"},
		{"foreign-serve", fixtureTailnetStatus, `{"TCP":{"443":{"HTTPS":true}},"Web":{"mac.tail123.ts.net:443":{"Handlers":{"/":{"Proxy":"http://localhost:9999"}}}}}`, "already serves something else"},
		{"funnel", fixtureTailnetStatus, `{"AllowFunnel":{"mac.tail123.ts.net:443":true}}`, "Funnel is on"},
		{"port-busy", fixtureTailnetStatus, "", "port is already in use"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, c, store := manager(t)
			cliFixture(t, c, store)
			dir := fakeTailscale(t, tc.status)
			if tc.serve != "" {
				if os.WriteFile(filepath.Join(dir, "serve.json"), []byte(tc.serve), 0600) != nil {
					t.Fatal("serve fixture")
				}
			}
			previousOS, previousExe := webPlatform, webExecutable
			webPlatform = "linux"
			webExecutable = func() (string, error) { return fakeBinary, nil }
			t.Cleanup(func() { webPlatform, webExecutable = previousOS, previousExe })
			bind := "127.0.0.1:0"
			if tc.name == "port-busy" {
				l, err := net.Listen("tcp4", bind)
				if err != nil {
					t.Fatal("occupied fixture port")
				}
				defer l.Close()
				bind = l.Addr().String()
			}
			var out, errs bytes.Buffer
			if CLI(context.Background(), []string{"web", "start", "--no-menubar", "--bind", bind, "--tailnet"}, strings.NewReader(""), &out, &errs) == 0 {
				t.Fatal("failed start reported success")
			}
			if !strings.Contains(errs.String(), tc.want) || !strings.Contains(errs.String(), filepath.Join(store, "web.log")) {
				t.Fatal("startup cause or full web.log path missing")
			}
			if strings.Contains(out.String()+errs.String(), "?token=") {
				t.Fatal("startup access leaked")
			}
			cleanupFixtureWeb(t, filepath.Join(store, "web-fixture"))
		})
	}
}

func TestMissingTailscaleStartupCause(t *testing.T) {
	_, c, store := manager(t)
	cliFixture(t, c, store)
	dir := fakeTailscale(t, "")
	if os.Remove(filepath.Join(dir, "tailscale")) != nil {
		t.Fatal("missing executable fixture")
	}
	t.Setenv("PATH", dir)
	_, err := findTailscale()
	if err == nil {
		t.Fatal("missing Tailscale accepted")
	}
	cause := (webStartupFailure{Code: webStartupCode(err.Error())}).cause()
	if !strings.Contains(cause, "Tailscale CLI was not found") || !strings.Contains(cause, "install Tailscale") {
		t.Fatal("missing executable cause lost")
	}
	if (webStartupFailure{Code: "untrusted diagnostic"}).cause() != "" {
		t.Fatal("untrusted startup diagnostic was echoed")
	}
}

func TestBackgroundStartupResultIsolation(t *testing.T) {
	_, c, store := manager(t)
	cliFixture(t, c, store)
	previousOS, previousExe := webPlatform, webExecutable
	webPlatform = "linux"
	webExecutable = func() (string, error) { return fakeBinary, nil }
	t.Cleanup(func() { webPlatform, webExecutable = previousOS, previousExe })
	dir := filepath.Join(store, "web-launch")
	if privateDir(dir) != nil || saveWebFile(filepath.Join(dir, "failure.json"), webStartupFailure{Code: "stopped"}) != nil {
		t.Fatal("stale result fixture")
	}
	if os.WriteFile(filepath.Join(store, "web.log"), []byte("old unrelated diagnostic\n"), 0600) != nil {
		t.Fatal("stale log fixture")
	}
	var out, errs bytes.Buffer
	if CLI(context.Background(), []string{"web", "start", "--no-menubar", "--bind", "127.0.0.1:0"}, strings.NewReader(""), &out, &errs) != 0 {
		t.Fatal("stale diagnostic affected new start")
	}
	if strings.Contains(errs.String(), "old unrelated") {
		t.Fatal("raw log was returned")
	}
}
