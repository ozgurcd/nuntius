package nuntius

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var webPlatform = runtime.GOOS
var webExecutable = os.Executable

type webAccess struct {
	Token        string `json:"token"`
	Notify       string `json:"notify,omitempty"`
	PID          int    `json:"pid"`
	ProcessStart string `json:"process_start"`
}

type webRequest struct {
	Args []string `json:"args"`
}

type webStopState struct {
	ServeCleanupFailed bool `json:"serve_cleanup_failed"`
}

// The private FIFO transfers environment in memory, never into a plist/log.
// This entry runs only for the transient web job, then execs the serving binary.
func webExec(dir string) int {
	if !filepath.IsAbs(dir) || filepath.Base(dir) != "web-launch" {
		return 2
	}
	var request webRequest
	if readWebFile(filepath.Join(dir, "request.json"), &request) != nil {
		return 1
	}
	env, err := readLaunchEnvironment(dir)
	if err != nil {
		return 1
	}
	exe, err := os.Executable()
	if err != nil {
		return 1
	}
	if syscall.Exec(exe, append([]string{exe}, request.Args...), env) != nil {
		return 1
	}
	return 0
}

func readWebFile(path string, value any) error {
	f, err := privateFile(path, os.O_RDONLY)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil || len(b) > 65536 {
		return errors.New("dashboard state unavailable")
	}
	return strictJSON(b, value)
}

func saveWebFile(path string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return atomicPrivate(path, b)
}

func webLaunchctl(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, "launchctl", args...).Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 113 {
		return nil, errJobAbsent
	}
	if err != nil {
		return nil, errors.New("launchctl command failed")
	}
	return b, nil
}

