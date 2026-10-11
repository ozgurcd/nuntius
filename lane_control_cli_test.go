package nuntius

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func laneCLIConfig(t *testing.T, c Config) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	p := filepath.Join(t.TempDir(), "lanes.json")
	b, _ := json.Marshal(c)
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func laneCLI(t *testing.T, config, store string, args ...string) (int, string, string) {
	t.Helper()
	before, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	code := CLI(context.Background(), append([]string{"--config", config, "--store", store}, args...), strings.NewReader(""), &out, &errs)
	after, err := os.ReadFile(config)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("lane command changed config bytes")
	}
	return code, out.String(), errs.String()
}

func laneCLITree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		value := fmt.Sprint(fi.Mode(), fi.ModTime().UnixNano())
		if fi.Mode().IsRegular() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			value += string(b)
		}
		out[p] = value
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestLaneCLISyntaxAndDurability(t *testing.T) {
	c, store := fixture(t)
	config := laneCLIConfig(t, c)
	for _, args := range [][]string{{"lane"}, {"lane", "pause"}, {"lane", "pause", "codex", "extra"}, {"lane", "resume", "codex", "--after-current"}, {"lane", "list", "extra"}, {"lane", "list", "--json=false"}, {"lane", "pause", "codex", "--after-current=false"}, {"lane", "pause", "../codex"}, {"lane", "pause", "codex ", "--after-current"}, {"lane", "list", "--unknown"}} {
		code, _, _ := laneCLI(t, config, store, args...)
		if code != 2 {
			t.Errorf("syntax %v returned %d", args, code)
		}
	}
	if _, err := os.Stat(store); !os.IsNotExist(err) {
		t.Fatal("syntax created store")
	}
	for _, args := range [][]string{{"lane", "pause", "codex"}, {"lane", "pause", "codex", "--after-current"}, {"lane", "resume", "codex"}, {"lane", "resume", "codex"}} {
		code, out, errs := laneCLI(t, config, store, args...)
		if code != 0 || strings.TrimSpace(out) == "" {
			t.Fatalf("durable command %v failed: %d %s", args, code, errs)
		}
	}
	if code, _, _ := laneCLI(t, config, store, "lane", "pause", "unknown"); code != 1 {
		t.Fatal("unknown exact name must refuse")
	}
}

