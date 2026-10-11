package nuntius

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"unicode"
)

var privateBoundary = regexp.MustCompile(`-----([A-Z]*PRIVATEKEY)-----`)

// A malformed delimiter is evidence of uncertain privacy, never evidence
// that a block ended. Normalize whitespace only for detection, not output.
func privateMarkers(text string) (begin, end, uncertain bool) {
	if !strings.Contains(text, "---") {
		return
	}
	normal := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return unicode.ToUpper(r)
	}, text)
	markers := privateBoundary.FindAllStringIndex(normal, -1)
	for _, marker := range markers {
		label := normal[marker[0]+5 : marker[1]-5]
		if strings.HasPrefix(label, "BEGIN") && !begin && !end {
			begin = true
		} else if strings.HasPrefix(label, "END") && !end {
			end = true
		} else {
			uncertain = true
		}
	}
	// Any unrecognized or partial delimiter keeps the context private.
	uncertain = uncertain || strings.Contains(privateBoundary.ReplaceAllString(normal, ""), "---")
	return
}

func privateEventMarkers(line []byte) (begin, end, uncertain bool) {
	if !bytes.Contains(line, []byte("---")) && !bytes.Contains(line, []byte(`\u002d`)) && !bytes.Contains(line, []byte(`\u002D`)) {
		return
	}
	var value any
	if json.Unmarshal(line, &value) != nil {
		return false, false, true
	}
	var visit func(any)
	visit = func(value any) {
		switch v := value.(type) {
		case string:
			b, e, u := privateMarkers(v)
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
	return
}

func privateEventLines(b []byte, lines [][]byte) []bool {
	if !bytes.Contains(b, []byte("---")) && !bytes.Contains(b, []byte(`\u002d`)) && !bytes.Contains(b, []byte(`\u002D`)) {
		return nil
	}
	private := make([]bool, len(lines))
	inside := false
	for i, line := range lines {
		begin, end, uncertain := privateEventMarkers(line)
		if uncertain || (end && !inside && !begin) {
			// An orphan END can refer to any preceding body in this window.
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
	var buffer [32768]byte
	var overlap []byte
	reader = io.LimitReader(reader, size)
	for {
		n, err := reader.Read(buffer[len(overlap):])
		copy(buffer[:], overlap)
		chunk := buffer[:len(overlap)+n]
		if bytes.Contains(chunk, []byte("---")) || bytes.Contains(chunk, []byte(`\u002d`)) || bytes.Contains(chunk, []byte(`\u002D`)) {
			return false
		}
		overlap = append(overlap[:0], chunk[max(0, len(chunk)-5):]...)
		size -= int64(n)
		if err != nil {
			return err == io.EOF && size == 0
		}
	}
}
