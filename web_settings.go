package nuntius

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"
)

type settingsChanges struct {
	Pools  map[string]int `json:"pools,omitempty"`
	Notify *bool          `json:"notify_imessage_enabled,omitempty"`
}
type settingsRequest struct {
	Version  int             `json:"schema_version"`
	Revision string          `json:"expected_revision"`
	Changes  settingsChanges `json:"changes"`
}
type settingsFault struct {
	Code, Field string
	Status      int
	Lane        string
}

func (e *settingsFault) Error() string { return e.Code }
func settingsError(code, field string, status int) error {
	return &settingsFault{Code: code, Field: field, Status: status}
}

type settingsField struct{ Field, Before, After string }
type settingsIntent struct {
	Before, After [32]byte
	Fields        []settingsField
}

// Narrow injected filesystem seams. Production uses the existing private writer
// and durable audit appender. Tests inject failures with scratch stores only.
type settingsIO struct {
	write func(string, []byte) error
	audit func(string, string, string, string, string, ...string) error
}
type settingsBucket struct {
	At     time.Time
	Tokens float64
}
type webSettings struct {
	mu        sync.Mutex
	digest    [32]byte
	revision  string
	get, post settingsBucket
}

func (b *settingsBucket) allow(now time.Time, rate, burst float64) bool {
	if b.At.IsZero() {
		b.Tokens = burst
	} else {
		b.Tokens += now.Sub(b.At).Seconds() * rate
	}
	if b.Tokens > burst {
		b.Tokens = burst
	}
	b.At = now
	if b.Tokens < 1 {
		return false
	}
	b.Tokens--
	return true
}
func configIdentity(path string) ([32]byte, error) {
	var zero [32]byte
	f, err := os.Open(path)
	if err != nil {
		return zero, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil || len(b) > 65536 {
		return zero, errors.New("configuration unavailable")
	}
	return sha256.Sum256(b), nil
}
func (s *webSettings) revisionFor(identity [32]byte) (string, error) {
	if s.revision == "" || s.digest != identity {
		var b [24]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		s.revision = hex.EncodeToString(b[:])
		s.digest = identity
	}
	return s.revision, nil
}
func (m *Manager) settingsAudit(result, reason string) error {
	if m.settingsIO != nil && m.settingsIO.audit != nil {
		return m.settingsIO.audit("settings_change", "", "", "", result, reason)
	}
	return m.appendAudit(true, "settings_change", "", "", "", result, reason)
}

func (m *Manager) recoverSettings() error {
	if !m.mu.TryLock() {
		return errBusy
	}
	defer m.mu.Unlock()
	if m.config.SourcePath == "" {
		return nil
	}
	guard, err := privateFile(filepath.Join(m.store, "locks", "control.lock"), os.O_CREATE|os.O_RDWR)
	if err != nil {
		return err
	}
	defer guard.Close()
	if err = syscall.Flock(int(guard.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return errBusy
		}
		return err
	}
	return m.reconcileSettings(m.config.SourcePath)
}
func (m *Manager) reconcileSettings(path string) error {
	p := filepath.Join(m.store, "settings-intent.json")
	f, err := privateFile(p, os.O_RDONLY)
	if os.IsNotExist(err) {
		return nil
	}
	// privateFile deliberately returns a safe error, so inspect absence separately.
	if err != nil {
		if _, e := os.Lstat(p); os.IsNotExist(e) {
			return nil
		}
		return err
	}
	b, err := io.ReadAll(io.LimitReader(f, 8193))
	f.Close()
	var intent settingsIntent
	if err != nil || len(b) > 8192 || strictJSON(b, &intent) != nil || len(intent.Fields) < 1 || len(intent.Fields) > 4 {
		return errors.New("settings audit unavailable")
	}
	digest, err := configIdentity(path)
	if err != nil {
		return err
	}
	result := "uncertain"
	if digest == intent.After {
		result = "committed"
	} else if digest == intent.Before {
		result = "not_committed"
	}
	for _, field := range intent.Fields {
		if !validSettingsField(field) {
			return errors.New("settings audit unavailable")
		}
		if err = m.settingsAudit(result, field.Field+" "+field.Before+" -> "+field.After); err != nil {
			return err
		}
	}
	if err = os.Remove(p); err != nil {
		return err
	}
	return syncDir(m.store)
}
func validSettingsField(f settingsField) bool {
	if f.Field == "notify_imessage_enabled" {
		return (f.Before == "on" || f.Before == "off") && (f.After == "on" || f.After == "off")
	}
	if f.Field != "pools.claude" && f.Field != "pools.codex" && f.Field != "pools.opencode" {
		return false
	}
	return len(f.Before) == 1 && len(f.After) == 1 && f.Before[0] >= '0' && f.Before[0] <= '8' && f.After[0] >= '0' && f.After[0] <= '8'
}
func (m *Manager) settingsCandidate(c Config, change settingsChanges, remote bool) (Config, []settingsField, error) {
	next := c
	if len(change.Pools) > 0 {
		if c.Pools == nil {
			return next, nil, settingsError("legacy_local_only", "pools", 422)
		}
		next.Pools = make(map[string]AgentPool, len(c.Pools))
		for kind, p := range c.Pools {
			next.Pools[kind] = p
		}
	}
	fields := []settingsField{}
	keys := make([]string, 0, len(change.Pools))
	for k := range change.Pools {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, kind := range keys {
		size := change.Pools[kind]
		p, ok := next.Pools[kind]
		if (kind != "claude" && kind != "codex" && kind != "opencode") || !ok || size < 0 || size > 8 {
			return next, nil, settingsError("pool_size_or_template", "pools."+safeKind(kind), 422)
		}
		if size != p.Size {
			fields = append(fields, settingsField{"pools." + kind, string(rune('0' + p.Size)), string(rune('0' + size))})
		}
		p.Size = size
		next.Pools[kind] = p
	}
	if change.Notify != nil {
		if *change.Notify && c.NotifyIMessage == "" {
			return next, nil, settingsError("recipient_not_configured", "notify_imessage_enabled", 422)
		}
		on := func(b bool) string {
			if b {
				return "on"
			}
			return "off"
		}
		before := c.NotifyIMessageEnabled == nil || *c.NotifyIMessageEnabled
		if before != *change.Notify {
			fields = append(fields, settingsField{"notify_imessage_enabled", on(before), on(*change.Notify)})
		}
		next.NotifyIMessageEnabled = change.Notify
	}
	if len(fields) == 0 && remote {
		return next, nil, settingsError("no_changes", "", 422)
	}
	if next.Pools != nil {
		next.Lanes = nil
	}
	if _, err := next.Validate(); err != nil {
		return next, nil, settingsError("invalid_configuration", "", 422)
	}
	expanded, _ := next.expanded()
	retained := map[string]bool{}
	for _, l := range expanded {
		retained[l.Name] = true
	}
	for _, l := range c.Lanes {
		if retained[l.Name] {
			continue
		}
		r, err := m.latest(l.Name)
		if err != nil {
			return next, nil, settingsError("store_unavailable", "", 503)
		}
		busy, err := m.busy(l.Name)
		if err != nil && !errors.Is(err, errBusy) {
			return next, nil, settingsError("store_unavailable", "", 503)
		}
		if busy || (r != nil && r.State == "running" && processExists(r.PID)) {
			return next, nil, &settingsFault{Code: "busy_agent", Field: "pools." + l.Kind, Status: 409, Lane: cell(l.Name)}
		}
	}
	return next, fields, nil
}
func safeKind(k string) string {
	if k == "claude" || k == "codex" || k == "opencode" {
		return k
	}
	return "unknown"
}

// Caller holds both Manager.mu and the cooperating writers' control lock.
// All CLI and web scalar setters pass through this policy and single writer.
func (m *Manager) commitSettings(c Config, path string, change settingsChanges, remote bool) error {
	if err := m.reconcileSettings(path); err != nil {
		return settingsError("audit_unavailable", "", 503)
	}
	next, fields, err := m.settingsCandidate(c, change, remote)
	if err != nil {
		return err
	}
	if len(fields) == 0 {
		return nil
	}
	b, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if len(b) > 65536 {
		return settingsError("configuration_too_large", "", 422)
	}
	before, err := configIdentity(path)
	if err != nil {
		return err
	}
	intent := settingsIntent{before, sha256.Sum256(b), fields}
	journal, _ := json.Marshal(intent)
	if err = atomicPrivate(filepath.Join(m.store, "settings-intent.json"), journal); err != nil {
		return settingsError("audit_unavailable", "", 503)
	}
	if err = m.settingsAudit("intent", "batch"); err != nil {
		return settingsError("audit_unavailable", "", 503)
	}
	write := atomicPrivate
	if m.settingsIO != nil && m.settingsIO.write != nil {
		write = m.settingsIO.write
	}
	if err = write(path, b); err != nil {
		// Rename or directory fsync can fail after the config became visible.
		// Keep intent for reconciliation; never claim rollback or retry blindly.
		_ = m.reload()
		return settingsError("save_unconfirmed", "", 503)
	}
	next.Lanes, _ = next.expanded()
	next.SourcePath = path
	m.config = next
	if err = m.reconcileSettings(path); err != nil {
		return settingsError("committed_audit_failed", "", 503)
	}
	return nil
}

// Reject duplicate keys and null before typed decoding (encoding/json otherwise
// silently overwrites duplicates and accepts null for an integer).
func settingsJSON(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	var walk func(string) error
	walk = func(path string) error {
		t, err := d.Token()
		if err != nil || t == nil {
			return errors.New("invalid JSON")
		}
		if delim, ok := t.(json.Delim); ok {
			if delim != '{' {
				return errors.New("objects required")
			}
			seen := map[string]bool{}
			for d.More() {
				key, e := d.Token()
				if e != nil {
					return e
				}
				k, ok := key.(string)
				if !ok || seen[k] {
					return errors.New("duplicate key")
				}
				// encoding/json also matches struct keys case-insensitively.
				// Accept only the exact envelope/change schema, so aliases cannot
				// silently replace a separately submitted change.
				if (path == "" && k != "schema_version" && k != "expected_revision" && k != "changes") || (path == "changes" && k != "pools" && k != "notify_imessage_enabled") {
					return errors.New("unknown key")
				}
				seen[k] = true
				next := k
				if path != "" {
					next = path + "." + k
				}
				if e = walk(next); e != nil {
					return e
				}
			}
			_, err = d.Token()
		}
		return err
	}
	if err := walk(""); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return strictJSON(b, v)
}
func (w *Web) settings(out http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" && r.Method != "POST" {
		out.Header().Set("Allow", "GET, POST")
		out.WriteHeader(405)
		return
	}
	s := &w.settingsState
	if !s.mu.TryLock() {
		settingsReply(out, 503, map[string]string{"code": "locked"})
		return
	}
	defer s.mu.Unlock()
	bucket, rate, burst := &s.get, 1.0, 10.0
	if r.Method == "POST" {
		bucket, rate, burst = &s.post, 1.0/6, 3
	}
	if !bucket.allow(time.Now(), rate, burst) {
		out.Header().Set("Retry-After", "6")
		settingsReply(out, 429, map[string]string{"code": "rate_limited"})
		return
	}
	fail := func(err error) {
		var f *settingsFault
		if !errors.As(err, &f) {
			f = &settingsFault{Code: "settings_unavailable", Status: 503}
		}
		settingsReply(out, f.Status, map[string]string{"code": f.Code, "field": f.Field, "lane": f.Lane})
	}
	var request settingsRequest
	if r.Method == "POST" {
		kind, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || kind != "application/json" {
			fail(settingsError("json_required", "", 400))
			return
		}
		_ = http.NewResponseController(out).SetReadDeadline(time.Now().Add(5 * time.Second))
		defer http.NewResponseController(out).SetReadDeadline(time.Time{})
		b, err := io.ReadAll(http.MaxBytesReader(out, r.Body, 8192))
		if err != nil || settingsJSON(b, &request) != nil || request.Version != 1 || request.Revision == "" || (len(request.Changes.Pools) == 0 && request.Changes.Notify == nil) || len(request.Changes.Pools) > 3 {
			fail(settingsError("invalid_request", "", 400))
			return
		}
	}
	m := w.manager
	if !m.mu.TryLock() {
		fail(settingsError("locked", "", 503))
		return
	}
	defer m.mu.Unlock()
	guard, err := privateFile(filepath.Join(m.store, "locks", "control.lock"), os.O_CREATE|os.O_RDWR)
	if err != nil {
		fail(err)
		return
	}
	defer guard.Close()
	if syscall.Flock(int(guard.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		fail(settingsError("locked", "", 503))
		return
	}
	path := m.config.SourcePath
	if path == "" {
		fail(settingsError("configuration_unavailable", "", 503))
		return
	}
	c, err := LoadConfig(path)
	if err != nil {
		fail(settingsError("invalid_configuration", "", 503))
		return
	}
	id, err := configIdentity(path)
	if err != nil {
		fail(err)
		return
	}
	revision, err := s.revisionFor(id)
	if err != nil {
		fail(err)
		return
	}
	if r.Method == "POST" {
		if request.Revision != revision {
			fail(settingsError("revision_conflict", "", 409))
			return
		}
		err = m.commitSettings(c, path, request.Changes, true)
		if err != nil {
			var fault *settingsFault
			if errors.As(err, &fault) && (fault.Code == "save_unconfirmed" || fault.Code == "committed_audit_failed") {
				identity, readErr := configIdentity(path)
				if readErr == nil {
					currentRevision, revisionErr := s.revisionFor(identity)
					if revisionErr == nil {
						settingsReply(out, 503, map[string]string{"code": fault.Code, "revision": currentRevision})
						return
					}
				}
			}
			fail(err)
			return
		}
		c = m.config
		id, err = configIdentity(path)
		if err != nil {
			fail(err)
			return
		}
		revision, err = s.revisionFor(id)
		if err != nil {
			fail(err)
			return
		}
	}
	settingsReply(out, 200, settingsView(c, revision))
}
func settingsReply(out http.ResponseWriter, status int, value any) {
	b, err := json.Marshal(value)
	if err != nil || len(b) > 32768 {
		status = 503
		b = []byte(`{"code":"settings_response_too_large"}`)
	}
	out.Header().Set("Content-Type", "application/json")
	out.WriteHeader(status)
	_, _ = out.Write(b)
}
func settingsView(c Config, revision string) map[string]any {
	pools := map[string]any{}
	for _, kind := range []string{"claude", "codex", "opencode"} {
		p, ok := c.Pools[kind]
		size := p.Size
		if c.Pools == nil {
			for _, l := range c.Lanes {
				if l.Kind == kind {
					size++
				}
			}
		}
		pools[kind] = map[string]any{"size": size, "configured": ok, "editable": ok}
	}
	lanes := []map[string]any{}
	row := func(l Lane) {
		model, effort := effectiveIdentity(l)
		lanes = append(lanes, map[string]any{"name": cell(l.Name), "kind": l.Kind, "model": cell(bounded(model, 128)), "effort": cell(bounded(effort, 128)), "workdir": cell(bounded(Redact(l.Workdir), 1024)), "binary": cell(bounded(Redact(l.Binary), 1024)), "start_mode": l.StartMode, "extra_args": "Configured locally", "continuation_configured": l.ContinueText != ""})
	}
	if c.Pools == nil {
		for _, l := range c.Lanes {
			row(l)
		}
	} else {
		for _, kind := range []string{"claude", "codex", "opencode"} {
			if p, ok := c.Pools[kind]; ok {
				l := p.Template
				l.Kind = kind
				l.Name = kind + " pool template"
				row(l)
			}
		}
	}
	names := []string{}
	for _, l := range c.Lanes {
		names = append(names, cell(l.Name))
	}
	format := "pools"
	if c.Pools == nil {
		format = "legacy"
	}
	return map[string]any{"schema_version": 1, "revision": revision, "format": format, "pools": pools, "notify_imessage_enabled": c.NotifyIMessageEnabled == nil || *c.NotifyIMessageEnabled, "notify_effective": c.imessageEnabled(), "recipient_configured": c.NotifyIMessage != "", "max_runtime": c.MaxRuntime, "auto_resume": c.AutoResume == nil || *c.AutoResume, "notifications": c.Notifications == nil || *c.Notifications, "reports_dir": cell(bounded(Redact(c.ReportsDir), 1024)), "oversight_path": cell(bounded(Redact(c.OversightPath), 1024)), "lanes": lanes, "lane_names": names, "at_login": "Unavailable · check locally", "tailnet_default": "Off · launch option", "local_only_reason": "Edit on the Mac; run nuntius config check"}
}
