package nuntius

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"strings"
	"unicode/utf8"
)

const goalBriefMaxBytes = 128 * 1024

var errGoalBrief = errors.New("the brief needs review: check its encoding, header, standards and size")
var errGoalPreflight = errors.New("preflight_review_required: the source observation changed; review and reseal before approval")

type goalBriefProfile struct {
	Name      string `json:"name"`
	Revision  string `json:"revision"`
	Separator string `json:"separator"`
	Marker    string `json:"marker"`
	Wrapper   string `json:"wrapper"`
}

type goalBriefInput struct {
	Body, Standards                             []byte
	Profile                                     goalBriefProfile
	Authority, SourceState, ReviewedSourceState string
}

type goalBriefSeal struct {
	Version            int              `json:"version"`
	GoalID             string           `json:"goal_id"`
	Size               nativeGoalSize   `json:"size"`
	Revision           uint64           `json:"revision"`
	SpecRef            string           `json:"spec_ref"`
	Profile            goalBriefProfile `json:"profile"`
	Authority          string           `json:"authority"`
	SourceState        string           `json:"source_state"`
	Body               string           `json:"body"`
	Standards          string           `json:"standards"`
	BodySHA256         string           `json:"body_sha256"`
	StandardsSHA256    string           `json:"standards_sha256"`
	FinalSHA256        string           `json:"final_sha256"`
	DispatchSHA256     string           `json:"dispatch_sha256"`
	Bytes              int              `json:"bytes"`
	CodePoints         int              `json:"code_points"`
	DispatchBytes      int              `json:"dispatch_bytes"`
	DispatchCodePoints int              `json:"dispatch_code_points"`
}

// deliveryText preserves the approved bytes. A pointer carries no second
// standards append; its artifact contains this exact full text.
func (s goalBriefSeal) deliveryText() (full, dispatch string) {
	full = s.Body + s.Profile.Separator + s.Standards
	return full, s.Profile.Wrapper + full
}

func assembleGoalBrief(spec nativeGoalSpec, revision uint64, in goalBriefInput) (goalBriefSeal, error) {
	var zero goalBriefSeal
	p := in.Profile
	if len(in.Body)+len(p.Separator)+len(in.Standards) > goalBriefMaxBytes {
		return zero, goalBriefRefusal(in, "assembled brief exceeds the 128 KiB limit")
	}
	if !validNativeGoalSpec(spec) || revision == 0 || revision >= goalJournalMaxEvents ||
		!goalBriefLine(in.Authority) || !goalBriefLine(in.SourceState) || !goalBriefLine(in.ReviewedSourceState) ||
		!goalBriefLine(p.Name) || !goalBriefLine(p.Revision) || !goalBriefLine(p.Marker) ||
		p.Separator == "" || len(p.Separator) > 1024 || !goalBriefText([]byte(p.Separator)) ||
		(p.Wrapper != "" && p.Wrapper != "/goal\n") || !goalBriefText(in.Body) || !goalBriefText(in.Standards) ||
		strings.HasPrefix(strings.TrimSpace(string(in.Body)), "PLACEHOLDER") {
		return zero, goalBriefRefusal(in, "use valid UTF-8 without a byte order mark, a real body and a valid declared profile and one-line references")
	}
	if in.SourceState != in.ReviewedSourceState {
		return zero, errGoalPreflight
	}
	line, _, _ := strings.Cut(string(in.Body), "\n")
	// One declared header grammar, without inferring identity or size from prose.
	header := regexp.MustCompile(`^GOAL: ` + regexp.QuoteMeta(spec.ID) + ` \(size ` + string(spec.Size) + `(?:, [^()\r\n]*)?\)(?:[ .;—].*)?$`)
	if !goalDecisionReason(line) || !header.MatchString(line) {
		return zero, goalBriefRefusal(in, "the first body line must match the stored goal ID and size on one line")
	}
	if strings.Contains(string(in.Body), p.Marker) || strings.Contains(p.Separator, p.Marker) || strings.Count(string(in.Standards), p.Marker) != 1 {
		return zero, goalBriefRefusal(in, "the body must omit shared standards and the standards must contain their declared marker exactly once")
	}
	final := string(in.Body) + p.Separator + string(in.Standards)
	if strings.Count(final, p.Marker) != 1 {
		return zero, goalBriefRefusal(in, "the declared standards marker must occur exactly once in the assembled brief")
	}
	dispatch := p.Wrapper + final
	specBytes, err := json.Marshal(spec)
	if err != nil {
		return zero, errGoalBrief
	}
	return goalBriefSeal{Version: 1, GoalID: spec.ID, Size: spec.Size, Revision: revision,
		SpecRef: goalBlobName("goal-spec-", specBytes), Profile: p, Authority: in.Authority, SourceState: in.SourceState,
		Body: string(in.Body), Standards: string(in.Standards), BodySHA256: goalBriefDigest(in.Body), StandardsSHA256: goalBriefDigest(in.Standards),
		FinalSHA256: goalBriefDigest([]byte(final)), DispatchSHA256: goalBriefDigest([]byte(dispatch)),
		Bytes: len(final), CodePoints: utf8.RuneCountInString(final), DispatchBytes: len(dispatch), DispatchCodePoints: utf8.RuneCountInString(dispatch)}, nil
}

