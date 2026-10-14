package nuntius

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// This internal protocol has no executor or public transport. N3 owns the
// eventual goal bridge. Hash bindings here detect damage, not authentication.
const integrationVersion = 1
const integrationLogName = "integration-journal.bin"
const integrationHeaderBytes = 84

var errIntegration = errors.New("integration unavailable; preserve and review its store")
var errIntegrationStale = errors.New("integration source or revision changed")
var errIntegrationUnconfirmed = errors.New("integration save unconfirmed; inspect the operation before proceeding")
var errIntegrationLocked = errors.New("integration is locked")

type integrationSource struct {
	GoalID         string `json:"goal_id"`
	GoalRevision   uint64 `json:"goal_revision"`
	GoalDigest     string `json:"goal_digest"`
	ReviewID       string `json:"review_id"`
	ReviewRevision uint64 `json:"review_revision"`
	ReviewDigest   string `json:"review_digest"`
	ReviewVerified bool   `json:"review_verified"`
}

type integrationEntry struct {
	ID              string            `json:"id"`
	Source          integrationSource `json:"source"`
	SourceCommit    string            `json:"source_commit"`
	BaseCommit      string            `json:"base_commit"`
	Tree            string            `json:"tree"`
	OperationDigest string            `json:"operation_digest"`
	FragmentDigest  string            `json:"fragment_digest"`
	ReviewerDigest  string            `json:"reviewer_digest"`
	RepoID          string            `json:"repo_id"`
	GroupDigest     string            `json:"group_digest"`
	Scope           []string          `json:"scope"`
	SemanticKeys    []string          `json:"semantic_keys"`
	PolicyDigest    string            `json:"policy_digest"`
	PolicyEpoch     uint64            `json:"policy_epoch"`
	ProfileDigest   string            `json:"profile_digest"`
	AuthorityDigest string            `json:"authority_digest"`
	Predecessor     string            `json:"predecessor"`
	Attempt         uint64            `json:"attempt"`
	Dependencies    []string          `json:"dependencies"`
}

type integrationSeal struct {
	EntryDigest    string `json:"entry_digest"`
	ReviewDigest   string `json:"review_digest"`
	ReviewerDigest string `json:"reviewer_digest"`
}

type integrationObservation struct {
	OperationID  string `json:"operation_id"`
	IntentDigest string `json:"intent_digest"`
	ResultDigest string `json:"result_digest"`
	Outcome      string `json:"outcome"` // settled, failed, or uncertain; never acceptance
}

type integrationRequest struct {
	Version          int                     `json:"version"`
	OperationID      string                  `json:"operation_id"`
	EntryID          string                  `json:"entry_id"`
	ExpectedRevision uint64                  `json:"expected_revision"`
	Kind             string                  `json:"kind"`
	Entry            *integrationEntry       `json:"entry,omitempty"`
	Seal             *integrationSeal        `json:"seal,omitempty"`
	Observation      *integrationObservation `json:"observation,omitempty"`
	Effect           string                  `json:"effect,omitempty"`
	State            string                  `json:"state,omitempty"`
}

type integrationRecord struct {
	Entry    integrationEntry
	Revision uint64
	State    string
	Seal     *integrationSeal
	Pending  string
}

type integrationOperation struct {
	Input        string
	Sequence     uint64
	EntryID      string
	Revision     uint64
	IntentDigest string
	Observation  *integrationObservation
}

type integrationProjection struct {
	Sequence   uint64
	Entries    map[string]integrationRecord
	Operations map[string]integrationOperation
}

type integrationIO struct{ checkpoint func(string) error }

type integrationCapabilities struct {
	Version               int
	MaxBytes              int
	MaxEvents             int
	MaxEntries            int
	MaxFrameBytes         int
	Executor              bool
	AuthenticatedReceipts bool
}

func integrationCaps() integrationCapabilities {
	return integrationCapabilities{Version: 1, MaxBytes: 16 << 20, MaxEvents: 8192, MaxEntries: 1024, MaxFrameBytes: 32768}
}

