package nuntius

import (
	"bytes"
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

type referenceStep struct {
	name    string
	element bool
	ordinal int
}
type referenceString struct {
	text      string
	key       bool
	path      []referenceStep
	a, b      int
	units     []oracleRange
	primitive bool
}
type referenceEvent struct {
	raw     []byte
	strings []referenceString
	spans   []oracleRange
	hidden  bool
	a, b    int
}
type referenceMarker struct {
	a, b             int
	direction, label string
}

// The library decoder supplies the reference's structural walk; an independent
// escaped-string lexer supplies source units. No production parsing is reused.
func referenceDecode(raw []byte) ([]referenceString, bool) {
	if len(raw) > 1<<20 || !utf8.Valid(raw) || !json.Valid(raw) {
		return nil, false
	}
	var tokens []referenceString
	lex := regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
	for _, m := range lex.FindAllIndex(raw, -1) {
		var text string
		if json.Unmarshal(raw[m[0]:m[1]], &text) != nil {
			return nil, false
		}
		var units []oracleRange
		for i := m[0] + 1; i < m[1]-1; {
			a := i
			var part string
			if raw[i] == '\\' {
				i += 2
				if raw[a+1] == 'u' {
					i = a + 6
					if i > m[1]-1 {
						return nil, false
					}
					unit, parseErr := strconv.ParseUint(string(raw[a+2:i]), 16, 16)
					if parseErr != nil {
						return nil, false
					}
					if unit >= 0xdc00 && unit <= 0xdfff {
						return nil, false
					}
					if unit >= 0xd800 && unit <= 0xdbff {
						// A surrogate is valid only as an adjacent high/low pair.
						if i+6 > m[1]-1 || raw[i] != '\\' || raw[i+1] != 'u' {
							return nil, false
						}
						low, err := strconv.ParseUint(string(raw[i+2:i+6]), 16, 16)
						if err != nil || low < 0xdc00 || low > 0xdfff {
							return nil, false
						}
						i += 6
					}
				}
				if json.Unmarshal(append(append([]byte{'"'}, raw[a:i]...), '"'), &part) != nil {
					return nil, false
				}
			} else {
				_, n := utf8.DecodeRune(raw[i:])
				i += n
				part = string(raw[a:i])
			}
			for range len(part) {
				units = append(units, oracleRange{a, i})
			}
		}
		if len(tokens) >= 8192 {
			return nil, false
		}
		tokens = append(tokens, referenceString{text: text, a: m[0], b: m[1], units: units})
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	next := 0
	var ordered []referenceString
	var visit func([]referenceStep, int) bool
	visit = func(path []referenceStep, depth int) bool {
		if depth > 64 {
			return false
		}
		begin := int(decoder.InputOffset())
		token, err := decoder.Token()
		if err != nil {
			return false
		}
		switch v := token.(type) {
		case string:
			if next >= len(tokens) {
				return false
			}
			tokens[next].path = append([]referenceStep(nil), path...)
			ordered = append(ordered, tokens[next])
			next++
		case json.Delim:
			if v == '{' {
				seen := map[string]bool{}
				for decoder.More() {
					t, err := decoder.Token()
					if err != nil {
						return false
					}
					name, ok := t.(string)
					if !ok || seen[name] || next >= len(tokens) {
						return false
					}
					seen[name] = true
					tokens[next].key = true
					tokens[next].path = append([]referenceStep(nil), path...)
					ordered = append(ordered, tokens[next])
					next++
					child := append(append([]referenceStep(nil), path...), referenceStep{name: name})
					if !visit(child, depth+1) {
						return false
					}
				}
			} else if v == '[' {
				for index := 0; decoder.More(); index++ {
					child := append(append([]referenceStep(nil), path...), referenceStep{element: true, ordinal: index})
					if !visit(child, depth+1) {
						return false
					}
				}
			} else {
				return false
			}
			if _, err := decoder.Token(); err != nil {
				return false
			}
		default:
			end := int(decoder.InputOffset())
			for begin < end && bytes.ContainsRune([]byte(" ,:\r\n\t"), rune(raw[begin])) {
				begin++
			}
			ordered = append(ordered, referenceString{primitive: true, path: append([]referenceStep(nil), path...), a: begin, b: end})
		}
		return len(ordered) <= 8192
	}
	if !visit(nil, 0) || next != len(tokens) {
		return nil, false
	}
	return ordered, true
}

func referenceAt(tokens []referenceString, names ...string) string {
	for _, s := range tokens {
		if s.key || len(s.path) != len(names) {
			continue
		}
		ok := true
		for i, n := range names {
			ok = ok && !s.path[i].element && s.path[i].name == n
		}
		if ok {
			return s.text
		}
	}
	return ""
}

func referenceRole(s referenceString, tokens []referenceString) string {
	if s.key {
		return ""
	}
	is := func(i int, n string) bool { return len(s.path) > i && !s.path[i].element && s.path[i].name == n }
	kind := referenceAt(tokens, "type")
	if kind == "text" && len(s.path) == 2 && is(0, "part") && is(1, "text") && referenceAt(tokens, "part", "type") == "text" {
		return "O"
	}
	if (kind == "item.completed" || kind == "item.started" || kind == "item.updated") && len(s.path) == 2 && is(0, "item") && is(1, "text") && referenceAt(tokens, "item", "type") == "agent_message" {
		return "C"
	}
	if kind == "assistant" && len(s.path) == 4 && is(0, "message") && is(1, "content") && s.path[2].element && is(3, "text") {
		for _, t := range tokens {
			if !t.key && len(t.path) == 4 && t.path[0] == s.path[0] && t.path[1] == s.path[1] && t.path[2] == s.path[2] && t.path[3].name == "type" && !t.path[3].element && t.text == "text" {
				return "A"
			}
		}
	}
	return ""
}

func referenceMarkers(s string) ([]referenceMarker, bool, bool, bool) {
	n, units := oracleNormalize(s)
	var markers []referenceMarker
	for _, m := range oracleBoundary.FindAllStringSubmatchIndex(n, -1) {
		word := n[m[2]:m[3]]
		d := "END"
		if strings.HasPrefix(word, "BEGIN") {
			d = "BEGIN"
		}
		markers = append(markers, referenceMarker{units[m[0]].a, units[m[1]-1].b, d, strings.TrimPrefix(word, d)})
	}
	rest := oracleBoundary.ReplaceAllString(n, "")
	bad := oracleFragment(rest, false) || len(markers) > 8192
	for _, line := range strings.Split(s, "\n") {
		v, _ := oracleNormalize(line)
		bad = bad || oracleFragment(oracleBoundary.ReplaceAllString(v, ""), false)
	}
	pending := strings.HasSuffix(n, "-")
	if len(markers) > 0 && markers[len(markers)-1].b == len(strings.TrimRightFunc(s, unicode.IsSpace)) {
		pending = false
	}
	return markers, bad, pending, oracleFragment(n, true)
}

func referenceFixedControl(s referenceString, tokens []referenceString) bool {
	if s.key {
		return false
	}
	kind := referenceAt(tokens, "type")
	path := s.path
	member := func(i int, n string) bool { return i < len(path) && !path[i].element && path[i].name == n }
	if len(path) == 1 && member(0, "type") {
		for _, v := range []string{"text", "assistant", "tool_use", "step_start", "step_finish", "item.started", "item.completed", "item.updated"} {
			if kind == v && s.text == v {
				return true
			}
		}
	}
	if len(path) == 2 && member(0, "part") && member(1, "type") {
		expected := map[string]string{"text": "text", "tool_use": "tool", "step_start": "step-start", "step_finish": "step-finish"}
		v, ok := expected[kind]
		return ok && s.text == v
	}
	if kind == "assistant" && len(path) == 4 && member(0, "message") && member(1, "content") && path[2].element && member(3, "type") {
		return s.text == "text" || s.text == "tool_use"
	}
	if len(path) == 2 && member(0, "item") && member(1, "type") && (kind == "item.started" || kind == "item.completed" || kind == "item.updated") {
		for _, v := range []string{"agent_message", "command_execution", "file_change", "mcp_tool_call", "request_user_input", "reasoning"} {
			if s.text == v {
				return true
			}
		}
	}
	return false
}

func referenceProjection(raw []byte, complete bool) ([]referenceEvent, int) {
	var events []referenceEvent
	if len(raw) > 64<<20 {
		return []referenceEvent{{hidden: true}}, 0
	}
	for offset := 0; offset < len(raw); {
		n := bytes.IndexByte(raw[offset:], '\n')
		if n < 0 {
			events = append(events, referenceEvent{a: offset, b: len(raw), hidden: true})
			break
		}
		end := offset + n + 1
		line := raw[offset:end]
		values, ok := referenceDecode(line)
		events = append(events, referenceEvent{raw: line, strings: values, a: offset, b: end, hidden: !ok})
		offset = end
	}
	held := -1
	label, role, session := "", "", ""
	pending, latched := false, false
	frontier := 0
	for i := range events {
		e := &events[i]
		if latched {
			e.hidden = true
			continue
		}
		if len(e.raw) == 0 {
			if held < 0 {
				held = i
			}
			break
		}
		bad := e.hidden
		currentSession := referenceAt(e.strings, "sessionID")
		if currentSession == "" {
			currentSession = referenceAt(e.strings, "session_id")
		}
		for _, s := range e.strings {
			markers, uncertain, dashes, continuation := referenceMarkers(s.text)
			currentRole := referenceRole(s, e.strings)
			bad = bad || uncertain || pending && continuation || len(markers) > 0 && currentRole == "" || label != "" && currentRole != "" && (currentRole != role || currentSession != session)
			var ranges []oracleRange
			for _, pattern := range []string{`(?:ghp_|gho_|ghs_|github_pat_|sk-ant-|sk-|xox[a-z]-)[A-Za-z0-9_-]*`, `eyJ[A-Za-z0-9_-]*\.eyJ[A-Za-z0-9_-]*\.[A-Za-z0-9_-]*`, `(?im)^.*(?:password|secret|token)\s*=.*$`} {
				for _, m := range regexp.MustCompile(pattern).FindAllStringIndex(s.text, -1) {
					ranges = append(ranges, oracleRange{m[0], m[1]})
				}
			}
			body := 0
			inside := label != ""
			if inside && s.primitive {
				bad = true
			}
			if inside && s.key {
				switch s.text {
				case "type", "part", "text", "message", "content", "item", "sessionID", "session_id":
				default:
					ranges = append(ranges, oracleRange{0, len(s.text)})
				}
			}
			for _, m := range markers {
				if m.direction == "BEGIN" {
					if label != "" {
						bad = true
						continue
					}
					if held < 0 {
						held = i
					}
					label, role, session = m.label, currentRole, currentSession
					inside = true
					body = 0
					if pos := strings.LastIndexByte(s.text[:m.a], '\n'); pos >= 0 {
						body = pos + 1
					}
				} else {
					if label == "" || m.label != label || currentRole != role || currentSession != session {
						bad = true
						continue
					}
					end := len(s.text)
					if pos := strings.IndexByte(s.text[m.b:], '\n'); pos >= 0 {
						end = m.b + pos
					}
					ranges = append(ranges, oracleRange{body, end})
					label = ""
					inside = false
				}
			}
			if inside && !s.key && !referenceFixedControl(s, e.strings) {
				ranges = append(ranges, oracleRange{body, len(s.text)})
			}
			if dashes {
				if held < 0 {
					held = i
				}
				pending = true
			}
			for _, r := range ranges {
				if r.a < r.b && r.b <= len(s.units) {
					e.spans = append(e.spans, oracleRange{s.units[r.a].a, s.units[r.b-1].b})
				}
			}
		}
		if held >= 0 && i > held && (label != "" || pending) {
			e.hidden = true
		}
		if held >= 0 && e.b-events[held].a > 1<<20 {
			bad = true
		}
		if len(e.spans) > 8192 {
			bad = true
		}
		if bad {
			first := i
			if held >= 0 {
				first = held
			}
			for j := first; j <= i; j++ {
				events[j].hidden = true
			}
			latched = true
			held = -1
		} else if label == "" && !pending {
			held = -1
			frontier = e.b
		}
	}
	if held >= 0 {
		hide := !complete || label != "" || len(events) > 0 && len(events[len(events)-1].raw) == 0
		for i := held; i < len(events); i++ {
			events[i].hidden = hide
		}
		if !hide {
			frontier = len(raw)
		}
	}
	return events, frontier
}

func referenceWire(e referenceEvent) []byte {
	if e.hidden {
		return []byte("[REDACTED]")
	}
	sort.Slice(e.spans, func(i, j int) bool { return e.spans[i].a < e.spans[j].a })
	var ranges []oracleRange
	for _, r := range e.spans {
		if len(ranges) > 0 && r.a <= ranges[len(ranges)-1].b {
			if r.b > ranges[len(ranges)-1].b {
				ranges[len(ranges)-1].b = r.b
			}
		} else {
			ranges = append(ranges, r)
		}
	}
	var b bytes.Buffer
	last := 0
	for _, r := range ranges {
		b.Write(e.raw[last:r.a])
		b.WriteString("[REDACTED]")
		last = r.b
	}
	b.Write(e.raw[last:])
	return b.Bytes()
}

func compareReference(t testing.TB, wire []byte, complete bool) {
	t.Helper()
	want, frontier := referenceProjection(wire, complete)
	got := approveEvents(wire, complete)
	if len(want) != len(got.events) || frontier != got.frontier {
		t.Fatal("P-REFERENCE scopes/frontier")
	}
	for i, e := range want {
		if e.hidden != got.events[i].hidden || e.a != got.events[i].start || e.b != got.events[i].end || !bytes.Equal(referenceWire(e), got.events[i].wire) {
			t.Fatal("P-REFERENCE coordinates/output")
		}
	}
}

func TestRedactionEventReference(t *testing.T) {
	body := proofBody(t, []byte("reference"))
	for _, wire := range [][]byte{
		proofText("ordinary π"), proofText(proofBlock(body) + "\nafter"), proofText(body + "\n-----END PRIVATE KEY-----"),
		append(append(proofText("before\n-----BEGIN PRIVATE KEY-----"), proofText(body)...), proofText("-----END PRIVATE KEY-----\nafter")...),
		proofPendingStream(body, 0), proofPendingStream(body, 3), []byte(`{"a":"public","a":"duplicate"}` + "\n"),
		[]byte(`{"public":"\ufffd"}` + "\n"), []byte(`{"public":"\ud800\u0061"}` + "\n"),
		[]byte(`{"type":"text","part":{"type":"text","text":"\u03c0\n-----BEGIN PRIVATE KEY-----\nBODY\n-----END PRIVATE KEY-----\n\ud83d\ude00"}}` + "\n"),
	} {
		for _, complete := range []bool{false, true} {
			compareReference(t, wire, complete)
		}
		for cut := 0; cut <= len(wire); cut++ {
			compareReference(t, wire[:cut], false)
		}
	}
}
