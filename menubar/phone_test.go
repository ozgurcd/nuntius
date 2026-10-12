package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

type phoneTransport func(*http.Request) (*http.Response, error)

func (f phoneTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMenubarControlExactKeys(t *testing.T) {
	malformed := []string{
		`{"lanes":[],"actions":{"phone":{"ENABLED":true}}}`,
		`{"lanes":[],"actions":{"phone":{"enabled":false,"Enabled":true}}}`,
		`{"lanes":[],"actions":{"phone":{"enabled":true},"../qr":{"enabled":true}}}`,
		`{"lanes":[],"lanes":[],"actions":{}}`,
		`{"actions":{},"actions":{}}`,
		`{"actions":{"phone":{},"phone":{}}}`,
		`{"actions":{"phone":{},"ph\u006fne":{}}}`,
		`{"actions":{"phone":{"enabled":true,"enabl\u0065d":false}}}`,
		`{"lanes":[{"lane":"a","lane":"b"}],"actions":{}}`,
		`{"Lanes":[],"actions":{}}`, `{"lanes":[{"Goal_id":"x"}],"actions":{}}`,
		`{"actions":{"Phone":{"enabled":true}}}`, `{"actions":{"phone":{"Code":"recipient_missing"}}}`,
		`{"actions":{"phone":{"Reason":"untrusted"}}}`,
		`{"actions":null}`, `{"lanes":null}`, `{"lanes":[null]}`, `{"actions":{"phone":null}}`,
		`{"actions":{"phone":{"enabled":null}}}`, `{"actions":{"phone":{"enabled":"true"}}}`,
		`{"lanes":[{"lane":1}]}`, `{"lanes":[{"question":1}]}`, `{"lanes":[{"elapsed_seconds":1.5}]}`,
		`{"lanes":[{"elapsed_seconds":9223372036854775808}]}`, `{"actions":{"phone":{"reason":null}}}`,
		`{"lanes":[]} {"actions":{}}`, `{"lanes":[]} trailing`, `[]`, `null`, strings.Repeat("x", 32769),
	}
	for _, separator := range []string{`\u2028`, `\u2029`, `\u0085`, `\n`, `\r`} {
		malformed = append(malformed, `{"lanes":[{"state":"a`+separator+`b"}]}`,
			`{"actions":{"phone":{"reason":"a`+separator+`b"}}}`)
	}
	for i, body := range malformed {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			client := &socketClient{http: &http.Client{Transport: phoneTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}}
			_, err := client.Status()
			if err == nil || err.Error() != actionReason("") {
				t.Fatal("malformed status did not return fixed refusal")
			}
			for _, key := range []string{"qr", "phone"} {
				state := safeAction(client.PhoneActions()[key])
				if state.Enabled || state.Code != "" || state.Reason != actionReason("") || len(state.Reason) > 256 {
					t.Fatal("malformed status enabled action or exposed input")
				}
			}
		})
	}
	for _, body := range []string{`{"lanes":[],"actions":{"qr":{"enabled":true},"phone":{"enabled":true}}}`,
		`{"lanes":[{"lane":"a","goal_id":"G","state":"idle","elapsed_seconds":1,"question":false,"failed":false}],"actions":{"ph\u006fne":{"enabl\u0065d":true},"qr":{"enabled":true}}}`} {
		client := &socketClient{http: &http.Client{Transport: phoneTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		})}}
		if _, err := client.Status(); err != nil || !client.PhoneActions()["phone"].Enabled || !client.PhoneActions()["qr"].Enabled {
			t.Fatal("canonical control refused")
		}
	}
	client := &socketClient{http: &http.Client{Transport: phoneTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})}}
	if _, err := client.Status(); err != nil || client.PhoneActions()["phone"].Enabled || client.PhoneActions()["qr"].Enabled {
		t.Fatal("omitted optional fields did not remain unavailable")
	}
}

