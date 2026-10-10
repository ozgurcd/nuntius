package nuntius

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"rsc.io/qr"
)

var menubarOpenFile = func(ctx context.Context, path string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	cmd := exec.CommandContext(ctx, name, path)
	cmd.Stdout, cmd.Stderr = nil, nil
	return cmd.Run()
}
var menubarSend = sendIMessage
var menubarFind = menubarExecutable
var menubarGUI = func() bool {
	if runtime.GOOS == "darwin" {
		_, e := exec.Command("launchctl", "print", fmt.Sprintf("gui/%d", os.Getuid())).Output()
		return e == nil
	}
	return os.Getenv("DBUS_SESSION_BUS_ADDRESS") != "" && (os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != "")
}
var menubarLaunch = func(exe string, args []string) error {
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func menubarExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	exe, _ = filepath.EvalSymlinks(exe)
	dir := filepath.Dir(exe)
	if p, e := exec.LookPath("nuntius-menubar"); e == nil {
		resolved, e := filepath.EvalSymlinks(p)
		if e == nil && filepath.IsAbs(resolved) {
			return resolved, nil
		}
	}
	for _, p := range []string{filepath.Join(dir, "Nuntius Menu Bar.app", "Contents", "MacOS", "nuntius-menubar"), filepath.Join(dir, "nuntius-menubar")} {
		if st, e := os.Stat(p); e == nil && st.Mode().IsRegular() && st.Mode().Perm()&0111 != 0 {
			return p, nil
		}
	}
	return "", errors.New("menu bar helper is not installed")
}

func (m *Manager) startMenubar() {
	exe, err := menubarFind()
	if err != nil {
		return
	}
	if !menubarGUI() {
		return
	}
	nuntius, err := os.Executable()
	if err != nil {
		return
	}
	m.mu.Lock()
	configPath := m.config.SourcePath
	m.mu.Unlock()
	_ = menubarLaunch(exe, []string{"--store", m.store, "--config", configPath, "--nuntius", nuntius})
}

func (w *Web) menubarHandler() http.Handler {
	return http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
		out.Header().Set("Cache-Control", "no-store")
		if r.URL.Path == "/status" {
			if r.Method != "GET" {
				out.WriteHeader(405)
				return
			}
			lanes, err := w.manager.Status()
			if err != nil {
				out.WriteHeader(503)
				return
			}
			rows := make([]map[string]any, 0, len(lanes))
			for _, s := range lanes {
				state := s.DisplayState
				if state == "" {
					state = s.State
				}
				question := s.AskedInteractively
				if s.ReportExists {
					if report, err := w.manager.Report(s.GoalID); err == nil {
						question = question || menubarReportQuestion(report)
					}
				}
				rows = append(rows, map[string]any{"lane": cell(s.Lane), "goal_id": cell(s.GoalID), "state": state, "elapsed_seconds": s.ElapsedSeconds, "question": question, "failed": s.Badge == "red" || s.State == "ended_no_report"})
			}
			_ = json.NewEncoder(out).Encode(map[string]any{"lanes": rows})
			return
		}
		if r.URL.Path != "/open" && r.URL.Path != "/qr" && r.URL.Path != "/phone" {
			out.WriteHeader(404)
			return
		}
		if r.Method != "POST" {
			out.WriteHeader(405)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 95*time.Second)
		defer cancel()
		var err error
		if r.URL.Path == "/phone" {
			if _, err := w.manager.Status(); err != nil {
				out.WriteHeader(503)
				return
			}
			w.manager.mu.Lock()
			enabled := w.manager.config.NotifyIMessageEnabled == nil || *w.manager.config.NotifyIMessageEnabled
			w.manager.mu.Unlock()
			if !enabled {
				out.WriteHeader(409)
				return
			}
			if w.notify == "" || w.allowHost == "" {
				out.WriteHeader(409)
				return
			}
			err = menubarSend(ctx, w.notify, "nuntius dashboard: https://"+w.allowHost+"/?token="+w.token)
		} else {
			err = w.menubarPresentation(ctx, r.URL.Path == "/qr")
		}
		if err != nil {
			out.WriteHeader(503)
			return
		}
		out.WriteHeader(200)
	})
}

func menubarReportQuestion(report string) bool {
	questions := strings.TrimSpace(reportSections(report)["QUESTIONS"])
	questions = strings.Trim(questions, "-* .\t\r\n")
	return questions != "" && !strings.EqualFold(questions, "none")
}