func (m *Manager) webCommand(ctx context.Context, args []string, out, errOut io.Writer) int {
	if len(args) > 0 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h") {
		fmt.Fprintln(out, "web start [--replace] [--bind 127.0.0.1:PORT] [--tailnet] [--tailnet-optional] [--tailnet-keep] [--notify-imessage HANDLE] [--allow-host HOST]\nweb stop | status | link [--notify]\nweb [same flags] stays in the foreground; use start to free the terminal.")
		return 0
	}
	action := "foreground"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		action, args = args[0], args[1:]
	}
	if action == "stop" || action == "status" || action == "link" {
		if len(args) > 0 && !(action == "link" && len(args) == 1 && args[0] == "--notify") {
			fmt.Fprintln(errOut, "invalid web flags")
			return 2
		}
		guard, err := m.lock("web-control")
		if err != nil {
			fmt.Fprintln(errOut, "dashboard control is busy")
			return 1
		}
		defer guard.Close()
		state := m.webDashboard()
		if action == "status" {
			fmt.Fprintln(out, webStatusLine(state))
			return 0
		}
		if action == "stop" {
			if err = m.stopWeb(ctx); err != nil {
				fmt.Fprintln(errOut, err.Error())
				return 1
			}
			if state == nil {
				fmt.Fprintln(out, "web: not running")
			} else {
				fmt.Fprintln(out, "web: stopped")
			}
			return 0
		}
		if state == nil {
			fmt.Fprintln(errOut, "web: not running")
			return 1
		}
		if err = m.webLink(ctx, state, len(args) == 1, out, errOut); err != nil {
			fmt.Fprintln(errOut, err.Error())
			return 1
		}
		return 0
	}
	if action != "foreground" && action != "start" && action != "__background" {
		fmt.Fprintln(errOut, "unknown web command")
		return 2
	}
	wf := flag.NewFlagSet("web", flag.ContinueOnError)
	wf.SetOutput(io.Discard)
	bind := wf.String("bind", "127.0.0.1:8123", "loopback address")
	var hosts []string
	wf.Func("allow-host", "one exact tailnet DNS name", func(s string) error { hosts = append(hosts, s); return nil })
	tnMode := wf.Bool("tailnet", false, "automatic Tailscale Serve")
	optional := wf.Bool("tailnet-optional", false, "allow loopback fallback")
	keep := wf.Bool("tailnet-keep", false, "keep created mapping")
	notify := wf.String("notify-imessage", "", "owner iMessage handle")
	replace := wf.Bool("replace", false, "restart background dashboard")
	noMenubar := wf.Bool("no-menubar", false, "skip menu bar helper")
	job := wf.String("job", "", "internal background job")
	if wf.Parse(args) != nil || len(wf.Args()) != 0 || (!*tnMode && (*optional || *keep)) || (*notify != "" && !*tnMode && len(hosts) == 0) || (*replace && action != "start") || (*job != "" && action != "__background") {
		fmt.Fprintln(errOut, "invalid web flags")
		return 2
	}
	if *notify != "" && !validIMessageHandle(*notify) {
		fmt.Fprintln(errOut, "notify-imessage must be an email address or E.164 phone number")
		return 2
	}
	w, err := NewWeb(m, *bind, hosts...)
	if err != nil {
		fmt.Fprintln(errOut, err.Error())
		return 1
	}
	if action == "start" {
		guard, err := m.lock("web-control")
		if err != nil {
			fmt.Fprintln(errOut, "dashboard control is busy")
			return 1
		}
		defer guard.Close()
		if state := m.webDashboard(); state != nil {
			if !*replace {
				if !*noMenubar {
					m.startMenubar()
				}
				fmt.Fprintln(out, "web: already running; use --replace to restart")
				return 0
			}
			if err = m.stopWeb(ctx); err != nil {
				fmt.Fprintln(errOut, err.Error())
				return 1
			}
		} else if *replace {
			if err = m.stopWeb(ctx); err != nil {
				fmt.Fprintln(errOut, err.Error())
				return 1
			}
		}
		// Build a canonical argv; no credential is generated or put on argv.
		childArgs := []string{"--config", m.config.SourcePath, "--store", m.store, "web", "__background", "--bind", *bind}
		if *tnMode {
			childArgs = append(childArgs, "--tailnet")
		}
		if *optional {
			childArgs = append(childArgs, "--tailnet-optional")
		}
		if *keep {
			childArgs = append(childArgs, "--tailnet-keep")
		}
		for _, host := range hosts {
			childArgs = append(childArgs, "--allow-host", host)
		}
		if *notify != "" {
			childArgs = append(childArgs, "--notify-imessage", *notify)
		}
		if err = m.startWeb(ctx, childArgs); err != nil {
			fmt.Fprintln(errOut, err.Error())
			return 1
		}
		state := m.webDashboard()
		if state == nil {
			fmt.Fprintln(errOut, "dashboard readiness unavailable")
			return 1
		}
		fmt.Fprintf(out, "summary: local=http://%s/ tailnet=%s serve=%s imessage=%s\n", state.Address, webHost(state.Host), state.Serve, state.IMessage)
		if !*noMenubar {
			m.startMenubar()
		}
		if err = m.webLink(ctx, state, false, out, errOut); err != nil {
			fmt.Fprintln(errOut, err.Error())
			return 1
		}
		return 0
	}
	w.background, w.job = action == "__background", *job
	if w.background {
		start, err := processStart(os.Getpid())
		if err != nil {
			return 1
		}
		if saveWebFile(filepath.Join(m.store, "web-launch", "process.json"), webState{PID: os.Getpid(), ProcessStart: start, Job: *job}) != nil {
			return 1
		}
		out = io.Discard
	}
	w.notify = m.config.NotifyIMessage
	if *notify != "" {
		w.notify = *notify
	}
	if *tnMode {
		exe, e := findTailscale()
		host := ""
		if e == nil {
			host, e = detectTailnet(exe)
		}
		if e == nil && len(hosts) == 1 && hosts[0] != host {
			fmt.Fprintln(errOut, "--allow-host "+cell(hosts[0])+" does not match the Tailscale name "+host)
			return 1
		}
		if e != nil && !*optional {
			fmt.Fprintln(errOut, e.Error())
			return 1
		}
		if e != nil {
			fmt.Fprintln(errOut, "warning: "+e.Error()+"; the dashboard is loopback-only")
			w.allowHost = ""
		} else {
			w.allowHost = host
			w.tailnet = &tailnet{exe: exe, host: host, optional: *optional, keep: *keep}
		}
	}
	if err = w.Run(ctx, out, errOut); err != nil {
		fmt.Fprintln(errOut, Redact(err.Error()))
		return 1
	}
	return 0
}

func webHost(host string) string {
	if host == "" {
		return "off"
	}
	return cell(host)
}
func webStatusLine(state *webState) string {
	if state == nil {
		return "web: not running"
	}
	job := state.Job
	if job == "" {
		job = "detached/foreground"
	}
	_, port, _ := net.SplitHostPort(state.Address)
	return fmt.Sprintf("web: running since=%s pid=%d job=%s port=%s tailnet=%s serve=%s imessage=%s", state.Started.Format(time.RFC3339), state.PID, cell(job), port, webHost(state.Host), cell(state.Serve), cell(state.IMessage))
}

