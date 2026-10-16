package nuntius

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPathResolution(t *testing.T) {
	home := t.TempDir()
	if !notificationsSupported("darwin") || notificationsSupported("linux") || notificationsSupported("plan9") {
		t.Fatal("notification OS guard")
	}
	for _, tc := range []struct{ goos, xdg, want string }{
		{"darwin", "", filepath.Join(home, "Library", "Application Support", "nuntius")},
		{"darwin", filepath.Join(home, "xdg"), filepath.Join(home, "Library", "Application Support", "nuntius")},
		{"linux", "", filepath.Join(home, ".local", "share", "nuntius")},
		{"linux", filepath.Join(home, "xdg"), filepath.Join(home, "xdg", "nuntius")},
	} {
		t.Run(tc.goos+tc.xdg, func(t *testing.T) {
			env := map[string]string{"XDG_DATA_HOME": tc.xdg}
			p, err := resolvePaths(tc.goos, home, func(k string) string { return env[k] }, "", "", false, false)
			if err != nil || p.Data != tc.want || p.DataSource != "default" || p.Config != filepath.Join(home, ".config", "nuntius", "lanes.json") || p.ConfigSource != "default" {
				t.Fatal("default resolution")
			}
		})
	}
	env := map[string]string{"NUNTIUS_DATA_DIR": filepath.Join(home, "env-data"), "NUNTIUS_CONFIG": filepath.Join(home, "env-config"), "XDG_CONFIG_HOME": filepath.Join(home, "config-xdg")}
	lookup := func(k string) string { return env[k] }
	for _, goos := range []string{"darwin", "linux", "plan9"} {
		p, err := resolvePaths(goos, home, lookup, "", "", false, false)
		if err != nil || p.Data != env["NUNTIUS_DATA_DIR"] || p.Config != env["NUNTIUS_CONFIG"] || p.DataSource != "env" || p.ConfigSource != "env" {
			t.Fatal("env resolution")
		}
		p, err = resolvePaths(goos, home, lookup, filepath.Join(home, "flag-config"), filepath.Join(home, "flag-data"), true, true)
		if err != nil || p.DataSource != "flag" || p.ConfigSource != "flag" || p.Data != filepath.Join(home, "flag-data") || p.Config != filepath.Join(home, "flag-config") {
			t.Fatal("flag precedence")
		}
	}
	delete(env, "NUNTIUS_CONFIG")
	p, err := resolvePaths("linux", home, lookup, "", "", false, false)
	if err != nil || p.Config != filepath.Join(env["XDG_CONFIG_HOME"], "nuntius", "lanes.json") {
		t.Fatal("XDG config")
	}
	delete(env, "NUNTIUS_DATA_DIR")
	if _, err = resolvePaths("plan9", home, lookup, "", "", false, false); err == nil || !strings.Contains(err.Error(), "NUNTIUS_DATA_DIR") {
		t.Fatal("unsupported OS must refuse")
	}
	if _, err = resolvePaths("linux", home, lookup, "", "", false, true); err == nil {
		t.Fatal("empty explicit store must refuse")
	}
}

