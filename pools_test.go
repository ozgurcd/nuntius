package nuntius

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

func settingsFixture(t *testing.T) (*Manager, *Web, string) {
	t.Helper()
	m, c, _ := manager(t)
	c, err := c.poolConfig()
	if err != nil {
		t.Fatal(err)
	}
	c.NotifyIMessage = "fixture@example.invalid"
	c.Pools["codex"] = AgentPool{Size: 2, Template: c.Pools["codex"].Template}
	p := filepath.Join(t.TempDir(), "config.json")
	b, _ := json.Marshal(c)
	if os.WriteFile(p, b, 0600) != nil {
		t.Fatal("config fixture")
	}
	m.mu.Lock()
	m.config.SourcePath = p
	m.mu.Unlock()
	w, err := NewWeb(m, "127.0.0.1:8123", "fixture.ts.net")
	if err != nil {
		t.Fatal(err)
	}
	return m, w, p
}

// settingsQuietFixture keeps a live settings owner without periodic Status
// workers competing with the locks that these offline proofs control.
func settingsQuietFixture(t *testing.T) (*Manager, *Web, string) {
	t.Helper()
	prior, _, path := settingsFixture(t)
	if err := prior.Close(); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	c.SourcePath = path
	m, err := newReadManager(c, prior.store)
	if err != nil {
		t.Fatal(err)
	}
	m.readOnly = false
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Error(err)
		}
	})
	if m.life == nil || m.life.Err() != nil {
		t.Fatal("settings fixture must have a live owner")
	}
	w, err := NewWeb(m, "127.0.0.1:8123", "fixture.ts.net")
	if err != nil {
		t.Fatal(err)
	}
	return m, w, path
}

func settingsHTTP(t *testing.T, w *Web, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	if w.manager.life == nil || w.manager.life.Err() != nil {
		t.Fatal("settings fixture must have a live owner")
	}
	r := httptest.NewRequest(method, "http://localhost:8123/settings", strings.NewReader(body))
	r.Header.Set("X-Nuntius-Token", w.token)
	r.Header.Set("Origin", "http://localhost:8123")
	r.Header.Set("Content-Type", "application/json")
	out := httptest.NewRecorder()
	w.ServeHTTP(out, r)
	return out
}
func settingsRevision(t *testing.T, w *Web) string {
	t.Helper()
	out := settingsHTTP(t, w, "GET", "")
	var value struct{ Revision string }
	if out.Code != 200 || json.Unmarshal(out.Body.Bytes(), &value) != nil || value.Revision == "" {
		t.Fatal("settings read failed")
	}
	return value.Revision
}
func settingsBody(revision, changes string) string {
	return fmt.Sprintf(`{"schema_version":1,"expected_revision":%q,"changes":%s}`, revision, changes)
}

func TestSettingsStrictFraming(t *testing.T) {
	m, w, p := settingsFixture(t)
	revision := settingsRevision(t, w)
	before, _ := os.ReadFile(p)
	for _, change := range []string{`{}`, `{"pools":{"claude":null}}`, `{"pools":{"claude":"2"}}`, `{"pools":{"claude":1.5}}`, `{"pools":{"claude":1,"claude":2}}`, `{"notify_imessage_enabled":null}`, `{"reports_dir":"ignored"}`, `{"pools":{"claude":2},"pools":{"codex":2}}`, `{"notify_imessage_enabled":true,"notify_imessage_enabled":false}`, `{"Pools":{"claude":2}}`, `{"Notify_imessage_enabled":false}`} {
		w.settingsState.post = settingsBucket{}
		if settingsHTTP(t, w, "POST", settingsBody(revision, change)).Code != 400 {
			t.Fatal("malformed batch accepted")
		}
	}
	for _, body := range []string{settingsBody(revision, `{"pools":{"claude":2}}`) + `{}`, strings.Repeat(" ", 8193), `{"schema_version":1,"schema_version":1,"expected_revision":"ignored","changes":{"pools":{"claude":2}}}`, strings.Replace(settingsBody(revision, `{"pools":{"claude":2}}`), `"changes"`, `"Changes"`, 1)} {
		w.settingsState.post = settingsBucket{}
		if settingsHTTP(t, w, "POST", body).Code != 400 {
			t.Fatal("invalid framing accepted")
		}
	}
	for _, change := range []string{`{"pools":{"other":2}}`, `{"pools":{"opencode":1}}`, `{"pools":{"claude":-1}}`, `{"pools":{"claude":9}}`, `{"pools":{"claude":1}}`} {
		w.settingsState.post = settingsBucket{}
		if settingsHTTP(t, w, "POST", settingsBody(revision, change)).Code != 422 {
			t.Fatal("invalid setting accepted")
		}
	}
	after, _ := os.ReadFile(p)
	if !bytes.Equal(before, after) {
		t.Fatal("invalid request wrote config")
	}
	data, _ := os.ReadFile(filepath.Join(m.store, "audit.jsonl"))
	if strings.Contains(string(data), "committed") {
		t.Fatal("invalid request recorded commit")
	}
	r := httptest.NewRequest("POST", "http://localhost:8123/settings", strings.NewReader(settingsBody(revision, `{"pools":{"claude":2}}`)))
	r.Header.Set("X-Nuntius-Token", w.token)
	r.Header.Set("Origin", "http://localhost:8123")
	r.Header.Set("Content-Type", "text/plain")
	w.settingsState.post = settingsBucket{}
	out := httptest.NewRecorder()
	w.ServeHTTP(out, r)
	if out.Code != 400 {
		t.Fatal("non JSON accepted")
	}
}