func captureIntegrationSource(path, goal, review string) (integrationSource, error) {
	if !goalPattern.MatchString(goal) || !goalPattern.MatchString(review) || goal == review {
		return integrationSource{}, errIntegration
	}
	root, err := openLaneControlRoot(path)
	if err != nil {
		return integrationSource{}, errIntegration
	}
	defer root.Close()
	// This lock is released before any integration CAS or effect. The separate
	// source directory is read-only; a missing control lock is a hold.
	lock, err := laneControlReadLock(root)
	if err != nil {
		return integrationSource{}, errIntegration
	}
	defer lock.Close()
	p, err := replayGoalJournalRoot(root)
	if err != nil || p.tornTail {
		return integrationSource{}, errIntegration
	}
	g, gok := p.records[goal]
	r, rok := p.records[review]
	spec, _ := p.spec(review)
	metadata, err := goalRecordMetadata(r)
	if !gok || !rok || spec.WorkKind != "review" || err != nil {
		return integrationSource{}, errIntegration
	}
	return integrationSource{GoalID: goal, GoalRevision: g.revision, GoalDigest: integrationGoalDigest(g), ReviewID: review, ReviewRevision: r.revision, ReviewDigest: integrationGoalDigest(r), ReviewVerified: metadata.State == nativeGoalVerified}, nil
}

func integrationGoalDigest(r goalReplayRecord) string {
	return integrationDigestJSON([]string{r.spec, r.sealRef, r.record})
}

func readIntegration(path string) (*integrationProjection, error) {
	root, err := openLaneControlRoot(path)
	if err != nil {
		return nil, errIntegration
	}
	defer root.Close()
	data, err := readIntegrationData(root)
	if err != nil {
		return nil, err
	}
	return replayIntegration(data)
}

func commitIntegration(path, sourcePath string, req integrationRequest, fault integrationIO) (integrationOperation, error) {
	// Detach caller-owned arrays/pointers before validation or lock acquisition.
	data, err := json.Marshal(req)
	if err != nil {
		return integrationOperation{}, errIntegration
	}
	var owned integrationRequest
	if err := decodeIntegration(data, &owned); err != nil {
		return integrationOperation{}, err
	}
	req = owned
	before, err := readIntegration(path)
	if err != nil {
		return integrationOperation{}, err
	}
	entry := before.Entries[req.EntryID].Entry
	if req.Entry != nil {
		entry = *req.Entry
	}
	snapshot, err := captureIntegrationSource(sourcePath, entry.Source.GoalID, entry.Source.ReviewID)
	if err != nil {
		return integrationOperation{}, err
	}
	if snapshot != entry.Source {
		return integrationOperation{}, errIntegrationStale
	}
	if fault.check("snapshot") != nil {
		return integrationOperation{}, errIntegration
	}
	// The store lock protects only replay, CAS and bounded durable append.
	// No callback, source lock, process, Git operation or effect runs inside it.
	result, err := appendIntegration(path, req, fault)
	if err != nil {
		return integrationOperation{}, err
	}
	if fault.check("ack") != nil {
		return integrationOperation{}, errIntegrationUnconfirmed
	}
	fresh, err := captureIntegrationSource(sourcePath, entry.Source.GoalID, entry.Source.ReviewID)
	if err != nil || fresh != snapshot {
		return integrationOperation{}, errIntegrationUnconfirmed
	}
	return result, nil
}

func (f integrationIO) check(stage string) error {
	if f.checkpoint != nil {
		return f.checkpoint(stage)
	}
	return nil
}

func integrationDigest(b []byte) string {
	d := sha256.Sum256(b)
	return hex.EncodeToString(d[:])
}

func integrationDigestJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return integrationDigest(b)
}

// goalReplayRecord has private fields: bind their exact bytes explicitly.
func (s integrationSource) valid() bool {
	return goalPattern.MatchString(s.GoalID) && goalPattern.MatchString(s.ReviewID) && s.GoalID != s.ReviewID && s.GoalRevision > 0 && s.ReviewRevision > 0 && validGoalDigest(s.GoalDigest) && validGoalDigest(s.ReviewDigest)
}

func validIntegrationEntry(e integrationEntry) bool {
	if !validGoalOperationID(e.ID) || !e.Source.valid() || !validGoalOperationID(e.RepoID) || e.PolicyEpoch == 0 || e.Attempt == 0 {
		return false
	}
	for _, v := range []string{e.OperationDigest, e.FragmentDigest, e.ReviewerDigest, e.GroupDigest, e.PolicyDigest, e.ProfileDigest, e.AuthorityDigest} {
		if !validGoalDigest(v) {
			return false
		}
	}
	for _, v := range []string{e.SourceCommit, e.BaseCommit, e.Tree} {
		if len(v) != 40 && len(v) != 64 {
			return false
		}
		for _, c := range v {
			if !strings.ContainsRune("0123456789abcdef", c) {
				return false
			}
		}
	}
	return e.Scope != nil && e.SemanticKeys != nil && e.Dependencies != nil &&
		nativeStringSet(e.Scope, 32, nativeWrite) && nativeStringSet(e.SemanticKeys, 32, validGoalOperationID) &&
		nativeStringSet(e.Dependencies, 32, validGoalOperationID) && (e.Predecessor == "" || validGoalOperationID(e.Predecessor))
}

