package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"

	"fyne.io/systray"
)

//go:embed assets/*
var assets embed.FS

type socketClient struct {
	http    *http.Client
	actions map[string]ActionState
}

func (s *socketClient) Status() ([]Lane, error) {
	s.actions = nil
	r, err := s.http.Get("http://unix/status")
	if err != nil {
		return nil, errors.New("dashboard unavailable")
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		var state ActionState
		if decodeControl(r.Body, &state) != nil {
			return nil, actionFailure{""}
		}
		return nil, actionFailure{safeAction(state).Code}
	}
	var state struct {
		Lanes   []Lane                 `json:"lanes"`
		Actions map[string]ActionState `json:"actions"`
	}
	err = decodeControl(r.Body, &state)
	if err == nil {
		s.actions = map[string]ActionState{}
		for _, key := range []string{"qr", "phone"} {
			s.actions[key] = safeAction(state.Actions[key])
		}
	}
	return state.Lanes, err
}
func (s *socketClient) PhoneActions() map[string]ActionState { return s.actions }
func (s *socketClient) Action(a string) error {
	r, err := s.http.Post("http://unix/"+a, "application/json", nil)
	if err != nil {
		return actionFailure{""}
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		var state ActionState
		if decodeControl(r.Body, &state) != nil {
			return actionFailure{""}
		}
		return actionFailure{safeAction(state).Code}
	}
	return nil
}

type nativeTray struct {
	rows            []*systray.MenuItem
	actions         map[string]*systray.MenuItem
	settingsMenu    *systray.MenuItem
	settingsRows    []*systray.MenuItem
	settingsActions []string
	choose          func(string)
}

func (t *nativeTray) Settings(rows []Setting) {
	t.settingsActions = make([]string, len(rows))
	for i, row := range rows {
		if i >= len(t.settingsRows) {
			item := t.settingsMenu.AddSubMenuItem("", "")
			t.settingsRows = append(t.settingsRows, item)
			go func(index int) {
				for range item.ClickedCh {
					t.choose(strconv.Itoa(index))
				}
			}(i)
		}
		item := t.settingsRows[i]
		item.SetTitle(row.Title)
		item.Show()
		t.settingsActions[i] = row.Action
		if row.Action == "" {
			item.Disable()
		} else {
			item.Enable()
		}
	}
	for i := len(rows); i < len(t.settingsRows); i++ {
		t.settingsRows[i].Hide()
	}
}

func (t *nativeTray) Update(title, tip string, rows []string, online bool) {
	if runtime.GOOS == "linux" {
		base, _ := assets.ReadFile("assets/relay-linux.png")
		systray.SetIcon(stateIcon(base, title))
	}
	systray.SetTitle(title)
	systray.SetTooltip(tip)
	for i, row := range rows {
		if i >= len(t.rows) {
			t.rows = append(t.rows, systray.AddMenuItem("", ""))
			t.rows[i].Disable()
		}
		t.rows[i].SetTitle(row)
		t.rows[i].Show()
	}
	for i := len(rows); i < len(t.rows); i++ {
		t.rows[i].Hide()
	}
	for _, a := range []string{"open", "stop"} {
		if online {
			t.actions[a].Enable()
		} else {
			t.actions[a].Disable()
		}
	}
	if online {
		t.actions["start"].Disable()
	} else {
		t.actions["start"].Enable()
	}
}

