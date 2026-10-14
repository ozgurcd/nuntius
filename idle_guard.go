package nuntius

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"slices"
	"sync"
	"time"
)

const idleGuardPeriod = time.Minute
const idleGuardGrace = 5 * time.Minute

type idleGuardCategory string

const (
	idleGuardRunning     idleGuardCategory = "running"
	idleGuardPermitted   idleGuardCategory = "permitted_idle"
	idleGuardUnexplained idleGuardCategory = "unexplained_idle"
	idleGuardBlocked     idleGuardCategory = "blocked_no_demand"
	idleGuardUnknown     idleGuardCategory = "unknown"
)

// Only trusted in-process producers can supply proofs. Portable Queue has no
// proof channel and cannot establish a claim, scope, work class or owner control.
type idleGuardProof struct {
	Code, Lane, Candidate, EvidenceID   string
	Observed, Expires                   time.Time
	Revision                            uint64
	verified, ownerAuthenticated, heavy bool
	loadSamples                         int
	loadPerCPU                          float64
}

type idleGuardLane struct {
	ID, Kind, Capacity string
	Controlled         bool
}

type idleGuardSnapshot struct {
	At     time.Time
	Lanes  []idleGuardLane
	Queue  queueSnapshot
	Proofs []idleGuardProof
	Fault  string
}

type idleGuardCoverage struct {
	Candidate, Evidence, Code string
}

type idleGuardPoint struct {
	Lane              string
	Category          idleGuardCategory
	Reason            string
	Candidates        int
	Coverage          []idleGuardCoverage
	UnknownCandidates []string
	IntervalID        string
	OpenedAt          time.Time
	IdleNanos         int64
	IncidentID        string
	Delivered         bool
	Open              bool
}

// The transport receives no title, reason text, path, goal, prompt or recipient.
type idleGuardNotice struct {
	IncidentID string
	Candidates int
}

type idleGuardNotifier interface {
	Notify(context.Context, idleGuardNotice) error
}

type idleGuardIO struct {
	fault func(string) error
}

type idleGuardObserver struct {
	mu             sync.Mutex
	store          *idleGuardStore
	notifier       idleGuardNotifier
	ops            idleGuardIO
	epoch          string
	last           time.Time // Retains monotonic time within this process; never serialized.
	frame          idleGuardFrame
	failed, closed bool
}

func classifyIdleGuard(s idleGuardSnapshot) []idleGuardPoint {
	out := make([]idleGuardPoint, 0, len(s.Lanes))
	updated, valid := queueTime(s.Queue.Updated)
	queueKnown := valid && s.Queue.Status == "ok" && s.Queue.Version == 1 && !updated.After(s.At) && s.At.Sub(updated) <= 2*idleGuardPeriod
	for _, lane := range s.Lanes {
		p := idleGuardPoint{Lane: lane.ID, Category: idleGuardUnknown, Reason: "capacity_unknown"}
		if !queueKnown || s.Fault != "" {
			p.Reason = "input_unavailable"
		} else if lane.Capacity == "running" {
			p.Category, p.Reason = idleGuardRunning, ""
		} else if lane.Capacity == "free" {
			var candidates []queueGoal
			for _, g := range s.Queue.Goals {
				if g.State == "ready" && !g.NeedsOwner && len(g.DependsOn) == 0 && (g.Lane == "" || g.Lane == lane.ID) && slices.Contains(g.LaneKinds, lane.Kind) {
					candidates = append(candidates, g)
				}
			}
			p.Candidates = len(candidates)
			p.Category, p.Reason = idleGuardBlocked, ""
			if len(candidates) == 0 && lane.Controlled {
				p.Category, p.Reason = idleGuardUnknown, "proof_unavailable"
			}
			if len(candidates) > 0 {
				uncovered, invalid := false, false
				primary := 4
				for _, g := range candidates {
					covered, bad := false, false
					for _, proof := range s.Proofs {
						if proof.Candidate != g.ID {
							continue
						}
						if proof.Lane != lane.ID {
							bad = true
							continue
						}
						rank := idleGuardProofRank(proof, s.At)
						if rank < 0 {
							bad = true
							continue
						}
						covered = true
						primary = min(primary, rank)
						p.Coverage = append(p.Coverage, idleGuardCoverage{Candidate: idleGuardDigest(g.ID), Evidence: proof.EvidenceID, Code: proof.Code})
					}
					if !covered {
						if bad {
							invalid = true
							p.UnknownCandidates = append(p.UnknownCandidates, idleGuardDigest(g.ID))
						} else {
							uncovered = true
						}
					}
				}
				p.Category, p.Reason = idleGuardPermitted, ""
				switch {
				case uncovered && !lane.Controlled:
					p.Category = idleGuardUnexplained // Lower safe work defeats a partial excuse.
				case invalid || lane.Controlled && uncovered:
					p.Category, p.Reason = idleGuardUnknown, "proof_unavailable"
				case uncovered:
					p.Category = idleGuardUnexplained
				default:
					p.Reason = []string{"OWNER_HOLD_ALL", "LOAD_ABOVE_LIMIT", "CLAIM_HELD", "FILE_CONFLICT"}[primary]
				}
			}
		}
		out = append(out, p)
	}
	return out
}

