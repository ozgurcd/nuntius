package nuntius

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPoolsConversionResizeAndAgents(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c, store := fixture(t)
	c.Lanes[0].Name = "identuum"
	c.Lanes[1].Name = "codex-1"
	p := filepath.Join(c.ReportsDir, "lanes.json")
	b, _ := json.Marshal(c)
	if err := os.WriteFile(p, b, 0644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	run := func(args ...string) int {
		out.Reset()
		return CLI(context.Background(), append([]string{"--config", p, "--store", store}, args...), strings.NewReader(""), &out, &out)
	}
	if run("config", "convert") != 0 {
		t.Fatal("convert failed")
	}
	c, err := LoadConfig(p)
	if err != nil || len(c.Pools) != 2 || c.Lanes[0].Name != "claude-1" {
		t.Fatal("pool conversion")
	}
	if st, err := os.Stat(p); err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("config mode")
	}
	if run("set", "claude-agent-pool", "8") != 0 {
		t.Fatal("resize failed")
	}
	c, err = LoadConfig(p)
	if err != nil || len(c.Lanes) != 9 {
		t.Fatal("pool expansion")
	}
	if run("agents", "--json") != 0 || !json.Valid(out.Bytes()) || !strings.Contains(out.String(), "claude-8") {
		t.Fatal("agents JSON")
	}
	if run("agents") != 0 || !strings.Contains(out.String(), "goal_then_continue") {
		t.Fatal("agents text")
	}
	for _, n := range []string{"-1", "9", "x"} {
		if run("set", "claude-agent-pool", n) == 0 {
			t.Fatal("invalid size")
		}
	}
	m, err := NewManager(c, store)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	_, err = m.Dispatch("claude-8", "POOL-1", "wait")
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, m, "claude-8")
	before, _ := os.ReadFile(p)
	if run("set", "claude-agent-pool", "7") == 0 {
		t.Fatal("busy shrink accepted")
	}
	after, _ := os.ReadFile(p)
	if !bytes.Equal(before, after) {
		t.Fatal("refusal changed config")
	}
	_ = m.Cancel("claude-8")
	waitState(t, m, "claude-8", "ended")
	if run("set", "claude-agent-pool", "0") != 0 || run("set", "codex-agent-pool", "0") != 0 {
		t.Fatal("zero pools refused")
	}
	c, err = LoadConfig(p)
	if err != nil || len(c.Lanes) != 0 {
		t.Fatal("zero pool config")
	}
	if _, err = call(m, "nuntius_set", json.RawMessage(`{}`)); err == nil {
		t.Fatal("pool MCP exposed")
	}
}

func TestPoolsRejectLossyConversionAndUnsafeZeroTemplate(t *testing.T) {
	c, _ := fixture(t)
	c.Lanes = append(c.Lanes, Lane{Name: "codex-2", Kind: "codex", Binary: fakeBinary, Workdir: c.ReportsDir, ExtraArgs: []string{"--model", "different"}})
	if _, err := c.poolConfig(); err == nil {
		t.Fatal("lossy conversion accepted")
	}
	c.Lanes = nil
	c.Pools = map[string]AgentPool{"claude": {Size: 0, Template: Lane{Binary: fakeBinary, Workdir: c.ReportsDir, ExtraArgs: []string{"--yolo"}}}}
	if _, err := c.Validate(); err == nil {
		t.Fatal("zero template bypass accepted")
	}
}

func TestCodexResumeArgvScope(t *testing.T) {
	l := Lane{Kind: "codex", ExtraArgs: []string{"--skip-git-repo-check", "--sandbox", "workspace-write", "--model=m"}}
	want := []string{"exec", "--json", "--skip-git-repo-check", "--sandbox", "workspace-write", "--model=m", "resume", "fixture-thread", "-"}
	if !reflect.DeepEqual(agentArgs(l, "fixture-thread"), want) {
		t.Fatal("resume flag scope incorrect")
	}
	m, c, store := manager(t)
	_ = m.Close()
	c.Lanes[1].ExtraArgs = l.ExtraArgs
	next, err := NewManager(c, store)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if _, err = next.Dispatch("codex", "ARGV-1", "hello"); err != nil {
		t.Fatal(err)
	}
	waitState(t, next, "codex", "ended")
	if _, err = next.Reply("codex", "answer"); err != nil {
		t.Fatal(err)
	}
	s := waitState(t, next, "codex", "ended")
	if s.Exit == nil || *s.Exit != 0 {
		t.Fatal("strict fake rejected resume")
	}
}

func TestStrictFakeRejectsWrongCodexOrder(t *testing.T) {
	cmd := exec.Command(fakeBinary, "exec", "resume", "fixture-thread", "--sandbox", "workspace-write", "--json", "-")
	cmd.Stdin = strings.NewReader("hello")
	err := cmd.Run()
	if err == nil || cmd.ProcessState.ExitCode() != 2 {
		t.Fatal("fake failed to reject wrong resume order")
	}
}
