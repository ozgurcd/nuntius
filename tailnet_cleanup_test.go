package nuntius

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A command can apply its mapping before reporting failure. Cleanup must
// inspect the current mapping immediately, without relying on a later release.
func TestWebEnsurePartialChildServeCleanup(t *testing.T) {
	for _, tc := range []struct {
		name, mapping, marker, reason string
		keep, removed                 bool
	}{
		{name: "owned", removed: true},
		{name: "keep still cleans failed create", keep: true, removed: true},
		{name: "foreign port", mapping: fixtureEnsureMapping("9999"), reason: "Serve cleanup refused"},
		{name: "foreign host", mapping: strings.ReplaceAll(fixtureEnsureMapping("8123"), "mac.tail123", "other.tail123"), reason: "Serve cleanup refused"},
		{name: "unreadable", mapping: "{", reason: "Serve cleanup refused"},
		{name: "already absent", mapping: "null"},
		{name: "status unavailable", marker: "serve-status-fail-after-create", reason: "Serve cleanup unavailable"},
		{name: "remove fails", marker: "serve-off-fail", removed: true, reason: "Serve cleanup failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := fakeTailscale(t, fixtureTailnetStatus)
			if os.WriteFile(filepath.Join(dir, "serve-partial-fail"), []byte(tc.mapping), 0600) != nil {
				t.Fatal("fake partial failure setup")
			}
			if tc.marker != "" && os.WriteFile(filepath.Join(dir, tc.marker), nil, 0600) != nil {
				t.Fatal("fake cleanup failure setup")
			}
			exe, err := findTailscale()
			if err != nil {
				t.Fatal("fake discovery")
			}
			tn := &tailnet{exe: exe, host: "mac.tail123.ts.net", keep: tc.keep}
			if _, err := tn.ensure("8123"); err == nil || !strings.Contains(err.Error(), "could not be set up") || tc.reason != "" && !strings.Contains(err.Error(), tc.reason) {
				t.Fatal("failed create or cleanup error was not reported")
			}
			calls, err := os.ReadFile(filepath.Join(dir, "calls.log"))
			if err != nil || strings.Contains(string(calls), "serve --https=443 off") != tc.removed {
				t.Fatal("cleanup did not respect current mapping ownership")
			}
			b, err := os.ReadFile(filepath.Join(dir, "serve.json"))
			if err != nil {
				t.Fatal("fake mapping unavailable")
			}
			if tc.removed && tc.marker == "" {
				if state, err := servePlan(b, tn.host, "8123"); err != nil || state != "absent" {
					t.Fatal("failed create retained its exact owned mapping")
				}
			} else if tc.mapping != "" && string(b) != tc.mapping {
				t.Fatal("cleanup changed an unowned or uncertain mapping")
			}
		})
	}
}