func (t *nativeTray) PhoneActions(actions map[string]ActionState) {
	for key, title := range map[string]string{"phone": "Send phone link", "qr": "Show phone QR"} {
		item := t.actions[key]
		a := safeAction(actions[key])
		if a.Enabled {
			item.SetTitle(title)
			item.SetTooltip(title)
			item.Enable()
		} else {
			item.Disable()
			item.SetTitle(title + " · " + a.Reason)
			item.SetTooltip(a.Reason)
		}
	}
}
func privateDirectory(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	st, err := os.Lstat(path)
	if err != nil || !st.IsDir() || st.Mode().Perm()&0077 != 0 {
		return errors.New("private directory required")
	}
	return nil
}
func main() {
	fs := flag.NewFlagSet("nuntius-menubar", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	store := fs.String("store", "", "private store")
	config := fs.String("config", "", "configuration")
	binary := fs.String("nuntius", "", "main executable")
	if fs.Parse(os.Args[1:]) != nil || len(fs.Args()) != 0 || !filepath.IsAbs(*store) || !filepath.IsAbs(*config) || !filepath.IsAbs(*binary) {
		fmt.Println("nuntius-menubar: requires absolute store, config and nuntius paths")
		return
	}
	if !desktopSupported() {
		fmt.Println("nuntius-menubar: no supported desktop tray; GNOME needs an AppIndicator extension, XFCE a StatusNotifier plugin")
		return
	}
	dir := filepath.Join(*store, "menubar")
	if privateDirectory(*store) != nil || privateDirectory(dir) != nil {
		fmt.Println("nuntius-menubar: private store unavailable")
		return
	}
	lock, err := os.OpenFile(filepath.Join(dir, "helper.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return
	}
	defer lock.Close()
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return
	}
	path := filepath.Join(dir, "helper.sock")
	if st, e := os.Lstat(path); e == nil {
		if st.Mode()&os.ModeSocket == 0 {
			return
		}
		_ = os.Remove(path)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		fmt.Println("nuntius-menubar: helper socket unavailable")
		return
	}
	defer ln.Close()
	defer os.Remove(path)
	if os.Chmod(path, 0600) != nil {
		return
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	closeServer := &http.Server{ReadHeaderTimeout: time.Second, WriteTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/quit" {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(200)
		cancel()
	})}
	go func() { _ = closeServer.Serve(ln) }()
	defer closeServer.Close()
	client := &socketClient{http: &http.Client{Timeout: 100 * time.Second, Transport: &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", filepath.Join(dir, "dashboard.sock"))
	}}}}
	defer client.http.CloseIdleConnections()
	var controller *Controller
	var mu sync.Mutex
	systray.Run(func() {
		setRelayIcon()
		tray := &nativeTray{actions: map[string]*systray.MenuItem{}}
		tray.settingsMenu = systray.AddMenuItem("Local status & login", "Agent counts and local helper login toggle")
		tray.choose = func(index string) {
			mu.Lock()
			defer mu.Unlock()
			i, err := strconv.Atoi(index)
			if err == nil && i < len(tray.settingsActions) {
				controller.Action(tray.settingsActions[i])
			}
		}
		actions := []struct{ key, title string }{{"settings", "Settings…"}, {"open", "Open dashboard"}, {"phone", "Send phone link"}, {"qr", "Show phone QR"}, {"start", "Start dashboard"}, {"stop", "Stop dashboard"}, {"quit", "Quit menu bar"}}
		controller = &Controller{tray: tray, client: client, quit: cancel, run: func(a string) error {
			args := localWebArgs(*config, *store, a)
			// Stopping the dashboard also requests helper quit. Let its CLI
			// finish stopping the server after the helper context is cancelled.
			runCtx, done := context.WithTimeout(context.Background(), 30*time.Second)
			defer done()
			cmd := exec.CommandContext(runCtx, *binary, args...)
			var diagnostic startupDiagnostic
			cmd.Stdout = io.Discard
			cmd.Stderr = &diagnostic
			if err := cmd.Run(); err != nil {
				return startFailure(diagnostic.String())
			}
			return nil
		}}
		controller.apply = func(args []string) error {
			runCtx, done := context.WithTimeout(context.Background(), 30*time.Second)
			defer done()
			return exec.CommandContext(runCtx, *binary, append([]string{"--config", *config, "--store", *store}, args...)...).Run()
		}
		controller.settings = func() (Settings, error) {
			runCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
			defer done()
			cmd := exec.CommandContext(runCtx, *binary, "--config", *config, "--store", *store, "set", "--json")
			var value Settings
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				return value, err
			}
			if err = cmd.Start(); err != nil {
				return value, err
			}
			err = json.NewDecoder(io.LimitReader(stdout, 65536)).Decode(&value)
			if waitErr := cmd.Wait(); err == nil {
				err = waitErr
			}
			home, _ := os.UserHomeDir()
			_, loginErr := os.Stat(filepath.Join(home, "Library", "LaunchAgents", "io.nuntius.menubar.plist"))
			value.AtLogin = loginErr == nil
			value.LoginKnown = runtime.GOOS == "darwin" && (loginErr == nil || os.IsNotExist(loginErr))
			return value, err
		}
		for _, a := range actions {
			item := systray.AddMenuItem(a.title, a.title)
			tray.actions[a.key] = item
			go func(key string, item *systray.MenuItem) {
				for {
					select {
					case <-ctx.Done():
						return
					case <-item.ClickedCh:
						mu.Lock()
						controller.Action(key)
						mu.Unlock()
					}
				}
			}(a.key, item)
		}
		systray.AddSeparator()
		go func() {
			tick := time.NewTicker(3 * time.Second)
			defer tick.Stop()
			for {
				mu.Lock()
				controller.Refresh()
				mu.Unlock()
				select {
				case <-ctx.Done():
					systray.Quit()
					return
				case <-tick.C:
				}
			}
		}()
	}, func() { cancel() })
}
