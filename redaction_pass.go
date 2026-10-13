package nuntius

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"
)

const (
	redactionPolicy      = "source-pass-2"
	redactionHidden      = "[REDACTED]"
	redactionSourceLimit = 64 << 20
	redactionEventLimit  = 1 << 20
	redactionHeldLimit   = 1 << 20
	redactionScalarLimit = 8192
	redactionDepthLimit  = 64
)

// approvedSource contains only sealed wire, never undecided source bytes.
// Offsets are against the original snapshot, before formatting or clipping.
type approvedSource struct {
	events   []approvedEvent
	frontier int
	lexical  map[[32]byte]sourceLexicalProof
}

// Reusable lexical facts are exact approved complete bytes, not body hashes.
// They cannot authorize bytes under OPEN/PENDING/UNCERTAIN or omitted context.
type sourceLexicalProof struct {
	wire     []byte
	terminal bool
}

type approvedEvent struct {
	wire       []byte
	start, end int
	hidden     bool
	terminal   bool
}

type sourceSpan struct{ start, end int }
type sourcePathPart struct {
	member string
	index  int
	array  bool
}
type sourceScalar struct {
	text       string
	path       []sourcePathPart
	key        bool
	start, end int
	origins    []sourceSpan // one interval per decoded byte, including full escapes
	primitive  bool
}

// sourceJSON is a bounded wire-order parser. Maps cannot be privacy authority:
// they discard duplicate names and the order in which evidence arrived.
type sourceJSON struct {
	b       []byte
	i       int
	scalars []sourceScalar
	ok      bool
}

func (p *sourceJSON) space() {
	for p.i < len(p.b) && (p.b[p.i] == ' ' || p.b[p.i] == '\n' || p.b[p.i] == '\r' || p.b[p.i] == '\t') {
		p.i++
	}
}

func (p *sourceJSON) scalar(path []sourcePathPart, key bool) string {
	start := p.i
	p.i++
	var text strings.Builder
	var origins []sourceSpan
	for p.ok && p.i < len(p.b) && p.b[p.i] != '"' {
		a := p.i
		var unit string
		if p.b[p.i] == '\\' {
			p.i += 2
			if p.i > len(p.b) {
				p.ok = false
				break
			}
			if p.b[a+1] == 'u' {
				p.i = a + 6
				if p.i > len(p.b) {
					p.ok = false
					break
				}
				var r rune
				for _, c := range p.b[a+2 : p.i] {
					r <<= 4
					switch {
					case c >= '0' && c <= '9':
						r += rune(c - '0')
					case c >= 'a' && c <= 'f':
						r += rune(c - 'a' + 10)
					case c >= 'A' && c <= 'F':
						r += rune(c - 'A' + 10)
					default:
						p.ok = false
					}
				}
				if r >= 0xd800 && r <= 0xdbff {
					if p.i+6 > len(p.b) || p.b[p.i] != '\\' || p.b[p.i+1] != 'u' {
						p.ok = false
						break
					}
					var low rune
					for _, c := range p.b[p.i+2 : p.i+6] {
						low <<= 4
						switch {
						case c >= '0' && c <= '9':
							low += rune(c - '0')
						case c >= 'a' && c <= 'f':
							low += rune(c - 'a' + 10)
						case c >= 'A' && c <= 'F':
							low += rune(c - 'A' + 10)
						default:
							p.ok = false
						}
					}
					if low < 0xdc00 || low > 0xdfff {
						p.ok = false
						break
					}
					p.i += 6
				} else if r >= 0xdc00 && r <= 0xdfff {
					p.ok = false
					break
				}
			}
			if json.Unmarshal(append(append([]byte{'"'}, p.b[a:p.i]...), '"'), &unit) != nil {
				p.ok = false
				break
			}
		} else {
			r, n := utf8.DecodeRune(p.b[p.i:])
			if r == utf8.RuneError && n == 1 || r < 0x20 {
				p.ok = false
				break
			}
			p.i += n
			unit = string(p.b[a:p.i])
		}
		text.WriteString(unit)
		for range len(unit) {
			origins = append(origins, sourceSpan{a, p.i})
		}
	}
	if p.i >= len(p.b) || !p.ok {
		p.ok = false
		return ""
	}
	p.i++
	if len(p.scalars) >= redactionScalarLimit {
		p.ok = false
		return ""
	}
	s := text.String()
	p.scalars = append(p.scalars, sourceScalar{s, append([]sourcePathPart(nil), path...), key, start, p.i, origins, false})
	return s
}