func (m *Manager) startWeb(ctx context.Context, args []string) (result error) {
	dir := filepath.Join(m.store, "web-launch")
	if err := privateDir(dir); err != nil {
		return err
	}
	var pending webState
	if readWebFile(filepath.Join(dir, "process.json"), &pending) == nil && pending.PID > 1 {
		if start, err := processStart(pending.PID); err == nil && start == pending.ProcessStart {
			return errors.New("web: dashboard start is already in progress; use web stop first")
		}
	}
	launched := false
	defer func() {
		if result != nil && launched {
			cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := m.stopWeb(cleanup); err != nil {
				result = errors.Join(result, err)
			}
		}
	}()
	if webPlatform == "darwin" {
		target := fmt.Sprintf("gui/%d/nuntius.web", os.Getuid())
		if _, err := webLaunchctl("print", target); !errors.Is(err, errJobAbsent) {
			if err == nil {
				return errors.New("web: launchd job already exists; no second job started")
			}
			return err
		}
		args = append(args, "--job", target)
	}
	// Clear only our stale state after confirming there is no live dashboard.
	if err := m.clearWebFiles(); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(m.store, "web-stopped.json")); err != nil && !os.IsNotExist(err) {
		return errors.New("cannot clear dashboard shutdown state")
	}
	log, err := privateFile(filepath.Join(m.store, "web.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY)
	if err != nil {
		return err
	}
	defer log.Close()
	exe, err := webExecutable()
	if err != nil {
		return errors.New("dashboard executable unavailable")
	}
	if webPlatform == "darwin" {
		if err = saveWebFile(filepath.Join(dir, "request.json"), webRequest{Args: args}); err != nil {
			return err
		}
		stop, err := environmentPipe(dir, os.Environ())
		if err != nil {
			return err
		}
		defer stop()
		plist := `<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict><key>Label</key><string>nuntius.web</string><key>ProgramArguments</key><array>` + plistString(exe) + plistString("__nuntius_web_exec") + plistString(dir) + `</array><key>WorkingDirectory</key>` + plistString(m.store) + `<key>StandardInPath</key><string>/dev/null</string><key>StandardOutPath</key>` + plistString(log.Name()) + `<key>StandardErrorPath</key>` + plistString(log.Name()) + `<key>RunAtLoad</key><false/><key>KeepAlive</key><false/><key>LaunchOnlyOnce</key><true/><key>Umask</key><integer>` + strconv.Itoa(dispatchUmask) + `</integer></dict></plist>`
		path := filepath.Join(dir, "job.plist")
		if err = atomicPrivate(path, []byte(plist)); err != nil {
			return err
		}
		domain := fmt.Sprintf("gui/%d", os.Getuid())
		if err = saveWebFile(filepath.Join(dir, "process.json"), webState{Job: domain + "/nuntius.web"}); err != nil {
			return err
		}
		if _, err = webLaunchctl("bootstrap", domain, path); err != nil {
			return err
		}
		launched = true
		if _, err = webLaunchctl("kickstart", "-p", domain+"/nuntius.web"); err != nil {
			return err
		}
	} else {
		cmd := exec.Command(exe, args...)
		cmd.Dir = m.store
		cmd.Stdout, cmd.Stderr = log, log
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err = cmd.Start(); err != nil {
			return errors.New("cannot start detached dashboard")
		}
		launched = true
		_ = cmd.Process.Release()
	}
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); {
		if state := m.webDashboard(); state != nil {
			r, err := client.Get("http://" + state.Address + "/")
			if err == nil {
				r.Body.Close()
				if r.StatusCode == 200 {
					return nil
				}
			}
		}
		var pending webState
		if readWebFile(filepath.Join(dir, "process.json"), &pending) == nil && pending.PID > 1 {
			if start, err := processStart(pending.PID); err != nil || start != pending.ProcessStart {
				return errors.New("dashboard start failed; inspect web.log")
			}
		}
		select {
		case <-ctx.Done():
			return errors.New("dashboard start cancelled; use web stop to clean up")
		case <-time.After(25 * time.Millisecond):
		}
	}
	return errors.New("dashboard did not become ready; inspect web.log, then use web stop")
}

