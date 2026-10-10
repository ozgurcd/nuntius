package nuntius

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const ensureAddress = "127.0.0.1:8123"

// All response fields are an allowlist. Private state and adapter errors never
// enter this object, the protocol, or the audit record.
type webEnsureResult struct {
	Local        string `json:"local"`
	Operation    string `json:"operation"`
	Tailnet      string `json:"tailnet"`
	Notification string `json:"notification"`
	SignIn       bool   `json:"sign_in_required"`
	RemoteClient string `json:"remote_client"`
	Reason       string `json:"reason"`
}

var errWebProcessAbsent = errors.New("dashboard process absent")

type webEnsureRuntime struct {
	find    func() (string, error)
	run     func(context.Context, string, ...string) ([]byte, error)
	process func(int) (string, error)
	launch  func(context.Context, ...string) ([]byte, error)
	signal  func(int) error
	probe   func(context.Context, webState, string) bool
	port    func(string) bool
	start   func(context.Context, []string) error
	notify  func(context.Context, string, string) error
}

func (m *Manager) ensureAdapters() webEnsureRuntime {
	if m.webEnsure != nil {
		return *m.webEnsure
	}
	return webEnsureRuntime{
		find: findTailscale, run: ensureCommand, process: ensureProcess,
		launch: func(ctx context.Context, args ...string) ([]byte, error) {
			b, err := ensureCommand(ctx, "launchctl", args...)
			var exit *exec.ExitError
			if errors.As(err, &exit) && exit.ExitCode() == 113 {
				return nil, errJobAbsent
			}
			return b, err
		},
		signal: func(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) },
		probe:  ensureProbe, port: func(address string) bool {
			l, err := net.Listen("tcp4", address)
			if err != nil {
				return false
			}
			_ = l.Close()
			return true
		},
		start: m.startWeb, notify: sendIMessage,
	}
}

// Commands have fixed call-site argv, bounded output and a child deadline. No
// shell, stdin prompts, inherited proxy, raw output/error logging or retries.
func ensureCommand(ctx context.Context, exe string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, args...)
	var out cappedWebOutput
	cmd.Stdout, cmd.Stderr = &out, io.Discard
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if err != nil {
		return nil, err
	}
	if out.exceeded {
		return nil, errors.New("command output limit")
	}
	return out.buffer.Bytes(), nil
}

type cappedWebOutput struct {
	buffer   bytes.Buffer
	exceeded bool
}

func (b *cappedWebOutput) Write(p []byte) (int, error) {
	n := len(p)
	if b.buffer.Len()+n > 65536 {
		b.exceeded = true
		p = p[:max(0, 65536-b.buffer.Len())]
	}
	_, _ = b.buffer.Write(p)
	return n, nil
}

func ensureProcess(pid int) (string, error) {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return "", errWebProcessAbsent
	} else if err != nil {
		return "", err
	}
	return processStart(pid)
}

func ensureProbe(ctx context.Context, state webState, token string) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+state.Address+"/data", nil)
	if err != nil {
		return false
	}
	req.Host = state.Host
	req.Header.Set("X-Nuntius-Token", token)
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close() // Do not read or log authenticated data.
	return resp.StatusCode == http.StatusOK && resp.Header.Get("Cache-Control") == "no-store"
}

// Distinguish absence from malformed/inaccessible state. Check parent paths too;
// O_NOFOLLOW alone does not protect a linked intermediate directory.
func (m *Manager) ensureRead(name string, value any) (bool, error) {
	path := filepath.Join(m.store, name)
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		st, err := os.Lstat(dir)
		if os.IsNotExist(err) && dir != m.store {
			continue
		}
		if err != nil || !st.IsDir() || st.Mode().Perm()&0077 != 0 {
			return false, errors.New("state directory unavailable")
		}
		if dir == m.store {
			break
		}
	}
	st, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0600 {
		return false, errors.New("state file unavailable")
	}
	if err := readWebFile(path, value); err != nil {
		return false, err
	}
	return true, nil
}

func ensureValidState(s webState, full bool) bool {
	if s.PID <= 1 || s.ProcessStart == "" || len(s.ProcessStart) > 256 {
		return false
	}
	if s.Job != "" && s.Job != "gui/"+strconv.Itoa(os.Getuid())+"/nuntius.web" {
		return false
	}
	if !full {
		return true
	}
	host, port, err := net.SplitHostPort(s.Address)
	n, e := strconv.Atoi(port)
	return err == nil && e == nil && host == "127.0.0.1" && n > 0 && n <= 65535 && strconv.Itoa(n) == port && (s.Host == "" || validTailnetHost(s.Host))
}

