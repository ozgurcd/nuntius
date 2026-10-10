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
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Web struct {
	manager        *Manager
	address, token string
}

func NewWeb(m *Manager, address string) (*Web, error) {
	host, port, e := net.SplitHostPort(address)
	if e != nil || host != "127.0.0.1" {
		return nil, errors.New("web must bind to 127.0.0.1 only")
	}
	n, e := strconv.Atoi(port)
	if e != nil || n < 0 || n > 65535 {
		return nil, errors.New("invalid web port")
	}
	b := make([]byte, 32)
	if _, e = rand.Read(b); e != nil {
		return nil, e
	}
	return &Web{m, address, hex.EncodeToString(b)}, nil
}

func (w *Web) ServeHTTP(out http.ResponseWriter, r *http.Request) {
	out.Header().Set("Cache-Control", "no-store")
	out.Header().Set("Referrer-Policy", "no-referrer")
	out.Header().Set("X-Content-Type-Options", "nosniff")
	out.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	host, port, e := net.SplitHostPort(r.Host)
	_, wantPort, _ := net.SplitHostPort(w.address)
	if e != nil || (host != "127.0.0.1" && host != "localhost") || port != wantPort {
		http.Error(out, "forbidden", http.StatusForbidden)
		return
	}
	// Only the static, data-free shell is public. Data and actions use headers
	// exclusively, so a query credential never authenticates an API request.
	shell := r.URL.Path == "/" || r.URL.Path == "/app.css" || r.URL.Path == "/app.js"
	if !shell && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Nuntius-Token")), []byte(w.token)) != 1 {
		http.Error(out, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method == http.MethodPost && r.Header.Get("Origin") != "http://"+r.Host {
		http.Error(out, "forbidden", http.StatusForbidden)
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
	case "/data":
		v, e := w.data()
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
		e = w.manager.AddOwnerNote(a.Text)
	default:
		e = errors.New("unknown action")
	}
	if e != nil {
		http.Error(out, bounded(Redact(e.Error()), 4096), http.StatusBadRequest)
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
	sections := map[string]string{"QUESTIONS": "", "DECISIONS": ""}
	current := ""
	for _, line := range strings.Split(report, "\n") {
		head := strings.TrimSpace(strings.TrimLeft(line, "# "))
		label, inline, colon := strings.Cut(head, ":")
		label = strings.TrimSpace(label)
		if label == "QUESTIONS" || label == "DECISIONS" {
			current = label
			if colon && strings.TrimSpace(inline) != "" {
				sections[current] += strings.TrimSpace(inline) + "\n"
			}
			continue
		}
		if (colon || strings.HasPrefix(strings.TrimSpace(line), "#")) && label == strings.ToUpper(label) {
			current = ""
		}
		if current != "" {
			sections[current] += line + "\n"
		}
	}
	return sections
}

func (w *Web) data() (any, error) {
	m := w.manager
	status, e := m.Status()
	if e != nil {
		return nil, e
	}
	m.mu.Lock()
	runs, e := m.history()
	config := m.config
	m.mu.Unlock()
	if e != nil {
		return nil, e
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
		relation, chain, depth := runRelation(r, index)
		report, _ := m.Report(r.GoalID)
		views = append(views, map[string]any{"kind": r.Kind, "model": r.Model, "run_id": shortRunID(r.ID), "chain_id": shortRunID(chain), "chain_depth": depth, "relation": relation, "lane": r.Lane, "goal_id": r.GoalID, "started": r.Started, "ended": r.Ended, "state": r.State, "display_state": terminalDisplay(r, r.State), "resume_count": r.ResumeCount, "exit": r.Exit, "goal": readDisplay(filepath.Join(m.store, "runs", r.ID, "goal.txt"), 12000), "report": report, "sections": reportSections(report)})
	}
	tails := map[string][]string{}
	lanes := []webLane{}
	m.mu.Lock()
	for _, s := range status {
		kind, eta, current := m.agentKind(s.Lane), "-", "-"
		if s.RunID != "" {
			b, _ := readWindow(filepath.Join(m.store, "runs", s.RunID, "events.jsonl"), true)
			current = activity(b)
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
	notes, e := m.OwnerNotes(time.Time{})
	if e != nil {
		return nil, e
	}
	oversight := config.OversightPath
	if oversight == "" {
		oversight = filepath.Join(filepath.Dir(config.ReportsDir), "OVERSIGHT.md")
	}
	audit, _ := readWindow(filepath.Join(m.store, "audit.jsonl"), true)
	frozen, e := m.freezeState()
	if e != nil {
		return nil, e
	}
	return map[string]any{"freeze": frozen, "lanes": lanes, "tails": tails, "history": views, "notes": notes, "audit": bounded(Redact(string(audit)), 64000), "oversight": readDisplay(oversight, 64000)}, nil
}

// webLane adds presentation fields without changing Status JSON for CLI or MCP.
type webLane struct {
	Status
	Kind     string `json:"kind"`
	ETA      string `json:"eta"`
	Activity string `json:"activity"`
}

func (w *Web) Run(ctx context.Context, out io.Writer) error {
	listener, e := net.Listen("tcp4", w.address)
	if e != nil {
		return errors.New("cannot listen on local web address")
	}
	w.address = listener.Addr().String()
	server := &http.Server{Handler: w, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16384}
	fmt.Fprintf(out, "http://%s/?token=%s\n", w.address, w.token)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = server.Shutdown(shutdown)
		case <-done:
		}
	}()
	e = server.Serve(listener)
	if errors.Is(e, http.ErrServerClosed) {
		return nil
	}
	return e
}

//go:embed index.html app.css app.js
var dashboardAssets embed.FS
