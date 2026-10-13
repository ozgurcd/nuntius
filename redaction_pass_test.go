package nuntius

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// The reference deliberately has no production classifier, span, path or state
// helper calls. It searches a complete buffer and applies dominance first.
type oracleRange struct{ a, b int }

var oracleBoundary = regexp.MustCompile(`-----((?:BEGIN|END)(?:RSA|EC|DSA|OPENSSH|ENCRYPTED)?PRIVATEKEY)-----`)

func oracleNormalize(s string) (string, []oracleRange) {
	var out strings.Builder
	var origins []oracleRange
	for i, r := range s {
		if unicode.IsSpace(r) {
			continue
		}
		up := string(unicode.ToUpper(r))
		out.WriteString(up)
		for range len(up) {
			origins = append(origins, oracleRange{i, i + utf8.RuneLen(r)})
		}
	}
	return out.String(), origins
}

func oracleFragment(s string, continuation bool) bool {
	if strings.Contains(s, "-") && strings.Contains(s, "PRIVATEKEY") {
		return true
	}
	for i := 0; i < len(s); i++ {
		if s[i] != '-' && !(i == 0 && continuation) {
			continue
		}
		j := i
		for j < len(s) && s[j] == '-' {
			j++
		}
		r := s[j:]
		if r == "" {
			continue
		}
		for _, d := range []string{"BEGIN", "END"} {
			if strings.HasPrefix(r, d) && (j-i >= 5 || continuation || strings.Contains(r[len(d):], "PRI")) {
				return true
			}
			for _, l := range []string{"", "RSA", "EC", "DSA", "OPENSSH", "ENCRYPTED"} {
				h := d + l + "PRIVATEKEY"
				if strings.HasPrefix(h, r) || strings.HasPrefix(r, h) || strings.HasPrefix(r, d+l+"PRI") {
					return true
				}
			}
		}
	}
	return false
}

func oraclePlain(s string) string {
	if !utf8.ValidString(s) || len(s) > 64<<20 {
		return "[REDACTED]"
	}
	n, origin := oracleNormalize(s)
	if oracleFragment(oracleBoundary.ReplaceAllString(n, ""), false) {
		return "[REDACTED]"
	}
	for _, line := range strings.Split(s, "\n") {
		v, _ := oracleNormalize(line)
		if oracleFragment(oracleBoundary.ReplaceAllString(v, ""), false) {
			return "[REDACTED]"
		}
	}
	open := -1
	label := ""
	var ranges []oracleRange
	for _, m := range oracleBoundary.FindAllStringSubmatchIndex(n, -1) {
		token := n[m[2]:m[3]]
		a, b := origin[m[0]].a, origin[m[1]-1].b
		if strings.HasPrefix(token, "BEGIN") {
			if open >= 0 {
				return "[REDACTED]"
			}
			open = 0
			if x := strings.LastIndexByte(s[:a], '\n'); x >= 0 {
				open = x + 1
			}
			label = strings.TrimPrefix(token, "BEGIN")
		} else {
			if open < 0 || label != strings.TrimPrefix(token, "END") {
				return "[REDACTED]"
			}
			end := len(s)
			if x := strings.IndexByte(s[b:], '\n'); x >= 0 {
				end = b + x
			}
			ranges = append(ranges, oracleRange{open, end})
			open = -1
		}
	}
	if open >= 0 {
		return "[REDACTED]"
	}
	// Separate literal declarations are the reference's filter specification.
	for _, pattern := range []string{`(?:ghp_|gho_|ghs_|github_pat_|sk-ant-|sk-|xox[a-z]-)[A-Za-z0-9_-]*`, `eyJ[A-Za-z0-9_-]*\.eyJ[A-Za-z0-9_-]*\.[A-Za-z0-9_-]*`, `(?im)^.*(?:password|secret|token)\s*=.*$`} {
		for _, m := range regexp.MustCompile(pattern).FindAllStringIndex(s, -1) {
			ranges = append(ranges, oracleRange{m[0], m[1]})
		}
	}
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].a < ranges[j].a })
	var union []oracleRange
	for _, r := range ranges {
		if len(union) > 0 && r.a <= union[len(union)-1].b {
			if r.b > union[len(union)-1].b {
				union[len(union)-1].b = r.b
			}
		} else {
			union = append(union, r)
		}
	}
	var out strings.Builder
	last := 0
	for _, r := range union {
		out.WriteString(s[last:r.a])
		out.WriteString("[REDACTED]")
		last = r.b
	}
	out.WriteString(s[last:])
	return out.String()
}

func proofBody(t testing.TB, recipe []byte) string {
	t.Helper()
	seed := sha256.Sum256(recipe)
	key := ed25519.NewKeyFromSeed(seed[:])
	der, e := x509.MarshalPKCS8PrivateKey(key)
	if e != nil {
		t.Fatal("KEY-ENCODE")
	}
	parsed, e := x509.ParsePKCS8PrivateKey(der)
	if e != nil {
		t.Fatal("KEY-PARSE")
	}
	p, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		t.Fatal("KEY-TYPE")
	}
	message := []byte("offline-redaction-control")
	if !ed25519.Verify(p.Public().(ed25519.PublicKey), message, ed25519.Sign(p, message)) {
		t.Fatal("KEY-SIGN")
	}
	return base64.StdEncoding.EncodeToString(der)
}

