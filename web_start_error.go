package nuntius

import "strings"

// Persist only a known cause, never child output or a tail of web.log.
type webStartupFailure struct {
	Code string `json:"code"`
}

func webStartupCode(text string) string {
	for _, cause := range []struct{ match, code string }{
		{"state Stopped", "stopped"}, {"state NeedsLogin", "logged_out"},
		{"not connected to Tailscale", "disconnected"},
		{"Tailscale CLI was not found", "missing"},
		{"Tailscale status is unavailable", "status"},
		{"port is already in use", "port"},
		{"cannot listen on local web address", "listen"},
		{"already serves something else", "serve_conflict"},
		{"Funnel is on", "funnel"},
		{"serve status is unavailable", "serve_status"},
		{"serve status is unreadable", "serve_status"},
		{"serve could not be set up", "serve_setup"},
		{"mapping was not confirmed", "serve_readback"},
		{"no tailnet DNS name", "dns"},
		{"HTTPS certificates are off", "https"},
	} {
		if strings.Contains(text, cause.match) {
			return cause.code
		}
	}
	return "unknown"
}

func (f webStartupFailure) cause() string {
	return map[string]string{
		"stopped":        "Tailscale is not connected (state Stopped): open Tailscale and log in",
		"logged_out":     "Tailscale is not connected (state NeedsLogin): open Tailscale and log in",
		"disconnected":   "Tailscale is not connected: open Tailscale and log in",
		"missing":        "the Tailscale CLI was not found; install Tailscale so that tailscale is on PATH or in /Applications",
		"status":         "the Tailscale status is unavailable; start Tailscale and log in",
		"port":           "local web port is already in use; choose another --bind port",
		"listen":         "cannot listen on local web address; check the --bind address",
		"serve_conflict": "https:443 on this Mac already serves something else; check: tailscale serve status",
		"funnel":         "tailscale Funnel is on for this Mac and nuntius refuses tailnet mode; check: tailscale funnel status",
		"serve_status":   "the tailscale serve status is unavailable or unreadable; run: tailscale serve status",
		"serve_setup":    "tailscale serve could not be set up; check: tailscale serve status",
		"serve_readback": "the dashboard mapping was not confirmed; check: tailscale serve status",
		"dns":            "this Mac has no tailnet DNS name; turn on MagicDNS in the tailnet DNS settings",
		"https":          "HTTPS certificates are off; turn on HTTPS certificates in the tailnet DNS settings",
	}[f.Code]
}
