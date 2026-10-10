package nuntius

import (
	"bytes"
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"rsc.io/qr"
)

func TestTailnetGuards(t *testing.T) {
	m, _, _ := manager(t)
	const host = "mac.tail123.ts.net"
	w, err := NewWeb(m, "127.0.0.1:8123", host)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, host, origin, path string
		auth                     bool
		want                     int
	}{
		{"host", host, "", "/", false, 200},
		{"443", host + ":443", "", "/", false, 200},
		{"data", host, "", "/data", true, 200},
		{"no header", host, "", "/data", false, 403},
		{"query", host, "", "/data?token=" + w.token, false, 403},
		{"https post", host, "https://" + host, "/action", true, 200},
		{"443 post", host + ":443", "https://" + host, "/action", true, 200},
		{"http", host, "http://" + host, "/action", true, 403},
		{"origin port", host, "https://" + host + ":443", "/action", true, 403},
		{"origin suffix", host, "https://" + host + ".evil.test", "/action", true, 403},
		{"missing origin", host, "POST", "/action", true, 403},
		{"other tailnet", "mac.other.ts.net", "", "/", true, 403},
		{"suffix", host + ".evil.test", "", "/", true, 403},
		{"subdomain", "evil." + host, "", "/", true, 403},
		{"wrong port", host + ":8123", "", "/", true, 403},
		{"forwarded spoof", "evil.test", "https://" + host, "/action", true, 403},
		{"loopback", "127.0.0.1:8123", "http://127.0.0.1:8123", "/action", true, 200},
		{"cross local", "127.0.0.1:8123", "https://" + host, "/action", true, 403},
		{"cross tailnet", host, "http://127.0.0.1:8123", "/action", true, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			method := "GET"
			if tc.origin != "" {
				method = "POST"
			}
			r := httptest.NewRequest(method, "http://127.0.0.1:8123"+tc.path, strings.NewReader(`{"action":"unfreeze"}`))
			r.Host = tc.host
			if tc.origin != "POST" {
				r.Header.Set("Origin", tc.origin)
			}
			r.Header.Set("X-Forwarded-Host", host)
			r.Header.Set("X-Forwarded-Proto", "https")
			r.Header.Set("Forwarded", "host="+host+";proto=https")
			if tc.auth {
				r.Header.Set("X-Nuntius-Token", w.token)
			}
			out := httptest.NewRecorder()
			w.ServeHTTP(out, r)
			if out.Code != tc.want {
				t.Fatalf("status=%d, want=%d", out.Code, tc.want)
			}
			if out.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatal("CORS enabled")
			}
		})
	}
	local, _ := NewWeb(m, "127.0.0.1:8123")
	r := httptest.NewRequest("GET", "http://"+host+"/", nil)
	out := httptest.NewRecorder()
	local.ServeHTTP(out, r)
	if out.Code != 403 {
		t.Fatal("tailnet enabled without opt-in")
	}
	for _, invalid := range []string{"", "ts.net", ".ts.net", "*.tail.ts.net", "mac.tail.ts.net:443", "https://mac.tail.ts.net", "mac.tail.ts.net/", "mac.tail.ts.net.", "mac..ts.net", "-mac.tail.ts.net", "mac_.tail.ts.net", "mac.tail.example", strings.Repeat("x", 64) + ".ts.net", strings.Repeat("x.", 125) + "ts.net"} {
		if _, err := NewWeb(m, "127.0.0.1:8123", invalid); err == nil {
			t.Fatal("invalid allow-host accepted")
		}
	}
	if _, err := NewWeb(m, "0.0.0.0:8123", host); err == nil {
		t.Fatal("remote bind allowed")
	}
}

func TestTailnetCLIRefusals(t *testing.T) {
	_, c, store := manager(t)
	cliFixture(t, c, store)
	for _, flags := range [][]string{{"--allow-host", "evil.test"}, {"--allow-host", ""}, {"--allow-host", "mac.tail.ts.net", "--allow-host", "other.tail.ts.net"}, {"--allow-host", "mac.tail.ts.net", "--bind", "0.0.0.0:8123"}} {
		var out, diagnostic bytes.Buffer
		if CLI(context.Background(), append([]string{"web"}, flags...), strings.NewReader(""), &out, &diagnostic) != 1 || out.Len() != 0 {
			t.Fatal("unsafe web CLI accepted")
		}
	}
}

func TestTailnetSignInQR(t *testing.T) {
	m, _, _ := manager(t)
	w, _ := NewWeb(m, "127.0.0.1:8123", "mac.tail123.ts.net")
	var out bytes.Buffer
	if err := w.writeLinks(&out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(out.String(), "\n")
	if lines[0] != "http://127.0.0.1:8123/?token="+w.token || lines[1] != "https://mac.tail123.ts.net/?token="+w.token {
		t.Fatal("incorrect sign-in links")
	}
	code, err := qr.Encode(lines[1], qr.M)
	if err != nil {
		t.Fatal("fixture QR encoding failed")
	}
	// Reconstruct every terminal module, including the four-module quiet zone.
	if len(lines) != code.Size+12 {
		t.Fatal("wrong QR height")
	}
	for y := -4; y < code.Size+4; y++ {
		line := strings.TrimPrefix(lines[y+7], "\x1b[30;47m")
		line = strings.TrimSuffix(line, "\x1b[0m")
		cells := []rune(line)
		if len(cells) != 2*(code.Size+8) {
			t.Fatal("wrong QR width")
		}
		for x := -4; x < code.Size+4; x++ {
			want := ' '
			if code.Black(x, y) {
				want = '█'
			}
			if cells[2*(x+4)] != want || cells[2*(x+4)+1] != want {
				t.Fatal("terminal QR changed encoded URL")
			}
		}
	}
}
