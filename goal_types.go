package nuntius

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	nativeGoalVersion        = 1
	nativeGoalMaxBytes       = 64 * 1024
	nativeGoalPublicMaxBytes = 32 * 1024
	nativeGoalMaxList        = 32
)

var errNativeGoalInvalid = errors.New("goal data is invalid or incomplete; check its version, fields and work scope")
var nativeResourceName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
var nativeWriteComponent = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)

type nativeGoalSize string
type nativeEngineKind string
type nativeWorkKind string
type nativeLaneBinding string
type nativeGoalState string
type nativeGoalOutcome string

const (
	nativeGoalDrafted   nativeGoalState = "drafted"
	nativeGoalApproved  nativeGoalState = "approved"
	nativeGoalRunning   nativeGoalState = "running"
	nativeGoalEnded     nativeGoalState = "ended"
	nativeGoalVerified  nativeGoalState = "verified"
	nativeGoalCancelled nativeGoalState = "cancelled"
	nativeGoalParked    nativeGoalState = "parked"

	nativeOutcomeCompleted            nativeGoalOutcome = "completed"
	nativeOutcomeTimedOut             nativeGoalOutcome = "timed_out"
	nativeOutcomeCancelled            nativeGoalOutcome = "cancelled"
	nativeOutcomeInterrupted          nativeGoalOutcome = "interrupted"
	nativeOutcomeReportDeliveryFailed nativeGoalOutcome = "report_delivery_failed"
	nativeOutcomeEndedNoReport        nativeGoalOutcome = "ended_no_report"
	nativeOutcomeUnknownExit          nativeGoalOutcome = "unknown_exit"
	nativeOutcomeFailed               nativeGoalOutcome = "failed"
)

// nativeGoalSpec is creation data, never an approval or a run record.
type nativeGoalSpec struct {
	Version       int                 `json:"version"`
	ID            string              `json:"id"`
	Title         string              `json:"title"`
	Size          nativeGoalSize      `json:"size"`
	LaneKind      nativeEngineKind    `json:"lane_kind"`
	Kinds         []nativeEngineKind  `json:"kinds"`
	WorkKind      nativeWorkKind      `json:"work_kind"`
	PreferredLane string              `json:"preferred_lane,omitempty"`
	LaneBinding   nativeLaneBinding   `json:"lane_binding"`
	Order         int64               `json:"order"`
	After         []string            `json:"after"`
	Writes        []string            `json:"writes"`
	Resources     []string            `json:"resources"`
	OwnerHold     *nativeOwnerHold    `json:"owner_hold,omitempty"`
	Coverage      *nativeCoverageHold `json:"coverage,omitempty"`
}

type nativeOwnerHold struct {
	Categories []string `json:"categories"`
	Reason     string   `json:"reason"`
}

type nativeCoverageHold struct {
	CoveredBy string `json:"covered_by"`
	Reason    string `json:"reason"`
}

