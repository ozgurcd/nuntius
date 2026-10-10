package nuntius

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Tailscale is driven only through its CLI with fixed argv. Status and serve
// status are read; the only state changes are one https:443 mapping to this
// dashboard's loopback port and the removal of a mapping nuntius created.
// Funnel is never enabled or disabled.
var tailscaleAppPath = "/Applications/Tailscale.app/Contents/MacOS/Tailscale"

type tailnet struct {
	exe, host      string
	optional, keep bool
	created        bool
}

func findTailscale() (string, error) {
	if exe, err := exec.LookPath("tailscale"); err == nil {
		return exe, nil
	}
	if fi, err := os.Stat(tailscaleAppPath); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0111 != 0 {
		return tailscaleAppPath, nil
	}
	return "", errors.New("the Tailscale CLI was not found; install Tailscale so that tailscale is on PATH or in /Applications")
}

// runTailscale returns stdout only; stderr can name peers or paths and is dropped.
func runTailscale(exe string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, exe, args...).Output()
	if err != nil {
		return nil, errors.New("tailscale " + strings.Join(args, " ") + " failed")
	}
	return out, nil
}

var backendStatePattern = regexp.MustCompile(`^[A-Za-z]{1,32}$`)

// detectTailnet returns this node's MagicDNS name once Tailscale is running and
// issues HTTPS certificates for it. Every refusal is one sentence that says
// what to turn on.
func detectTailnet(exe string) (string, error) {
	out, err := runTailscale(exe, "status", "--json")
	var status struct {
		BackendState string
		Self         struct{ DNSName string }
		CertDomains  []string
	}
	if err != nil || json.Unmarshal(out, &status) != nil {
		return "", errors.New("the Tailscale status is unavailable; start Tailscale and log in")
	}
	if status.BackendState != "Running" {
		state := status.BackendState
		if !backendStatePattern.MatchString(state) {
			state = "unknown"
		}
		return "", fmt.Errorf("this Mac is not connected to Tailscale (state %s); open Tailscale and log in", state)
	}
	host := strings.TrimSuffix(status.Self.DNSName, ".")
	if !validTailnetHost(host) {
		return "", errors.New("this Mac has no tailnet DNS name; turn on MagicDNS in the tailnet DNS settings")
	}
	for _, domain := range status.CertDomains {
		if strings.EqualFold(strings.TrimSuffix(domain, "."), host) {
			return host, nil
		}
	}
	return "", fmt.Errorf("HTTPS certificates are off for %s; turn on HTTPS certificates in the tailnet DNS settings", host)
}