func (p *sourceJSON) value(path []sourcePathPart, depth int) {
	p.space()
	if !p.ok || p.i >= len(p.b) || depth > redactionDepthLimit {
		p.ok = false
		return
	}
	switch p.b[p.i] {
	case '"':
		p.scalar(path, false)
	case '{':
		p.i++
		p.space()
		seen := map[string]bool{}
		if p.i < len(p.b) && p.b[p.i] == '}' {
			p.i++
			return
		}
		for p.ok {
			p.space()
			if p.i >= len(p.b) || p.b[p.i] != '"' {
				p.ok = false
				return
			}
			key := p.scalar(path, true)
			if seen[key] {
				p.ok = false
				return
			}
			seen[key] = true
			p.space()
			if p.i >= len(p.b) || p.b[p.i] != ':' {
				p.ok = false
				return
			}
			p.i++
			child := append(append([]sourcePathPart(nil), path...), sourcePathPart{member: key})
			p.value(child, depth+1)
			p.space()
			if p.i >= len(p.b) {
				p.ok = false
				return
			}
			if p.b[p.i] == '}' {
				p.i++
				return
			}
			if p.b[p.i] != ',' {
				p.ok = false
				return
			}
			p.i++
		}
	case '[':
		p.i++
		p.space()
		if p.i < len(p.b) && p.b[p.i] == ']' {
			p.i++
			return
		}
		for index := 0; p.ok; index++ {
			child := append(append([]sourcePathPart(nil), path...), sourcePathPart{index: index, array: true})
			p.value(child, depth+1)
			p.space()
			if p.i >= len(p.b) {
				p.ok = false
				return
			}
			if p.b[p.i] == ']' {
				p.i++
				return
			}
			if p.b[p.i] != ',' {
				p.ok = false
				return
			}
			p.i++
		}
	default:
		start := p.i
		for p.i < len(p.b) && !bytes.ContainsRune([]byte(" ,]}\r\n\t"), rune(p.b[p.i])) {
			p.i++
		}
		if start == p.i || !json.Valid(p.b[start:p.i]) {
			p.ok = false
		}
		if len(p.scalars) >= redactionScalarLimit {
			p.ok = false
			return
		}
		p.scalars = append(p.scalars, sourceScalar{path: append([]sourcePathPart(nil), path...), start: start, end: p.i, primitive: true})
	}
}

func sourceScalars(b []byte) ([]sourceScalar, bool) {
	if len(b) > redactionEventLimit || !utf8.Valid(b) || !json.Valid(b) {
		return nil, false
	}
	p := sourceJSON{b: b, ok: true}
	p.value(nil, 0)
	p.space()
	for _, scalar := range p.scalars {
		if scalar.start < 0 || scalar.end > len(b) || scalar.start >= scalar.end {
			p.ok = false
		}
	}
	return p.scalars, p.ok && p.i == len(b)
}

func scalarAt(sc []sourceScalar, members ...string) string {
	for _, s := range sc {
		if s.key || len(s.path) != len(members) {
			continue
		}
		match := true
		for i, m := range members {
			if s.path[i].array || s.path[i].member != m {
				match = false
			}
		}
		if match {
			return s.text
		}
	}
	return ""
}

