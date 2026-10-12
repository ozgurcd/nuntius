package nuntius

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func nativeSchemaFixture() map[string]any {
	return map[string]any{
		"version": 1, "id": "EXAMPLE-DOCS", "title": "Review documentation", "size": "S",
		"lane_kind": "codex", "kinds": []string{"codex", "claude"}, "work_kind": "review",
		"preferred_lane": "codex-1", "lane_binding": "preferred", "order": 4,
		"after": []string{"EXAMPLE-REVIEW"}, "writes": []string{}, "resources": []string{},
	}
}

func TestNativeGoalSchemaBounds(t *testing.T) {
	doc := nativeSchemaFixture()
	doc["id"] = strings.Repeat("A", 64)
	doc["title"] = strings.Repeat("é", 120)
	doc["order"] = int64(2147483647)
	doc["kinds"] = []string{"claude", "codex", "opencode"}
	doc["lane_binding"] = "required"
	writes, resources, after, categories := []string{}, []string{}, []string{}, []string{}
	for i := 0; i < 32; i++ {
		writes = append(writes, fmt.Sprintf("repo/%02d", i)+strings.Repeat("a", 233))
		resources = append(resources, fmt.Sprintf("r%02d", i)+strings.Repeat("a", 61))
		after = append(after, fmt.Sprintf("D%02d", i)+strings.Repeat("A", 61))
		categories = append(categories, fmt.Sprintf("c%02d", i)+strings.Repeat("a", 61))
	}
	doc["writes"], doc["resources"], doc["after"] = writes, resources, after
	doc["owner_hold"] = map[string]any{"categories": categories, "reason": strings.Repeat("<", 512)}
	doc["coverage"] = map[string]any{"covered_by": "EXAMPLE-OTHER", "reason": strings.Repeat("<", 512)}
	b := nativeSchemaBytes(t, doc)
	s, err := decodeNativeGoalSpec(b)
	if err != nil {
		t.Fatal("boundary spec refused")
	}
	view, err := nativeGoalView(s, nativeGoalParked, nativeOutcomeUnknownExit)
	if err != nil {
		t.Fatal("boundary public projection refused")
	}
	encoded, err := json.Marshal(view)
	if err != nil || len(encoded) > nativeGoalPublicMaxBytes {
		t.Fatal("public projection exceeded byte budget")
	}
	// The spec byte bound includes whitespace, not just decoded field sizes.
	padded := append(append([]byte{}, b...), []byte(strings.Repeat(" ", nativeGoalMaxBytes-len(b)))...)
	if _, err := decodeNativeGoalSpec(padded); err != nil {
		t.Fatal("exact byte boundary refused")
	}
	if _, err := decodeNativeGoalSpec(append(padded, ' ')); err != errNativeGoalInvalid {
		t.Fatal("byte overflow not safely refused")
	}
	for _, key := range []string{"after", "writes", "resources"} {
		copyDoc := nativeSchemaFixture()
		values := append([]string{}, doc[key].([]string)...)
		values = append(values, "EXTRA")
		copyDoc[key] = values
		if _, err := decodeNativeGoalSpec(nativeSchemaBytes(t, copyDoc)); err != errNativeGoalInvalid {
			t.Fatal("list overflow accepted")
		}
	}
	for _, work := range []string{"mechanical", "design", "preparation", "code", "review", "release"} {
		control := nativeSchemaFixture()
		control["work_kind"] = work
		delete(control, "preferred_lane")
		control["writes"] = []string{"repo", "workspace/_scratch/goal-a"}
		control["resources"] = []string{"mint", "wiki-pin"}
		if _, err := decodeNativeGoalSpec(nativeSchemaBytes(t, control)); err != nil {
			t.Fatal("canonical scope or work class refused")
		}
		control["lane_binding"] = "required"
		if _, err := decodeNativeGoalSpec(nativeSchemaBytes(t, control)); err == nil {
			t.Fatal("required binding lacks named lane")
		}
	}
}

