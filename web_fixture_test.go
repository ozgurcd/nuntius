package nuntius

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The compiled supervisor owns Wait even when startup fails before web.json.
// Cleanup uses fixture files, never an owner PID or system service.
func cleanupFixtureWeb(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "stop"), nil, 0600); err != nil {
		t.Error("cannot request fixture cleanup")
		return
	}
	for deadline := time.Now().Add(5 * time.Second); ; {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Error("cannot check fixture children")
			return
		}
		pending := false
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".started") {
				if _, err := os.Stat(filepath.Join(dir, strings.TrimSuffix(entry.Name(), ".started")+".reaped")); err != nil {
					pending = true
				}
			}
		}
		if !pending {
			return
		}
		if time.Now().After(deadline) {
			t.Error("fixture dashboard leaked: child was not killed and reaped")
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestBackgroundFixtureReaped(t *testing.T) {
	_, c, store := manager(t)
	cliFixture(t, c, store)
	dir := filepath.Join(store, "web-fixture")
	if privateDir(filepath.Join(store, "web-launch")) != nil {
		t.Fatal("fixture launch directory")
	}
	cmd := exec.Command(fakeBinary, "__fixture_web", "--config", os.Getenv("NUNTIUS_CONFIG"), "--store", store, "web", "__background", "--bind", "127.0.0.1:0")
	if err := cmd.Start(); err != nil {
		t.Fatal("fixture supervisor start")
	}
	// Own and reap the supervisor itself even if an assertion fails.
	t.Cleanup(func() { cleanupFixtureWeb(t, dir); _ = cmd.Wait() })
	for deadline := time.Now().Add(3 * time.Second); ; {
		entries, _ := filepath.Glob(filepath.Join(dir, "*.started"))
		if len(entries) > 0 && (&Manager{store: store}).webDashboard() != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("dashboard fixture has no owned running child")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cleanupFixtureWeb(t, dir)
}

func TestBackgroundPartialStartReaped(t *testing.T) {
	_, c, store := manager(t)
	cliFixture(t, c, store)
	dir := fakeTailscale(t, fixtureTailnetStatus)
	if os.Symlink(fakeBinary, filepath.Join(dir, "launchctl")) != nil {
		t.Fatal("launch fixture")
	}
	t.Setenv("NUNTIUS_FAKE_LAUNCHCTL", dir)
	previousOS, previousExe := webPlatform, webExecutable
	webPlatform = "darwin"
	webExecutable = func() (string, error) { return fakeBinary, nil }
	t.Cleanup(func() { webPlatform, webExecutable = previousOS, previousExe })
	if os.WriteFile(filepath.Join(dir, "kickstart-after-start-fail"), nil, 0600) != nil {
		t.Fatal("failure fixture")
	}
	var out, errs bytes.Buffer
	if CLI(context.Background(), []string{"web", "start", "--bind", "127.0.0.1:0", "--no-menubar"}, strings.NewReader(""), &out, &errs) == 0 {
		t.Fatal("partial failure accepted")
	}
	entries, _ := filepath.Glob(filepath.Join(store, "web-fixture", "*.started"))
	if len(entries) == 0 {
		t.Fatal("failure did not start a child")
	}
	cleanupFixtureWeb(t, filepath.Join(store, "web-fixture"))
}