// Only exact typed paths can establish a text role. Indexes remain in origins;
// an explicitly validated content array supplies its ordered text-stream role.
func scalarRole(s sourceScalar, sc []sourceScalar) string {
	if s.key {
		return ""
	}
	part := func(i int, m string) bool { return i < len(s.path) && !s.path[i].array && s.path[i].member == m }
	switch scalarAt(sc, "type") {
	case "text":
		if len(s.path) == 2 && part(0, "part") && part(1, "text") && scalarAt(sc, "part", "type") == "text" {
			return "opencode-text"
		}
	case "assistant":
		if len(s.path) == 4 && part(0, "message") && part(1, "content") && s.path[2].array && part(3, "text") {
			for _, t := range sc {
				if t.key || len(t.path) != 4 {
					continue
				}
				if t.path[0] == s.path[0] && t.path[1] == s.path[1] && t.path[2] == s.path[2] && t.path[3] == (sourcePathPart{member: "type"}) && t.text == "text" {
					return "claude-text"
				}
			}
		}
	case "item.completed", "item.started", "item.updated":
		if len(s.path) == 2 && part(0, "item") && part(1, "text") && scalarAt(sc, "item", "type") == "agent_message" {
			return "codex-text"
		}
	}
	return ""
}

type sourceMarker struct {
	start, end int
	label      string
	begin      bool
}

// A member called type is not structural authority. Only exact schema paths
// and fixed, validated payload literals can survive inside OPEN. Session and
// unrelated metadata values are display data here; raw lifecycle parsing is
// separate and still sees the original snapshot.
func sourceStructuralLiteral(s sourceScalar, sc []sourceScalar) bool {
	if s.key {
		return false
	}
	is := func(i int, name string) bool { return i < len(s.path) && !s.path[i].array && s.path[i].member == name }
	kind := scalarAt(sc, "type")
	if len(s.path) == 1 && is(0, "type") {
		switch kind {
		case "text", "assistant", "tool_use", "step_start", "step_finish", "item.started", "item.completed", "item.updated":
			return s.text == kind
		}
	}
	if len(s.path) == 2 && is(0, "part") && is(1, "type") {
		return kind == "text" && s.text == "text" || kind == "tool_use" && s.text == "tool" || kind == "step_start" && s.text == "step-start" || kind == "step_finish" && s.text == "step-finish"
	}
	if kind == "assistant" && len(s.path) == 4 && is(0, "message") && is(1, "content") && s.path[2].array && is(3, "type") {
		return s.text == "text" || s.text == "tool_use"
	}
	if (kind == "item.started" || kind == "item.completed" || kind == "item.updated") && len(s.path) == 2 && is(0, "item") && is(1, "type") {
		switch s.text {
		case "agent_message", "command_execution", "file_change", "mcp_tool_call", "request_user_input", "reasoning":
			return true
		}
	}
	return false
}

type sourceText struct {
	markers                          []sourceMarker
	uncertain, pending, continuation bool
}

func inspectSourceText(text string) sourceText {
	var normalized strings.Builder
	var origins []sourceSpan
	for start, r := range text {
		if unicode.IsSpace(r) {
			continue
		}
		n := string(unicode.ToUpper(r))
		normalized.WriteString(n)
		for range len(n) {
			origins = append(origins, sourceSpan{start, start + utf8.RuneLen(r)})
		}
	}
	normal := normalized.String()
	result := sourceText{pending: strings.HasSuffix(normal, "-")}
	remaining := normal
	for _, idx := range privateBoundary.FindAllStringIndex(normal, -1) {
		m := normal[idx[0]+5 : idx[1]-5]
		begin := strings.HasPrefix(m, "BEGIN")
		label := strings.TrimPrefix(m, "END")
		if begin {
			label = strings.TrimPrefix(m, "BEGIN")
		}
		valid := false
		for _, allowed := range []string{"PRIVATEKEY", "RSAPRIVATEKEY", "ECPRIVATEKEY", "DSAPRIVATEKEY", "OPENSSHPRIVATEKEY", "ENCRYPTEDPRIVATEKEY"} {
			valid = valid || label == allowed
		}
		if !valid || !begin && !strings.HasPrefix(m, "END") {
			result.uncertain = true
		}
		result.markers = append(result.markers, sourceMarker{origins[idx[0]].start, origins[idx[1]-1].end, label, begin})
	}
	remaining = privateBoundary.ReplaceAllString(remaining, "")
	if len(result.markers) > redactionScalarLimit {
		result.uncertain = true
	}
	result.uncertain = result.uncertain || privateFragment(remaining, false)
	for _, line := range strings.Split(text, "\n") {
		n := strings.Map(func(r rune) rune {
			if unicode.IsSpace(r) {
				return -1
			}
			return unicode.ToUpper(r)
		}, line)
		result.uncertain = result.uncertain || privateFragment(privateBoundary.ReplaceAllString(n, ""), false)
	}
	result.continuation = privateFragment(normal, true)
	// A closing delimiter belongs to its completed marker, not to pending.
	if len(result.markers) > 0 && result.markers[len(result.markers)-1].end == len(strings.TrimRightFunc(text, unicode.IsSpace)) {
		result.pending = false
	}
	return result
}

