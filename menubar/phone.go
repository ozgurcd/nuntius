package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

type ActionState struct {
	Enabled bool   `json:"enabled"`
	Code    string `json:"code"`
	Reason  string `json:"reason"`
}

// Never display response text, command output, a recipient or a sign-in link.
func actionReason(code string) string {
	if text := map[string]string{
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
		"port_busy":                 "Local web port is already in use. Choose another local port on the Mac.",
		"dashboard_stopped":         "Dashboard stopped. Start it locally.",
	}[code]; text != "" {
		return text
	}
	return "Dashboard control is unavailable. Start it locally or check matching versions."
}

func safeAction(a ActionState) ActionState {
	if a.Enabled && a.Code == "" {
		return ActionState{Enabled: true}
	}
	reason := actionReason(a.Code)
	if reason == actionReason("") {
		a.Code = ""
	}
	return ActionState{Code: a.Code, Reason: reason}
}

func decodeControl(r io.Reader, target any) error {
	b, err := io.ReadAll(io.LimitReader(r, 32769))
	if err != nil || len(b) > 32768 {
		return errors.New(actionReason(""))
	}
	decoder := json.NewDecoder(bytes.NewReader(b))
	if !uniqueControlJSON(b) {
		return errors.New(actionReason(""))
	}
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil || decoder.Decode(new(any)) != io.EOF {
		return errors.New(actionReason(""))
	}
	return nil
}

func uniqueControlJSON(b []byte) bool {
	d := json.NewDecoder(bytes.NewReader(b))
	var value func(int) bool
	value = func(depth int) bool {
		if depth > 16 {
			return false
		}
		t, err := d.Token()
		if err != nil || t == nil {
			return false
		}
		delim, ok := t.(json.Delim)
		if !ok {
			return true
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				k, e := d.Token()
				key, ok := k.(string)
				if e != nil || !ok || seen[key] {
					return false
				}
				seen[key] = true
				if !value(depth + 1) {
					return false
				}
			}
		case '[':
			for d.More() {
				if !value(depth + 1) {
					return false
				}
			}
		default:
			return false
		}
		_, err = d.Token()
		return err == nil
	}
	return value(0)
}

func localWebArgs(config, store, action string) []string {
	args := []string{"--config", config, "--store", store, "web", action}
	if action == "start" {
		args = append(args, "--no-menubar")
	}
	return args
}

type actionFailure struct{ code string }

func (e actionFailure) Error() string { return actionReason(e.code) }

func startFailure(text string) error {
	for _, cause := range []struct{ match, code string }{
		{"phone_restart_required", "phone_restart_required"},
		{"state Stopped", "tailnet_not_running"}, {"state NeedsLogin", "tailnet_not_running"}, {"not connected to Tailscale", "tailnet_not_running"},
		{"CLI was not found", "tailnet_missing"}, {"port is already in use", "port_busy"},
		{"already serves something else", "serve_conflict"}, {"Funnel is on", "funnel_enabled"},
		{"no tailnet DNS name", "tailnet_dns_missing"}, {"HTTPS certificates are off", "tailnet_https_off"},
	} {
		if bytes.Contains([]byte(text), []byte(cause.match)) {
			return actionFailure{cause.code}
		}
	}
	return actionFailure{""}
}

// Stdout is discarded: successful startup may print a private sign-in link.
type startupDiagnostic struct{ bytes.Buffer }

func (b *startupDiagnostic) Write(p []byte) (int, error) {
	n := len(p)
	room := 4096 - b.Len()
	if room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		_, _ = b.Buffer.Write(p)
	}
	return n, nil
}
