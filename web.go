package nuntius

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"rsc.io/qr"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

type Web struct {
	reports        reportProjectionCache
	settingsState  webSettings
	mutationMu     sync.Mutex
	dataMu         sync.Mutex
	observer       *Manager
	controlIO      *laneControlIO // nil in production; deterministic durability fault proofs.
	manager        *Manager
	address, token string
	allowHost      string
	tailnet        *tailnet
	notify         string
	background     bool
	job            string
}

// TIOCGWINSZ is shared by Darwin and Linux. Unknown width does not suppress QR.
var terminalColumns = func(out io.Writer) int {
	f, ok := out.(*os.File)
	if !ok {
		return 0
	}
	var size struct{ Rows, Cols, X, Y uint16 }
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&size)))
	if errno != 0 {
		return 0
	}
	return int(size.Cols)
}

func NewWeb(m *Manager, address string, allowHost ...string) (*Web, error) {
	allowed := ""
	if len(allowHost) > 1 {
		return nil, errors.New("only one allow-host is supported")
	}
	if len(allowHost) == 1 {
		allowed = allowHost[0]
		if !validTailnetHost(allowed) {
			return nil, errors.New("allow-host must be a DNS name ending in .ts.net, without a port or wildcard")
		}
	}
	host, port, e := net.SplitHostPort(address)
	if e != nil || host != "127.0.0.1" {
		return nil, errors.New("web must bind to 127.0.0.1 only")
	}
	n, e := strconv.Atoi(port)
	if e != nil || n < 0 || n > 65535 {
		return nil, errors.New("invalid web port")
	}
	b := make([]byte, 32)
	// A busy manager/control lock defers recovery until the next scalar write;
	// it must not make otherwise healthy dashboard construction intermittent.
	if e = m.recoverSettings(); e != nil && !errors.Is(e, errBusy) {
		return nil, errors.New("settings recovery unavailable")
	}
	if _, e = rand.Read(b); e != nil {
		return nil, e
	}
	return &Web{manager: m, address: address, token: hex.EncodeToString(b), allowHost: allowed}, nil
}