func loopbackTarget(proxy, port string) bool {
	u, err := url.Parse(proxy)
	if err != nil || u.Scheme != "http" || u.Path != "" || u.RawQuery != "" || u.User != nil {
		return false
	}
	return (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost") && u.Port() == port
}

// servePlan reads a serve configuration: "absent" when nothing uses https:443
// and "reused" when 443 already proxies only to this dashboard's loopback port.
// Anything else on 443, or Funnel anywhere on this node, is refused with the
// exact command the owner can run.
func servePlan(b []byte, host, port string) (string, error) {
	var config struct {
		TCP map[string]struct {
			HTTPS        bool
			HTTP         bool
			TCPForward   string
			TerminateTLS string
		}
		Web map[string]struct {
			Handlers map[string]struct{ Proxy, Path, Text string }
		}
		AllowFunnel map[string]bool
	}
	if json.Unmarshal(b, &config) != nil {
		return "", errors.New("the tailscale serve status is unreadable; run: tailscale serve status")
	}
	funnel := []string{}
	for key, on := range config.AllowFunnel {
		if on {
			funnel = append(funnel, key)
		}
	}
	if len(funnel) > 0 {
		sort.Strings(funnel)
		_, p, err := net.SplitHostPort(funnel[0])
		if err != nil || p == "" {
			p = "443"
		}
		return "", fmt.Errorf("tailscale Funnel is on for this Mac and nuntius refuses tailnet mode; turn Funnel off with: tailscale funnel --https=%s off", p)
	}
	tcp, tcpOK := config.TCP["443"]
	webOn443 := 0
	for key := range config.Web {
		if strings.HasSuffix(key, ":443") {
			webOn443++
		}
	}
	if !tcpOK && webOn443 == 0 {
		return "absent", nil
	}
	web, webOK := config.Web[host+":443"]
	if tcpOK && tcp.HTTPS && !tcp.HTTP && tcp.TCPForward == "" && tcp.TerminateTLS == "" && webOK && webOn443 == 1 && len(web.Handlers) == 1 {
		if h, ok := web.Handlers["/"]; ok && h.Path == "" && h.Text == "" && loopbackTarget(h.Proxy, port) {
			return "reused", nil
		}
	}
	return "", errors.New("https:443 on this Mac already serves something else; remove it with: tailscale serve --https=443 off")
}

// ensure makes https:443 proxy to the dashboard port, reusing an identical
// mapping, and confirms a new one with serve status before claiming it.
func (t *tailnet) ensure(port string) (string, error) {
	out, err := runTailscale(t.exe, "serve", "status", "--json")
	if err != nil {
		return "", errors.New("the tailscale serve status is unavailable; run: tailscale serve status")
	}
	state, err := servePlan(out, t.host, port)
	if err != nil || state == "reused" {
		return state, err
	}
	if _, err = runTailscale(t.exe, "serve", "--bg", "--https=443", "localhost:"+port); err != nil {
		return "", errors.New("tailscale serve could not be set up; run: tailscale serve --bg --https=443 localhost:" + port)
	}
	t.created = true
	if out, err = runTailscale(t.exe, "serve", "status", "--json"); err == nil {
		state, err = servePlan(out, t.host, port)
	}
	if err != nil || state != "reused" {
		return "", errors.New("the dashboard mapping was not confirmed by tailscale serve status; check: tailscale serve status")
	}
	return "created", nil
}

// release removes only a mapping this run created, unless the owner keeps it.
func (t *tailnet) release() error {
	if t == nil || !t.created || t.keep {
		return nil
	}
	_, err := runTailscale(t.exe, "serve", "--https=443", "off")
	return err
}

func machineName(host string) string {
	name, _, _ := strings.Cut(host, ".")
	return name
}

// The script is fixed; the handle and text arrive as argv and are never spliced
// into AppleScript source.
const imessageScript = `on run argv
	tell application "Messages"
		set targetService to 1st account whose service type = iMessage
		set targetBuddy to participant (item 1 of argv) of targetService
		send (item 2 of argv) to targetBuddy
	end tell
end run`

var emailHandle = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._%+-]{0,63}@[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)+$`)
var phoneHandle = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)

func validIMessageHandle(h string) bool {
	return len(h) <= 254 && (emailHandle.MatchString(h) || phoneHandle.MatchString(h))
}

// sendIMessage delivers one message through Messages. Output is discarded: it
// could echo the text, and the text carries the sign-in link.
func sendIMessage(ctx context.Context, handle, text string) error {
	if !validIMessageHandle(handle) {
		return errors.New("the iMessage handle must be an email address or E.164 phone number")
	}
	exe, err := exec.LookPath("osascript")
	if err != nil {
		return errors.New("osascript not found; iMessage delivery needs macOS")
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	if exec.CommandContext(ctx, exe, "-e", imessageScript, handle, text).Run() != nil {
		return errors.New("the Messages app did not send it; allow nuntius to control Messages when macOS asks, and check the handle")
	}
	return nil
}

// webState records a live tailnet dashboard for status --long. It never holds
// the sign-in credential.
type webState struct {
	PID          int       `json:"pid"`
	ProcessStart string    `json:"process_start"`
	Host         string    `json:"tailnet_host"`
	Serve        string    `json:"serve"`
	Started      time.Time `json:"started"`
}

func (m *Manager) saveWebState(host, serve string) error {
	start, err := processStart(os.Getpid())
	if err != nil {
		return errors.New("cannot record the dashboard process identity")
	}
	b, err := json.Marshal(webState{PID: os.Getpid(), ProcessStart: start, Host: host, Serve: serve, Started: time.Now().UTC()})
	if err != nil {
		return err
	}
	return atomicPrivate(filepath.Join(m.store, "web.json"), b)
}

func (m *Manager) clearWebState() error {
	if err := os.Remove(filepath.Join(m.store, "web.json")); err != nil && !os.IsNotExist(err) {
		return err
	}
	return syncDir(m.store)
}

// webDashboard returns the recorded tailnet dashboard while its process lives.
func (m *Manager) webDashboard() *webState {
	f, err := privateFile(filepath.Join(m.store, "web.json"), os.O_RDONLY)
	if err != nil {
		return nil
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 4097))
	var v webState
	if err != nil || len(b) > 4096 || strictJSON(b, &v) != nil || v.PID <= 1 || !validTailnetHost(v.Host) {
		return nil
	}
	if start, err := processStart(v.PID); err != nil || start != v.ProcessStart {
		return nil
	}
	return &v
}