func TestMenubarRefusalExactKeys(t *testing.T) {
	for _, body := range []string{`{"CODE":"recipient_missing"}`, `{"code":"recipient_missing","Code":"messages_failed"}`,
		`{"code":"recipient_missing","co\u0064e":"messages_failed"}`, `{"extra":true}`, `{"code":null}`,
		`{"enabled":1}`, `{"reason":"a\u2028b"}`, `{"reason":"a\u2029b"}`, `{"reason":"a\u0085b"}`, `{} {}`} {
		client := &socketClient{http: &http.Client{Transport: phoneTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 409, Body: io.NopCloser(strings.NewReader(body))}, nil
		})}}
		_, statusErr := client.Status()
		for _, err := range []error{statusErr, client.Action("phone"), client.Action("qr")} {
			if err == nil || err.Error() != actionReason("") {
				t.Fatal("malformed refusal did not use fixed fallback")
			}
		}
	}
}

func TestMenubarCurrentRefusalSupersedesOld(t *testing.T) {
	body := `{"lanes":[],"actions":{"phone":{"code":"notifications_off"},"qr":{"code":"tailnet_not_running"}}}`
	client := &socketClient{http: &http.Client{Transport: phoneTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}}
	tray := &phoneTray{}
	c := Controller{tray: tray, client: client, phoneFailures: map[string]ActionState{
		"phone": safeAction(ActionState{Code: "messages_failed"}), "qr": safeAction(ActionState{Code: "presentation_open_failed"}),
	}}
	for i := 0; i < 2; i++ {
		c.Refresh()
		for key, code := range map[string]string{"phone": "notifications_off", "qr": "tailnet_not_running"} {
			if state := tray.actions[key]; state.Enabled || state.Code != code || state.Reason != actionReason(code) {
				t.Fatal("current prerequisite hidden by old failure")
			}
		}
		if len(c.phoneFailures) != 0 {
			t.Fatal("superseded failure retained")
		}
	}
	body = `{"lanes":[],"actions":{"phone":{"code":"recipient_missing"},"qr":{"enabled":true}}}`
	c.Refresh()
	if tray.actions["phone"].Code != "recipient_missing" || !tray.actions["qr"].Enabled {
		t.Fatal("current independent prerequisites lost")
	}
	body = `{"lanes":[],"actions":{"phone":{"code":"unknown"}}}`
	c.Refresh()
	if tray.actions["phone"].Code != "" || tray.actions["phone"].Reason != actionReason("") {
		t.Fatal("cleared failure resurrected")
	}
}

type observingPhoneClient struct {
	fakeClient
	observe func(string)
}

func (f *observingPhoneClient) Action(action string) error {
	f.observe(action)
	return f.fakeClient.Action(action)
}

func TestMenubarPhoneAttemptClearsStaleFailure(t *testing.T) {
	for key, old := range map[string]string{"phone": "messages_failed", "qr": "presentation_open_failed"} {
		for _, failure := range []error{nil, actionFailure{"readiness_unavailable"}} {
			tray := &phoneTray{}
			c := Controller{tray: tray, phoneFailures: map[string]ActionState{key: safeAction(ActionState{Code: old})}}
			c.client = &observingPhoneClient{fakeClient: fakeClient{err: failure}, observe: func(action string) {
				if _, present := c.phoneFailures[action]; present {
					t.Error("old failure still cached during new attempt")
				}
			}}
			c.Action(key)
			if failure == nil {
				if len(c.phoneFailures) != 0 {
					t.Fatal("successful attempt retained old failure")
				}
			} else {
				c.updatePhoneActions(nil)
				if tray.actions[key].Code != "readiness_unavailable" {
					t.Fatal("new click failure not visible")
				}
				c.updatePhoneActions(map[string]ActionState{key: {Enabled: true}})
				if !tray.actions[key].Enabled || len(c.phoneFailures) != 0 {
					t.Fatal("positive refresh retained failure")
				}
			}
		}
	}
}

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
	if tray.actions["phone"].Code != "recipient_missing" || len(c.phoneFailures) != 0 {
		t.Fatal("refresh hid current prerequisite refusal")
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