func idleGuardProofRank(p idleGuardProof, at time.Time) int {
	if !p.verified || p.Revision == 0 || len(p.EvidenceID) != 64 || !queueDigest.MatchString(p.EvidenceID) || p.Observed.After(at) || at.Sub(p.Observed) > 30*time.Second || !p.Expires.After(at) {
		return -1
	}
	switch p.Code {
	case "OWNER_HOLD_ALL":
		if p.ownerAuthenticated {
			return 0
		}
	case "LOAD_ABOVE_LIMIT":
		if p.heavy && p.loadSamples >= 3 && !math.IsInf(p.loadPerCPU, 0) && !math.IsNaN(p.loadPerCPU) && p.loadPerCPU > 0.90 {
			return 1
		}
	case "CLAIM_HELD":
		return 2
	case "FILE_CONFLICT":
		return 3
	}
	return -1
}

func idleGuardDigest(s string) string { v := sha256.Sum256([]byte(s)); return hex.EncodeToString(v[:]) }

func (o *idleGuardObserver) sample(ctx context.Context, s idleGuardSnapshot) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed || o.failed || !idleGuardSnapshotValid(s) {
		return errIdleGuard
	}
	frame := idleGuardFrame{Version: 1, Sequence: o.frame.Sequence + 1, At: s.At.UTC(), Epoch: o.epoch, Lanes: classifyIdleGuard(s)}
	delta := time.Duration(0)
	if !o.frame.At.IsZero() {
		delta = s.At.Sub(o.frame.At)
		frame.GapUnknown = o.last.IsZero() || delta > 2*idleGuardPeriod
		// UTC establishes report boundaries; elapsed grace uses the process's
		// monotonic clock when it is available. Cross-process continuity is
		// deliberately unknown rather than asserting a surviving host boot.
		wallDelta := delta
		if !o.last.IsZero() {
			delta = s.At.Sub(o.last)
		}
		if !o.last.IsZero() && s.At.Sub(o.last) < 0 || delta < 0 {
			frame.At, frame.GapUnknown, frame.Fault = o.frame.At, true, "clock_rollback"
			for i := range frame.Lanes {
				frame.Lanes[i].Category, frame.Lanes[i].Reason = idleGuardUnknown, "clock_rollback"
			}
		} else if wallDelta < 0 {
			frame.At, frame.GapUnknown, frame.Fault = o.frame.At, true, "clock_rollback"
			for i := range frame.Lanes {
				frame.Lanes[i].Category, frame.Lanes[i].Reason = idleGuardUnknown, "clock_rollback"
			}
		} else if !o.last.IsZero() && (delta > 2*idleGuardPeriod || wallDelta-delta > 5*time.Second || delta-wallDelta > 5*time.Second) {
			frame.GapUnknown = true
		}
		if frame.GapUnknown && frame.Fault == "" {
			frame.Fault = "sampling_gap"
		}
	}
	newIncident := false
	for i := range frame.Lanes {
		p := &frame.Lanes[i]
		var previous idleGuardPoint
		for _, prev := range o.frame.Lanes {
			if prev.Lane == p.Lane {
				previous = prev
				break
			}
		}
		p.IntervalID, p.OpenedAt, p.IdleNanos, p.IncidentID, p.Delivered, p.Open = previous.IntervalID, previous.OpenedAt, previous.IdleNanos, previous.IncidentID, previous.Delivered, previous.Open
		if previous.Open && previous.Category == idleGuardUnexplained && !frame.GapUnknown && delta > 0 {
			p.IdleNanos += int64(delta)
		}
		if p.Open && p.IdleNanos >= int64(idleGuardGrace) && p.IncidentID == "" {
			p.IncidentID = p.IntervalID
			newIncident = true
		}
		if p.Category == idleGuardUnexplained && !p.Open {
			p.IntervalID = "ig-" + idleGuardDigest(fmt.Sprintf("%s/%d/%s", o.epoch, frame.Sequence, p.Lane))
			p.OpenedAt, p.IdleNanos, p.IncidentID, p.Delivered, p.Open = frame.At, 0, "", false, true
		} else if p.Category != idleGuardUnknown && p.Category != idleGuardUnexplained {
			p.Open = false
		}
	}
	stage := "interval"
	if newIncident {
		stage = "incident"
	}
	if err := o.store.append(frame, stage, o.ops); err != nil {
		o.failed = true
		return errIdleGuard
	}
	o.frame, o.last = frame, s.At
	// Persistence precedes any call. No production transport is installed in S1.
	if o.notifier == nil {
		return nil
	}
	for i := range o.frame.Lanes {
		p := o.frame.Lanes[i]
		if p.IncidentID == "" || p.Delivered {
			continue
		}
		if err := o.notifier.Notify(ctx, idleGuardNotice{IncidentID: p.IncidentID, Candidates: p.Candidates}); err != nil {
			continue
		}
		receipt := o.frame
		receipt.Lanes = append([]idleGuardPoint(nil), o.frame.Lanes...)
		receipt.Sequence++
		receipt.GapUnknown = false
		receipt.Lanes[i].Delivered = true
		if err := o.store.append(receipt, "receipt", o.ops); err != nil {
			o.failed = true
			return errIdleGuard
		}
		o.frame = receipt
	}
	return nil
}

func idleGuardSnapshotValid(s idleGuardSnapshot) bool {
	if s.At.IsZero() || len(s.Lanes) > 32 || len(s.Queue.Goals) > 500 || len(s.Proofs) > 2000 {
		return false
	}
	seen := map[string]bool{}
	for _, l := range s.Lanes {
		if !namePattern.MatchString(l.ID) || seen[l.ID] {
			return false
		}
		seen[l.ID] = true
	}
	return true
}

func (o *idleGuardObserver) close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil
	}
	o.closed = true
	return o.store.close()
}

type idleGuardLaneReport struct {
	Lane                                                         string
	RunningSeconds, PermittedIdleSeconds, UnexplainedIdleSeconds float64
	BlockedNoDemandSeconds, UnknownSeconds                       float64
	OpportunitySeconds, UnexplainedSeconds                       float64
	ObservationRatio                                             *float64
	RatioDisplay                                                 string
}

type idleGuardReport struct {
	Mode, Coverage, Fault string
	From, Until           time.Time
	Lanes                 []idleGuardLaneReport
}