func TestSettingsConflictLocksAndParity(t *testing.T) {
	m, w, p := settingsQuietFixture(t)
	revision := settingsRevision(t, w)
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	other, err := newReadManager(c, m.store)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	other.readOnly = false
	if other.life == nil || other.life.Err() != nil {
		t.Fatal("settings sibling fixture must have a live owner")
	}
	if other.SetPool(p, "claude", 3, false) != nil {
		t.Fatal("CLI writer failed")
	}
	if out := settingsHTTP(t, w, "POST", settingsBody(revision, `{"pools":{"codex":1}}`)); out.Code != 409 {
		t.Fatalf("stale revision overwrote CLI: HTTP %d", out.Code)
	}
	revision = settingsRevision(t, w)
	lock, err := m.controlLock()
	if err != nil {
		t.Fatal(err)
	}
	if settingsHTTP(t, w, "POST", settingsBody(revision, `{"pools":{"codex":1}}`)).Code != 503 {
		t.Fatal("held control lock accepted")
	}
	lock.Close()
	m.mu.Lock()
	if settingsHTTP(t, w, "GET", "").Code != 503 {
		t.Fatal("manager lock accepted")
	}
	_, startupErr := NewWeb(m, "127.0.0.1:8123")
	m.mu.Unlock()
	if startupErr != nil {
		t.Fatal("busy settings recovery prevented dashboard construction")
	}
	// Lock the removed lane, as another manager/surviving agent would do.
	busy, err := m.lock("codex-2")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	w.settingsState.post = settingsBucket{}
	before, _ := os.ReadFile(p)
	if settingsHTTP(t, w, "POST", settingsBody(revision, `{"pools":{"claude":4,"codex":0}}`)).Code != 409 || other.SetPool(p, "codex", 0, false) == nil {
		t.Fatal("busy shrink parity")
	}
	after, _ := os.ReadFile(p)
	if !bytes.Equal(before, after) {
		t.Fatal("busy batch partially applied")
	}
	_ = syscall.Flock(int(busy.Fd()), syscall.LOCK_UN)
	auditLock, err := privateFile(filepath.Join(m.store, "audit.jsonl"), os.O_RDWR|os.O_CREATE)
	if err != nil {
		t.Fatal(err)
	}
	if syscall.Flock(int(auditLock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		t.Fatal("audit lock fixture")
	}
	w.settingsState.post = settingsBucket{}
	if settingsHTTP(t, w, "POST", settingsBody(revision, `{"pools":{"claude":4}}`)).Code != 503 {
		t.Fatal("held audit lock accepted")
	}
	auditLock.Close()
	after, _ = os.ReadFile(p)
	if !bytes.Equal(before, after) {
		t.Fatal("audit refusal mutated config")
	}
	for _, n := range []int{-1, 9} {
		if other.SetPool(p, "codex", n, false) == nil {
			t.Fatal("CLI size policy loosened")
		}
	}
	w.settingsState.post = settingsBucket{}
	if settingsHTTP(t, w, "POST", settingsBody(revision, `{"pools":{"claude":4,"codex":0},"notify_imessage_enabled":false}`)).Code != 200 {
		t.Fatal("good batch refused")
	}
	loaded, err := LoadConfig(p)
	if err != nil || loaded.Pools["claude"].Size != 4 || loaded.Pools["codex"].Size != 0 || loaded.NotifyIMessage != c.NotifyIMessage || !reflect.DeepEqual(loaded.Pools["codex"].Template, c.Pools["codex"].Template) {
		t.Fatal("untouched template/recipient not retained")
	}
	if other.reload() != nil || other.config.Pools["claude"].Size != 4 {
		t.Fatal("sibling did not reload")
	}
}

func TestSettingsRevisionIgnoresFileMetadata(t *testing.T) {
	_, w, p := settingsQuietFixture(t)
	stamp := time.Unix(1700000000, 0)
	if err := os.Chtimes(p, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	revision := settingsRevision(t, w)
	changed := bytes.Replace(before, []byte(`"size":1`), []byte(`"size":3`), 1)
	if len(before) != len(changed) || bytes.Equal(before, changed) {
		t.Fatal("fixture needs different contents with the same size")
	}
	if err := os.WriteFile(p, changed, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(p)
	if err != nil || beforeInfo.Size() != afterInfo.Size() || !beforeInfo.ModTime().Equal(afterInfo.ModTime()) {
		t.Fatal("fixture metadata changed")
	}
	current := settingsRevision(t, w)
	if current == revision {
		t.Fatal("changed contents kept the old revision")
	}
	if err := os.Chtimes(p, stamp.Add(time.Hour), stamp.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if settingsRevision(t, w) != current {
		t.Fatal("metadata alone changed the revision")
	}
	if out := settingsHTTP(t, w, "POST", settingsBody(revision, `{"pools":{"codex":1}}`)); out.Code != 409 {
		t.Fatalf("stale revision with identical metadata: HTTP %d", out.Code)
	}
	after, err := os.ReadFile(p)
	if err != nil || !bytes.Equal(changed, after) {
		t.Fatal("stale request changed persisted settings")
	}
}

func TestSettingsStaleRevisionWhileLocked(t *testing.T) {
	m, w, p := settingsQuietFixture(t)
	revision := settingsRevision(t, w)
	if err := m.SetPool(p, "claude", 3, false); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	// Status holds this mutex before it reads the config and the run store.
	m.mu.Lock()
	out := settingsHTTP(t, w, "POST", settingsBody(revision, `{"pools":{"codex":1}}`))
	m.mu.Unlock()
	var failure struct{ Code string }
	if out.Code != 503 || json.Unmarshal(out.Body.Bytes(), &failure) != nil || failure.Code != "locked" {
		t.Fatalf("locked stale request: HTTP %d", out.Code)
	}
	after, err := os.ReadFile(p)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("lock refusal changed persisted settings")
	}
	if out := settingsHTTP(t, w, "POST", settingsBody(revision, `{"pools":{"codex":1}}`)); out.Code != 409 {
		t.Fatalf("unlocked stale request: HTTP %d", out.Code)
	}
	after, err = os.ReadFile(p)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("revision conflict changed persisted settings")
	}
}

func TestSettingsAuditFailureRecovery(t *testing.T) {
	for _, stage := range []string{"intent", "before_rename", "after_rename", "outcome"} {
		t.Run(stage, func(t *testing.T) {
			m, w, p := settingsFixture(t)
			revision := settingsRevision(t, w)
			before, _ := os.ReadFile(p)
			m.settingsIO = &settingsIO{audit: func(tool, lane, goal, digest, result string, reasons ...string) error {
				if stage == "intent" && result == "intent" || stage == "outcome" && result == "committed" {
					return errors.New("injected audit failure")
				}
				return m.audit(tool, lane, goal, digest, result, reasons...)
			}, write: func(path string, b []byte) error {
				if stage == "before_rename" {
					return errors.New("injected rename failure")
				}
				if err := atomicPrivate(path, b); err != nil {
					return err
				}
				if stage == "after_rename" {
					return errors.New("injected directory fsync failure")
				}
				return nil
			}}
			out := settingsHTTP(t, w, "POST", settingsBody(revision, `{"pools":{"claude":2},"notify_imessage_enabled":false}`))
			if out.Code != 503 {
				t.Fatal("write failure hidden")
			}
			after, _ := os.ReadFile(p)
			committed := stage == "outcome" || stage == "after_rename"
			if bytes.Equal(before, after) == committed {
				t.Fatal("incorrect commit state")
			}
			if stage == "outcome" && !strings.Contains(out.Body.String(), "committed_audit_failed") {
				t.Fatal("commit falsely reported rollback")
			}
			if committed && !strings.Contains(out.Body.String(), `"revision"`) {
				t.Fatal("committed or uncertain response lacks current revision")
			}
			m.settingsIO = nil
			if settingsRevision(t, w) == revision && committed {
				t.Fatal("revision not changed after visible commit")
			}
			if _, err := os.Stat(filepath.Join(m.store, "settings-intent.json")); err != nil {
				t.Fatal("GET unexpectedly reconciled intent")
			}
			if _, err := NewWeb(m, "127.0.0.1:8123"); err != nil {
				t.Fatal("local web construction did not reconcile intent")
			}
			if _, err := os.Stat(filepath.Join(m.store, "settings-intent.json")); !os.IsNotExist(err) {
				t.Fatal("intent not reconciled")
			}
			log, err := os.ReadFile(filepath.Join(m.store, "audit.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(log), "fixture@example.invalid") || strings.Contains(string(log), p) || strings.Contains(string(log), "extra_args") {
				t.Fatal("private config entered audit")
			}
			result := "not_committed"
			if committed {
				result = "committed"
			}
			if strings.Count(string(log), `"result":"`+result+`"`) != 2 {
				t.Fatal("per-field recovered outcomes missing")
			}
		})
	}
}

func TestSettingsLegacyRecipientTemplateAndLimits(t *testing.T) {
	m, w, p := settingsFixture(t)
	c, _ := LoadConfig(p)
	c.NotifyIMessage = ""
	c.NotifyIMessageEnabled = new(bool)
	c.Lanes = nil
	b, _ := json.Marshal(c)
	_ = os.WriteFile(p, b, 0600)
	revision := settingsRevision(t, w)
	if settingsHTTP(t, w, "POST", settingsBody(revision, `{"notify_imessage_enabled":true}`)).Code != 422 || m.SetIMessage(p, true) == nil {
		t.Fatal("missing recipient policy")
	}
	c.Pools["codex"] = AgentPool{Size: 0, Template: Lane{}}
	b, _ = json.Marshal(c)
	_ = os.WriteFile(p, b, 0600)
	if settingsHTTP(t, w, "GET", "").Code != 503 {
		t.Fatal("invalid disabled template exposed")
	}
	_, legacy, _ := manager(t)
	b, _ = json.Marshal(legacy)
	_ = os.WriteFile(p, b, 0600)
	w.settingsState.post = settingsBucket{}
	revision = settingsRevision(t, w)
	if settingsHTTP(t, w, "POST", settingsBody(revision, `{"pools":{"claude":2}}`)).Code != 422 {
		t.Fatal("implicit legacy conversion")
	}
	w.settingsState.get = settingsBucket{}
	for i := 0; i < 10; i++ {
		if settingsHTTP(t, w, "GET", "").Code != 200 {
			t.Fatal("GET burst control")
		}
	}
	if out := settingsHTTP(t, w, "GET", ""); out.Code != 429 || out.Header().Get("Retry-After") == "" {
		t.Fatal("GET limiter missing")
	}
	w.settingsState.post = settingsBucket{}
	for i := 0; i < 3; i++ {
		if settingsHTTP(t, w, "POST", "{}").Code != 400 {
			t.Fatal("POST burst control")
		}
	}
	if settingsHTTP(t, w, "POST", "{}").Code != 429 {
		t.Fatal("POST limiter missing")
	}
}

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
	if s.Exit != nil || next.session(s.RunID) == "" {
		t.Fatal("strict fake rejected resume or invented exit custody")
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
