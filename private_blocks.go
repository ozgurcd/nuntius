package nuntius

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"unicode"
)

var privateBoundary = regexp.MustCompile(`-----([A-Z]*PRIVATEKEY)-----`)

// Privacy rule: only a complete, ordered, non-nested BEGIN/END PRIVATE KEY
// pair establishes a public suffix. A private label with displaced/missing
// dashes, a proper prefix of a private header, split headers, nesting or an
// orphan END makes the entire window uncertain and suppresses it. Dash-only
// fragments carry across events until the next payload; they are not themselves
// private markers. Marker-free Markdown stays unchanged. An omitted prefix is
// public only if a bounded scan through its suffix has no marker intent.
// Whitespace/case normalization is for detection only, never for output.
func privateMarkers(text string) (begin, end, uncertain bool) {
	if !strings.Contains(text, "-") {
		return
	}
	normal := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return unicode.ToUpper(r)
	}, text)
	markers := privateBoundary.FindAllStringIndex(normal, -1)
	openLabel := ""
	for _, marker := range markers {
		label := normal[marker[0]+5 : marker[1]-5]
		if strings.HasPrefix(label, "BEGIN") && !begin && !end {
			begin = true
			openLabel = strings.TrimPrefix(label, "BEGIN")
		} else if strings.HasPrefix(label, "END") && !end {
			end = true
			uncertain = uncertain || (openLabel != "" && strings.TrimPrefix(label, "END") != openLabel)
		} else {
			uncertain = true
		}
	}
	uncertain = uncertain || privateFragment(privateBoundary.ReplaceAllString(normal, ""), false)
	if !uncertain && strings.Contains(text, "\n") {
		for _, line := range strings.Split(text, "\n") {
			normalLine := strings.Map(func(r rune) rune {
				if unicode.IsSpace(r) {
					return -1
				}
				return unicode.ToUpper(r)
			}, line)
			uncertain = uncertain || privateFragment(privateBoundary.ReplaceAllString(normalLine, ""), false)
		}
	}
	return
}

// A complete private label with any delimiter is suspicious. Proper prefixes
// include a header cut anywhere, such as -B, -----BEGIN RSA PRI or END PR.
func privateFragment(normal string, continuation bool) bool {
	if strings.Contains(normal, "-") && strings.Contains(normal, "PRIVATEKEY") {
		return true
	}
	for i := 0; i < len(normal); i++ {
		if normal[i] != '-' && !(continuation && i == 0) {
			continue
		}
		j := i
		for j < len(normal) && normal[j] == '-' {
			j++
		}
		rest := normal[j:]
		if rest == "" {
			break
		}
		for _, direction := range []string{"BEGIN", "END"} {
			if strings.HasPrefix(rest, direction) && (j-i >= 5 || continuation || strings.Contains(rest[len(direction):], "PRI")) {
				return true
			}
			for _, label := range []string{"", "RSA", "EC", "DSA", "OPENSSH", "ENCRYPTED"} {
				header := direction + label + "PRIVATEKEY"
				if strings.HasPrefix(header, rest) || strings.HasPrefix(rest, header) || strings.HasPrefix(rest, direction+label+"PRI") {
					return true
				}
			}
		}
		if j > i {
			i = j - 1
		}
	}
	return false
}

type privateEventScanner struct {
	dashes bool
	label  string
}

// Include every dash except a numeric suffix immediately followed by the JSON
// closing quote (e.g. cmd-123). That suffix cannot start an alphabetic private
// header. Escapes always take the decoded path, including escaped closing quotes.
func privateEventCandidate(line []byte) bool {
	if bytes.IndexByte(line, '\\') >= 0 {
		return true
	}
	for i := 0; i < len(line); i++ {
		n := bytes.IndexByte(line[i:], '-')
		if n < 0 {
			break
		}
		i += n
		j := i + 1
		for j < len(line) && line[j] >= '0' && line[j] <= '9' {
			j++
		}
		if j > i+1 && j < len(line) && line[j] == '"' {
			i = j
			continue
		}
		return true
	}
	return false
}