func TestCLIHumanAndJSONStatus(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c, store := fixture(t)
	path := filepath.Join(t.TempDir(), "lanes.json")
	b, _ := json.Marshal(c)
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NUNTIUS_CONFIG", path)
	t.Setenv("NUNTIUS_DATA_DIR", store)
	for _, command := range [][]string{{"status"}, {"status", "--json"}, {"agents"}, {"agents", "--json"}, {"paths"}} {
		var out, errOut bytes.Buffer
		if CLI(context.Background(), command, strings.NewReader(""), &out, &errOut) != 0 {
			t.Fatal("CLI failed", errOut.String())
		}
		if len(command) == 2 {
			if !json.Valid(out.Bytes()) {
				t.Fatal("JSON invalid")
			}
			if command[0] == "status" {
				m, err := newReadManager(c, store)
				if err != nil {
					t.Fatal(err)
				}
				want, err := m.StatusView()
				_ = m.Close()
				if err != nil {
					t.Fatal(err)
				}
				normalize := func(b []byte) string {
					var v map[string]any
					if json.Unmarshal(b, &v) != nil {
						t.Fatal("status object invalid")
					}
					counts, ok := v["concurrency"].(map[string]any)
					if !ok {
						t.Fatal("status concurrency absent")
					}
					live, ok := counts["live"].(float64)
					if !ok || live < 1 || live != float64(int(live)) {
						t.Fatal("status live census invalid")
					}
					delete(counts, "live") // changes between two observations by design
					metrics, ok := v["control_metrics"].(map[string]any)
					if !ok || metrics["enabled"] != controlMetricsEnabled {
						t.Fatal("status metrics shape invalid")
					}
					if controlMetricsEnabled {
						sites, ok := metrics["sites"].(map[string]any)
						if !ok || len(sites) != 4 {
							t.Fatal("status histogram sites invalid")
						}
						for _, name := range []string{"writer", "shared_reader", "admission", "mutex"} {
							site, ok := sites[name].(map[string]any)
							if !ok || len(site) != 2 {
								t.Fatal("status histogram site invalid")
							}
							for _, kind := range []string{"wait", "hold"} {
								buckets, ok := site[kind].([]any)
								if !ok || len(buckets) != 8 {
									t.Fatal("status histogram buckets invalid")
								}
								for _, bucket := range buckets {
									n, ok := bucket.(float64)
									if !ok || n < 0 || n != float64(uint64(n)) {
										t.Fatal("status histogram count invalid")
									}
								}
							}
						}
						delete(metrics, "sites") // observation itself increments these counters
					}
					stable, err := json.Marshal(v)
					if err != nil {
						t.Fatal("status serialization failed")
					}
					return string(stable)
				}
				if normalize(out.Bytes()) != normalize([]byte(toolText(want))) {
					t.Fatal("JSON shape changed")
				}
			}
		} else {
			if json.Valid(out.Bytes()) || strings.Contains(out.String(), "\x1b") {
				t.Fatal("human output")
			}
			if command[0] == "status" && (!strings.Contains(out.String(), "frozen=no") || !strings.Contains(out.String(), store) || !strings.Contains(out.String(), "ACTIVITY")) {
				t.Fatal("status header")
			}
			if command[0] == "paths" && (!strings.Contains(out.String(), "env") || !strings.Contains(out.String(), path) || !strings.Contains(out.String(), store)) {
				t.Fatal("paths provenance")
			}
		}
	}
}

func TestReleaseGOOS(t *testing.T) {
	b, err := os.ReadFile(".goreleaser.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := checkReleaseGOOS(string(b)); err != nil {
		t.Fatal(err)
	}
	for _, yaml := range []string{"goos:\n  - windows\n", "goos: [darwin, windows]\n", "goos:\n  - freebsd\n", "goos:\n", "goarch:\n  - amd64\n"} {
		if checkReleaseGOOS(yaml) == nil {
			t.Fatal("unsupported release OS accepted")
		}
	}
}

func TestStatusETAAndActivity(t *testing.T) {
	m, _, _ := manager(t)
	now := time.Now().UTC()
	add := func(i int, lane, size string, duration time.Duration) Run {
		id := fmt.Sprintf("%032x", i)
		start := now.Add(-time.Hour + time.Duration(i)*time.Second)
		end := start.Add(duration)
		r := Run{ID: id, Lane: lane, GoalID: "ETA-1", State: "ended", Started: start, Ended: &end}
		d := filepath.Join(m.store, "runs", id)
		if err := privateDir(d); err != nil {
			t.Fatal(err)
		}
		if err := saveAt(d, &r); err != nil {
			t.Fatal(err)
		}
		if err := putPrivate(filepath.Join(d, "goal.txt"), []byte("/goal ETA-1 — size "+size+".\n")); err != nil {
			t.Fatal(err)
		}
		return r
	}
	for i, duration := range []time.Duration{10 * time.Minute, 12 * time.Minute, 14 * time.Minute} {
		add(i+1, "claude", "S", duration)
	}
	if err := os.WriteFile(filepath.Join(m.store, "runs", fmt.Sprintf("%032x", 1), "goal.txt"), []byte("/goal ETA-1 size S"), 0600); err != nil {
		t.Fatal(err)
	}
	add(4, "codex", "S", time.Minute)
	add(5, "claude", "M", time.Minute)
	r := add(6, "claude", "S", time.Minute)
	r.Started = now.Add(-2 * time.Minute)
	r.Ended = nil
	r.State = "running"
	if got := m.estimateETA(r, "claude", "S", 2*time.Minute); got != "~10m left (n=3)" {
		t.Fatal("median ETA", got)
	}
	if got := m.estimateETA(r, "claude", "S", 20*time.Minute); got != "over usual (+8m)" {
		t.Fatal("over median", got)
	}
	if got := m.estimateETA(r, "codex", "S", time.Minute); got != "-" {
		t.Fatal("minimum sample")
	}
	if goalSize([]byte("/goal X\nsize S")) != "" || goalSize([]byte("/goal X size XL")) != "" {
		t.Fatal("first-line size only")
	}
	for _, tc := range []struct{ event, want string }{
		{`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit"}]}}`, "editing files"},
		{`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"go test ./..."}}]}}`, "running tests"},
		{`{"type":"item.started","item":{"type":"command_execution","command":"git push origin main"}}`, "git push"},
		{`{"type":"item.completed","item":{"type":"file_change"}}`, "editing files"},
		{`{"type":"item.started","item":{"type":"mcp_tool_call","tool":"unknown"}}`, "MCP tool"},
		{`{"type":"item.started","item":{"type":"command_execution","command":"echo private"}}`, "running command"},
	} {
		if got := activity([]byte(tc.event + "\n")); got != tc.want {
			t.Fatal("activity", got)
		}
	}
	if shortDuration(432*time.Second) != "7m12s" {
		t.Fatal("elapsed format")
	}
}