func TestLaneCLIReadNoCreateOrReconcile(t *testing.T) {
	for _, state := range []string{"missing_store", "missing_lock", "intent", "ended", "stuck"} {
		t.Run(state, func(t *testing.T) {
			c, store := fixture(t)
			config := laneCLIConfig(t, c)
			if state != "missing_store" {
				m, err := NewManager(c, store)
				if err != nil {
					t.Fatal(err)
				}
				_ = m.Close()
				if state == "missing_lock" {
					if err := os.Remove(filepath.Join(store, "locks", "control.lock")); err != nil {
						t.Fatal(err)
					}
				}
				if state == "intent" {
					if err := os.WriteFile(filepath.Join(store, "lane-control-intent.json"), []byte("{}"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if state == "ended" || state == "stuck" {
					r := seedCrashRun(t, m, strings.Repeat("b", 32), "PURE-READ", "failed")
					if state == "ended" {
						r.State = "ended"
						r.LaunchdLabel = "invalid-fixture"
					} else {
						r.Started = time.Now().Add(-20 * time.Minute)
					}
					if err := m.save(&r); err != nil {
						t.Fatal(err)
					}
				}
			}
			before := laneCLITree(t, store)
			for _, args := range [][]string{{"status"}, {"status", "--json"}, {"status", "--long"}, {"agents", "--json"}, {"lane", "list"}, {"lane", "list", "--json"}} {
				code, out, errs := laneCLI(t, config, store, args...)
				wantCode := 0
				if args[0] == "lane" && (state == "missing_lock" || state == "missing_store" || state == "intent") {
					wantCode = 1
				}
				if code != wantCode {
					t.Fatalf("read failed %v: %d %s", args, code, errs)
				}
				if state == "intent" || state == "missing_lock" || state == "missing_store" {
					if !strings.Contains(out, "unavailable") && !strings.Contains(out, "cannot be read") {
						t.Fatal("unavailable controls look enabled")
					}
					if !strings.Contains(out, "Lane hold is unknown") {
						t.Fatal("unavailable hold is not explained as unknown")
					}
				}
				if !reflect.DeepEqual(before, laneCLITree(t, store)) {
					t.Fatalf("read %v mutated tree", args)
				}
			}
		})
	}
}

func TestLaneCLISelectorsAndManagerJSON(t *testing.T) {
	m, store := laneControlCommitFixture(t)
	config := laneCLIConfig(t, m.config)
	if code, _, _ := laneCLI(t, config, store, "lane", "pause", "codex"); code != 0 {
		t.Fatal("pause")
	}
	want, err := m.laneControls()
	if err != nil {
		t.Fatal(err)
	}
	code, out, _ := laneCLI(t, config, store, "lane", "list", "--json")
	if code != 0 || strings.TrimSpace(out) != toolText(want) {
		t.Fatal("list differs from manager snapshot")
	}
	t.Setenv("NUNTIUS_CONFIG", config)
	t.Setenv("NUNTIUS_DATA_DIR", store)
	var env bytes.Buffer
	if CLI(context.Background(), []string{"lane", "list", "--json"}, strings.NewReader(""), &env, &env) != 0 || env.String() != out {
		t.Fatal("environment selectors")
	}
	cmd := exec.Command(cliBinary, "--config", config, "--store", store, "lane", "list", "--json")
	got, err := cmd.Output()
	if err != nil || string(got) != out {
		t.Fatal("compiled CLI snapshot")
	}
}

func TestLaneCLIOutcomeAndDrain(t *testing.T) {
	m, store := laneControlCommitFixture(t)
	config := laneCLIConfig(t, m.config)
	id, err := m.Dispatch("codex", "CLI-DRAIN", "admission-barrier")
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, m, "codex")
	defer func() { _ = m.Cancel("codex") }()
	if code, _, _ := laneCLI(t, config, store, "lane", "pause", "codex", "--after-current"); code != 0 {
		t.Fatal("pause")
	}
	before := admissionArtifacts(t, store)
	for _, args := range [][]string{{"lane", "list", "--json"}, {"status", "--json"}, {"agents", "--json"}} {
		code, out, _ := laneCLI(t, config, store, args...)
		if code != 0 || !strings.Contains(out, `"phase":"draining"`) || !strings.Contains(out, `"paused":true`) {
			t.Fatal("running hold missing")
		}
	}
	if !reflect.DeepEqual(before, admissionArtifacts(t, store)) {
		t.Fatal("read created a child or changed run")
	}
	r, err := m.latest("codex")
	if err != nil || r.ID != id || r.StopAt != nil || !sameProcess(*r) {
		t.Fatal("pause interrupted run")
	}
	if err := os.WriteFile(filepath.Join(m.config.ReportsDir, "admission-release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	waitState(t, m, "codex", "ended")
	if err := os.WriteFile(filepath.Join(m.config.ReportsDir, "CLI-DRAIN.txt"), []byte("fixture report"), 0600); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	r, _ = m.latest("codex")
	r.StopReason = "failed"
	x := 1
	r.Exit = &x
	err = m.save(r)
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	code, out, _ := laneCLI(t, config, store, "status", "--json")
	if code != 0 || !strings.Contains(out, `"phase":"paused"`) || !strings.Contains(out, `"outcome":"failed"`) || !strings.Contains(out, `"badge":"red"`) || !strings.Contains(out, `"report_exists":true`) {
		t.Fatal("hold hid failed outcome or report")
	}
	_, human, _ := laneCLI(t, config, store, "status")
	if !strings.Contains(human, "Paused by you at") || !strings.Contains(human, "via CLI") || !strings.Contains(human, "failed") {
		t.Fatal("plain hold explanation missing")
	}
}

func TestLaneCLIRetentionPoolAndFrozenResume(t *testing.T) {
	m, store := laneControlCommitFixture(t)
	c := m.config
	config := laneCLIConfig(t, c)
	if code, _, _ := laneCLI(t, config, store, "lane", "pause", "codex"); code != 0 {
		t.Fatal("pause")
	}
	c.Lanes = c.Lanes[:1]
	b, _ := json.Marshal(c)
	if err := os.WriteFile(config, b, 0600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := laneCLI(t, config, store, "lane", "list", "--json")
	if code != 0 || !strings.Contains(out, `"configured":false`) || strings.Index(out, `"name":"claude"`) > strings.Index(out, `"name":"codex"`) {
		t.Fatal("retained sorted orphan missing")
	}
	if code, _, _ := laneCLI(t, config, store, "lane", "pause", "codex"); code != 1 {
		t.Fatal("orphan pause")
	}
	if code, _, _ := laneCLI(t, config, store, "lane", "resume", "codex"); code != 0 {
		t.Fatal("orphan resume")
	}
	c.Lanes = append(c.Lanes, Lane{Name: "codex", Kind: "claude", Workdir: c.Lanes[0].Workdir, Binary: fakeBinary})
	b, _ = json.Marshal(c)
	_ = os.WriteFile(config, b, 0600)
	if code, _, _ := laneCLI(t, config, store, "lane", "pause", "codex"); code != 0 {
		t.Fatal("readded kind pause")
	}
	// Removing/re-adding an exact name, with a different kind, retains its hold.
	c.Lanes = c.Lanes[:1]
	b, _ = json.Marshal(c)
	_ = os.WriteFile(config, b, 0600)
	c.Lanes = append(c.Lanes, Lane{Name: "codex", Kind: "codex", Workdir: c.Lanes[0].Workdir, Binary: fakeBinary})
	b, _ = json.Marshal(c)
	_ = os.WriteFile(config, b, 0600)
	if code, out, _ := laneCLI(t, config, store, "lane", "list", "--json"); code != 0 || !strings.Contains(out, `"paused":true`) {
		t.Fatal("changed kind erased retained hold")
	}
	if err := m.Freeze("fixture freeze"); err != nil {
		t.Fatal(err)
	}
	frozen, _ := os.ReadFile(filepath.Join(store, "freeze.json"))
	if code, out, _ := laneCLI(t, config, store, "lane", "resume", "codex"); code != 0 || !strings.Contains(out, "frozen") {
		t.Fatal("resume hides freeze")
	}
	after, _ := os.ReadFile(filepath.Join(store, "freeze.json"))
	if !bytes.Equal(frozen, after) {
		t.Fatal("resume cleared freeze")
	}
	c.Pools = map[string]AgentPool{"codex": {Size: 0, Template: Lane{Kind: "codex", Workdir: c.Lanes[0].Workdir, Binary: fakeBinary}}}
	c.Lanes = nil
	b, _ = json.Marshal(c)
	_ = os.WriteFile(config, b, 0600)
	if code, out, _ := laneCLI(t, config, store, "lane", "list", "--json"); code != 0 || !strings.Contains(out, `"configured":false`) {
		t.Fatal("zero pool retained record")
	}
	if code, _, _ := laneCLI(t, config, store, "lane", "resume", "codex"); code != 0 {
		t.Fatal("zero pool orphan resume")
	}
}

func TestLaneCLIHeldReviewExplicit(t *testing.T) {
	m, h := heldReviewFixture(t, "fixture-thread")
	config := laneCLIConfig(t, m.config)
	for _, args := range [][]string{{"lane", "list", "--json"}, {"status", "--json"}, {"agents", "--json"}} {
		code, out, _ := laneCLI(t, config, m.store, args...)
		if code != 0 || strings.Contains(out, reviewedText) || strings.Contains(out, h.SessionID) {
			t.Fatal("implicit private content exposure")
		}
	}
	before := admissionArtifacts(t, m.store)
	code, out, _ := laneCLI(t, config, m.store, "lane", "held", "review", h.Lane, h.RunID)
	if code != 0 || !strings.Contains(out, reviewedText) || strings.Contains(out, h.SessionID) || !reflect.DeepEqual(before, admissionArtifacts(t, m.store)) {
		t.Fatal("explicit review missing or mutating")
	}
	args := []string{"lane", "held", "send", h.Lane, h.RunID, "--revision", strconv.FormatUint(h.Revision, 10), "--digest", h.ContentSHA256}
	bad := append([]string{}, args...)
	bad[6] = "99"
	if code, _, _ := laneCLI(t, config, m.store, bad...); code != 1 {
		t.Fatal("stale review sent")
	}
	if code, _, _ := laneCLI(t, config, m.store, args...); code != 0 {
		t.Fatal("explicit reviewed send")
	}
	waitState(t, m, h.Lane, "ended")
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := laneCLI(t, config, m.store, args...); code != 0 {
		t.Fatalf("send receipt retry exit=%d: %s", code, errs)
	}
	heldReviewChildren(t, m, h, 1)
}

func TestLaneCLIHeldDiscardAndBounds(t *testing.T) {
	m, h := heldReviewFixture(t, "")
	config := laneCLIConfig(t, m.config)
	if code, _, errs := laneCLI(t, config, m.store, "lane", "held", "send", h.Lane, h.RunID, "--revision", strconv.FormatUint(h.Revision, 10), "--digest", h.ContentSHA256); code != 1 || !strings.Contains(errs, "session") {
		t.Fatal("missing session explanation")
	}
	for i := 0; i < 2; i++ {
		if code, _, _ := laneCLI(t, config, m.store, "lane", "held", "discard", h.Lane, h.RunID, "--revision", strconv.FormatUint(h.Revision, 10), "--digest", h.ContentSHA256); code != 0 {
			t.Fatal("discard receipt")
		}
	}
	heldReviewChildren(t, m, h, 0)
}

func TestLaneCLIHeldBoundedRedactedAndRefused(t *testing.T) {
	m, h := heldReviewFixture(t, "fixture-thread")
	config := laneCLIConfig(t, m.config)
	text := strings.Repeat("review text ", 950) + "\npassword=synthetic-only-value"
	h.ContentSHA256 = laneControlDigest([]byte(text))
	message, _ := json.Marshal(map[string]string{"text": text})
	binding, _ := json.Marshal(h)
	if err := atomicPrivate(filepath.Join(m.store, "runs", h.RunID, "message.json"), message); err != nil {
		t.Fatal(err)
	}
	if err := atomicPrivate(filepath.Join(m.store, "runs", h.RunID, "message-hold.json"), binding); err != nil {
		t.Fatal(err)
	}
	code, out, _ := laneCLI(t, config, m.store, "lane", "held", "review", h.Lane, h.RunID)
	var view struct {
		Text string `json:"text"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &view) != nil || len(view.Text) > 8192 || !strings.Contains(view.Text, "[truncated]") || strings.Contains(out, "synthetic-only-value") || strings.Contains(out, h.SessionID) {
		t.Fatalf("review bounds or redaction: exit=%d, text bytes=%d, truncated=%t, synthetic leak=%t, session leak=%t", code, len(view.Text), strings.Contains(view.Text, "[truncated]"), strings.Contains(out, "synthetic-only-value"), strings.Contains(out, h.SessionID))
	}
	before := admissionArtifacts(t, m.store)
	args := []string{"lane", "held", "send", h.Lane, h.RunID, "--revision", strconv.FormatUint(h.Revision, 10), "--digest", h.ContentSHA256}
	if code, _, _ := laneCLI(t, config, m.store, "lane", "pause", h.Lane); code != 0 {
		t.Fatal("pause")
	}
	if code, _, errs := laneCLI(t, config, m.store, args...); code != 1 || !strings.Contains(errs, "paused by you") {
		t.Fatal("paused message send")
	}
	if code, _, _ := laneCLI(t, config, m.store, "lane", "resume", h.Lane); code != 0 {
		t.Fatal("resume")
	}
	if !reflect.DeepEqual(before, admissionArtifacts(t, m.store)) {
		t.Fatal("resume replayed or consumed held message")
	}
	if err := m.Freeze("fixture"); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := laneCLI(t, config, m.store, args...); code != 1 || !strings.Contains(errs, "frozen") {
		t.Fatal("frozen message send")
	}
	wrong := append([]string{}, args...)
	wrong[4] = strings.Repeat("c", 32)
	if code, _, _ := laneCLI(t, config, m.store, wrong...); code != 1 {
		t.Fatal("wrong original run")
	}
	for _, args := range [][]string{{"lane", "held", "review", h.Lane}, {"lane", "held", "send", h.Lane, h.RunID}, {"lane", "held", "send", h.Lane, h.RunID, "--revision", "01", "--digest", h.ContentSHA256}, {"lane", "held", "discard", h.Lane, h.RunID, "--revision", "1", "--revision", "1"}} {
		if code, _, _ := laneCLI(t, config, m.store, args...); code != 2 {
			t.Fatal("held syntax")
		}
	}
	heldReviewChildren(t, m, h, 0)
}