func (m *Manager) webLink(ctx context.Context, state *webState, notify bool, out, errOut io.Writer) error {
	var access webAccess
	if readWebFile(filepath.Join(m.store, "web-access.json"), &access) != nil || access.PID != state.PID || access.ProcessStart != state.ProcessStart || len(access.Token) != 64 {
		return errors.New("owner-only link unavailable; restart with web start --replace")
	}
	w := &Web{address: state.Address, allowHost: state.Host, token: access.Token}
	if notify {
		if access.Notify == "" || state.Host == "" {
			return errors.New("no tailnet iMessage handle configured; restart with --notify-imessage")
		}
		state.IMessage = "sent"
		if sendIMessage(ctx, access.Notify, "nuntius dashboard ("+machineName(state.Host)+"): https://"+state.Host+"/?token="+access.Token) != nil {
			state.IMessage = "failed"
			fmt.Fprintln(errOut, "warning: iMessage not sent")
		}
		if err := saveWebFile(filepath.Join(m.store, "web.json"), state); err != nil {
			return err
		}
		fmt.Fprintln(out, "imessage="+state.IMessage)
	}
	return w.writeLinks(out)
}

func (m *Manager) clearWebFiles() error {
	if err := m.clearWebState(); err != nil {
		return err
	}
	for _, name := range []string{"web-access.json", "web.pid"} {
		if err := os.Remove(filepath.Join(m.store, name)); err != nil && !os.IsNotExist(err) {
			return errors.New("cannot remove dashboard state")
		}
	}
	return syncDir(m.store)
}

func (m *Manager) stopWeb(ctx context.Context) error {
	m.stopMenubar()
	state := m.webDashboard()
	if state == nil {
		var pending webState
		if readWebFile(filepath.Join(m.store, "web-launch", "process.json"), &pending) == nil {
			state = &pending
		}
	}
	if state != nil && state.Job != "" && state.Job != fmt.Sprintf("gui/%d/nuntius.web", os.Getuid()) {
		return errors.New("unrecorded dashboard job refused")
	}
	if state != nil && state.PID > 1 {
		if start, err := processStart(state.PID); err == nil {
			if start != state.ProcessStart {
				return errors.New("dashboard process identity mismatch; no signal sent")
			}
			if err = syscall.Kill(state.PID, syscall.SIGTERM); err != nil {
				return errors.New("cannot stop dashboard")
			}
			for deadline := time.Now().Add(10 * time.Second); ; {
				start, err = processStart(state.PID)
				if err != nil || start != state.ProcessStart {
					break
				}
				if time.Now().After(deadline) {
					return errors.New("dashboard is still stopping; no forced kill")
				}
				select {
				case <-ctx.Done():
					return errors.New("dashboard stop cancelled")
				case <-time.After(25 * time.Millisecond):
				}
			}
		}
	}
	if state != nil && state.Job != "" {
		if state.Job != fmt.Sprintf("gui/%d/nuntius.web", os.Getuid()) {
			return errors.New("unrecorded dashboard job refused")
		}
		b, err := webLaunchctl("print", state.Job)
		if err != nil && !errors.Is(err, errJobAbsent) {
			return err
		}
		if err == nil {
			identified := false
			for _, line := range strings.Split(string(b), "\n") {
				line = strings.TrimSpace(line)
				if line == "state = not running" || line == "state = waiting" {
					identified = true
				}
				if strings.HasPrefix(line, "pid = ") {
					pid, _ := strconv.Atoi(strings.TrimPrefix(line, "pid = "))
					if pid != state.PID {
						return errors.New("dashboard job identity mismatch; no removal")
					}
					identified = true
				}
			}
			if !identified {
				return errors.New("dashboard job state unrecognized; no removal")
			}
			if _, err = webLaunchctl("bootout", state.Job); err != nil && !errors.Is(err, errJobAbsent) {
				return err
			}
		}
	}
	if err := m.clearWebFiles(); err != nil {
		return err
	}
	for _, name := range []string{"process.json", "request.json", "job.plist"} {
		_ = os.Remove(filepath.Join(m.store, "web-launch", name))
	}
	var stopped webStopState
	if readWebFile(filepath.Join(m.store, "web-stopped.json"), &stopped) == nil {
		_ = os.Remove(filepath.Join(m.store, "web-stopped.json"))
		if stopped.ServeCleanupFailed {
			return errors.New("dashboard stopped; Serve cleanup failed; run: tailscale serve --https=443 off")
		}
	}
	return nil
}