func decodeIntegration(b []byte, target any) error {
	if len(b) > integrationCaps().MaxFrameBytes || !utf8.Valid(b) || !nativeJSONScalars(b) {
		return errIntegration
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	raw, err := queueJSON(d, 1)
	if err != nil {
		return errIntegration
	}
	if _, err := d.Token(); err != io.EOF {
		return errIntegration
	}
	if json.Unmarshal(b, target) != nil {
		return errIntegration
	}
	canonical, err := json.Marshal(target)
	if err != nil {
		return errIntegration
	}
	c := json.NewDecoder(bytes.NewReader(canonical))
	c.UseNumber()
	want, err := queueJSON(c, 1)
	if err != nil || !reflect.DeepEqual(raw, want) {
		return errIntegration
	}
	return nil
}

type integrationEvent struct {
	Version  int                `json:"version"`
	Previous string             `json:"previous"`
	Input    string             `json:"input"`
	Request  integrationRequest `json:"request"`
}

func readIntegrationData(root *os.File) ([]byte, error) {
	b, err := readGoalPrivate(root, integrationLogName, int64(integrationCaps().MaxBytes))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errIntegration
	}
	return b, nil
}

func replayIntegration(data []byte) (*integrationProjection, error) {
	caps := integrationCaps()
	if len(data) > caps.MaxBytes {
		return nil, errIntegration
	}
	p := &integrationProjection{Entries: map[string]integrationRecord{}, Operations: map[string]integrationOperation{}}
	previous := ""
	for offset := 0; offset < len(data); {
		// Even a valid torn tail holds integration; nothing repairs or truncates it.
		if len(data)-offset < integrationHeaderBytes || p.Sequence >= uint64(caps.MaxEvents) {
			return nil, errIntegration
		}
		h := data[offset : offset+integrationHeaderBytes]
		if string(h[:4]) != "NIJF" || binary.BigEndian.Uint32(h[4:8]) != integrationVersion || binary.BigEndian.Uint64(h[8:16]) != p.Sequence+1 {
			return nil, errIntegration
		}
		head := sha256.Sum256(h[:20])
		if !bytes.Equal(head[:], h[20:52]) {
			return nil, errIntegration
		}
		n := int(binary.BigEndian.Uint32(h[16:20]))
		if n == 0 || n > caps.MaxFrameBytes || n > len(data)-offset-integrationHeaderBytes {
			return nil, errIntegration
		}
		payload := data[offset+integrationHeaderBytes : offset+integrationHeaderBytes+n]
		sum := sha256.Sum256(payload)
		if !bytes.Equal(sum[:], h[52:84]) {
			return nil, errIntegration
		}
		var e integrationEvent
		if decodeIntegration(payload, &e) != nil || e.Version != integrationVersion || e.Previous != previous || e.Input != integrationDigestJSON(e.Request) {
			return nil, errIntegration
		}
		if _, err := applyIntegration(p, e.Request, e.Input); err != nil {
			return nil, errIntegration
		}
		previous = integrationDigest(payload)
		offset += integrationHeaderBytes + n
	}
	return p, nil
}