func TestNativeGoalPublicDTO(t *testing.T) {
	doc := nativeSchemaFixture()
	// Synthetic marker data is constructed only inside this fixture and is never
	// included in assertion messages or logs.
	sensitive := "password=" + strings.Repeat("x", 20)
	doc["title"] = sensitive
	doc["owner_hold"] = map[string]any{"categories": []string{"scope"}, "reason": sensitive}
	doc["coverage"] = map[string]any{"covered_by": "EXAMPLE-OTHER", "reason": sensitive}
	s, err := decodeNativeGoalSpec(nativeSchemaBytes(t, doc))
	if err != nil {
		t.Fatal("projection fixture invalid")
	}
	before, _ := json.Marshal(s)
	v, err := nativeGoalView(s, nativeGoalEnded, nativeOutcomeUnknownExit)
	if err != nil {
		t.Fatal("valid public projection refused")
	}
	b, err := json.Marshal(v)
	if err != nil || strings.Contains(string(b), sensitive) {
		t.Fatal("projection exposed private marker data")
	}
	var fields map[string]any
	if json.Unmarshal(b, &fields) != nil {
		t.Fatal("projection is not JSON")
	}
	if len(fields) != 17 {
		t.Fatal("public DTO allowlist changed")
	}
	for _, key := range []string{"approval", "brief_path", "report", "pid", "session_id", "lane_control", "ready", "startable"} {
		if _, ok := fields[key]; ok {
			t.Fatal("private data or admission claim in DTO")
		}
	}
	if v.State != nativeGoalEnded || v.Outcome != nativeOutcomeUnknownExit {
		t.Fatal("projection inferred verification or success")
	}
	// Returned slices and holds do not alias the spec. Rendering cannot alter it.
	v.Kinds[0], v.After[0], v.OwnerHold.Categories[0], v.OwnerHold.Reason, v.Coverage.Reason = "opencode", "OTHER", "other", "Other", "Other"
	after, _ := json.Marshal(s)
	if string(before) != string(after) {
		t.Fatal("projection mutated spec")
	}
	for _, state := range []nativeGoalState{nativeGoalDrafted, nativeGoalApproved, nativeGoalRunning, nativeGoalEnded, nativeGoalVerified, nativeGoalCancelled, nativeGoalParked} {
		if _, err := nativeGoalView(s, state, ""); err != nil {
			t.Fatal("typed state refused")
		}
	}
	for _, outcome := range []nativeGoalOutcome{nativeOutcomeCompleted, nativeOutcomeTimedOut, nativeOutcomeCancelled, nativeOutcomeInterrupted, nativeOutcomeReportDeliveryFailed, nativeOutcomeEndedNoReport, nativeOutcomeUnknownExit, nativeOutcomeFailed} {
		if _, err := nativeGoalView(s, nativeGoalEnded, outcome); err != nil {
			t.Fatal("typed outcome refused")
		}
	}
	if _, err := nativeGoalView(s, "failed", ""); err != errNativeGoalInvalid {
		t.Fatal("failed invented a goal state")
	}
	if _, err := nativeGoalView(s, nativeGoalEnded, "success"); err != errNativeGoalInvalid {
		t.Fatal("unknown outcome accepted")
	}
	s.Writes = nil
	if _, err := nativeGoalView(s, nativeGoalDrafted, ""); err != errNativeGoalInvalid {
		t.Fatal("unknown scope projected as read-only")
	}
}

func nativeSchemaBytes(t *testing.T, doc map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal("fixture encoding failed")
	}
	return b
}