func sourceLineStart(s string, i int) int {
	if n := strings.LastIndexByte(s[:i], '\n'); n >= 0 {
		return n + 1
	}
	return 0
}
func sourceLineEnd(s string, i int) int {
	if n := strings.IndexByte(s[i:], '\n'); n >= 0 {
		return i + n
	}
	return len(s)
}

func sourceUnion(spans []sourceSpan) []sourceSpan {
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	var out []sourceSpan
	for _, s := range spans {
		if s.start >= s.end {
			continue
		}
		if len(out) > 0 && s.start <= out[len(out)-1].end {
			out[len(out)-1].end = max(out[len(out)-1].end, s.end)
		} else {
			out = append(out, s)
		}
	}
	return out
}

func ordinarySourceSpans(text string, terminal bool) []sourceSpan {
	var out []sourceSpan
	for _, pattern := range secretPatterns[1:] {
		for _, idx := range pattern.FindAllStringIndex(text, -1) {
			out = append(out, sourceSpan{idx[0], idx[1]})
		}
	}
	if terminal {
		for _, pattern := range terminalPrivateText {
			for _, idx := range pattern.FindAllStringIndex(text, -1) {
				out = append(out, sourceSpan{idx[0], idx[1]})
			}
		}
	}
	return out
}

func spliceSource(text string, spans []sourceSpan, replacement string) string {
	var b strings.Builder
	last := 0
	for _, span := range sourceUnion(spans) {
		b.WriteString(text[last:span.start])
		b.WriteString(replacement)
		last = span.end
	}
	b.WriteString(text[last:])
	return b.String()
}

func approvePlain(text string, terminal bool) string {
	if !utf8.ValidString(text) || len(text) > redactionSourceLimit {
		return redactionHidden
	}
	// The established private-marker grammar requires an ASCII dash. The
	// ordinary and terminal filters still inspect the complete original text.
	if !strings.Contains(text, "-") {
		spans := ordinarySourceSpans(text, terminal)
		if len(spans) > redactionScalarLimit {
			return redactionHidden
		}
		return spliceSource(text, spans, redactionHidden)
	}
	inspection := inspectSourceText(text)
	if inspection.uncertain {
		return redactionHidden
	}
	open := -1
	label := ""
	spans := ordinarySourceSpans(text, terminal)
	if len(spans) > redactionScalarLimit {
		return redactionHidden
	}
	for _, marker := range inspection.markers {
		if marker.begin {
			if open >= 0 {
				return redactionHidden
			}
			open = sourceLineStart(text, marker.start)
			label = marker.label
		} else {
			if open < 0 || label != marker.label {
				return redactionHidden
			}
			spans = append(spans, sourceSpan{open, sourceLineEnd(text, marker.end)})
			open = -1
		}
	}
	if open >= 0 {
		return redactionHidden
	}
	return spliceSource(text, spans, redactionHidden)
}

type tentativeEvent struct {
	wire       []byte
	start, end int
	spans      []sourceSpan
	whole      bool
	terminal   bool
	cacheSafe  bool
	provedWire []byte
}

// approveEvents is one transaction over one immutable original-byte snapshot.
// Holding is internal. Returned values have no raw or undecided getter. Warm
// reuse is legal only for the digest of this exact snapshot and policy.
func approveEvents(input []byte, complete bool) approvedSource {
	return approveEventsWithCache(input, complete, nil)
}