func applyIntegration(p *integrationProjection, r integrationRequest, input string) (integrationOperation, error) {
	if r.Version != integrationVersion || !validGoalOperationID(r.OperationID) || !validGoalOperationID(r.EntryID) || !validGoalDigest(input) {
		return integrationOperation{}, errIntegration
	}
	if _, exists := p.Operations[r.OperationID]; exists {
		return integrationOperation{}, errIntegration
	}
	old, exists := p.Entries[r.EntryID]
	if old.Revision != r.ExpectedRevision {
		return integrationOperation{}, errIntegrationStale
	}
	if old.Revision >= uint64(integrationCaps().MaxEvents) {
		return integrationOperation{}, errIntegration
	}
	// Every kind is closed over its relevant fields; no silently ignored payload.
	if (r.Entry != nil) != (r.Kind == "submit" || r.Kind == "revise") || (r.Seal != nil) != (r.Kind == "seal") || (r.Observation != nil) != (r.Kind == "observe") || (r.Effect != "") != (r.Kind == "intent") || (r.State != "") != (r.Kind == "hold") {
		return integrationOperation{}, errIntegration
	}
	next := old
	switch r.Kind {
	case "submit":
		if exists || len(p.Entries) >= integrationCaps().MaxEntries || !validIntegrationEntry(*r.Entry) || r.Entry.ID != r.EntryID {
			return integrationOperation{}, errIntegration
		}
		e := *r.Entry
		if e.Predecessor == "" {
			if e.Attempt != 1 {
				return integrationOperation{}, errIntegration
			}
		} else {
			prior, ok := p.Entries[e.Predecessor]
			if !ok || prior.Pending != "" || (prior.State != "parked" && prior.State != "rejected") || prior.Entry.Source.GoalID != e.Source.GoalID || prior.Entry.RepoID != e.RepoID || e.Attempt != prior.Entry.Attempt+1 {
				return integrationOperation{}, errIntegration
			}
		}
		for _, dep := range e.Dependencies {
			if dep == e.ID {
				return integrationOperation{}, errIntegration
			}
			if _, ok := p.Entries[dep]; !ok {
				return integrationOperation{}, errIntegration
			}
		}
		next = integrationRecord{Entry: e, State: "submitted"}
	case "revise":
		if !exists || old.Pending != "" || old.State == "rejected" || !validIntegrationEntry(*r.Entry) || r.Entry.ID != r.EntryID {
			return integrationOperation{}, errIntegration
		}
		if r.Entry.Source.GoalID != old.Entry.Source.GoalID || r.Entry.RepoID != old.Entry.RepoID || r.Entry.Attempt != old.Entry.Attempt || r.Entry.Predecessor != old.Entry.Predecessor || !reflect.DeepEqual(r.Entry.Dependencies, old.Entry.Dependencies) {
			return integrationOperation{}, errIntegration
		}
		next.Entry, next.State, next.Seal = *r.Entry, "submitted", nil
	case "seal":
		if !exists || old.Pending != "" || old.State != "submitted" || !old.Entry.Source.ReviewVerified || r.Seal.EntryDigest != integrationDigestJSON(old.Entry) || r.Seal.ReviewDigest != old.Entry.Source.ReviewDigest || r.Seal.ReviewerDigest != old.Entry.ReviewerDigest {
			return integrationOperation{}, errIntegration
		}
		next.Seal, next.State = r.Seal, "reviewed"
	case "intent":
		if !exists || old.Pending != "" || !integrationEligible(p, r.EntryID) {
			return integrationOperation{}, errIntegration
		}
		switch r.Effect {
		case "prepare", "gate", "promote":
		default:
			return integrationOperation{}, errIntegration
		}
		next.Pending = r.OperationID
	case "observe":
		o := r.Observation
		intent, ok := p.Operations[o.OperationID]
		if !exists || !ok || old.Pending != o.OperationID || intent.EntryID != r.EntryID || intent.IntentDigest == "" || intent.IntentDigest != o.IntentDigest || intent.Observation != nil || !validGoalDigest(o.ResultDigest) {
			return integrationOperation{}, errIntegration
		}
		switch o.Outcome {
		case "settled", "failed":
			next.Pending = ""
		case "uncertain":
		default:
			return integrationOperation{}, errIntegration
		}
		// Uncertain observations preserve ownership; a later explicit observation
		// may settle the same intent, but no new effect may start in the meantime.
		if o.Outcome != "uncertain" {
			intent.Observation = o
			p.Operations[o.OperationID] = intent
		}
		if o.Outcome == "uncertain" {
			next.State = "uncertain"
		}
		if o.Outcome == "failed" {
			next.State = "parked"
		}
		if o.Outcome == "settled" {
			next.State = "reviewed"
		}
	case "hold":
		if !exists || old.State == "rejected" || (r.State != "parked" && r.State != "rejected" && r.State != "uncertain") || (old.Pending != "" && r.State != "uncertain") {
			return integrationOperation{}, errIntegration
		}
		next.State = r.State
	default:
		return integrationOperation{}, errIntegration
	}
	next.Revision = old.Revision + 1
	p.Sequence++
	op := integrationOperation{Input: input, Sequence: p.Sequence, EntryID: r.EntryID, Revision: next.Revision}
	if r.Kind == "intent" {
		op.IntentDigest = input
	}
	p.Entries[r.EntryID], p.Operations[r.OperationID] = next, op
	return op, nil
}

// No receipt here is authenticated. Dependency-bearing work remains ineligible
// until the later anchored-acceptance component can establish its predecessors.
func integrationEligible(p *integrationProjection, id string) bool {
	r, ok := p.Entries[id]
	return ok && r.State == "reviewed" && r.Pending == "" && r.Seal != nil && r.Seal.EntryDigest == integrationDigestJSON(r.Entry) && len(r.Entry.Dependencies) == 0
}