func validTailnetHost(host string) bool {
	if len(host) > 253 || !strings.HasSuffix(host, ".ts.net") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

func (w *Web) writeLinks(out io.Writer) error {
	_, err := fmt.Fprintf(out, "http://%s/?token=%s\n", w.address, w.token)
	if err != nil {
		return err
	}
	link := "http://" + w.address + "/?token=" + w.token
	if w.allowHost != "" {
		link = "https://" + w.allowHost + "/?token=" + w.token
		if _, err = fmt.Fprintln(out, link); err != nil {
			return err
		}
	}
	code, err := qr.Encode(link, qr.L)
	if err != nil {
		return errors.New("cannot encode sign-in QR")
	}
	var b strings.Builder
	if width := terminalColumns(out); width > 0 && width < code.Size+4 {
		_, err = fmt.Fprintln(out, "QR did not fit this terminal; use the sign-in link above.")
		return err
	}
	b.WriteString("Scan to sign in (keep this QR private):\n")
	// One cell holds two vertical modules, with a two-module quiet zone.
	for y := -2; y < code.Size+2; y += 2 {
		b.WriteString("\x1b[30;47m")
		for x := -2; x < code.Size+2; x++ {
			top, bottom := code.Black(x, y), code.Black(x, y+1)
			switch {
			case top && bottom:
				b.WriteRune('█')
			case top:
				b.WriteRune('▀')
			case bottom:
				b.WriteRune('▄')
			default:
				b.WriteByte(' ')
			}
		}
		b.WriteString("\x1b[0m\n")
	}
	_, err = io.WriteString(out, b.String())
	return err
}

func (w *Web) ServeHTTP(out http.ResponseWriter, r *http.Request) {
	out.Header().Set("Cache-Control", "no-store")
	out.Header().Set("Referrer-Policy", "no-referrer")
	out.Header().Set("X-Content-Type-Options", "nosniff")
	out.Header().Set("Content-Security-Policy", "default-src 'none'; img-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	host, port, e := net.SplitHostPort(r.Host)
	_, wantPort, _ := net.SplitHostPort(w.address)
	local := e == nil && (host == "127.0.0.1" || host == "localhost") && port == wantPort
	tailnet := w.allowHost != "" && (r.Host == w.allowHost || r.Host == w.allowHost+":443")
	if !local && !tailnet {
		if w.controlRoute(r.URL.Path) {
			webControlReply(out, 403, "forbidden", nil)
			return
		}
		http.Error(out, webControlReason("forbidden"), http.StatusForbidden)
		return
	}
	// Only the static, data-free shell is public. Data and actions use headers
	// exclusively, so a query credential never authenticates an API request.
	shell := r.URL.Path == "/" || r.URL.Path == "/app.css" || r.URL.Path == "/app.js" || r.URL.Path == "/favicon.svg" || r.URL.Path == "/assets/brand/nuntius-logo.svg" || r.URL.Path == "/assets/brand/nuntius-logo-dark.svg"
	if !shell && (len(r.Header.Values("X-Nuntius-Token")) != 1 || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Nuntius-Token")), []byte(w.token)) != 1) {
		if w.controlRoute(r.URL.Path) {
			webControlReply(out, 403, "forbidden", nil)
			return
		}
		http.Error(out, webControlReason("forbidden"), http.StatusForbidden)
		return
	}
	origin := "http://" + r.Host
	if tailnet {
		origin = "https://" + w.allowHost
	}
	if r.Method == http.MethodPost && (len(r.Header.Values("Origin")) != 1 || r.Header.Get("Origin") != origin) {
		if w.controlRoute(r.URL.Path) {
			webControlReply(out, 403, "forbidden", nil)
			return
		}
		http.Error(out, webControlReason("forbidden"), http.StatusForbidden)
		return
	}
	if r.Method == http.MethodPost && (w.controlRoute(r.URL.Path) || r.URL.Path == "/settings" || r.URL.Path == "/action") {
		if err := w.allowMutation(); err != nil {
			var fault *laneControlFault
			if errors.As(err, &fault) && fault.Code == "rate_limited" {
				out.Header().Set("Retry-After", "6")
			}
			webControlFailure(out, err)
			return
		}
	}
	if w.controlRoute(r.URL.Path) {
		w.laneMutation(out, r)
		return
	}
	if r.URL.Path == "/settings" {
		w.settings(out, r)
		return
	}
	if r.URL.Path == "/action" {
		if r.Method != http.MethodPost {
			out.Header().Set("Allow", "POST")
			http.Error(out, "POST required", http.StatusMethodNotAllowed)
			return
		}
		w.action(out, r)
		return
	}
	if r.Method != http.MethodGet {
		out.Header().Set("Allow", "GET")
		http.Error(out, "GET required", http.StatusMethodNotAllowed)
		return
	}
	switch r.URL.Path {
	case "/queue":
		if r.URL.RawQuery != "" || r.URL.ForceQuery {
			http.Error(out, "queue selectors are not supported", http.StatusBadRequest)
			return
		}
		v := w.manager.queue()
		out.Header().Set("Content-Type", "application/json")
		if v.Status == "missing" || v.Status == "invalid" || v.Status == "unavailable" {
			out.WriteHeader(http.StatusServiceUnavailable)
		}
		encoded, _ := json.Marshal(v)
		var display map[string]any
		_ = json.Unmarshal(encoded, &display)
		_ = json.NewEncoder(out).Encode(safeValue(display))
	case "/":
		out.Header().Set("Content-Type", "text/html; charset=utf-8")
		page, _ := dashboardAssets.ReadFile("index.html")
		_, _ = out.Write(page)
	case "/app.css", "/app.js":
		contentType := "text/css; charset=utf-8"
		if r.URL.Path == "/app.js" {
			contentType = "text/javascript; charset=utf-8"
		}
		out.Header().Set("Content-Type", contentType)
		asset, _ := dashboardAssets.ReadFile(strings.TrimPrefix(r.URL.Path, "/"))
		_, _ = out.Write(asset)
	case "/favicon.svg", "/assets/brand/nuntius-logo.svg", "/assets/brand/nuntius-logo-dark.svg":
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "favicon.svg" {
			name = "assets/brand/favicon.svg"
		}
		asset, _ := dashboardAssets.ReadFile(name)
		out.Header().Set("Content-Type", "image/svg+xml")
		_, _ = out.Write(asset)
	case "/data":
		v, e := w.dataContext(r.Context())
		if e != nil {
			http.Error(out, "dashboard data unavailable", http.StatusInternalServerError)
			return
		}
		out.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(out).Encode(safeValue(v))
	default:
		http.NotFound(out, r)
	}
}

func (w *Web) action(out http.ResponseWriter, r *http.Request) {
	b, e := io.ReadAll(http.MaxBytesReader(out, r.Body, 65536))
	var a struct {
		Action string `json:"action"`
		Lane   string `json:"lane"`
		Text   string `json:"text"`
		Stop   bool   `json:"stop"`
	}
	if e != nil || strictJSON(b, &a) != nil {
		http.Error(out, "invalid action", http.StatusBadRequest)
		return
	}
	switch a.Action {
	case "freeze":
		e = w.manager.Freeze(a.Text)
	case "unfreeze":
		e = w.manager.Unfreeze()
	case "cancel":
		e = w.manager.Cancel(a.Lane)
	case "message":
		e = w.manager.SendMessage(a.Lane, a.Text, a.Stop)
	case "note":
		e = w.manager.AddOwnerNote(a.Text, a.Lane)
	default:
		e = errors.New("unknown action")
	}
	if e != nil {
		var admission *laneAdmissionFault
		if errors.As(e, &admission) {
			webControlFailure(out, e)
			return
		}
		http.Error(out, "The action could not be completed. Read current state and check the lane and request locally before retrying.", http.StatusBadRequest)
		return
	}
	out.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(out, `{"ok":true}`)
}

func readDisplay(path string, limit int) string {
	f, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return ""
	}
	defer f.Close()
	fi, e := f.Stat()
	if e != nil || !fi.Mode().IsRegular() {
		return ""
	}
	b, e := io.ReadAll(io.LimitReader(f, int64(limit+4096)))
	if e != nil {
		return ""
	}
	return bounded(Redact(string(b)), limit)
}

func reportSections(report string) map[string]string {
	return projectReport(report).sections()
}

func (w *Web) data() (any, error) {
	return w.dataContext(context.Background())
}

func (w *Web) dataContext(parent context.Context) (any, error) {
	ctx, cancel := context.WithTimeout(parent, time.Second)
	defer cancel()
	if err := settingsReadLock(ctx, func() (bool, error) { return w.dataMu.TryLock(), nil }); err != nil {
		return nil, err
	}
	defer w.dataMu.Unlock()
	// A separate read-only observer uses value copies; never toggle the serving
	// manager's mode or run recovery/supervision from a dashboard GET.
	owner := w.manager
	if err := settingsReadLock(ctx, func() (bool, error) { return owner.mu.TryLock(), nil }); err != nil {
		return nil, err
	}
	if err := owner.reload(); err != nil {
		owner.mu.Unlock()
		return nil, err
	}
	config, store := owner.config, owner.store
	owner.mu.Unlock()
	m := w.observer
	if m == nil {
		var e error
		m, e = newReadManager(config, store)
		if e != nil {
			return nil, e
		}
		w.observer = m
	} else {
		m.config = config
	}
	var controls laneControlSnapshot
	status, e := m.status(true, &controls)
	if e != nil {
		return nil, e
	}
	m.mu.Lock()
	runs, e := m.history()
	config = m.config
	m.mu.Unlock()
	if e != nil {
		return nil, e
	}
	for i := range runs {
		m.observeEndedRun(&runs[i])
	}
	historyRuns := runs
	if len(historyRuns) > 50 {
		historyRuns = historyRuns[:50]
	}
	views := []any{}
	index := runIndex(runs)
	groups := make([]HistoryEntry, 0, len(historyRuns))
	for _, r := range historyRuns {
		_, chain, depth := runRelation(r, index)
		groups = append(groups, HistoryEntry{RunID: r.ID, ChainID: chain, ChainDepth: depth})
	}
	for _, entry := range groupedHistory(groups) {
		r := index[entry.RunID]
		if r.Model == "" && r.Effort == "" {
			for _, l := range config.Lanes {
				if l.Name == r.Lane {
					model, effort := effectiveIdentity(l)
					r.Model, r.Effort = model+" (configured)", effort+" (configured)"
					break
				}
			}
		}
		relation, chain, depth := runRelation(r, index)
		report := ""
		association := "Unavailable for this attempt; goal-level report cannot establish attempt delivery."
		if before := m.currentReport(r); before != nil {
			report, _ = m.Report(r.GoalID)
			if after := m.currentReport(r); after == nil || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) || before.Size() != after.Size() {
				report = ""
			} else if report != "" {
				association = "Associated by report time with this attempt; native attempt metadata is not available yet."
			}
		}
		projection := w.reports.project(report)
		views = append(views, map[string]any{"kind": r.Kind, "model": identityValue(r.Model), "effort": identityValue(r.Effort), "run_id": shortRunID(r.ID), "chain_id": shortRunID(chain), "chain_depth": depth, "relation": relation, "lane": r.Lane, "goal_id": r.GoalID, "started": r.Started, "ended": r.Ended, "state": r.State, "display_state": terminalDisplay(r, r.State), "badge": badgeTone(r, r.State), "resume_count": r.ResumeCount, "exit": r.Exit, "goal": readDisplay(filepath.Join(m.store, "runs", r.ID, "goal.txt"), 12000), "report": report, "sections": projection.sections(), "overview": projection, "report_association": association})
	}
	tails := map[string][]string{}
	lanes := []webLane{}
	m.mu.Lock()
	for _, s := range status {
		kind, eta, current := m.agentKind(s.Lane), "-", "-"
		if s.RunID != "" {
			b, _ := readWindow(filepath.Join(m.store, "runs", s.RunID, "events.jsonl"), true)
			current = m.eventWindow(s.RunID, b, false).activity
			if s.State == "running" && s.Started != nil {
				eta = m.etaFromHistory(runs, Run{ID: s.RunID, Started: *s.Started}, kind, m.runSize(s.RunID), time.Duration(s.ElapsedSeconds)*time.Second)
			}
		}
		s.RunID = shortRunID(s.RunID)
		s.LaunchdLabel = ""
		lanes = append(lanes, webLane{Status: s, Kind: kind, ETA: eta, Activity: current})
	}
	m.mu.Unlock()
	for _, s := range status {
		tails[s.Lane], _ = m.Tail(s.Lane, 50)
	}
	notes, e := m.readNotes()
	if e != nil {
		return nil, e
	}
	if len(notes) > 50 {
		notes = notes[:50]
	}
	oversight := config.OversightPath
	if oversight == "" {
		oversight = filepath.Join(filepath.Dir(config.ReportsDir), "OVERSIGHT.md")
	}
	audit, e := m.Audit("")
	if e != nil {
		return nil, e
	}
	signals, e := webObservedSignals(m.store)
	if e != nil {
		return nil, e
	}
	laneSignals := map[string][]string{}
	for _, signal := range signals {
		for _, s := range status {
			if strings.HasPrefix(signal, s.Lane+".") {
				laneSignals[s.Lane] = append(laneSignals[s.Lane], signal)
			}
		}
	}
	frozen, e := m.freezeState()
	if e != nil {
		return nil, e
	}
	return map[string]any{"lane_control_version": controls.Version, "lane_control": controls, "held_messages": m.webHeldMessages(controls), "freeze": frozen, "lanes": lanes, "tails": tails, "history": views, "notes": notes, "audit": audit, "signals": laneSignals, "oversight": readDisplay(oversight, 64000)}, nil
}