func proofBlock(body string) string {
	return "-----BEGIN PRIVATE KEY-----\n" + body + "\n-----END PRIVATE KEY-----"
}

func proofText(text string) []byte {
	b, _ := json.Marshal(text)
	return append(append([]byte(` { "type":"text", "sessionID":"proof-session", "part":{"type":"text","text":`), b...), []byte("}}\r\n")...)
}

func proofWire(p approvedSource) []byte {
	var b []byte
	for _, e := range p.events {
		b = append(b, e.wire...)
	}
	return b
}

func proofNoBody(t testing.TB, body string, outputs ...string) {
	t.Helper()
	for _, s := range outputs {
		if strings.Contains(s, body) {
			t.Fatal("P-NOKEY complete body")
		}
		if len(body) > 32 && (strings.Contains(s, body[:32]) || strings.Contains(s, body[len(body)-32:])) {
			t.Fatal("P-NOKEY protected piece")
		}
	}
}

func TestRedactionValidKeys(t *testing.T) {
	message := sha256.Sum256([]byte("offline-control"))
	for _, kind := range []string{"RSA", "EC"} {
		t.Run(kind, func(t *testing.T) {
			var key crypto.Signer
			var err error
			if kind == "RSA" {
				key, err = rsa.GenerateKey(rand.Reader, 2048)
			} else {
				key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			}
			if err != nil {
				t.Fatal("KEY-GENERATE")
			}
			der, err := x509.MarshalPKCS8PrivateKey(key)
			if err != nil {
				t.Fatal("KEY-ENCODE")
			}
			parsed, err := x509.ParsePKCS8PrivateKey(der)
			if err != nil {
				t.Fatal("KEY-PARSE")
			}
			sig, err := parsed.(crypto.Signer).Sign(rand.Reader, message[:], crypto.SHA256)
			if err != nil {
				t.Fatal("KEY-SIGN")
			}
			valid := false
			switch p := key.Public().(type) {
			case *rsa.PublicKey:
				valid = rsa.VerifyPKCS1v15(p, crypto.SHA256, message[:], sig) == nil
			case *ecdsa.PublicKey:
				valid = ecdsa.VerifyASN1(p, message[:], sig)
			}
			if !valid {
				t.Fatal("KEY-VERIFY")
			}
			encoded := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
			body := base64.StdEncoding.EncodeToString(der)
			proofNoBody(t, body, Redact(string(encoded)), string(proofWire(approveEvents(proofText(string(encoded)), false))))
		})
	}
}

func TestRedactionPlainOracle(t *testing.T) {
	body := proofBody(t, []byte("plain-oracle"))
	block := proofBlock(body)
	for _, text := range []string{
		"Public π\r\n" + block + "\r\npublic suffix",
		body + "\n-----END PRIVATE KEY-----",
		body + "\n" + block + "\n-----END PRIVATE KEY-----\n" + block,
		"prefix\n-----BEGIN PRIVATE KEY-----\n" + body,
		"-----BEGIN EC PRIVATE KEY-----\n" + body + "\n-----END RSA PRIVATE KEY-----\n" + block,
		"-----BEGIN PRIVATE KEY-----\n" + block + "\n-----END PRIVATE KEY-----",
		"ordinary Markdown\n---\npublic conclusion",
		"----- b e g i n RSA private key -----\n" + body + "\n----- end RSA private key -----",
	} {
		if Redact(text) != oraclePlain(text) {
			t.Fatal("P-OUTSIDE plain differential")
		}
		if strings.Contains(text, body) {
			proofNoBody(t, body, Redact(text))
		}
	}
	if Redact(body+"\n"+block+"\n-----END PRIVATE KEY-----") != "[REDACTED]" {
		t.Fatal("PM01 dominance")
	}
}