func openIntegrationLock(root *os.File) (*controlFileLock, error) {
	fd, err := unix.Openat(int(root.Fd()), "integration.lock", unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, errIntegration
	}
	f := os.NewFile(uintptr(fd), "integration-lock")
	if checkLaneControlFile(fd) != nil {
		f.Close()
		return nil, errIntegration
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, errIntegrationLocked
		}
		return nil, errIntegration
	}
	return &controlFileLock{File: f}, nil
}

func encodeIntegrationFrame(sequence uint64, payload []byte) []byte {
	b := make([]byte, integrationHeaderBytes+len(payload))
	copy(b, "NIJF")
	binary.BigEndian.PutUint32(b[4:8], integrationVersion)
	binary.BigEndian.PutUint64(b[8:16], sequence)
	binary.BigEndian.PutUint32(b[16:20], uint32(len(payload)))
	head, body := sha256.Sum256(b[:20]), sha256.Sum256(payload)
	copy(b[20:52], head[:])
	copy(b[52:84], body[:])
	copy(b[84:], payload)
	return b
}

func appendIntegration(path string, r integrationRequest, fault integrationIO) (integrationOperation, error) {
	root, err := openLaneControlRoot(path)
	if err != nil {
		return integrationOperation{}, errIntegration
	}
	defer root.Close()
	lock, err := openIntegrationLock(root)
	if err != nil {
		return integrationOperation{}, err
	}
	defer lock.Close()
	data, err := readIntegrationData(root)
	if err != nil {
		return integrationOperation{}, err
	}
	p, err := replayIntegration(data)
	if err != nil {
		return integrationOperation{}, err
	}
	input := integrationDigestJSON(r)
	if op, ok := p.Operations[r.OperationID]; ok {
		if op.Input != input {
			return integrationOperation{}, errIntegration
		}
		// A saved operation is not an effect-success receipt. Confirm durability
		// again before acknowledging the original journal identity.
		if err := syncIntegration(root, fault); err != nil {
			return integrationOperation{}, errIntegrationUnconfirmed
		}
		return op, nil
	}
	if p.Sequence >= uint64(integrationCaps().MaxEvents) {
		return integrationOperation{}, errIntegration
	}
	op, err := applyIntegration(p, r, input)
	if err != nil {
		return integrationOperation{}, err
	}
	previous := ""
	if len(data) != 0 {
		// Derive the previous payload from the validated frame chain.
		for offset := 0; offset < len(data); {
			n := int(binary.BigEndian.Uint32(data[offset+16 : offset+20]))
			previous = integrationDigest(data[offset+84 : offset+84+n])
			offset += 84 + n
		}
	}
	payload, err := json.Marshal(integrationEvent{Version: 1, Previous: previous, Input: input, Request: r})
	if err != nil || len(payload) > integrationCaps().MaxFrameBytes {
		return integrationOperation{}, errIntegration
	}
	frame := encodeIntegrationFrame(op.Sequence, payload)
	if len(data)+len(frame) > integrationCaps().MaxBytes {
		return integrationOperation{}, errIntegration
	}
	// Validate the complete candidate using the exact replay predicate.
	if _, err := replayIntegration(append(append([]byte(nil), data...), frame...)); err != nil {
		return integrationOperation{}, err
	}
	if fault.check("before_append") != nil {
		return integrationOperation{}, errIntegration
	}
	fd, err := unix.Openat(int(root.Fd()), integrationLogName, unix.O_WRONLY|unix.O_APPEND|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0600)
	if err != nil {
		return integrationOperation{}, errIntegration
	}
	f := os.NewFile(uintptr(fd), "integration-journal")
	defer f.Close()
	st, err := f.Stat()
	if err != nil || checkLaneControlFile(fd) != nil || st.Size() != int64(len(data)) {
		return integrationOperation{}, errIntegration
	}
	if n, err := f.Write(frame); err != nil || n != len(frame) {
		return integrationOperation{}, errIntegrationUnconfirmed
	}
	if fault.check("journal_sync") != nil || f.Sync() != nil {
		return integrationOperation{}, errIntegrationUnconfirmed
	}
	if fault.check("directory_sync") != nil || root.Sync() != nil {
		return integrationOperation{}, errIntegrationUnconfirmed
	}
	return op, nil
}

func syncIntegration(root *os.File, fault integrationIO) error {
	fd, err := unix.Openat(int(root.Fd()), integrationLogName, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return errIntegration
	}
	f := os.NewFile(uintptr(fd), "integration-confirm")
	defer f.Close()
	if checkLaneControlFile(fd) != nil || fault.check("journal_sync") != nil || f.Sync() != nil || fault.check("directory_sync") != nil || root.Sync() != nil {
		return errIntegrationUnconfirmed
	}
	return nil
}
