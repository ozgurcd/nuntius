package nuntius

import (
	"reflect"
	"testing"
)

func TestRequiredAgentFloorFlags(t *testing.T) {
	cases := []struct {
		name, kind string
		args       []string
		allowed    bool
	}{
		{"claude-absent", "claude", nil, false},
		{"claude-model-only", "claude", []string{"--model", "example"}, false},
		{"claude-substring", "claude", []string{"--model=model--permission-mode=auto"}, false},
		{"claude-missing-value", "claude", []string{"--permission-mode"}, false},
		{"claude-empty-value", "claude", []string{"--permission-mode="}, false},
		{"claude-unsafe-value", "claude", []string{"--permission-mode=bypassPermissions"}, false},
		{"claude-wrong-kind", "claude", []string{"--sandbox=read-only"}, false},
		{"codex-absent", "codex", nil, false},
		{"codex-skip-only", "codex", []string{"--skip-git-repo-check"}, false},
		{"codex-substring", "codex", []string{"--model=model--sandbox=read-only"}, false},
		{"codex-missing-value", "codex", []string{"--sandbox"}, false},
		{"codex-empty-value", "codex", []string{"--sandbox="}, false},
		{"codex-unsafe-value", "codex", []string{"--sandbox=danger-full-access"}, false},
		{"codex-wrong-kind", "codex", []string{"--permission-mode=auto"}, false},
		{"opencode-no-floor", "opencode", nil, true},
		{"opencode-options", "opencode", []string{"--agent=build", "--variant", "example"}, true},
		{"opencode-permission-mode", "opencode", []string{"--permission-mode=auto"}, false},
		{"opencode-sandbox", "opencode", []string{"--sandbox=read-only"}, false},
	}
	for _, mode := range []string{"auto", "acceptEdits", "plan", "manual"} {
		cases = append(cases,
			struct {
				name, kind string
				args       []string
				allowed    bool
			}{"claude-split-" + mode, "claude", []string{"--permission-mode", mode}, true},
			struct {
				name, kind string
				args       []string
				allowed    bool
			}{"claude-equals-" + mode, "claude", []string{"--permission-mode=" + mode}, true})
	}
	for _, mode := range []string{"read-only", "workspace-write"} {
		cases = append(cases,
			struct {
				name, kind string
				args       []string
				allowed    bool
			}{"codex-split-" + mode, "codex", []string{"--sandbox", mode}, true},
			struct {
				name, kind string
				args       []string
				allowed    bool
			}{"codex-equals-" + mode, "codex", []string{"--sandbox=" + mode}, true})
	}
	for _, tc := range cases {
		for _, shape := range []string{"static", "pool-enabled", "pool-disabled"} {
			t.Run(tc.name+"/"+shape, func(t *testing.T) {
				c, _ := fixture(t)
				lane := c.Lanes[0]
				lane.Name, lane.Kind, lane.ExtraArgs = "floor", tc.kind, tc.args
				if tc.kind == "opencode" {
					lane.Model = "provider/model"
				}
				c.Lanes = []Lane{lane}
				if shape != "static" {
					lane.Name, lane.Kind = "", ""
					size := 1
					if shape == "pool-disabled" {
						size = 0
					}
					c.Lanes = nil
					c.Pools = map[string]AgentPool{tc.kind: {Size: size, Template: lane}}
				}
				before := append([]string(nil), tc.args...)
				_, err := c.Validate()
				if (err == nil) != tc.allowed {
					t.Fatalf("floor flag acceptance mismatch: %v", err)
				}
				if !reflect.DeepEqual(tc.args, before) {
					t.Fatal("validation rewrote the supplied arguments")
				}
			})
		}
	}
}