// Present credentials only in an owner-readable file, never an opener argv.
func (w *Web) menubarPresentation(ctx context.Context, showQR bool) error {
	dir := filepath.Join(w.manager.store, "menubar-pages")
	if err := privateDir(dir); err != nil {
		return err
	}
	link := "http://" + w.address + "/?token=" + w.token
	if showQR {
		if w.allowHost == "" {
			return errors.New("phone access is not configured")
		}
		link = "https://" + w.allowHost + "/?token=" + w.token
	}
	body := `<!doctype html><meta charset="utf-8"><meta name="referrer" content="no-referrer"><title>Nuntius</title>`
	if showQR {
		code, err := qr.Encode(link, qr.L)
		if err != nil {
			return errors.New("cannot create QR")
		}
		body += `<h1>Scan to sign in on your phone</h1><img alt="Private dashboard sign-in QR" src="data:image/png;base64,` + base64.StdEncoding.EncodeToString(code.PNG()) + `">`
	} else {
		body += `<meta http-equiv="refresh" content="0;url=` + html.EscapeString(link) + `">`
	}
	body += `<p><a href="` + html.EscapeString(link) + `">Open dashboard</a></p>`
	f, err := os.CreateTemp(dir, "view-*.html")
	if err != nil {
		return errors.New("cannot create private presentation")
	}
	path := f.Name()
	if err = f.Chmod(0600); err == nil {
		_, err = f.WriteString(body)
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		_ = os.Remove(path)
		return errors.New("cannot write private presentation")
	}
	time.AfterFunc(2*time.Minute, func() { _ = os.Remove(path) })
	if err = menubarOpenFile(ctx, path); err != nil {
		_ = os.Remove(path)
		return errors.New("cannot open presentation")
	}
	return nil
}

func (w *Web) serveMenubar() (func(), error) {
	dir := filepath.Join(w.manager.store, "menubar")
	if err := privateDir(dir); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "dashboard.sock")
	if st, err := os.Lstat(path); err == nil {
		if st.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("unsafe menu bar socket")
		}
		if err = os.Remove(path); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, errors.New("cannot create menu bar socket")
	}
	if err = os.Chmod(path, 0600); err != nil {
		ln.Close()
		return nil, err
	}
	srv := &http.Server{Handler: w.menubarHandler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 100 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 4096}
	go func() { _ = srv.Serve(ln) }()
	return func() {
		w.manager.stopMenubar()
		_ = srv.Close()
		_ = os.Remove(path)
		_ = os.RemoveAll(filepath.Join(w.manager.store, "menubar-pages"))
	}, nil
}

func (m *Manager) stopMenubar() {
	path := filepath.Join(m.store, "menubar", "helper.sock")
	st, err := os.Lstat(path)
	if err != nil || st.Mode()&os.ModeSocket == 0 || st.Mode().Perm() != 0600 {
		return
	}
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", path)
	}}}
	defer client.CloseIdleConnections()
	if resp, err := client.Post("http://unix/quit", "application/json", nil); err == nil {
		resp.Body.Close()
	}
}

func writeMenubarLogin(path, helper, store, config, main string, on bool) error {
	if !on {
		st, err := os.Lstat(path)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil || !st.Mode().IsRegular() {
			return errors.New("unsafe login file")
		}
		return os.Remove(path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		return errors.New("login file already exists; use off first")
	} else if !os.IsNotExist(err) {
		return err
	}
	text := `<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict><key>Label</key><string>io.nuntius.menubar</string><key>ProgramArguments</key><array>`
	for _, arg := range []string{helper, "--store", store, "--config", config, "--nuntius", main} {
		text += plistString(arg)
	}
	text += `</array><key>RunAtLoad</key><true/></dict></plist>`
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(text)
	closeErr := f.Close()
	return errors.Join(err, closeErr)
}

func menubarCommand(home, store, config string, args []string, out, errOut interface{ Write([]byte) (int, error) }) int {
	if len(args) != 2 || args[0] != "--at-login" || (args[1] != "on" && args[1] != "off") {
		fmt.Fprintln(errOut, "use nuntius menubar --at-login on|off")
		return 2
	}
	if runtime.GOOS != "darwin" {
		fmt.Fprintln(errOut, "start-at-login is available on macOS only")
		return 1
	}
	helper, main := "", ""
	if args[1] == "on" {
		var err error
		helper, err = menubarExecutable()
		if err != nil {
			fmt.Fprintln(errOut, "menu bar helper is not installed")
			return 1
		}
		main, _ = os.Executable()
	}
	if err := writeMenubarLogin(filepath.Join(home, "Library", "LaunchAgents", "io.nuntius.menubar.plist"), helper, store, config, main, args[1] == "on"); err != nil {
		fmt.Fprintln(errOut, "cannot update helper login file")
		return 1
	}
	fmt.Fprintln(out, "menu bar start-at-login "+strings.TrimSpace(args[1])+"; takes effect at next login; dashboard unchanged")
	return 0
}