func approveEventsWithCache(input []byte, complete bool, previous map[[32]byte]sourceLexicalProof) approvedSource {
	var events []tentativeEvent
	held := -1
	openLabel, openRole, openSession := "", "", ""
	pending, uncertain := false, false
	frontier := 0
	if len(input) > redactionSourceLimit {
		return approvedSource{events: []approvedEvent{{wire: []byte(redactionHidden), hidden: true}}}
	}
	for cursor := 0; cursor < len(input); {
		n := bytes.IndexByte(input[cursor:], '\n')
		if n < 0 { // An unfinished physical event is held, never sealed by polling.
			if held < 0 {
				held = len(events)
			}
			events = append(events, tentativeEvent{start: cursor, end: len(input), whole: true})
			break
		}
		end := cursor + n + 1
		line := input[cursor:end]
		e := tentativeEvent{wire: line, start: cursor, end: end}
		index := len(events)
		if uncertain {
			e.whole = true
			events = append(events, e)
			cursor = end
			continue
		}
		if held < 0 && openLabel == "" && !pending {
			if proof, ok := previous[sha256.Sum256(line)]; ok && bytes.Equal(proof.wire, line) {
				e.provedWire, e.cacheSafe, e.terminal = proof.wire, true, proof.terminal
				events = append(events, e)
				frontier = end
				cursor = end
				continue
			}
		}
		scalars, valid := sourceScalars(line)
		e.terminal = valid && scalarAt(scalars, "type") == "text" && scalarAt(scalars, "part", "type") == "text"
		if !valid || sourceUnprovedMCPArguments(scalars) {
			uncertain = true
		}
		session := scalarAt(scalars, "sessionID")
		if session == "" {
			session = scalarAt(scalars, "session_id")
		}
		for _, s := range scalars {
			inspect := inspectSourceText(s.text)
			role := scalarRole(s, scalars)
			if openLabel != "" && role != "" && (role != openRole || session != openSession) {
				uncertain = true
			}
			if inspect.uncertain || pending && inspect.continuation || len(inspect.markers) > 0 && role == "" {
				uncertain = true
			}
			spans := ordinarySourceSpans(s.text, false)
			bodyStart := 0
			wasOpen := openLabel != ""
			// Opaque scalar data has no safe display mapping in a private range.
			// Hide the whole held scope rather than coerce a value while rendering.
			if wasOpen && s.primitive {
				uncertain = true
			}
			if wasOpen && s.key {
				// Structural names needed to decode an approved text envelope are
				// fixed literals. Other member names are display data while OPEN.
				switch s.text {
				case "type", "part", "text", "message", "content", "item", "sessionID", "session_id":
				default:
					spans = append(spans, sourceSpan{0, len(s.text)})
				}
			}
			for _, marker := range inspect.markers {
				if marker.begin {
					if openLabel != "" {
						uncertain = true
						continue
					}
					if held < 0 {
						held = index
					}
					openLabel, openRole, openSession = marker.label, role, session
					bodyStart = sourceLineStart(s.text, marker.start)
					wasOpen = true
				} else {
					if openLabel == "" || openLabel != marker.label || role != openRole || session != openSession {
						uncertain = true
						continue
					}
					spans = append(spans, sourceSpan{bodyStart, sourceLineEnd(s.text, marker.end)})
					openLabel = ""
					wasOpen = false
				}
			}
			if wasOpen && !s.key && !sourceStructuralLiteral(s, scalars) {
				spans = append(spans, sourceSpan{bodyStart, len(s.text)})
			}
			// Preserve the suffix after every completed block. Metadata cannot
			// discharge it; subsequent undashed intent latches the whole range.
			if inspect.pending {
				if held < 0 {
					held = index
				}
				pending = true
			}
			for _, span := range sourceUnion(spans) {
				if span.start < span.end && span.end <= len(s.origins) {
					e.spans = append(e.spans, sourceSpan{s.origins[span.start].start, s.origins[span.end-1].end})
				}
			}
		}
		if held >= 0 && index > held && (openLabel != "" || pending) { // all intermediary display fields are unproved
			e.whole = true
		}
		if held >= 0 {
			start := e.start
			if held < len(events) {
				start = events[held].start
			}
			if end-start > redactionHeldLimit {
				uncertain = true
			}
		}
		events = append(events, e)
		if len(e.spans) > redactionScalarLimit {
			uncertain = true
		}
		if uncertain {
			first := index
			if held >= 0 {
				first = held
			}
			for i := first; i < len(events); i++ {
				events[i].whole = true
			}
			held = -1
		} else if openLabel == "" && !pending {
			// On closure the last event can expose its proved text suffix.
			held = -1
			frontier = end
			// Full JSON, decoded keys and provenance have already been checked.
			// Only literal-candidate-free, unchanged approved scopes are reusable.
			events[index].cacheSafe = len(e.spans) == 0 && !privateEventCandidate(line)
		}
		cursor = end
	}
	if held >= 0 {
		if !complete || openLabel != "" || len(events) > 0 && len(events[len(events)-1].wire) == 0 {
			for i := held; i < len(events); i++ {
				events[i].whole = true
			}
		} else { // true completion can release a fully inspected dash-only range
			for i := held; i < len(events); i++ {
				events[i].whole = false
			}
			frontier = len(input)
		}
	}
	result := approvedSource{frontier: frontier, lexical: make(map[[32]byte]sourceLexicalProof)}
	cacheBytes := 0
	for _, e := range events {
		var wire []byte
		if e.whole {
			wire = []byte(redactionHidden)
		} else {
			// Scalar spans include complete JSON escape units. Replacement is
			// ASCII and valid inside either a member or string value.
			if e.provedWire != nil {
				wire = e.provedWire
			} else {
				wire = []byte(spliceSource(string(e.wire), e.spans, redactionHidden))
			}
			if e.cacheSafe && len(result.lexical) < 8192 && cacheBytes+len(wire) <= 1<<20 {
				result.lexical[sha256.Sum256(wire)] = sourceLexicalProof{wire, e.terminal}
				cacheBytes += len(wire)
			}
		}
		result.events = append(result.events, approvedEvent{wire, e.start, e.end, e.whole, e.terminal})
	}
	return result
}

