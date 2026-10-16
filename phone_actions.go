package nuntius

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
)

// Only fixed public codes and text cross the private control socket.
type phoneAction struct {
	Enabled bool   `json:"enabled"`
	Code    string `json:"code"`
	Reason  string `json:"reason"`
}

func phoneReason(code string) string {
	return map[string]string{
		"phone_access_off":          "Phone access is off: the dashboard is running without Tailscale. Enable phone access in Settings, then restart locally.",
		"phone_restart_required":    "Phone access is enabled for the next start. Restart the dashboard locally.",
		"tailnet_missing":           "Tailscale is not installed. Install it on the Mac.",
		"tailnet_not_running":       "Tailscale not running. Open Tailscale and log in.",
		"tailnet_dns_missing":       "Phone setup is incomplete. Enable MagicDNS in Tailscale.",
		"tailnet_https_off":         "Phone setup is incomplete. Enable HTTPS certificates in Tailscale.",
		"serve_conflict":            "Tailscale serves something else. Check tailscale serve status locally.",
		"funnel_enabled":            "Tailscale Funnel is on. Turn Funnel off locally.",
		"readiness_unavailable":     "Phone access is not ready. Check Tailscale and restart locally if needed.",
		"setup_pending":             "Phone access is starting. Wait for local startup to finish.",
		"configuration_unavailable": "Settings are unavailable. Run nuntius config check locally.",
		"notifications_off":         "iMessage notifications are off. Enable iMessage in Settings.",
		"recipient_missing":         "iMessage recipient not set. Configure it on the Mac.",
		"messages_unavailable":      "Phone link sending needs Messages on macOS.",
		"messages_failed":           "Messages did not send the link. Allow access to Messages and check the recipient on the Mac.",
		"presentation_unavailable":  "The private QR page could not be created. Check the local data directory.",
		"presentation_open_failed":  "The private QR page could not be opened. Check the Mac's browser.",
		"method_refused":            "This action is unavailable. Use matching dashboard and helper versions.",
		"action_unknown":            "This action is unavailable. Use matching dashboard and helper versions.",
	}[code]
}

func phoneRefusal(out http.ResponseWriter, status int, code string) {
	out.Header().Set("Content-Type", "application/json")
	out.WriteHeader(status)
	_ = json.NewEncoder(out).Encode(phoneAction{Code: code, Reason: phoneReason(code)})
}

func phoneStartupReason(code string) string {
	switch code {
	case "stopped", "logged_out", "disconnected":
		return "tailnet_not_running"
	case "missing":
		return "tailnet_missing"
	case "dns":
		return "tailnet_dns_missing"
	case "https":
		return "tailnet_https_off"
	case "funnel":
		return "funnel_enabled"
	case "serve_conflict":
		return "serve_conflict"
	default:
		return "readiness_unavailable"
	}
}

var phoneMessagesAvailable = func() bool { _, err := exec.LookPath("osascript"); return runtime.GOOS == "darwin" && err == nil }

// Verify without creating/repairing mappings, reading API data, or logging a grant.
func (w *Web) phoneReadiness(ctx context.Context) string {
	if w.phoneCheck != nil {
		return w.phoneCheck(ctx)
	}
	w.phoneMu.RLock()
	ready := w.phoneReady
	w.phoneMu.RUnlock()
	if !ready {
		return "setup_pending"
	}
	start, err := processStart(os.Getpid())
	if err != nil {
		return "readiness_unavailable"
	}
	state := webState{PID: os.Getpid(), ProcessStart: start, Address: w.address, Host: w.allowHost}
	if w.background {
		var saved webState
		var access webAccess
		if readWebFile(filepath.Join(w.manager.store, "web.json"), &saved) != nil || readWebFile(filepath.Join(w.manager.store, "web-access.json"), &access) != nil || saved.PID != state.PID || saved.ProcessStart != start || saved.Address != w.address || saved.Host != w.allowHost || access.PID != state.PID || access.ProcessStart != start || access.Token != w.token {
			return "readiness_unavailable"
		}
	}
	if !ensureProbe(ctx, state, w.token) {
		return "readiness_unavailable"
	}
	if w.tailnet == nil {
		return ""
	} // Explicit manual --allow-host contract.
	d := webEnsureRuntime{run: ensureCommand}
	host, reason := ensureTailnet(ctx, d, w.tailnet.exe)
	switch reason {
	case "tailnet_inactive":
		return "tailnet_not_running"
	case "tailnet_name_unavailable":
		return "tailnet_dns_missing"
	case "tailnet_certificate_unavailable":
		return "tailnet_https_off"
	case "":
	default:
		return "readiness_unavailable"
	}
	if host != w.allowHost {
		return "readiness_unavailable"
	}
	_, port, _ := net.SplitHostPort(w.address)
	b, err := ensureCommand(ctx, w.tailnet.exe, "serve", "status", "--json")
	if err != nil {
		return "readiness_unavailable"
	}
	mapping, err := servePlan(b, host, port)
	if err != nil {
		return phoneStartupReason(webStartupCode(err.Error()))
	}
	if mapping != "reused" {
		return "readiness_unavailable"
	}
	return ""
}

func (w *Web) phoneCapabilities(ctx context.Context) (map[string]phoneAction, string) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	result := map[string]phoneAction{}
	refuse := func(code string) (map[string]phoneAction, string) {
		for _, a := range []string{"qr", "phone"} {
			result[a] = phoneAction{Code: code, Reason: phoneReason(code)}
		}
		return result, ""
	}
	snapshot, err := w.manager.displaySnapshot(ctx)
	if err != nil {
		return refuse("configuration_unavailable")
	}
	c := snapshot.config
	// A menu setting saved in this process is visible immediately. Contention
	// retains the advisory snapshot instead of waiting on a serving writer.
	if w.manager.mu.TryLock() {
		c = cloneDisplayConfig(w.manager.config)
		w.manager.mu.Unlock()
	}
	if w.allowHost == "" {
		if w.phoneFallback != "" {
			return refuse(w.phoneFallback)
		}
		if c.PhoneAccess {
			return refuse("phone_restart_required")
		}
		return refuse("phone_access_off")
	}
	if reason := w.phoneReadiness(ctx); reason != "" {
		return refuse(reason)
	}
	result["qr"] = phoneAction{Enabled: true}
	dir := filepath.Join(w.manager.store, "menubar-pages")
	if st, err := os.Lstat(dir); err == nil {
		owner, ok := st.Sys().(*syscall.Stat_t)
		if !st.IsDir() || st.Mode().Perm() != 0700 || !ok || owner.Uid != uint32(os.Getuid()) {
			result["qr"] = phoneAction{Code: "presentation_unavailable", Reason: phoneReason("presentation_unavailable")}
		}
	} else if !os.IsNotExist(err) {
		result["qr"] = phoneAction{Code: "presentation_unavailable", Reason: phoneReason("presentation_unavailable")}
	}
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	if _, err := exec.LookPath(opener); err != nil {
		result["qr"] = phoneAction{Code: "presentation_open_failed", Reason: phoneReason("presentation_open_failed")}
	}
	target := c.NotifyIMessage
	if w.notifyOverride {
		target = w.notify
	}
	code := ""
	if c.NotifyIMessageEnabled != nil && !*c.NotifyIMessageEnabled {
		code = "notifications_off"
	} else if target == "" {
		code = "recipient_missing"
	} else if !phoneMessagesAvailable() {
		code = "messages_unavailable"
	}
	result["phone"] = phoneAction{Enabled: code == "", Code: code, Reason: phoneReason(code)}
	return result, target
}