func TestNativeGoalSchema(t *testing.T) {
	t.Run("strict-control", func(t *testing.T) {
		s, err := decodeNativeGoalSpec(nativeSchemaBytes(t, nativeSchemaFixture()))
		if err != nil || s.ID != "EXAMPLE-DOCS" || s.Writes == nil || len(s.Writes) != 0 {
			t.Fatal("valid explicit read-only spec refused or changed")
		}
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatal("validated spec encoding failed")
		}
		round, err := decodeNativeGoalSpec(b)
		if err != nil || !reflect.DeepEqual(s, round) {
			t.Fatal("spec round-trip changed data")
		}
		for _, key := range []string{"approval", "state", "outcome", "revision", "paused", "run_id"} {
			var fields map[string]any
			if json.Unmarshal(b, &fields) != nil {
				t.Fatal("spec encoding invalid")
			}
			if _, ok := fields[key]; ok {
				t.Fatal("creation data carries authority or lifecycle data")
			}
		}
	})
	required := []string{"version", "id", "title", "size", "lane_kind", "kinds", "work_kind", "lane_binding", "order", "after", "writes", "resources"}
	for _, key := range required {
		t.Run("missing-"+key, func(t *testing.T) {
			doc := nativeSchemaFixture()
			delete(doc, key)
			if _, err := decodeNativeGoalSpec(nativeSchemaBytes(t, doc)); err == nil {
				t.Fatal("missing required data accepted")
			}
		})
	}
	for _, key := range append(required, "preferred_lane", "owner_hold", "coverage") {
		t.Run("null-"+key, func(t *testing.T) {
			doc := nativeSchemaFixture()
			doc[key] = nil
			if _, err := decodeNativeGoalSpec(nativeSchemaBytes(t, doc)); err == nil {
				t.Fatal("null data accepted")
			}
		})
	}
	invalid := map[string]map[string]any{
		"unknown": {"unknown": true}, "approval": {"approval": map[string]any{"approved": true}},
		"state": {"state": "approved"}, "outcome": {"outcome": "completed"}, "paused": {"paused": false},
		"actor": {"actor": "owner"}, "brief-path": {"brief_path": "/private/brief.txt"},
		"lower-id": {"id": "example"}, "short-id": {"id": "A"}, "long-id": {"id": strings.Repeat("A", 65)},
		"path-id": {"id": "../EXAMPLE"}, "space-id": {"id": "EXAMPLE TASK"},
		"empty-title": {"title": " "}, "title-byte-limit": {"title": strings.Repeat("é", 121)},
		"title-control": {"title": "line\nline"}, "title-bidi": {"title": "a\u202eb"},
		"size": {"size": "XL"}, "version": {"version": 2}, "string-version": {"version": "1"},
		"bool-version": {"version": true}, "string-order": {"order": "4"}, "fraction-order": {"order": 1.5},
		"negative-order": {"order": -1}, "overflow-order": {"order": int64(2147483648)},
		"empty-engines": {"kinds": []string{}}, "duplicate-engines": {"kinds": []string{"codex", "codex"}},
		"class-as-engine": {"kinds": []string{"code"}}, "pool-as-engine": {"lane_kind": "codex-1"},
		"preferred-outside-engines": {"kinds": []string{"claude"}}, "engine-as-class": {"work_kind": "codex"},
		"unknown-class": {"work_kind": "misc"}, "binding": {"lane_binding": "any"},
		"invalid-lane": {"preferred_lane": "codex_1"}, "blank-lane": {"preferred_lane": ""},
		"self-after": {"after": []string{"EXAMPLE-DOCS"}}, "duplicate-after": {"after": []string{"AB", "AB"}},
		"invalid-after": {"after": []string{"ab"}}, "string-after": {"after": "AB"},
		"compound-writes": {"writes": []string{"repo other-repo"}}, "traversal": {"writes": []string{"repo/../other"}},
		"absolute-write": {"writes": []string{"/repo"}}, "dot-write": {"writes": []string{"."}},
		"unclean-write": {"writes": []string{"repo//docs"}}, "backslash-write": {"writes": []string{`repo\docs`}},
		"trailing-write": {"writes": []string{"repo/"}}, "url-write": {"writes": []string{"https://example.com"}},
		"encoded-write": {"writes": []string{"repo/%2e%2e"}}, "empty-write": {"writes": []string{""}},
		"duplicate-write": {"writes": []string{"repo", "repo"}}, "compound-resource": {"resources": []string{"mint wiki-pin"}},
		"path-resource": {"resources": []string{"../mint"}}, "duplicate-resource": {"resources": []string{"mint", "mint"}},
		"owner-empty":      {"owner_hold": map[string]any{"categories": []string{}, "reason": "Ask first"}},
		"owner-reason":     {"owner_hold": map[string]any{"categories": []string{"scope"}, "reason": " "}},
		"owner-long":       {"owner_hold": map[string]any{"categories": []string{"scope"}, "reason": strings.Repeat("é", 257)}},
		"owner-unknown":    {"owner_hold": map[string]any{"categories": []string{"scope"}, "reason": "Ask first", "approved": true}},
		"owner-evidence":   {"owner_hold": map[string]any{"categories": []string{"scope"}, "reason": "Ask first", "approval_evidence": "yes"}},
		"owner-null":       {"owner_hold": map[string]any{"categories": nil, "reason": "Ask first"}},
		"owner-duplicate":  {"owner_hold": map[string]any{"categories": []string{"scope", "scope"}, "reason": "Ask first"}},
		"coverage-self":    {"coverage": map[string]any{"covered_by": "EXAMPLE-DOCS", "reason": "Same work"}},
		"coverage-id":      {"coverage": map[string]any{"covered_by": "example", "reason": "Same work"}},
		"coverage-reason":  {"coverage": map[string]any{"covered_by": "EXAMPLE-OTHER", "reason": ""}},
		"coverage-closure": {"coverage": map[string]any{"covered_by": "EXAMPLE-OTHER", "reason": "Same work", "closure_event": "done"}},
	}
	for name, changes := range invalid {
		t.Run(name, func(t *testing.T) {
			doc := nativeSchemaFixture()
			for key, value := range changes {
				doc[key] = value
			}
			if _, err := decodeNativeGoalSpec(nativeSchemaBytes(t, doc)); err == nil {
				t.Fatal("invalid spec accepted")
			}
		})
	}
	b := string(nativeSchemaBytes(t, nativeSchemaFixture()))
	for name, raw := range map[string]string{
		"duplicate":         strings.Replace(b, `"version":1`, `"version":1,"version":1`, 1),
		"escaped-duplicate": strings.Replace(b, `"version":1`, `"version":1,"\u0076ersion":1`, 1),
		"decimal-version":   strings.Replace(b, `"version":1`, `"version":1.0`, 1),
		"exponent-order":    strings.Replace(b, `"order":4`, `"order":4e0`, 1),
		"nested-duplicate":  strings.TrimSuffix(b, "}") + `,"owner_hold":{"categories":["scope"],"reason":"Ask","reason":"Ask"}}`,
		"trailing":          b + " {}", "root-null": "null", "root-array": "[]", "bom": "\ufeff" + b,
		"invalid-utf8": strings.Replace(b, "Review", string([]byte{0xff}), 1),
		"oversize":     b + strings.Repeat(" ", 65536),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeNativeGoalSpec([]byte(raw)); err == nil {
				t.Fatal("invalid JSON spec accepted")
			}
		})
	}
}