func goalBriefRefusal(in goalBriefInput, reason string) error {
	byteCount := len(in.Body) + len(in.Profile.Separator) + len(in.Standards)
	if !utf8.Valid(in.Body) || !utf8.Valid(in.Standards) || !utf8.ValidString(in.Profile.Separator) || !utf8.ValidString(in.Profile.Wrapper) {
		return fmt.Errorf("%w: %s (%d assembled bytes; code points unavailable because UTF-8 is invalid)", errGoalBrief, reason, byteCount)
	}
	points := utf8.RuneCount(in.Body) + utf8.RuneCount(in.Standards) + utf8.RuneCountInString(in.Profile.Separator)
	return fmt.Errorf("%w: %s (%d assembled UTF-8 bytes, %d Unicode code points; %d dispatch bytes, %d code points including the wrapper)", errGoalBrief, reason, byteCount, points, byteCount+len(in.Profile.Wrapper), points+utf8.RuneCountInString(in.Profile.Wrapper))
}

func goalBriefDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func goalBriefLine(s string) bool { return queueText(s, 128, false) && goalDecisionReason(s) }

func goalBriefText(b []byte) bool {
	return len(b) > 0 && len(b) <= goalBriefMaxBytes && utf8.Valid(b) && !bytes.Contains(b, []byte{0xef, 0xbb, 0xbf})
}

func (m *Manager) sealGoalBrief(req goalRecordRequest, in goalBriefInput) (goalCommitResult, error) {
	if req.ExpectedRevision == 0 || req.Seal != nil {
		return goalCommitResult{}, errGoalBrief
	}
	s, err := assembleGoalBrief(req.Spec, req.ExpectedRevision+1, in)
	if err != nil {
		return goalCommitResult{}, err
	}
	req.Seal, err = json.Marshal(s)
	if err != nil || len(req.Seal) > goalSealMaxBytes {
		return goalCommitResult{}, errGoalBrief
	}
	// B05 supplies CAS, receipts, immutable blobs, approval/grant revocation,
	// locking and terminal/busy refusal. Sealing itself never launches work.
	return m.updateGoal(req)
}

func validateGoalBrief(old goalReplayRecord, authority string, seal []byte) *goalDecisionSeal {
	spec, err := decodeNativeGoalSpec([]byte(old.spec))
	if err != nil || old.sealRef == "" || goalBlobName("goal-seal-", seal) != old.sealRef {
		return nil
	}
	s, err := decodeGoalBrief(spec, seal)
	if err != nil || s.Revision != old.revision || s.SpecRef != goalBlobName("goal-spec-", []byte(old.spec)) || s.Authority != authority {
		return nil
	}
	return &goalDecisionSeal{Revision: old.revision, SpecRef: s.SpecRef, SealRef: old.sealRef, Authority: s.Authority, SourceState: s.SourceState}
}

// The closed recognizer sees decoded key spelling before the typed decoder.
// Case aliases and duplicate escaped spellings cannot exploit encoding/json.
func decodeGoalBrief(spec nativeGoalSpec, b []byte) (goalBriefSeal, error) {
	var s goalBriefSeal
	if len(b) > goalSealMaxBytes || !utf8.Valid(b) || !nativeJSONScalars(b) {
		return s, errGoalBrief
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	v, err := queueJSON(d, 1)
	if err != nil {
		return s, errGoalBrief
	}
	if _, err := d.Token(); err != io.EOF {
		return s, errGoalBrief
	}
	m, ok := v.(map[string]any)
	if !ok || !nativeClosedFields(m, []string{"version", "goal_id", "size", "revision", "spec_ref", "profile", "authority", "source_state", "body", "standards", "body_sha256", "standards_sha256", "final_sha256", "dispatch_sha256", "bytes", "code_points", "dispatch_bytes", "dispatch_code_points"}, nil) {
		return s, errGoalBrief
	}
	p, ok := m["profile"].(map[string]any)
	if !ok || !nativeClosedFields(p, []string{"name", "revision", "separator", "marker", "wrapper"}, nil) || json.Unmarshal(b, &s) != nil {
		return goalBriefSeal{}, errGoalBrief
	}
	expected, err := assembleGoalBrief(spec, s.Revision, goalBriefInput{Body: []byte(s.Body), Standards: []byte(s.Standards), Profile: s.Profile, Authority: s.Authority, SourceState: s.SourceState, ReviewedSourceState: s.SourceState})
	if err != nil || !reflect.DeepEqual(s, expected) {
		return goalBriefSeal{}, errGoalBrief
	}
	return s, nil
}

func goalBriefPreflight(seal goalBriefSeal, in goalBriefInput) (bool, error) {
	if in.SourceState != seal.SourceState {
		return false, errGoalPreflight
	}
	return seal.BodySHA256 != goalBriefDigest(in.Body) || seal.StandardsSHA256 != goalBriefDigest(in.Standards) || seal.Profile != in.Profile, nil
}