func TestRedactionScopesAndOrigins(t *testing.T) {
	body := proofBody(t, []byte("scope-origin"))
	prior := proofText("public π and " + `\u literal`)
	for _, name := range []string{"F1", "AF03", "AF06", "FS01", "JP01", "JP02", "JP03", "JP04", "JP05", "JP06"} {
		t.Run(name, func(t *testing.T) {
			var event []byte
			b, _ := json.Marshal(body)
			switch name {
			case "F1":
				event = proofText(body + "\n-----END PRIVATE KEY-----\n" + proofBlock(body))
			case "AF03":
				event = []byte(`{"type":"assistant","ending":"---","message":{"content":[{"type":"text","text":` + string(b) + `}]},"orphan":"-----END PRIVATE KEY-----"}` + "\n")
			case "AF06":
				event = []byte(`{"type":"text","part":{"type":"text","text":` + string(b) + `},"metadata":"-----BEGIN PRIVATE KEY-----\nbody\n-----END PRIVATE KEY-----","orphan":"-----END PRIVATE KEY-----"}` + "\n")
			case "FS01":
				event = []byte(`{"type":"text","part":{"type":"text","text":` + string(b) + `},"other":"-----END PRIVATE KEY-----"}` + "\n")
			case "JP01":
				event = []byte(`{"type":"text","part/text":"-----BEGIN PRIVATE KEY-----","part":{"type":"text","text":` + string(b) + `}}` + "\n")
			case "JP02":
				event = []byte(`{"type":"assistant","message/content[0]/text":"-----BEGIN PRIVATE KEY-----","message":{"content":[{"type":"text","text":` + string(b) + `}]}}` + "\n")
			case "JP03":
				event = []byte(`{"type":"text","part":{"type":"text","text":` + string(b) + `},"\u002d\u002d\u002d\u002d\u002dEND PRIVATE KEY-----":"other"}` + "\n")
			case "JP04":
				event = []byte(`{"type":"text","part":{"type":"text","text":` + string(b) + `,"text":"public"}}` + "\n")
			case "JP05":
				event = []byte(`{"type":"text","part":{"type":"text","text":"\ud800"}}` + "\n")
			case "JP06":
				event = []byte(`{"type":"text","part":{"type":"text","text":"-----BEGIN PRIVATE KEY-----","array":[` + string(b) + `,"-----END PRIVATE KEY-----"]}}` + "\n")
			}
			input := append(bytes.Clone(prior), event...)
			p := approveEvents(input, false)
			if len(p.events) != 2 || !bytes.Equal(p.events[0].wire, prior) || !p.events[1].hidden || p.frontier != len(prior) {
				t.Fatal("P-DOMINANCE scope oracle")
			}
			proofNoBody(t, body, string(proofWire(p)))
		})
	}
	// Exact splicing preserves escape spelling around the complete hidden union.
	original := []byte(`{"type":"text","sessionID":"proof-session","part":{"type":"text","text":"\u03c0\r\n-----BEGIN PRIVATE KEY-----\nBODY\n-----END PRIVATE KEY-----\n\ud83d\ude00"}, "public":"\u0061"}` + "\n")
	want := bytes.Replace(original, []byte(`-----BEGIN PRIVATE KEY-----\nBODY\n-----END PRIVATE KEY-----`), []byte("[REDACTED]"), 1)
	p := approveEvents(original, false)
	if !bytes.Equal(proofWire(p), want) {
		t.Fatal("P-OUTSIDE escaped wire")
	}
	escapedMarkers := bytes.ReplaceAll(original, []byte("-"), []byte(`\u002d`))
	if !bytes.Equal(proofWire(approveEvents(escapedMarkers, false)), bytes.ReplaceAll(want, []byte("-"), []byte(`\u002d`))) {
		t.Fatal("P-OUTSIDE complete escape units")
	}
	multibyte := []byte(`{"type":"text","part":{"type":"text","text":"token=π"}}` + "\n")
	compareReference(t, multibyte, false)
	// Labels in real ordered content text may close; arbitrary arrays may not.
	ordered := []byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"before\n-----BEGIN PRIVATE KEY-----\nBODY"},{"type":"text","text":"-----END PRIVATE KEY-----\nafter"}]}}` + "\n")
	p = approveEvents(ordered, false)
	if len(p.events) != 1 || p.events[0].hidden || bytes.Contains(proofWire(p), []byte("BODY")) || !bytes.Contains(proofWire(p), []byte("before")) || !bytes.Contains(proofWire(p), []byte("after")) {
		t.Fatal("P-CHANNEL ordered positive")
	}
}

func proofPendingStream(body string, variant byte) []byte {
	var stream []byte
	stream = append(stream, proofText("earlier approved public")...)
	stream = append(stream, proofText(proofBlock(body)+"\npublic\n---")...)
	if variant&1 == 0 {
		stream = append(stream, proofText("BEGIN PRIVATE KEY")...)
	} else {
		stream = append(stream, []byte(`{"type":"tool_use","part":{"type":"tool","metadata":"BEGIN PRIVATE KEY"}}`+"\n")...)
	}
	stream = append(stream, proofText(body)...)
	if variant&2 == 0 {
		stream = append(stream, proofText("-----END PRIVATE KEY-----")...)
	} else {
		stream = append(stream, []byte(`{"type":"step_finish","part":{"type":"step-finish","reason":"stop","ending":"-----END PRIVATE KEY-----"}}`+"\n")...)
	}
	return stream
}

func TestRedactionPollingAndCache(t *testing.T) {
	body := proofBody(t, []byte("pending"))
	for variant := byte(0); variant < 4; variant++ {
		stream := proofPendingStream(body, variant)
		prior := proofText("earlier approved public")
		m := &Manager{}
		for cut := 0; cut <= len(stream); cut++ {
			prefix := stream[:cut]
			p := approveEvents(prefix, false)
			proofNoBody(t, body, string(proofWire(p)))
			if cut >= len(prior) && !bytes.Equal(p.events[0].wire, prior) {
				t.Fatal("CP01 earlier exact")
			}
			w := m.eventWindow("source", prefix, false)
			for _, proof := range w.lexical {
				proofNoBody(t, body, string(proof.wire))
			}
			proofNoBody(t, body, w.final, w.terminalFinal, strings.Join(w.tail, "\n"), w.latest.Summary)
			cold := (&Manager{}).eventWindow("source", prefix, false)
			if w.final != cold.final || w.terminalFinal != cold.terminalFinal || strings.Join(w.tail, "\n") != strings.Join(cold.tail, "\n") {
				t.Fatal("P-SNAPSHOT warm/cold")
			}
		}
		// Same size rewrite and eviction do not use previous raw-body line state.
		for i := 0; i < 70; i++ {
			m.eventWindow(fmt.Sprint(i), proofText(body), false)
		}
		proofNoBody(t, body, m.eventWindow("source", stream, false).final)
	}
	plain := proofText("literal separator ---")
	if !approveEvents(plain, false).events[0].hidden || approveEvents(plain, true).events[0].hidden {
		t.Fatal("P-PUBLISH true EOF control")
	}
	// Before the opener, marker-free complete events are outside the promise.
	prior := proofText(body)
	p := approveEvents(append(bytes.Clone(prior), proofText("-----END PRIVATE KEY-----")...), false)
	if !bytes.Equal(p.events[0].wire, prior) || !p.events[1].hidden {
		t.Fatal("FS01 earlier-event exception")
	}
}

func TestRedactionBounds(t *testing.T) {
	for _, count := range []int{8191, 8192, 8193} {
		input := strings.Repeat("sk-fixture ", count)
		output := Redact(input)
		if (output == redactionHidden) != (count > 8192) {
			t.Fatal("P-BOUNDS spans")
		}
	}
	for _, n := range []int{(1 << 20) - 1, 1 << 20, (1 << 20) + 1} {
		base := proofText("")
		line := proofText(strings.Repeat("x", max(0, n-len(base))))
		p := approveEvents(line, false)
		if len(p.events) != 1 || p.events[0].hidden != (len(line) > 1<<20) {
			t.Fatal("P-BOUNDS physical")
		}
	}
	for _, depth := range []int{63, 64, 65} {
		line := []byte(strings.Repeat("[", depth) + "0" + strings.Repeat("]", depth) + "\n")
		p := approveEvents(line, false)
		if p.events[0].hidden != (depth > 64) {
			t.Fatal("P-BOUNDS depth")
		}
	}
	for _, count := range []int{8191, 8192, 8193} {
		line := []byte("[" + strings.TrimSuffix(strings.Repeat(`"public",`, count), ",") + "]\n")
		if approveEvents(line, false).events[0].hidden != (count > 8192) {
			t.Fatal("P-BOUNDS scalars")
		}
		primitive := []byte("[" + strings.TrimSuffix(strings.Repeat("0,", count), ",") + "]\n")
		if approveEvents(primitive, false).events[0].hidden != (count > 8192) {
			t.Fatal("P-BOUNDS all scalar descriptors")
		}
		compareReference(t, primitive, false)
	}
	stream := append(proofText("---"), proofText(strings.Repeat("x", 1<<19))...)
	stream = append(stream, proofText(strings.Repeat("x", 1<<19))...)
	stream = append(stream, proofText("later public")...)
	if !approveEvents(stream, true).events[0].hidden {
		t.Fatal("P-BOUNDS held range cannot release")
	}
}

func TestRedactionProtectedMemberOrigin(t *testing.T) {
	body := proofBody(t, []byte("member-origin"))
	key, _ := json.Marshal(body)
	opener := proofText("-----BEGIN PRIVATE KEY-----")
	closing := []byte(`{"type":"text","sessionID":"proof-session",` + string(key) + `:"inside","part":{"type":"text","text":"-----END PRIVATE KEY-----\npublic"}}` + "\n")
	input := append(opener, closing...)
	proofNoBody(t, body, string(proofWire(approveEvents(input, false))))
	compareReference(t, input, false)
}

func TestRedactionOpenMetadataType(t *testing.T) {
	body := proofBody(t, []byte("OPEN-metadata-type"))
	encoded, _ := json.Marshal(body)
	opener := proofText("-----BEGIN PRIVATE KEY-----")
	closing := []byte(`{"type":"text","sessionID":"proof-session","metadata":{"type":` + string(encoded) + `},"part":{"type":"text","text":"-----END PRIVATE KEY-----\npublic-after"}}` + "\n")
	p := approveEvents(append(opener, closing...), false)
	proofNoBody(t, body, string(proofWire(p)))
	if !bytes.Contains(proofWire(p), []byte("public-after")) {
		t.Fatal("STRUCTURAL public suffix")
	}
	compareReference(t, append(bytes.Clone(opener), closing...), false)
}

func TestRedactionOpenOpaqueScalar(t *testing.T) {
	for _, value := range []string{"1234567890", "true", "null"} {
		wire := append(proofText("-----BEGIN PRIVATE KEY-----"), []byte(`{"type":"text","sessionID":"proof-session","metadata":`+value+`,"part":{"type":"text","text":"-----END PRIVATE KEY-----\npublic-after"}}`+"\n")...)
		p := approveEvents(wire, false)
		for _, e := range p.events {
			if !e.hidden {
				t.Fatal("OPAQUE scalar private scope")
			}
		}
		compareReference(t, wire, false)
	}
}

func TestIndependentReviewNestedArguments(t *testing.T) {
	_, body := hotfixReviewKey(t)
	inner := `{"command":"` + body + `","metadata":"\u002d\u002d\u002d\u002d\u002dEND PRIVATE KEY\u002d\u002d\u002d\u002d\u002d"}`
	encoded, err := json.Marshal(inner)
	if err != nil {
		t.Fatal("NESTED setup=false")
	}
	wire := []byte(`{"type":"item.completed","item":{"type":"mcp_tool_call","server":"fixture","tool":"fixture","arguments":` + string(encoded) + `}}` + "\n")
	projection := approveEvents(wire, false)
	if len(projection.events) != 1 || !projection.events[0].hidden {
		t.Error("NESTED semantic-scope-hidden=false")
	}
	w := (&Manager{}).eventWindow("fixture", wire, false)
	if strings.Contains(w.latest.Summary, body) || strings.Contains(strings.Join(w.tail, "\n"), body) {
		t.Error("NESTED cache-no-body=false")
	}
	hotfixReviewSinks(t, wire, "", body)
	var item map[string]any
	if json.Unmarshal([]byte(`{"type":"mcp_tool_call","server":"fixture","tool":"fixture","arguments":`+string(encoded)+`}`), &item) != nil {
		t.Fatal("NESTED standalone-setup=false")
	}
	if strings.Contains(actionSummary(item), body) {
		t.Error("NESTED standalone-no-body=false")
	}
}

func TestRedactionNestedArgumentControls(t *testing.T) {
	prior := proofText("earlier approved public")
	object := []byte(`{"type":"item.completed","item":{"type":"mcp_tool_call","server":"fixture","tool":"fixture","arguments":{"command":"inspect fixture"}}}` + "\n")
	if p := approveEvents(object, false); len(p.events) != 1 || p.events[0].hidden || !bytes.Equal(p.events[0].wire, object) {
		t.Fatal("NESTED object-exact=false")
	}
	if got := (&Manager{}).eventWindow("fixture", object, false).latest.Summary; got != "fixture.fixture inspect fixture" {
		t.Fatal("NESTED object-summary=false")
	}
	for _, raw := range []string{`{"command":"inspect fixture"}`, `{"command":`, `{"command":"a","\u0063ommand":"b"}`, "", `null`, `123`} {
		encoded, _ := json.Marshal(raw)
		wire := []byte(`{"type":"item.completed","item":{"type":"mcp_tool_call","arguments":` + string(encoded) + `}}` + "\n")
		stream := append(append(bytes.Clone(prior), wire...), object...)
		p := approveEvents(stream, false)
		if len(p.events) != 3 || !bytes.Equal(p.events[0].wire, prior) || !p.events[1].hidden || !p.events[2].hidden {
			t.Fatal("NESTED conservative-scope=false")
		}
	}
	for _, raw := range []string{`{"command":`, `{"command":"a","\u0063ommand":"b"}`, "", `null`, `123`} {
		if args := approvedSourceArguments(raw); len(args) != 0 {
			t.Fatal("NESTED invalid-arguments=false")
		}
	}
}

func TestIndependentReviewPendingEOFAndReplay(t *testing.T) {
	_, body := hotfixReviewKey(t)
	prior := proofText("earlier approved public")
	stream := append(bytes.Clone(prior), proofText(proofBlock(body)+"\n---")...)
	stream = append(stream, proofText(body)...)
	manager := &Manager{}
	// Prime identical marker-free lines before introducing their private context.
	manager.eventWindow("fixture", proofText(body), false)
	for _, input := range [][]byte{stream, append(bytes.Clone(stream), proofText("BEGIN PRIVATE KEY")...), prior, stream} {
		p := approveEvents(input, false)
		if !bytes.Equal(p.events[0].wire, prior) {
			t.Error("REPLAY earlier-exact=false")
		}
		warm := manager.eventWindow("fixture", input, false)
		cold := (&Manager{}).eventWindow("fixture", input, false)
		if warm.final != cold.final || strings.Join(warm.tail, "\n") != strings.Join(cold.tail, "\n") {
			t.Error("REPLAY warm-cold=false")
		}
		if strings.Contains(warm.final+strings.Join(warm.tail, "\n"), body) {
			t.Error("REPLAY no-body=false")
		}
	}
}

func TestIndependentReviewUnicodeAndEscapedKeys(t *testing.T) {
	_, body := hotfixReviewKey(t)
	encoded, _ := json.Marshal(body)
	for _, separator := range []string{"\u0085", "\u00a0", "\u2028", "\u2029", "\u3000"} {
		text := body + separator + "-----END" + separator + "PRIVATE" + separator + "KEY-----"
		if Redact(text) != redactionHidden {
			t.Error("UNICODE whole-scope=false")
		}
		wire := proofText(text)
		if !approveEvents(wire, false).events[0].hidden {
			t.Error("UNICODE event-hidden=false")
		}
	}
	wire := []byte(`{"type":"text","part":{"type":"text","text":` + string(encoded) + `},"metadata":{"\u002d\u002d\u002d\u002d\u002dEND PRIVATE KEY-----":"x"}}` + "\n")
	if !approveEvents(wire, false).events[0].hidden {
		t.Error("ESCAPED-KEY whole-event=false")
	}
	duplicate := []byte(`{"type":"text","\u0074ype":"text","part":{"type":"text","text":` + string(encoded) + `}}` + "\n")
	if !approveEvents(duplicate, false).events[0].hidden {
		t.Error("ESCAPED-KEY duplicate-hidden=false")
	}
}

func TestRedactionPublicSinks(t *testing.T) {
	body := proofBody(t, []byte("public-sinks"))
	m, c, store := manager(t)
	seedHistory(t, m, 1)
	r, err := m.latest("claude")
	if err != nil || r == nil {
		t.Fatal("SINK-FIXTURE")
	}
	path := filepath.Join(store, "runs", r.ID, "events.jsonl")
	text := body + "\n" + proofBlock(body) + "\n-----END PRIVATE KEY-----"
	if os.WriteFile(filepath.Join(c.ReportsDir, r.GoalID+".txt"), []byte(text), 0600) != nil {
		t.Fatal("REPORT-WRITE")
	}
	report, err := m.Report(r.GoalID)
	if err != nil || report != "[REDACTED]" {
		t.Fatal("REPORT-ENTRANCE")
	}
	for _, variant := range []byte{0, 1, 2, 3} {
		if os.WriteFile(path, proofPendingStream(body, variant), 0600) != nil {
			t.Fatal("EVENT-WRITE")
		}
		rows, err := m.Tail("claude", 50)
		if err != nil || !strings.Contains(strings.Join(rows, "\n"), "earlier approved public") {
			t.Fatal("TAIL-ENTRANCE")
		}
		w, err := NewWeb(m, "127.0.0.1:0")
		if err != nil {
			t.Fatal("WEB-FIXTURE")
		}
		req := httptest.NewRequest("GET", "http://127.0.0.1:0/data", nil)
		req.Header.Set("X-Nuntius-Token", w.token)
		out := httptest.NewRecorder()
		w.ServeHTTP(out, req)
		if out.Code != 200 {
			t.Fatal("DATA-ENTRANCE")
		}
		args, _ := json.Marshal(map[string]any{"lane": "claude", "lines": 50})
		response, err := call(m, "nuntius_tail", args)
		if err != nil {
			t.Fatal("MCP-ENTRANCE")
		}
		proofNoBody(t, body, report, strings.Join(rows, "\n"), out.Body.String(), toolText(response), string(safeJSON(response)))
		for _, cache := range m.eventWindows {
			proofNoBody(t, body, cache.final, cache.terminalFinal, cache.latest.Summary, cache.command.Summary, strings.Join(cache.tail, "\n"))
			for _, line := range cache.lines {
				proofNoBody(t, body, line.final)
				for _, a := range line.actions {
					proofNoBody(t, body, a.event.Summary, a.event.Text)
				}
			}
			for _, proof := range cache.lexical {
				proofNoBody(t, body, string(proof.wire))
			}
		}
	}
	// Original decoded map names are part of the MCP output boundary.
	proofNoBody(t, body, string(safeJSON(map[string]any{body + "\n-----END PRIVATE KEY-----": "value"})))
	// A clip cannot seal a segment. The orphan outside the report limit matters.
	if os.WriteFile(filepath.Join(c.ReportsDir, r.GoalID+".txt"), []byte(strings.Repeat("public", 11000)+"\n-----END PRIVATE KEY-----"), 0600) != nil {
		t.Fatal("REPORT-BOUND-WRITE")
	}
	if report, err := m.Report(r.GoalID); err != nil || report != "[REDACTED]" {
		t.Fatal("P-BOUNDS report clip")
	}
}

func TestRedactionTerminalCP02(t *testing.T) {
	body := proofBody(t, []byte("terminal-CP02"))
	c, store := opencodeFixture(t)
	m, err := NewManager(c, store)
	if err != nil {
		t.Fatal("TERMINAL-FIXTURE")
	}
	t.Cleanup(func() { _ = m.Close() })
	stream := terminalStart + terminalText(proofBlock(body)+"\npublic\n---") + `{"type":"tool_use","part":{"type":"tool","metadata":"BEGIN PRIVATE KEY"}}` + "\n" + terminalText(body) + strings.Replace(terminalStop, `"reason":"stop"`, `"reason":"stop","ending":"-----END PRIVATE KEY-----"`, 1)
	r := seedTerminalRun(t, m, stream, "ended")
	terminal, final := m.terminalTurn(r)
	if !terminal || final != "[REDACTED]" {
		t.Fatal("CP02 terminal reference")
	}
	rows, err := m.Tail(r.Lane, 50)
	if err != nil {
		t.Fatal("CP02 Tail entrance")
	}
	proofNoBody(t, body, final, strings.Join(rows, "\n"))
	for _, bad := range []string{strings.Replace(stream, `"reason":"stop"`, `"reason":"tool-calls"`, 1), stream + terminalText("post-stop"), strings.TrimSuffix(stream, "\n")} {
		if os.WriteFile(filepath.Join(store, "runs", r.ID, "events.jsonl"), []byte(bad), 0600) != nil {
			t.Fatal("LIFECYCLE-WRITE")
		}
		if ok, _ := m.terminalTurn(r); ok {
			t.Fatal("P-LIFECYCLE invalid stop")
		}
	}
}

func TestRedactionTerminalBalancedReference(t *testing.T) {
	body := proofBody(t, []byte("terminal-balanced"))
	c, store := opencodeFixture(t)
	m, err := NewManager(c, store)
	if err != nil {
		t.Fatal("BALANCED terminal fixture")
	}
	t.Cleanup(func() { _ = m.Close() })
	r := seedTerminalRun(t, m, terminalStart+terminalText("public\n"+proofBlock(body))+terminalStop, "ended")
	valid, final := m.terminalTurn(r)
	if !valid || final != "public\n[REDACTED]" {
		t.Fatal("BALANCED approved reference")
	}
	proofNoBody(t, body, final)
}

func fuzzNoise(recipe []byte) string {
	const alphabet = "abcXYZ012π \n\t-_[]/"
	runes := []rune(alphabet)
	var b strings.Builder
	for _, v := range recipe[:min(len(recipe), 128)] {
		b.WriteRune(runes[int(v)%len(runes)])
	}
	return b.String()
}

func FuzzRedactionBytes(f *testing.F) {
	for _, s := range []string{"F1", "PM01", "JP00", "TP00", "CP01", "CP02"} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, recipe []byte) {
		body := proofBody(t, recipe)
		noise := fuzzNoise(recipe)
		for _, text := range []string{noise + "\n" + proofBlock(body) + "\npublic", body + "\n" + proofBlock(body) + "\n-----END PRIVATE KEY-----", noise + "\n-----BE\n" + body} {
			if Redact(text) != oraclePlain(text) {
				t.Fatal("FUZZ-BYTES differential")
			}
			proofNoBody(t, body, Redact(text))
		}
		wire := proofText(noise + "\n" + proofBlock(body) + "\npublic")
		if len(recipe) > 0 {
			switch recipe[0] % 4 {
			case 0:
				wire = bytes.ReplaceAll(wire, []byte("-"), []byte(`\u002d`))
			case 1:
				wire = bytes.ReplaceAll(wire, []byte("PRIVATE"), []byte(`PRIV\u0041TE`))
			case 2:
				wire = append([]byte{0xff}, wire...)
			case 3:
				wire = bytes.Replace(wire, []byte(`"type":"text"`), []byte(`"type":"text","type":"text"`), 1)
			}
		}
		compareReference(t, wire, false)
		proofNoBody(t, body, string(proofWire(approveEvents(wire, false))))
	})
}

func FuzzRedactionEventTopology(f *testing.F) {
	for i := 0; i < 8; i++ {
		f.Add([]byte{byte(i)})
	}
	f.Fuzz(func(t *testing.T, recipe []byte) {
		body := proofBody(t, recipe)
		noise := fuzzNoise(recipe)
		prior := proofText("public sentinel")
		value, _ := json.Marshal(body + "\n-----END PRIVATE KEY-----")
		key := "meta"
		if len(recipe) > 0 && recipe[0]&1 != 0 {
			key = "part/text"
		}
		encoded, _ := json.Marshal(key)
		if len(recipe) > 0 && recipe[0]&4 != 0 {
			name := "type"
			if len(recipe) > 1 {
				name = []string{"type", "TYPE", "sessionID", "session_id", "thread_id", "reason"}[int(recipe[1])%6]
			}
			member, _ := json.Marshal(name)
			protected, _ := json.Marshal(body)
			closing := []byte(`{"type":"text","sessionID":"proof-session","metadata":{` + string(member) + `:` + string(protected) + `},"part":{"type":"text","text":"-----END PRIVATE KEY-----\npublic-after"}}` + "\n")
			wire := append(append(bytes.Clone(prior), proofText("-----BEGIN PRIVATE KEY-----")...), closing...)
			compareReference(t, wire, false)
			proofNoBody(t, body, string(proofWire(approveEvents(wire, false))))
			return
		}
		event := []byte(`{"type":"text","part":{"type":"text","text":` + string(value) + `},` + string(encoded) + `:"public"}` + "\n")
		if len(recipe) > 0 && recipe[0]&2 != 0 {
			event = proofText(proofBlock(body) + "\n" + noise + "\n-----END PRIVATE KEY-----")
		}
		p := approveEvents(append(bytes.Clone(prior), event...), false)
		compareReference(t, append(bytes.Clone(prior), event...), false)
		if len(p.events) != 2 || !bytes.Equal(p.events[0].wire, prior) || !p.events[1].hidden {
			t.Fatal("FUZZ-TOPOLOGY scope oracle")
		}
		proofNoBody(t, body, string(proofWire(p)))
	})
}

func FuzzRedactionPrefixes(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3})
	f.Add([]byte("CP02"))
	f.Fuzz(func(t *testing.T, recipe []byte) {
		body := proofBody(t, recipe)
		v := byte(0)
		if len(recipe) > 0 {
			v = recipe[0]
		}
		stream := proofPendingStream(body, v)
		cuts := []int{0, len(stream)}
		for _, b := range recipe[:min(len(recipe), 32)] {
			cuts = append(cuts, int(b)*len(stream)/255)
		}
		for _, cut := range cuts {
			compareReference(t, stream[:cut], false)
			proofNoBody(t, body, string(proofWire(approveEvents(stream[:cut], false))))
		}
	})
}

func FuzzRedactionCacheSchedule(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4})
	f.Add([]byte("W01-W10"))
	f.Fuzz(func(t *testing.T, recipe []byte) {
		body := proofBody(t, recipe)
		stream := proofPendingStream(body, 3)
		m := &Manager{}
		for _, op := range recipe[:min(len(recipe), 16)] {
			if op&1 != 0 {
				m.eventWindows = nil
			}
			prefix := stream[:int(op)*len(stream)/255]
			compareReference(t, prefix, false)
			w := m.eventWindow("source", prefix, false)
			cold := (&Manager{}).eventWindow("source", prefix, false)
			if w.final != cold.final || w.terminalFinal != cold.terminalFinal || strings.Join(w.tail, "\n") != strings.Join(cold.tail, "\n") {
				t.Fatal("FUZZ-CACHE differential")
			}
			proofNoBody(t, body, w.final, w.terminalFinal, strings.Join(w.tail, "\n"))
		}
	})
}

func FuzzRedactionPublicSinks(f *testing.F) {
	f.Add([]byte("report-data-MCP-Tail"))
	f.Add([]byte("CP01-CP02"))
	f.Fuzz(func(t *testing.T, recipe []byte) {
		body := proofBody(t, recipe)
		variant := byte(0)
		if len(recipe) > 0 {
			variant = recipe[0]
		}
		m, c, store := manager(t)
		seedHistory(t, m, 1)
		r, err := m.latest("claude")
		if err != nil || r == nil {
			t.Fatal("FUZZ-SINK fixture")
		}
		stream := proofPendingStream(body, variant)
		if os.WriteFile(filepath.Join(store, "runs", r.ID, "events.jsonl"), stream, 0600) != nil {
			t.Fatal("FUZZ-SINK event")
		}
		w := m.eventWindow(r.ID, stream, false)
		text := body + "\n" + proofBlock(body) + "\n-----END PRIVATE KEY-----"
		if os.WriteFile(filepath.Join(c.ReportsDir, r.GoalID+".txt"), []byte(text), 0600) != nil {
			t.Fatal("FUZZ-SINK report")
		}
		report, err := m.Report(r.GoalID)
		if err != nil || report != redactionHidden {
			t.Fatal("FUZZ-SINK report entrance")
		}
		rows, err := m.Tail("claude", 50)
		if err != nil || !strings.Contains(strings.Join(rows, "\n"), "earlier approved public") {
			t.Fatal("FUZZ-SINK Tail entrance")
		}
		web, err := NewWeb(m, "127.0.0.1:0")
		if err != nil {
			t.Fatal("FUZZ-SINK web")
		}
		req := httptest.NewRequest("GET", "http://127.0.0.1:0/data", nil)
		req.Header.Set("X-Nuntius-Token", web.token)
		out := httptest.NewRecorder()
		web.ServeHTTP(out, req)
		if out.Code != 200 {
			t.Fatal("FUZZ-SINK data entrance")
		}
		args, _ := json.Marshal(map[string]any{"lane": "claude", "lines": 50})
		response, err := call(m, "nuntius_tail", args)
		if err != nil {
			t.Fatal("FUZZ-SINK MCP entrance")
		}
		value := map[string]any{"report": Redact(text), "tail": strings.Join(w.tail, "\n"), "final": w.final}
		proofNoBody(t, body, report, strings.Join(rows, "\n"), out.Body.String(), toolText(response), string(safeJSON(response)), toolText(value), string(safeJSON(value)), w.terminalFinal)
	})
}
