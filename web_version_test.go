package nuntius

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestWebStatusVersions(t *testing.T) {
	for _, tc := range []struct {
		version, want string
		warning       bool
	}{
		{Version, "version=" + Version, false},
		{"v0.0.1", "version=v0.0.1", true},
		{"", "version=unknown", true},
		{"invalid\nversion", "version=unknown", true},
	} {
		state := webState{Address: "127.0.0.1:8123"}
		if field := reflect.ValueOf(&state).Elem().FieldByName("Version"); field.IsValid() {
			field.SetString(tc.version)
		}
		line := webStatusLine(&state)
		if !strings.Contains(line, tc.want) || strings.Contains(line, "warning:") != tc.warning || strings.Contains(line, "invalid") {
			t.Fatal("running version or mismatch warning incorrect")
		}
		if tc.warning && !strings.Contains(line, "installed="+Version) {
			t.Fatal("installed version missing from warning")
		}
	}
	if webStatusLine(nil) != "web: not running" {
		t.Fatal("stopped status changed")
	}
}

func TestBackgroundVersionPersisted(t *testing.T) {
	_, c, store := manager(t)
	cliFixture(t, c, store)
	previousOS, previousExe := webPlatform, webExecutable
	webPlatform = "linux"
	webExecutable = func() (string, error) { return fakeBinary, nil }
	t.Cleanup(func() { webPlatform, webExecutable = previousOS, previousExe })
	var out, errs bytes.Buffer
	if CLI(context.Background(), []string{"web", "start", "--no-menubar", "--bind", "127.0.0.1:0"}, strings.NewReader(""), &out, &errs) != 0 {
		t.Fatal("version fixture start")
	}
	var record struct {
		Version string `json:"version"`
	}
	b, err := os.ReadFile(filepath.Join(store, "web.json"))
	if err != nil || json.Unmarshal(b, &record) != nil || record.Version != Version {
		t.Fatal("running process did not record its version")
	}
	out.Reset()
	if CLI(context.Background(), []string{"web", "status"}, strings.NewReader(""), &out, &errs) != 0 || !strings.Contains(out.String(), "version="+Version) {
		t.Fatal("CLI running version absent")
	}
}