func decodeNativeGoalSpec(b []byte) (nativeGoalSpec, error) {
	if len(b) > nativeGoalMaxBytes || !utf8.Valid(b) {
		return nativeGoalSpec{}, errNativeGoalInvalid
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	raw, err := queueJSON(d, 1)
	if err != nil {
		return nativeGoalSpec{}, errNativeGoalInvalid
	}
	if _, err = d.Token(); err != io.EOF {
		return nativeGoalSpec{}, errNativeGoalInvalid
	}
	fields, ok := raw.(map[string]any)
	required := []string{"version", "id", "title", "size", "lane_kind", "kinds", "work_kind", "lane_binding", "order", "after", "writes", "resources"}
	if !ok || !nativeClosedFields(fields, required, []string{"preferred_lane", "owner_hold", "coverage"}) {
		return nativeGoalSpec{}, errNativeGoalInvalid
	}
	version, vok := fields["version"].(json.Number)
	order, ook := fields["order"].(json.Number)
	if !vok || version.String() != "1" || !ook {
		return nativeGoalSpec{}, errNativeGoalInvalid
	}
	n, err := order.Int64()
	if err != nil || n < 0 || n > 2147483647 {
		return nativeGoalSpec{}, errNativeGoalInvalid
	}
	if lane, exists := fields["preferred_lane"]; exists {
		name, ok := lane.(string)
		if !ok || !namePattern.MatchString(name) {
			return nativeGoalSpec{}, errNativeGoalInvalid
		}
	}
	if hold, exists := fields["owner_hold"]; exists {
		m, ok := hold.(map[string]any)
		if !ok || !nativeClosedFields(m, []string{"categories", "reason"}, nil) {
			return nativeGoalSpec{}, errNativeGoalInvalid
		}
	}
	if hold, exists := fields["coverage"]; exists {
		m, ok := hold.(map[string]any)
		if !ok || !nativeClosedFields(m, []string{"covered_by", "reason"}, nil) {
			return nativeGoalSpec{}, errNativeGoalInvalid
		}
	}
	var s nativeGoalSpec
	if json.Unmarshal(b, &s) != nil || !validNativeGoalSpec(s) {
		return nativeGoalSpec{}, errNativeGoalInvalid
	}
	return s, nil
}

// nativeClosedFields rejects unknown keys and null even for optional fields.
func nativeClosedFields(m map[string]any, required, optional []string) bool {
	allowed := make(map[string]bool, len(required)+len(optional))
	for _, key := range required {
		if v, ok := m[key]; !ok || v == nil {
			return false
		}
		allowed[key] = true
	}
	for _, key := range optional {
		allowed[key] = true
	}
	for key, v := range m {
		if !allowed[key] || v == nil {
			return false
		}
	}
	return true
}

func validNativeGoalSpec(s nativeGoalSpec) bool {
	if s.Version != nativeGoalVersion || !goalPattern.MatchString(s.ID) || !queueText(s.Title, 240, false) || strings.TrimSpace(s.Title) != s.Title {
		return false
	}
	if s.Size != "S" && s.Size != "M" && s.Size != "L" {
		return false
	}
	if !nativeEngine(s.LaneKind) || len(s.Kinds) == 0 || len(s.Kinds) > 8 {
		return false
	}
	engines := map[nativeEngineKind]bool{}
	for _, kind := range s.Kinds {
		if !nativeEngine(kind) || engines[kind] {
			return false
		}
		engines[kind] = true
	}
	if !engines[s.LaneKind] {
		return false
	}
	switch s.WorkKind {
	case "mechanical", "design", "preparation", "code", "review", "release":
	default:
		return false
	}
	if s.LaneBinding != "preferred" && s.LaneBinding != "required" {
		return false
	}
	if s.PreferredLane != "" && !namePattern.MatchString(s.PreferredLane) || s.LaneBinding == "required" && s.PreferredLane == "" {
		return false
	}
	if s.Order < 0 || s.Order > 2147483647 {
		return false
	}
	if !nativeStringSet(s.After, nativeGoalMaxList, func(id string) bool { return goalPattern.MatchString(id) && id != s.ID }) {
		return false
	}
	if !nativeStringSet(s.Writes, nativeGoalMaxList, nativeWrite) || !nativeStringSet(s.Resources, nativeGoalMaxList, nativeResourceName.MatchString) {
		return false
	}
	if s.OwnerHold != nil && (len(s.OwnerHold.Categories) == 0 || !nativeStringSet(s.OwnerHold.Categories, nativeGoalMaxList, nativeResourceName.MatchString) || !queueText(s.OwnerHold.Reason, 512, false)) {
		return false
	}
	if s.Coverage != nil && (!goalPattern.MatchString(s.Coverage.CoveredBy) || s.Coverage.CoveredBy == s.ID || !queueText(s.Coverage.Reason, 512, false)) {
		return false
	}
	return true
}

func nativeEngine(kind nativeEngineKind) bool {
	return kind == "claude" || kind == "codex" || kind == "opencode"
}

func nativeStringSet(values []string, max int, valid func(string) bool) bool {
	if values == nil || len(values) > max {
		return false
	}
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if seen[value] || !valid(value) {
			return false
		}
		seen[value] = true
	}
	return true
}