func (s *privateEventScanner) markers(line []byte) (begin, end, uncertain bool) {
	if len(bytes.TrimSpace(line)) == 0 {
		return
	}
	if !s.dashes && !privateEventCandidate(line) {
		return
	}
	var value any
	if json.Unmarshal(line, &value) != nil {
		return false, false, true
	}
	var visit func(any)
	pending := false
	continuation := false
	beginLabel, endLabel := "", ""
	visit = func(value any) {
		switch v := value.(type) {
		case string:
			b, e, u := privateMarkers(v)
			normal := strings.Map(func(r rune) rune {
				if unicode.IsSpace(r) {
					return -1
				}
				return unicode.ToUpper(r)
			}, v)
			if s.dashes && privateFragment(normal, true) {
				u = true // Ordering/field identity of a split header is uncertain.
			}
			if !b && !e && !u {
				pending = pending || strings.HasSuffix(normal, "-")
				continuation = continuation || (!strings.Contains(normal, "-") && privateFragment(normal, true))
			}
			for _, marker := range privateBoundary.FindAllString(normal, -1) {
				label := marker[5 : len(marker)-5]
				if strings.HasPrefix(label, "BEGIN") {
					beginLabel = strings.TrimPrefix(label, "BEGIN")
				} else if strings.HasPrefix(label, "END") {
					endLabel = strings.TrimPrefix(label, "END")
				}
			}
			if (b || e || u) && (begin || end || uncertain) {
				uncertain = true // Ordering between different JSON fields is unknown.
			}
			begin, end, uncertain = begin || b, end || e, uncertain || u
		case []any:
			for _, child := range v {
				visit(child)
			}
		case map[string]any:
			for key, child := range v {
				visit(key)
				visit(child)
			}
		}
	}
	visit(value)
	uncertain = uncertain || pending && continuation
	if begin && s.label != "" || end && s.label != "" && s.label != endLabel {
		uncertain = true
	}
	if begin && !end {
		s.label = beginLabel
	} else if end && !uncertain {
		s.label = ""
	}
	// Do not lose a delimiter fragment to metadata, whitespace-only chunks,
	// or intervening fields. No label means public; later marker intent latches.
	s.dashes = (s.dashes || pending) && !begin && !end && !uncertain
	return
}

func privateEventLines(b []byte, lines [][]byte) []bool {
	if !privateEventCandidate(b) {
		return nil
	}
	private := make([]bool, len(lines))
	inside := false
	var scanner privateEventScanner
	for i, line := range lines {
		begin, end, uncertain := scanner.markers(line)
		if uncertain || (begin && inside) || (end && !inside && !begin) {
			// Ambiguous order, nesting or an orphan END can refer to any body.
			for j := range private {
				private[j] = true
			}
			return private
		}
		private[i] = inside || begin || end
		inside = (inside || begin) && !end
	}
	return private
}

// The omitted prefix must be demonstrably marker-free. Bound the scan and
// retain only a short overlap, never raw omitted bodies. This intentionally
// hides a tail after any possible earlier marker, even a closed block.
func publicEventPrefix(reader io.Reader, size int64) bool {
	if size > 64<<20 {
		return false
	}
	buffer := bufio.NewReaderSize(io.LimitReader(reader, size), 1<<20)
	var scanner privateEventScanner
	for {
		line, err := buffer.ReadSlice('\n')
		if len(line) == 0 && err == io.EOF {
			return size == 0
		}
		if privateEventCandidate(line) || scanner.dashes {
			begin, end, uncertain := scanner.markers(line)
			if begin || end || uncertain {
				return false
			}
		}
		if err != nil && err != io.EOF {
			return false
		}
		size -= int64(len(line))
		if err != nil {
			return err == io.EOF && size == 0
		}
	}
}
