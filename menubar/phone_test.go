package main

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type phoneTransport func(*http.Request) (*http.Response, error)

func (f phoneTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMenubarRefusalReason(t *testing.T) {
	client := &socketClient{http: &http.Client{Transport: phoneTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 409, Body: io.NopCloser(strings.NewReader(`{"enabled":false,"code":"recipient_missing","reason":"untrusted private detail"}`))}, nil
	})}}
	err := client.Action("phone")
	if err == nil || !strings.Contains(err.Error(), "iMessage recipient not set") || strings.Contains(err.Error(), "private") {
		t.Fatal("fixed recipient reason unavailable")
	}
}

type phoneTray struct {
	fakeTray
	actions map[string]ActionState
}

func (t *phoneTray) PhoneActions(a map[string]ActionState) { t.actions = a }

func TestMenubarActionAvailabilityAndRefresh(t *testing.T) {
	body := `{"lanes":[],"actions":{"qr":{"enabled":true,"code":"","reason":""},"phone":{"enabled":false,"code":"recipient_missing","reason":"private detail"}}}`
	client := &socketClient{http: &http.Client{Transport: phoneTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}}
	tray := &phoneTray{}
	c := Controller{tray: tray, client: client}
	c.Refresh()
	if !tray.actions["qr"].Enabled || tray.actions["phone"].Enabled || strings.Contains(tray.actions["phone"].Reason, "private") {
		t.Fatal("action prerequisites collapsed")
	}
	c.phoneFailures = map[string]ActionState{"phone": safeAction(ActionState{Code: "messages_failed"})}
	c.Refresh()
	if tray.actions["phone"].Code != "messages_failed" {
		t.Fatal("refresh erased refusal")
	}
	body = `{"lanes":[],"actions":{"qr":{"enabled":true},"phone":{"enabled":true}}}`
	c.Refresh()
	if !tray.actions["phone"].Enabled || len(c.phoneFailures) != 0 {
		t.Fatal("new positive state did not clear refusal")
	}
	body = `{"lanes":[],"actions":{"phone":{"enabled":false,"code":"private-code","reason":"private detail"}}}`
	c.Refresh()
	if tray.actions["phone"].Code != "" || strings.Contains(tray.actions["phone"].Reason, "private") {
		t.Fatal("unknown response code retained")
	}
	for _, malformed := range []string{`{"lanes":[]}`, `{"actions":{"phone":{"enabled":true,"enabled":false}}}`, `{"actions":{"phone":{"enabled":"true"}}}`, strings.Repeat("x", 32769), `{"lanes":[]} {"actions":{}}`} {
		body = malformed
		c.Refresh()
		if tray.actions["phone"].Enabled {
			t.Fatal("malformed capability enabled action")
		}
	}
}

func TestMenubarStartFailureSafeCause(t *testing.T) {
	for _, tc := range []struct{ input, code string }{{"state Stopped", "tailnet_not_running"}, {"CLI was not found", "tailnet_missing"}, {"port is already in use", "port_busy"}, {"already serves something else", "serve_conflict"}, {"Funnel is on", "funnel_enabled"}, {"private arbitrary output", ""}} {
		failure := startFailure(tc.input)
		if failure.Error() != actionReason(tc.code) || strings.Contains(failure.Error(), "private") {
			t.Fatal("unsafe startup cause")
		}
	}
	args := strings.Join(localWebArgs("/fixture/config", "/fixture/store", "start"), " ")
	if !strings.Contains(args, "web start --no-menubar") || strings.Contains(args, "?token=") {
		t.Fatal("helper startup argv")
	}
	var diagnostic startupDiagnostic
	if n, err := diagnostic.Write([]byte(strings.Repeat("x", 5000))); err != nil || n != 5000 || diagnostic.Len() != 4096 {
		t.Fatal("startup diagnostics not bounded")
	}
	client := &socketClient{http: &http.Client{Transport: phoneTransport(func(*http.Request) (*http.Response, error) { return nil, errors.New("private output") })}}
	if err := client.Action("phone"); err == nil || strings.Contains(err.Error(), "private") {
		t.Fatal("transport error exposed")
	}
}