func ensureTailnet(ctx context.Context, d webEnsureRuntime, exe string) (string, string) {
	b, err := d.run(ctx, exe, "status", "--json")
	if err != nil || len(b) > 65536 || uniqueJSON(b) != nil {
		return "", "tailnet_unavailable"
	}
	var status struct {
		BackendState string
		Self         struct{ DNSName string }
		CertDomains  []string
	}
	if json.Unmarshal(b, &status) != nil {
		return "", "tailnet_unavailable"
	}
	if status.BackendState != "Running" {
		return "", "tailnet_inactive"
	}
	host := strings.TrimSuffix(status.Self.DNSName, ".")
	if !validTailnetHost(host) {
		return "", "tailnet_name_unavailable"
	}
	for _, name := range status.CertDomains {
		if strings.TrimSuffix(name, ".") == host {
			return host, ""
		}
	}
	return "", "tailnet_certificate_unavailable"
}

func ensureMapping(ctx context.Context, d webEnsureRuntime, exe, host, port string) (string, string) {
	b, err := d.run(ctx, exe, "serve", "status", "--json")
	if err != nil {
		return "", "serve_unavailable"
	}
	state, err := servePlan(b, host, port)
	if err != nil {
		return "", "serve_refused"
	}
	return state, ""
}

func (m *Manager) ensureWeb(parent context.Context) (result webEnsureResult) {
	// Reserve five seconds within the two-minute budget for safe cleanup.
	ctx, cancel := context.WithTimeout(parent, 115*time.Second)
	defer cancel()
	r := webEnsureResult{Local: "unknown", Operation: "none", Tailnet: "unknown", Notification: "disabled", RemoteClient: "unknown", Reason: "refused"}
	refuse := func(reason string) webEnsureResult { r.Reason = reason; return r }
	if m == nil {
		return refuse("unavailable")
	}
	if ctx.Err() != nil {
		return refuse("cancelled")
	}
	for _, dir := range []string{m.store, filepath.Join(m.store, "locks")} {
		st, err := os.Lstat(dir)
		if err != nil || !st.IsDir() || st.Mode().Perm()&0077 != 0 {
			return refuse("state_unavailable")
		}
	}
	guard, err := m.lock("web-control")
	if errors.Is(err, errBusy) {
		return refuse("busy")
	}
	if err != nil {
		return refuse("state_unavailable")
	}
	defer guard.Close()
	m.mu.Lock()
	err = m.reload()
	m.mu.Unlock()
	if err != nil {
		return refuse("configuration_unavailable")
	}
	d := m.ensureAdapters()
	var state, pending webState
	var access webAccess
	have, err := m.ensureRead("web.json", &state)
	if err != nil {
		return refuse("state_unavailable")
	}
	havePending, err := m.ensureRead("web-launch/process.json", &pending)
	if err != nil {
		return refuse("state_unavailable")
	}
	haveAccess, err := m.ensureRead("web-access.json", &access)
	if err != nil {
		return refuse("access_unavailable")
	}
	var owner webServeOwner
	if _, err := m.ensureRead("web-serve.json", &owner); err != nil {
		return refuse("state_unavailable")
	}
	address := ensureAddress
	live := false
	if have {
		if !ensureValidState(state, true) {
			return refuse("identity_refused")
		}
		address = state.Address
		start, err := d.process(state.PID)
		if err == nil {
			if start != state.ProcessStart {
				return refuse("identity_refused")
			}
			live = true
		} else if !errors.Is(err, errWebProcessAbsent) {
			return refuse("identity_unavailable")
		}
	}
	if havePending {
		if !ensureValidState(pending, false) {
			return refuse("start_pending")
		}
		start, err := d.process(pending.PID)
		if err == nil {
			if start != pending.ProcessStart {
				return refuse("identity_refused")
			}
			if !live || pending.PID != state.PID || pending.ProcessStart != state.ProcessStart || pending.Job != state.Job {
				return refuse("start_pending")
			}
		} else if !errors.Is(err, errWebProcessAbsent) {
			return refuse("identity_unavailable")
		}
	}
	if haveAccess {
		if access.PID <= 1 || access.ProcessStart == "" || len(access.Token) != 64 {
			return refuse("access_unavailable")
		}
		if live && (access.PID != state.PID || access.ProcessStart != state.ProcessStart) {
			return refuse("identity_refused")
		}
		if !live {
			_, err := d.process(access.PID)
			if !errors.Is(err, errWebProcessAbsent) {
				return refuse("identity_refused")
			}
		}
	}
	if live && !haveAccess {
		return refuse("access_unavailable")
	}
	job := ""
	if webPlatform == "darwin" {
		job = "gui/" + strconv.Itoa(os.Getuid()) + "/nuntius.web"
		b, err := d.launch(ctx, "print", job)
		if err == nil {
			owned := state
			if !have {
				owned = pending
			}
			if (!have && !havePending) || owned.Job != job || !ensureJob(b, owned.PID, live) {
				return refuse("job_refused")
			}
		} else if !errors.Is(err, errJobAbsent) {
			return refuse("job_unavailable")
		} else if live && state.Job != "" {
			return refuse("job_refused")
		}
	}
	exe, err := d.find()
	if err != nil {
		return refuse("tailnet_unavailable")
	}
	host, reason := ensureTailnet(ctx, d, exe)
	if reason != "" {
		return refuse(reason)
	}
	_, port, _ := net.SplitHostPort(address)
	mapping, reason := ensureMapping(ctx, d, exe, host, port)
	if reason != "" {
		return refuse(reason)
	}
	repairAttempt := false
	defer func() {
		if !repairAttempt || result.Reason == "ready" {
			return
		}
		cleanup, end := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer end()
		if current, why := ensureMapping(cleanup, d, exe, host, port); why == "" && current == "reused" {
			if _, err := d.run(cleanup, exe, "serve", "--https=443", "off"); err != nil {
				result.Reason = "serve_cleanup_failed"
			}
		}
		result.Tailnet = "unknown"
	}()
	if mapping == "reused" {
		r.Tailnet = "ready"
	} else {
		r.Tailnet = "absent"
	}
	healthy := live && state.Host == host && d.probe(ctx, state, access.Token)
	if ctx.Err() != nil {
		return refuse("cancelled")
	}
	if healthy {
		r.Local = "ready"
	}
	if !healthy {
		if live {
			if err := m.ensureStop(ctx, d, state); err != nil {
				r.Local = "pending"
				return refuse("stop_pending")
			}
			r.Operation = "restarted"
		} else {
			// A stale recorded job may be unloaded only after its state was verified.
			if job != "" {
				if b, err := d.launch(ctx, "print", job); err == nil {
					owned := state
					if !have {
						owned = pending
					}
					if owned.Job != job || !ensureJob(b, owned.PID, false) {
						return refuse("job_refused")
					}
					if _, err := d.launch(ctx, "bootout", job); err != nil {
						return refuse("job_refused")
					}
				} else if !errors.Is(err, errJobAbsent) {
					return refuse("job_unavailable")
				}
			}
		}
		if !d.port(address) {
			return refuse("port_occupied")
		}
		m.mu.Lock()
		config := m.config
		m.mu.Unlock()
		args := []string{"--config", config.SourcePath, "--store", m.store, "web", "__background", "--bind", address, "--tailnet", "--allow-host", host, "--ensure"}
		if r.Operation == "none" {
			r.Operation = "started"
		}
		if err := d.start(ctx, args); err != nil {
			r.Local = "pending"
			return refuse("start_pending")
		}
		have, err := m.ensureRead("web.json", &state)
		if err != nil || !have || !ensureValidState(state, true) || state.Address != address || state.Host != host {
			return refuse("readiness_unavailable")
		}
		have, err = m.ensureRead("web-access.json", &access)
		if err != nil || !have || access.PID != state.PID || access.ProcessStart != state.ProcessStart || len(access.Token) != 64 {
			return refuse("access_unavailable")
		}
		start, err := d.process(state.PID)
		if err != nil || start != state.ProcessStart {
			return refuse("identity_refused")
		}
		if state.Job != "" {
			b, err := d.launch(ctx, "print", state.Job)
			if err != nil || !ensureJob(b, state.PID, true) {
				return refuse("job_refused")
			}
		}
		if !d.probe(ctx, state, access.Token) {
			return refuse("readiness_unavailable")
		}
		r.Local = "ready"
		r.SignIn = true
	}
	// A start rechecks and creates Serve in the child. Repair a healthy listener
	// without rotating its grant, recording ownership before a potentially partial write.
	mapping, reason = ensureMapping(ctx, d, exe, host, port)
	if reason != "" {
		return refuse(reason)
	}
	if mapping == "absent" {
		if r.Operation == "none" {
			r.Operation = "serve_repaired"
		}
		if err := saveWebFile(filepath.Join(m.store, "web-serve.json"), webServeOwner{PID: state.PID, ProcessStart: state.ProcessStart, Host: host, Port: port}); err != nil {
			return refuse("state_unavailable")
		}
		repairAttempt = true
		_, writeErr := d.run(ctx, exe, "serve", "--bg", "--https=443", "localhost:"+port)
		mapping, reason = ensureMapping(ctx, d, exe, host, port)
		if writeErr != nil || reason != "" || mapping != "reused" {
			r.Tailnet = "unknown"
			return refuse("serve_readback_failed")
		}
		state.Serve = "created"
		if err := m.saveWebState(state); err != nil {
			return refuse("state_unavailable")
		}
	}
	start, err := d.process(state.PID)
	if err != nil || start != state.ProcessStart || !d.probe(ctx, state, access.Token) {
		r.Local = "unknown"
		return refuse("readiness_unavailable")
	}
	if currentHost, reason := ensureTailnet(ctx, d, exe); reason != "" || currentHost != host {
		r.Tailnet = "unknown"
		return refuse("tailnet_identity_changed")
	}
	if mapping, reason = ensureMapping(ctx, d, exe, host, port); reason != "" || mapping != "reused" {
		r.Tailnet = "unknown"
		return refuse("serve_readback_failed")
	}
	r.Tailnet = "ready"
	if r.Operation == "started" || r.Operation == "restarted" {
		m.mu.Lock()
		err = m.reload()
		config := m.config
		m.mu.Unlock()
		if err == nil && config.imessageEnabled() && validIMessageHandle(config.NotifyIMessage) {
			notifyCtx, end := context.WithTimeout(ctx, 10*time.Second)
			r.Notification = "sent"
			if d.notify(notifyCtx, config.NotifyIMessage, "nuntius dashboard ("+machineName(host)+"): https://"+host+"/?token="+access.Token) != nil {
				r.Notification = "failed"
			}
			end()
			state.IMessage = r.Notification
			_ = m.saveWebState(state)
		}
	}
	r.Reason = "ready"
	return r
}