// A string-valued MCP argument is another JSON document, without a proved
// mapping back to this event's original coordinates. Hide its entire scope.
// Object arguments already participate in the normal source scalar inspection.
func sourceUnprovedMCPArguments(scalars []sourceScalar) bool {
	if scalarAt(scalars, "item", "type") != "mcp_tool_call" {
		return false
	}
	for _, s := range scalars {
		if !s.key && !s.primitive && len(s.path) == 2 &&
			!s.path[0].array && s.path[0].member == "item" &&
			!s.path[1].array && s.path[1].member == "arguments" {
			return true
		}
	}
	return false
}

// Standalone summary callers must obtain approved values from the same source
// pass before selecting an argument. No semantic string decoding lives in the
// summary adapter. Stream events with this representation are hidden above.
func approvedSourceArguments(raw string) map[string]any {
	if len(raw) > redactionEventLimit-1 || strings.ContainsAny(raw, "\r\n") {
		return nil
	}
	projection := approveEvents([]byte(raw+"\n"), false)
	if len(projection.events) != 1 || projection.events[0].hidden {
		return nil
	}
	var args map[string]any
	if json.Unmarshal(projection.events[0].wire, &args) != nil {
		return nil
	}
	return args
}

// readSourceSnapshot starts at zero. Display callers never infer CLEAR from a
// clipped window. The same opened bytes are inspected and rendered.
func readSourceSnapshot(path string) ([]byte, error) {
	f, err := privateFile(path, os.O_RDONLY|syscall.O_NONBLOCK)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if checkLaneControlFile(int(f.Fd())) != nil {
		return nil, os.ErrPermission
	}
	before, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if before.Size() > redactionSourceLimit {
		return []byte(redactionHidden + "\n"), nil
	}
	b, err := io.ReadAll(io.LimitReader(f, redactionSourceLimit+1))
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(before, after) || before.Size() != int64(len(b)) || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return []byte(redactionHidden + "\n"), nil
	}
	return b, nil
}

func (m *Manager) publicEventWindow(runID string) *parsedEventWindow {
	b, err := readSourceSnapshot(filepath.Join(m.store, "runs", runID, "events.jsonl"))
	if err != nil {
		return &parsedEventWindow{activity: "-"}
	}
	return m.eventWindow(runID, b, false)
}