func TestEmptyStoreWarning(t *testing.T) {
	d := t.TempDir()
	alt := filepath.Join(d, "alt")
	def := filepath.Join(d, "default")
	if err := os.MkdirAll(filepath.Join(def, "runs", strings.Repeat("a", 32)), 0700); err != nil {
		t.Fatal(err)
	}
	if !emptyStoreWarning(alt, def) || emptyStoreWarning(def, def) {
		t.Fatal("default runs warning")
	}
	if err := os.MkdirAll(filepath.Join(alt, "runs", strings.Repeat("b", 32)), 0700); err != nil {
		t.Fatal(err)
	}
	if emptyStoreWarning(alt, def) {
		t.Fatal("nonempty override warning")
	}
}

func TestRunEndSignals(t *testing.T) {
	m, _, _ := manager(t)
	if _, err := m.Dispatch("codex", "SIGNAL-1", "hello"); err != nil {
		t.Fatal(err)
	}
	waitState(t, m, "codex", "ended")
	v, err := call(m, "nuntius_signals", nil)
	if err != nil || toolText(v) != `["codex.SIGNAL-1.ended"]` {
		t.Fatal("signal listing")
	}
	st, err := os.Stat(filepath.Join(m.store, "signals", "codex.SIGNAL-1.ended"))
	if err != nil || st.Size() != 0 || st.Mode().Perm() != 0600 {
		t.Fatal("empty private signal")
	}
	for _, name := range []string{"../outside", "/outside", "nested/file.ended", "..", "codex.SIGNAL-1.ended/../x"} {
		b, _ := json.Marshal(map[string]any{"names": []string{"codex.SIGNAL-1.ended", name}})
		if _, err := call(m, "nuntius_signals_ack", b); err == nil {
			t.Fatal("traversal accepted")
		}
	}
	if _, err := call(m, "nuntius_signals_ack", []byte(`{"names":["codex.SIGNAL-1.ended"]}`)); err != nil {
		t.Fatal(err)
	}
	v, err = call(m, "nuntius_signals", nil)
	if err != nil || toolText(v) != `[]` {
		t.Fatal("ack did not remove signal")
	}
}

func TestHumanStatusStatesAndRecoverySignals(t *testing.T) {
	m, _, _ := manager(t)
	view := map[string]any{"frozen": true, "lanes": []Status{
		{Lane: "claude", State: "idle"},
		{Lane: "codex", State: "ended_no_report", GoalID: "VIEW-1", ElapsedSeconds: 432},
		{Lane: "codex-2", State: "ended", GoalID: "VIEW-2", ReportExists: true},
		{Lane: "codex-3", State: "running", GoalID: "VIEW-3"},
	}}
	var out bytes.Buffer
	if err := m.writeStatus(&out, view); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"frozen=yes", "frozen", "ended_no_report", "7m12s", "yes", "running"} {
		if !strings.Contains(out.String(), want) {
			t.Fatal("human status missing state")
		}
	}
	for i, reason := range []string{"cancelled", "timed_out", "unknown_exit"} {
		r := Run{ID: fmt.Sprintf("%032x", i+1), Lane: "codex", GoalID: fmt.Sprintf("RECOVER-%d", i), State: "running", Started: time.Now().UTC(), StopReason: reason}
		if err := privateDir(filepath.Join(m.store, "runs", r.ID)); err != nil {
			t.Fatal(err)
		}
		if err := m.recoverEnded(&r); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(m.store, "signals", r.Lane+"."+r.GoalID+".ended")); err != nil {
			t.Fatal("recovered end signal")
		}
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(m.store, "signals", "codex.LINK-1.ended")); err != nil {
		t.Fatal(err)
	}
	if err := m.AckSignals([]string{"codex.LINK-1.ended"}); err == nil {
		t.Fatal("linked signal ack accepted")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("outside file removed")
	}
}