// nativeWrite checks syntax only. Later admission must resolve known workspace
// roots and claims; an unknown root is never permission to write there.
func nativeWrite(value string) bool {
	if len(value) == 0 || len(value) > 240 || path.IsAbs(value) || path.Clean(value) != value {
		return false
	}
	for _, component := range strings.Split(value, "/") {
		if !nativeWriteComponent.MatchString(component) {
			return false
		}
	}
	return true
}

// nativeGoalDTO contains only validated metadata, never approval, brief/report
// bytes, private filesystem paths, process or session identity, or lane control.
type nativeGoalDTO struct {
	Version       int                 `json:"version"`
	ID            string              `json:"id"`
	Title         string              `json:"title"`
	Size          nativeGoalSize      `json:"size"`
	LaneKind      nativeEngineKind    `json:"lane_kind"`
	Kinds         []nativeEngineKind  `json:"kinds"`
	WorkKind      nativeWorkKind      `json:"work_kind"`
	PreferredLane string              `json:"preferred_lane,omitempty"`
	LaneBinding   nativeLaneBinding   `json:"lane_binding"`
	Order         int64               `json:"order"`
	After         []string            `json:"after"`
	Writes        []string            `json:"writes"`
	Resources     []string            `json:"resources"`
	OwnerHold     *nativeOwnerHold    `json:"owner_hold,omitempty"`
	Coverage      *nativeCoverageHold `json:"coverage,omitempty"`
	State         nativeGoalState     `json:"state"`
	Outcome       nativeGoalOutcome   `json:"outcome,omitempty"`
}

// nativeGoalView is a pure bounded projection. Its state is supplied by a trusted
// record, not the create decoder. It makes no readiness or admission claim.
func nativeGoalView(s nativeGoalSpec, state nativeGoalState, outcome nativeGoalOutcome) (nativeGoalDTO, error) {
	if !validNativeGoalSpec(s) {
		return nativeGoalDTO{}, errNativeGoalInvalid
	}
	switch state {
	case nativeGoalDrafted, nativeGoalApproved, nativeGoalRunning, nativeGoalEnded, nativeGoalVerified, nativeGoalCancelled, nativeGoalParked:
	default:
		return nativeGoalDTO{}, errNativeGoalInvalid
	}
	switch outcome {
	case "", nativeOutcomeCompleted, nativeOutcomeTimedOut, nativeOutcomeCancelled, nativeOutcomeInterrupted, nativeOutcomeReportDeliveryFailed, nativeOutcomeEndedNoReport, nativeOutcomeUnknownExit, nativeOutcomeFailed:
	default:
		return nativeGoalDTO{}, errNativeGoalInvalid
	}
	s.Title = Redact(s.Title)
	s.Kinds = append([]nativeEngineKind{}, s.Kinds...)
	s.After = append([]string{}, s.After...)
	s.Writes = append([]string{}, s.Writes...)
	s.Resources = append([]string{}, s.Resources...)
	if s.OwnerHold != nil {
		s.OwnerHold = &nativeOwnerHold{Categories: append([]string{}, s.OwnerHold.Categories...), Reason: Redact(s.OwnerHold.Reason)}
	}
	if s.Coverage != nil {
		s.Coverage = &nativeCoverageHold{CoveredBy: s.Coverage.CoveredBy, Reason: Redact(s.Coverage.Reason)}
	}
	v := nativeGoalDTO{Version: s.Version, ID: s.ID, Title: s.Title, Size: s.Size, LaneKind: s.LaneKind, Kinds: s.Kinds, WorkKind: s.WorkKind, PreferredLane: s.PreferredLane, LaneBinding: s.LaneBinding, Order: s.Order, After: s.After, Writes: s.Writes, Resources: s.Resources, OwnerHold: s.OwnerHold, Coverage: s.Coverage, State: state, Outcome: outcome}
	b, err := json.Marshal(v)
	if err != nil || len(b) > nativeGoalPublicMaxBytes {
		return nativeGoalDTO{}, errNativeGoalInvalid
	}
	return v, nil
}