// webLane adds presentation fields without changing Status JSON for CLI or MCP.
type webLane struct {
	Status
	Kind     string `json:"kind"`
	ETA      string `json:"eta"`
	Activity string `json:"activity"`
}

// Run listens, then sets up Tailscale Serve, prints the sign-in links, serves
// the dashboard and sends the optional iMessage. The summary line and every
// warning omit the credential; only the link lines carry it.
func (w *Web) Run(ctx context.Context, out, errOut io.Writer) error {
	guard, e := w.manager.lock("web-serving")
	if e != nil {
		return errors.New("dashboard is already serving from this data directory")
	}
	defer guard.Close()
	listener, e := net.Listen("tcp4", w.address)
	if e != nil {
		if errors.Is(e, syscall.EADDRINUSE) {
			return errors.New("local web port is already in use; choose another --bind port")
		}
		return errors.New("cannot listen on local web address")
	}
	defer listener.Close()
	w.address = listener.Addr().String()
	closeMenubar, trayErr := w.serveMenubar()
	if trayErr != nil {
		fmt.Fprintln(errOut, "warning: menu bar API unavailable")
	} else {
		defer closeMenubar()
	}
	_, port, _ := net.SplitHostPort(w.address)
	serve := "none"
	if w.tailnet != nil {
		state, e := w.tailnet.ensure(port)
		if e != nil {
			// A mapping created but not confirmed is still ours to remove.
			_ = w.tailnet.release()
			if !w.tailnet.optional {
				return e
			}
			fmt.Fprintln(errOut, "warning: "+e.Error()+"; the dashboard is loopback-only")
			w.allowHost, w.tailnet = "", nil
		} else {
			serve = state
		}
	}
	defer func() {
		// An owner ensure may have repaired Serve while this listener stayed up.
		// Its private identity-bound ownership record lets this process clean only
		// the still-exact mapping, including when the initial mapping was reused.
		var owner webServeOwner
		if w.tailnet != nil && readWebFile(filepath.Join(w.manager.store, "web-serve.json"), &owner) == nil {
			start, err := processStart(os.Getpid())
			if err == nil && owner.PID == os.Getpid() && owner.ProcessStart == start && owner.Host == w.allowHost && owner.Port == port {
				w.tailnet.created = true
				w.tailnet.port = port
			}
		}
		_ = os.Remove(filepath.Join(w.manager.store, "web-serve.json"))
		failed := w.tailnet.release() != nil
		if w.background {
			_ = saveWebFile(filepath.Join(w.manager.store, "web-stopped.json"), webStopState{ServeCleanupFailed: failed})
		}
		if failed {
			fmt.Fprintln(errOut, "warning: the serve mapping was not removed; run: tailscale serve --https=443 off")
		}
	}()
	if w.allowHost != "" && serve == "none" {
		serve = "manual"
	}
	if !w.background {
		e = w.writeLinks(out)
	}
	if e != nil {
		return e
	}
	server := &http.Server{Handler: w, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16384}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	message, host := "off", "off"
	if w.allowHost != "" {
		host = w.allowHost
	}
	if w.notify != "" && w.allowHost != "" {
		message = "sent"
		if e := sendIMessage(ctx, w.notify, "nuntius dashboard ("+machineName(w.allowHost)+"): https://"+w.allowHost+"/?token="+w.token); e != nil {
			message = "failed"
			fmt.Fprintln(errOut, "warning: iMessage not sent: "+e.Error())
		}
	}
	start, e := processStart(os.Getpid())
	if e != nil {
		return errors.New("cannot record dashboard identity")
	}
	state := webState{Version: Version, PID: os.Getpid(), ProcessStart: start, Host: w.allowHost, Serve: serve, Started: time.Now().UTC(), Address: w.address, Job: w.job, IMessage: message}
	defer func() { _ = w.manager.clearWebFiles() }()
	if w.background {
		if e = saveWebFile(filepath.Join(w.manager.store, "web-access.json"), webAccess{Token: w.token, Notify: w.notify, PID: state.PID, ProcessStart: start}); e != nil {
			return errors.New("cannot save owner-only dashboard access")
		}
		if e = atomicPrivate(filepath.Join(w.manager.store, "web.pid"), []byte(strconv.Itoa(state.PID)+"\n")); e != nil {
			return e
		}
	}
	if e = w.manager.saveWebState(state); e != nil {
		return e
	}
	fmt.Fprintf(out, "summary: tailnet=%s serve=%s imessage=%s\n", host, serve, message)
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
		e = <-served
	case e = <-served:
	}
	if errors.Is(e, http.ErrServerClosed) {
		return nil
	}
	return e
}

//go:embed index.html app.css app.js assets/brand/*.svg
var dashboardAssets embed.FS
