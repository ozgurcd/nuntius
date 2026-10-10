package nuntius

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const queueMaxBytes = 1 << 20

var queueID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,63}$`)
var queueKind = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
var queueDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)
var queueTimestamp = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$`)
var errQueueInvalid = errors.New("invalid_snapshot")

type queueGoal struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Size        string   `json:"size"`
	LaneKinds   []string `json:"lane_kinds"`
	Lane        string   `json:"lane,omitempty"`
	Order       int64    `json:"order"`
	DependsOn   []string `json:"depends_on"`
	NeedsOwner  bool     `json:"needs_owner"`
	OwnerReason string   `json:"owner_reason,omitempty"`
	State       string   `json:"state"`
	Reason      string   `json:"reason,omitempty"`
	Created     string   `json:"created"`
	Updated     string   `json:"updated"`
	BriefSHA256 string   `json:"brief_sha256,omitempty"`
	BriefLink   string   `json:"brief_link,omitempty"`
}
type queueSnapshot struct {
	Status  string      `json:"status"`
	Version int         `json:"version,omitempty"`
	Updated string      `json:"updated,omitempty"`
	Goals   []queueGoal `json:"goals"`
	Reason  string      `json:"reason,omitempty"`
}

func queueFailure(status, reason string) queueSnapshot {
	return queueSnapshot{Status: status, Reason: reason, Goals: []queueGoal{}}
}
func queuePathValid(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && filepath.Base(path) != "." && path != "/" && queueText(path, 4096, false)
}

// Walk descriptor-relative components with O_NOFOLLOW; neither a directory
// swap nor a symlink can redirect the read. O_NONBLOCK prevents FIFO hangs.
func openQueueFile(path string) (*os.File, error) {
	if !queuePathValid(path) {
		return nil, errQueueInvalid
	}
	fd, e := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if e != nil {
		return nil, e
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, part := range parts {
		flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
		if i < len(parts)-1 {
			flags |= unix.O_DIRECTORY
		}
		next, err := unix.Openat(fd, part, flags, 0)
		_ = unix.Close(fd)
		if err != nil {
			return nil, err
		}
		fd = next
	}
	f := os.NewFile(uintptr(fd), "queue snapshot")
	info, e := f.Stat()
	if e != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, errQueueInvalid
	}
	return f, nil
}
func (m *Manager) queue() queueSnapshot {
	m.mu.Lock()
	configured := m.config.QueueFile
	store := m.store
	path := filepath.Join(store, "queue.json")
	explicit := configured != nil
	if explicit {
		path = *configured
	}
	m.mu.Unlock()
	if explicit && path == "" {
		return queueFailure("disabled", "")
	}
	f, e := openQueueFile(path)
	if e != nil {
		if errors.Is(e, os.ErrNotExist) {
			if explicit {
				return queueFailure("missing", "file_missing")
			}
			return queueFailure("not_configured", "")
		}
		return queueFailure("unavailable", "read_unavailable")
	}
	defer f.Close()
	return readQueue(f, io.ReadAll)
}

func readQueue(f *os.File, read func(io.Reader) ([]byte, error)) queueSnapshot {
	before, e := f.Stat()
	if e != nil {
		return queueFailure("unavailable", "read_unavailable")
	}
	b, e := read(io.LimitReader(f, queueMaxBytes+1))
	if len(b) > queueMaxBytes {
		return queueFailure("invalid", "invalid_snapshot")
	}
	after, statErr := f.Stat()
	if e != nil || statErr != nil || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || int64(len(b)) != after.Size() {
		return queueFailure("unavailable", "read_unavailable")
	}
	v, e := parseQueue(b)
	if e != nil {
		return queueFailure("invalid", "invalid_snapshot")
	}
	return v
}

