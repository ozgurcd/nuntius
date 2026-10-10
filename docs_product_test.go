package nuntius

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestVersionAliases(t *testing.T) {
	for _, arg := range []string{"version", "--version", "-v"} {
		var out, diagnostic bytes.Buffer
		if CLI(context.Background(), []string{arg}, strings.NewReader(""), &out, &diagnostic) != 0 || out.String() != "nuntius "+Version+"\n" {
			t.Fatalf("version alias %s failed", arg)
		}
	}
}

func TestLegacyConfiguredIdentity(t *testing.T) {
	c, store := fixture(t)
	c.Lanes[0].ExtraArgs = []string{"--model=chosen-model", "--effort=high"}
	m, err := NewManager(c, store)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	now := time.Now().UTC()
	end := now
	r := Run{ID: strings.Repeat("a", 32), Lane: "claude", GoalID: "OLD-1", State: "ended", Started: now.Add(-time.Minute), Ended: &end}
	dir := filepath.Join(store, "runs", r.ID)
	if err := privateDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := saveAt(dir, &r); err != nil {
		t.Fatal(err)
	}
	rows, err := m.Status()
	if err != nil || rows[0].Model != "chosen-model (configured)" || rows[0].Effort != "high (configured)" {
		t.Fatal("legacy status must label configured identity")
	}
	agents, err := m.Agents()
	if err != nil || agents[0].Model != "chosen-model (configured)" {
		t.Fatal("legacy agent identity must be marked configured")
	}
}

func TestETAExplanation(t *testing.T) {
	m, _, _ := manager(t)
	now := time.Now().UTC()
	runs := []Run{}
	for i, duration := range []time.Duration{20 * time.Minute, 24 * time.Minute, 28 * time.Minute} {
		id := strings.Repeat(string(rune('a'+i)), 32)
		start := now.Add(-2 * time.Hour)
		end := start.Add(duration)
		runs = append(runs, Run{ID: id, Lane: "codex", State: "ended", Started: start, Ended: &end})
		dir := filepath.Join(m.store, "runs", id)
		if err := privateDir(dir); err != nil {
			t.Fatal(err)
		}
		if err := putPrivate(filepath.Join(dir, "goal.txt"), []byte("GOAL size S")); err != nil {
			t.Fatal(err)
		}
	}
	r := Run{ID: strings.Repeat("d", 32), Lane: "codex", Started: now}
	v := m.etaDetails(runs, r, "codex", "S", 31*time.Minute)
	if v == nil || v.MedianSeconds != 1440 || v.Samples != 3 || v.Kind != "codex" || v.Size != "S" || v.Label != "over usual (+7m)" || v.Explanation != "Usual = 24m: median of 3 earlier codex size-S runs; this run 31m" {
		t.Fatal("ETA provenance or short label missing")
	}
	if m.etaDetails(runs[:2], r, "codex", "S", time.Minute) != nil {
		t.Fatal("insufficient history must not claim ETA")
	}
}

func TestNotificationSettingsCLI(t *testing.T) {
	c, store := fixture(t)
	c.NotifyIMessage = "+15551234567"
	cliFixture(t, c, store)
	for _, value := range []string{"off", "on"} {
		var out, diagnostic bytes.Buffer
		if CLI(context.Background(), []string{"set", "notify-imessage", value}, strings.NewReader(""), &out, &diagnostic) != 0 {
			t.Fatal("notification toggle refused")
		}
		loaded, err := LoadConfig(os.Getenv("NUNTIUS_CONFIG"))
		if err != nil || loaded.NotifyIMessage != c.NotifyIMessage || loaded.imessageEnabled() != (value == "on") {
			t.Fatal("toggle must preserve recipient and enabled state")
		}
	}
	var out, diagnostic bytes.Buffer
	if CLI(context.Background(), []string{"set", "--json"}, strings.NewReader(""), &out, &diagnostic) != 0 {
		t.Fatal("safe settings snapshot refused")
	}
	var snapshot map[string]any
	if json.Unmarshal(out.Bytes(), &snapshot) != nil || snapshot["max_runtime"] != "10s" || strings.Contains(out.String(), c.NotifyIMessage) || strings.Contains(out.String(), "extra_args") {
		t.Fatal("settings snapshot missing values or exposes recipient/flags")
	}
}