func ensureJob(b []byte, pid int, live bool) bool {
	if len(b) > 65536 {
		return false
	}
	seenPID, stopped := false, false
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "pid = ") {
			if seenPID || line != "pid = "+strconv.Itoa(pid) {
				return false
			}
			seenPID = true
		}
		if line == "state = not running" || line == "state = waiting" {
			stopped = true
		}
	}
	return (live && seenPID && !stopped) || (!live && stopped && !seenPID)
}

func (m *Manager) ensureStop(ctx context.Context, d webEnsureRuntime, s webState) error {
	start, err := d.process(s.PID)
	if err != nil || start != s.ProcessStart {
		return errors.New("identity unavailable")
	}
	if s.Job != "" {
		b, err := d.launch(ctx, "print", s.Job)
		if err != nil || !ensureJob(b, s.PID, true) {
			return errors.New("job unavailable")
		}
	}
	start, err = d.process(s.PID)
	if err != nil || start != s.ProcessStart {
		return errors.New("identity changed before signal")
	}
	if err := d.signal(s.PID); err != nil {
		return err
	}
	stop, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for {
		start, err = d.process(s.PID)
		if errors.Is(err, errWebProcessAbsent) {
			break
		}
		if err != nil || start != s.ProcessStart {
			return errors.New("stop uncertain")
		}
		select {
		case <-stop.Done():
			return errors.New("still stopping")
		case <-time.After(25 * time.Millisecond):
		}
	}
	if s.Job != "" {
		b, err := d.launch(ctx, "print", s.Job)
		if err == nil {
			if !ensureJob(b, s.PID, false) {
				return errors.New("job uncertain")
			}
			_, err = d.launch(ctx, "bootout", s.Job)
		}
		if err != nil && !errors.Is(err, errJobAbsent) {
			return err
		}
	}
	return nil
}

type webServeOwner struct {
	PID                      int
	ProcessStart, Host, Port string
}

// JSON's default decoder accepts duplicate keys and silently overwrites them.
// Reject those ambiguities (at every depth) before validating command schemas.
func uniqueJSON(b []byte) error {
	d := json.NewDecoder(bytes.NewReader(b))
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 32 {
			return errors.New("JSON depth limit")
		}
		t, err := d.Token()
		if err != nil {
			return err
		}
		if delim, ok := t.(json.Delim); ok {
			if delim == '{' {
				keys := map[string]bool{}
				for d.More() {
					k, err := d.Token()
					if err != nil {
						return err
					}
					key, ok := k.(string)
					key = strings.ToLower(key)
					if !ok || keys[key] {
						return errors.New("duplicate JSON key")
					}
					keys[key] = true
					if err := walk(depth + 1); err != nil {
						return err
					}
				}
			} else if delim == '[' {
				for d.More() {
					if err := walk(depth + 1); err != nil {
						return err
					}
				}
			} else {
				return errors.New("invalid JSON")
			}
			_, err = d.Token()
			return err
		}
		return nil
	}
	if err := walk(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("expected one JSON document")
	}
	return nil
}