// Decode all values (including ignored fields) with one depth/duplicate policy.
func queueJSON(d *json.Decoder, depth int) (any, error) {
	token, e := d.Token()
	if e != nil {
		return nil, errQueueInvalid
	}
	delim, container := token.(json.Delim)
	if !container {
		return token, nil
	}
	if depth > 8 {
		return nil, errQueueInvalid
	}
	switch delim {
	case '{':
		v := map[string]any{}
		for d.More() {
			key, e := d.Token()
			if e != nil {
				return nil, errQueueInvalid
			}
			k, ok := key.(string)
			if !ok {
				return nil, errQueueInvalid
			}
			if _, exists := v[k]; exists {
				return nil, errQueueInvalid
			}
			value, e := queueJSON(d, depth+1)
			if e != nil {
				return nil, e
			}
			v[k] = value
		}
		end, e := d.Token()
		if e != nil || end != json.Delim('}') {
			return nil, errQueueInvalid
		}
		return v, nil
	case '[':
		v := []any{}
		for d.More() {
			value, e := queueJSON(d, depth+1)
			if e != nil {
				return nil, e
			}
			v = append(v, value)
		}
		end, e := d.Token()
		if e != nil || end != json.Delim(']') {
			return nil, errQueueInvalid
		}
		return v, nil
	}
	return nil, errQueueInvalid
}
func queueText(s string, limit int, blank bool) bool {
	if !utf8.ValidString(s) || len(s) > limit || (!blank && strings.TrimSpace(s) == "") {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || r == 0x061c || r == 0x200e || r == 0x200f || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			return false
		}
	}
	return true
}
func queueTime(s string) (time.Time, bool) {
	t, e := time.Parse(time.RFC3339, s)
	return t, e == nil && queueTimestamp.MatchString(s)
}
func queueLink(s string) bool {
	if !queueText(s, 2048, false) {
		return false
	}
	u, e := url.Parse(s)
	if e != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(s, "#") || u.Opaque != "" {
		return false
	}
	h := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	// Require a DNS spelling, excluding all numeric/encoded IP alternatives.
	if h == "" || h == "localhost" || strings.HasSuffix(h, ".localhost") || strings.HasSuffix(h, ".local") || net.ParseIP(h) != nil || !strings.Contains(h, ".") {
		return false
	}
	labels := strings.Split(h, ".")
	tld := labels[len(labels)-1]
	letter := false
	for _, r := range tld {
		if r >= 'a' && r <= 'z' {
			letter = true
		}
	}
	if !letter {
		return false
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '-' {
				return false
			}
		}
	}
	return true
}
func queueStrings(v any, max int, pattern *regexp.Regexp) ([]string, bool) {
	a, ok := v.([]any)
	if !ok || len(a) > max {
		return nil, false
	}
	result := []string{}
	seen := map[string]bool{}
	for _, x := range a {
		s, ok := x.(string)
		if !ok || !pattern.MatchString(s) || seen[s] {
			return nil, false
		}
		seen[s] = true
		result = append(result, s)
	}
	return result, true
}
func parseQueue(b []byte) (queueSnapshot, error) {
	fail := func() (queueSnapshot, error) { return queueSnapshot{}, errQueueInvalid }
	if len(b) > queueMaxBytes || !utf8.Valid(b) {
		return fail()
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	raw, e := queueJSON(d, 1)
	if e != nil {
		return fail()
	}
	if _, e = d.Token(); e != io.EOF {
		return fail()
	}
	doc, ok := raw.(map[string]any)
	if !ok {
		return fail()
	}
	version, ok := doc["version"].(json.Number)
	if !ok || version.String() != "1" {
		return fail()
	}
	updated, ok := doc["updated"].(string)
	published, valid := queueTime(updated)
	if !ok || !valid {
		return fail()
	}
	goals, ok := doc["goals"].([]any)
	if !ok || len(goals) > 500 {
		return fail()
	}
	snapshot := queueSnapshot{Status: "ok", Version: 1, Updated: updated, Goals: []queueGoal{}}
	byID := map[string]queueGoal{}
	for _, raw := range goals {
		g, ok := raw.(map[string]any)
		if !ok {
			return fail()
		}
		// Validate each known key before constructing an allowlisted DTO.
		text := map[string]string{}
		for _, key := range []string{"id", "title", "size", "state", "created", "updated", "lane", "reason", "owner_reason", "brief_sha256", "brief_link"} {
			value, present := g[key]
			optional := key == "lane" || key == "reason" || key == "owner_reason" || key == "brief_sha256" || key == "brief_link"
			if !present && optional {
				continue
			}
			s, ok := value.(string)
			if !ok {
				return fail()
			}
			text[key] = s
		}
		id := text["id"]
		if !queueID.MatchString(id) {
			return fail()
		}
		if _, exists := byID[id]; exists {
			return fail()
		}
		if !queueText(text["title"], queueMaxBytes, true) || !queueText(strings.TrimSpace(text["title"]), 240, false) || (text["size"] != "S" && text["size"] != "M" && text["size"] != "L") {
			return fail()
		}
		kinds, ok := queueStrings(g["lane_kinds"], 8, queueKind)
		if !ok || len(kinds) == 0 {
			return fail()
		}
		deps, ok := queueStrings(g["depends_on"], 32, queueID)
		if !ok {
			return fail()
		}
		for _, dep := range deps {
			if dep == id {
				return fail()
			}
		}
		order, ok := g["order"].(json.Number)
		if !ok {
			return fail()
		}
		n, e := order.Int64()
		if e != nil || n < 0 || n > 2147483647 {
			return fail()
		}
		owner, ok := g["needs_owner"].(bool)
		if !ok {
			return fail()
		}
		if owner {
			if !queueText(text["owner_reason"], 512, false) {
				return fail()
			}
		} else if _, present := g["owner_reason"]; present {
			return fail()
		}
		state := text["state"]
		if state != "planned" && state != "blocked" && state != "ready" && state != "parked" {
			return fail()
		}
		if state == "ready" && (owner || len(deps) > 0) {
			return fail()
		}
		if state == "blocked" || state == "parked" {
			if !queueText(text["reason"], 512, false) {
				return fail()
			}
		}
		for _, key := range []string{"reason", "lane", "brief_sha256", "brief_link"} {
			if s, present := text[key]; present {
				switch key {
				case "reason":
					if !queueText(s, 512, true) {
						return fail()
					}
				case "lane":
					if !queueID.MatchString(s) {
						return fail()
					}
				case "brief_sha256":
					if !queueDigest.MatchString(s) {
						return fail()
					}
				case "brief_link":
					if !queueLink(s) {
						return fail()
					}
				}
			}
		}
		created, cok := queueTime(text["created"])
		changed, uok := queueTime(text["updated"])
		if !cok || !uok || created.After(changed) || changed.After(published) {
			return fail()
		}
		row := queueGoal{ID: id, Title: strings.TrimSpace(text["title"]), Size: text["size"], LaneKinds: kinds, Lane: text["lane"], Order: n, DependsOn: deps, NeedsOwner: owner, OwnerReason: text["owner_reason"], State: state, Reason: text["reason"], Created: text["created"], Updated: text["updated"], BriefSHA256: text["brief_sha256"], BriefLink: text["brief_link"]}
		snapshot.Goals = append(snapshot.Goals, row)
		byID[id] = row
	}
	visiting, done := map[string]bool{}, map[string]bool{}
	var visit func(string) bool
	visit = func(id string) bool {
		if visiting[id] {
			return false
		}
		if done[id] {
			return true
		}
		g, exists := byID[id]
		if !exists {
			return true
		}
		visiting[id] = true
		for _, dep := range g.DependsOn {
			if !visit(dep) {
				return false
			}
		}
		visiting[id] = false
		done[id] = true
		return true
	}
	for id := range byID {
		if !visit(id) {
			return fail()
		}
	}
	sort.Slice(snapshot.Goals, func(i, j int) bool {
		a, b := snapshot.Goals[i], snapshot.Goals[j]
		return a.Order < b.Order || a.Order == b.Order && a.ID < b.ID
	})
	return snapshot, nil
}
