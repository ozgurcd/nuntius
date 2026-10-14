package nuntius

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var igStart = time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)

func igStore(t *testing.T) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal("resolve temporary store")
	}
	if os.Chmod(p, 0700) != nil {
		t.Fatal("private temporary store")
	}
	return p
}

func igSnapshot(at time.Time) idleGuardSnapshot {
	return idleGuardSnapshot{At: at, Lanes: []idleGuardLane{{ID: "codex-1", Kind: "codex", Capacity: "free"}}, Queue: queueSnapshot{Status: "ok", Version: 1, Updated: at.Format(time.RFC3339), Goals: []queueGoal{{ID: "DOCS", State: "ready", LaneKinds: []string{"codex"}}}}}
}

type igTransport struct {
	notices []idleGuardNotice
	before  func(idleGuardNotice)
	fail    bool
}

func (n *igTransport) Notify(_ context.Context, v idleGuardNotice) error {
	if n.before != nil {
		n.before(v)
	}
	n.notices = append(n.notices, v)
	if n.fail {
		return errors.New("offline transport failure")
	}
	return nil
}

func igOpen(t *testing.T, store string, n idleGuardNotifier, ops idleGuardIO) *idleGuardObserver {
	t.Helper()
	o, err := newIdleGuard(store, n, ops)
	if err != nil {
		t.Fatal("observer did not open")
	}
	t.Cleanup(func() { _ = o.close() })
	return o
}

func igSample(t *testing.T, o *idleGuardObserver, s idleGuardSnapshot) {
	t.Helper()
	if o.sample(context.Background(), s) != nil {
		t.Fatal("sample failed")
	}
}

func TestIdleGuardNoModelBoundaryAndAcknowledgement(t *testing.T) {
	store := igStore(t)
	n := &igTransport{}
	o := igOpen(t, store, n, idleGuardIO{})
	n.before = func(v idleGuardNotice) {
		r := readIdleGuard(store, []string{"codex-1"}, igStart, igStart.Add(5*time.Minute))
		if r.Coverage != "known" || len(v.IncidentID) < 10 || v.Candidates != 1 {
			t.Error("notice preceded durable evidence or leaked untyped input")
		}
	}
	for i := 0; i < 5; i++ {
		igSample(t, o, igSnapshot(igStart.Add(time.Duration(i)*time.Minute)))
	}
	if len(n.notices) != 0 {
		t.Fatal("incident before N")
	}
	igSample(t, o, igSnapshot(igStart.Add(5*time.Minute)))
	if len(n.notices) != 1 {
		t.Fatal("no incident at N without a model or browser")
	}
	if h := readIdleGuardHealth(store, igStart.Add(5*time.Minute)); h.OpenIncidents != 1 {
		t.Fatal("acknowledgement resolved an ongoing violation")
	}
	_ = o.close()
	o = igOpen(t, store, n, idleGuardIO{})
	igSample(t, o, igSnapshot(igStart.Add(6*time.Minute)))
	igSample(t, o, igSnapshot(igStart.Add(7*time.Minute)))
	if len(n.notices) != 1 {
		t.Fatal("acknowledgement or restart repeated/resolved ongoing incident")
	}
	r := readIdleGuard(store, []string{"codex-1"}, igStart, igStart.Add(7*time.Minute))
	if len(r.Lanes) != 1 || r.Lanes[0].UnexplainedIdleSeconds < 300 {
		t.Fatal("ongoing violation disappeared after acknowledgement")
	}
}

func TestIdleGuardCandidateCompletenessAndLocalHold(t *testing.T) {
	s := igSnapshot(igStart)
	s.Queue.Goals = append([]queueGoal{{ID: "BLOCKED", State: "blocked", NeedsOwner: true, Order: 0}}, s.Queue.Goals...)
	if p := classifyIdleGuard(s); len(p) != 1 || p[0].Category != idleGuardUnexplained || p[0].Candidates != 1 {
		t.Fatal("blocked top or unrelated owner hold hid lower ready hint")
	}
	s.Proofs = []idleGuardProof{igProof("OWNER_HOLD_ALL", "BLOCKED")}
	if p := classifyIdleGuard(s); p[0].Category != idleGuardUnexplained {
		t.Error("unrelated owner proof hid independent hint")
	}
	s.Queue.Goals = append(s.Queue.Goals, queueGoal{ID: "LOWER", State: "ready", LaneKinds: []string{"codex"}})
	s.Proofs = []idleGuardProof{igProof("OWNER_HOLD_ALL", "DOCS")}
	s.Proofs[0].Observed = igStart.Add(-time.Minute)
	if p := classifyIdleGuard(s); p[0].Category != idleGuardUnexplained {
		t.Error("stale top-candidate evidence hid lower safe hint")
	}
	s.Proofs = []idleGuardProof{igProof("OWNER_HOLD_ALL", "DOCS")}
	if p := classifyIdleGuard(s); p[0].Category != idleGuardUnexplained {
		t.Fatal("partial exemption hid lower safe candidate")
	}
}

func igProof(code, candidate string) idleGuardProof {
	return idleGuardProof{Code: code, Lane: "codex-1", Candidate: candidate, EvidenceID: strings.Repeat("a", 64), Observed: igStart, Expires: igStart.Add(time.Minute), Revision: 1, verified: true, ownerAuthenticated: true, heavy: true, loadSamples: 3, loadPerCPU: 1.1}
}

func TestIdleGuardClosedReasons(t *testing.T) {
	for _, code := range []string{"OWNER_HOLD_ALL", "LOAD_ABOVE_LIMIT", "CLAIM_HELD", "FILE_CONFLICT"} {
		t.Run(code, func(t *testing.T) {
			s := igSnapshot(igStart)
			s.Proofs = []idleGuardProof{igProof(code, "DOCS")}
			p := classifyIdleGuard(s)
			if p[0].Category != idleGuardPermitted || p[0].Reason != code || len(p[0].Coverage) != 1 {
				t.Fatal("valid scoped reason refused")
			}
			for _, bad := range []string{"stale", "future", "wrong-lane", "wrong-candidate", "unverified", "expired", "unattributed", "no-policy", "no-evidence", "light", "load-low", "load-inf"} {
				t.Run(bad, func(t *testing.T) {
					v := igProof(code, "DOCS")
					switch bad {
					case "stale":
						v.Observed = igStart.Add(-31 * time.Second)
					case "future":
						v.Observed = igStart.Add(time.Second)
					case "wrong-lane":
						v.Lane = "codex-2"
					case "wrong-candidate":
						v.Candidate = "OTHER"
					case "unverified":
						v.verified = false
					case "expired":
						v.Expires = igStart
					case "unattributed":
						v.ownerAuthenticated = false
					case "no-policy":
						v.Revision = 0
					case "no-evidence":
						v.EvidenceID = ""
					case "light":
						v.heavy = false
					case "load-low":
						v.loadPerCPU = 0.5
					case "load-inf":
						v.loadPerCPU = math.Inf(1)
					}
					if (bad == "unattributed" && code != "OWNER_HOLD_ALL") || ((bad == "light" || bad == "load-low" || bad == "load-inf") && code != "LOAD_ABOVE_LIMIT") {
						return
					}
					s.Proofs = []idleGuardProof{v}
					want := idleGuardUnknown
					if bad == "wrong-candidate" || bad == "light" || bad == "load-low" {
						want = idleGuardUnexplained
					}
					if p := classifyIdleGuard(s); p[0].Category != want {
						t.Fatalf("proof disposition = %s, want %s", p[0].Category, want)
					}
				})
			}
		})
	}
	for _, code := range []string{"NO_READY_GOAL", "LANE_QUARANTINED", "NO_GRANT", "owner said wait", ""} {
		s := igSnapshot(igStart)
		s.Proofs = []idleGuardProof{igProof(code, "DOCS")}
		if p := classifyIdleGuard(s); p[0].Category != idleGuardUnknown {
			t.Fatal("unknown or free-text reason accepted")
		}
	}
	s := igSnapshot(igStart)
	s.Queue.Goals = append(s.Queue.Goals, queueGoal{ID: "LIGHT", State: "ready", LaneKinds: []string{"codex"}})
	s.Proofs = []idleGuardProof{igProof("LOAD_ABOVE_LIMIT", "DOCS")}
	if p := classifyIdleGuard(s); p[0].Category != idleGuardUnexplained {
		t.Fatal("heavy load hid light work")
	}
}

func TestIdleGuardCapacityAndMissingInput(t *testing.T) {
	for _, tc := range []struct {
		capacity string
		want     idleGuardCategory
	}{{"free", idleGuardUnexplained}, {"running", idleGuardRunning}, {"reserved", idleGuardUnknown}, {"starting", idleGuardUnknown}, {"uncertain", idleGuardUnknown}, {"held", idleGuardUnknown}, {"recovery", idleGuardUnknown}} {
		s := igSnapshot(igStart)
		s.Lanes[0].Capacity = tc.capacity
		if p := classifyIdleGuard(s); p[0].Category != tc.want {
			t.Error("capacity classification failed:", tc.capacity)
		}
	}
	for _, status := range []string{"missing", "unavailable", "invalid", "disabled", "not_configured"} {
		s := igSnapshot(igStart)
		s.Queue.Status = status
		if p := classifyIdleGuard(s); p[0].Category != idleGuardUnknown {
			t.Error("missing input earned known time")
		}
	}
	s := igSnapshot(igStart)
	s.Queue.Updated = igStart.Add(-121 * time.Second).Format(time.RFC3339)
	if p := classifyIdleGuard(s); p[0].Category != idleGuardUnknown {
		t.Fatal("stale Queue earned known time")
	}
	s = igSnapshot(igStart)
	s.Queue.Goals = nil
	if p := classifyIdleGuard(s); p[0].Category != idleGuardBlocked {
		t.Fatal("complete empty Queue is inventory")
	}
}

func TestIdleGuardControllerFence(t *testing.T) {
	store := igStore(t)
	o := igOpen(t, store, nil, idleGuardIO{})
	if other, err := newIdleGuard(store, nil, idleGuardIO{}); err == nil {
		_ = other.close()
		t.Fatal("second controller acquired store")
	}
	_ = o.close()
	other := igOpen(t, store, nil, idleGuardIO{})
	igSample(t, other, igSnapshot(igStart))
}

func TestIdleGuardCrashCommitsAndRestartGrace(t *testing.T) {
	for _, stage := range []string{"interval_commit", "interval_synced", "incident_commit", "incident_synced", "receipt_commit"} {
		t.Run(stage, func(t *testing.T) {
			store := igStore(t)
			n := &igTransport{}
			armed := false
			o := igOpen(t, store, n, idleGuardIO{fault: func(at string) error {
				if armed && at == stage {
					return errors.New("simulated crash")
				}
				return nil
			}})
			if strings.HasPrefix(stage, "interval") {
				armed = true
				_ = o.sample(context.Background(), igSnapshot(igStart))
			} else {
				for i := 0; i < 5; i++ {
					igSample(t, o, igSnapshot(igStart.Add(time.Duration(i)*time.Minute)))
				}
				armed = true
				_ = o.sample(context.Background(), igSnapshot(igStart.Add(5*time.Minute)))
			}
			if stage != "receipt_commit" && len(n.notices) != 0 {
				t.Fatal("notification escaped failed pre-notify persistence")
			}
			_ = o.close()
			o = igOpen(t, store, n, idleGuardIO{})
			for i := 5; i <= 11; i++ {
				igSample(t, o, igSnapshot(igStart.Add(time.Duration(i)*time.Minute)))
			}
			if len(n.notices) == 0 {
				t.Fatal("crash erased grace or incident")
			}
			for _, v := range n.notices {
				if v.IncidentID != n.notices[0].IncidentID {
					t.Fatal("one continuous violation acquired multiple incidents")
				}
			}
		})
	}
}

func TestIdleGuardStorageFaultsAndNoWritesOnRead(t *testing.T) {
	store := igStore(t)
	r := readIdleGuard(store, []string{"codex-1"}, igStart, igStart.Add(time.Hour))
	entries, _ := os.ReadDir(store)
	if len(entries) != 0 || r.Coverage != "unknown" {
		t.Fatal("read created monitoring records or invented history")
	}
	n := &igTransport{}
	o := igOpen(t, store, n, idleGuardIO{fault: func(string) error { return errors.New("disk full") }})
	if o.sample(context.Background(), igSnapshot(igStart)) == nil || len(n.notices) != 0 {
		t.Fatal("disk full was green or sent a notice")
	}
	_ = o.close()
	if os.WriteFile(filepath.Join(store, "idle-guard.journal"), []byte("corrupt\n"), 0600) != nil {
		t.Fatal("corrupt fixture")
	}
	if other, err := newIdleGuard(store, n, idleGuardIO{}); err == nil {
		_ = other.close()
		t.Fatal("corrupt journal repaired or accepted")
	}
	before, _ := os.ReadFile(filepath.Join(store, "idle-guard.journal"))
	r = readIdleGuard(store, []string{"codex-1"}, igStart, igStart.Add(time.Hour))
	after, _ := os.ReadFile(filepath.Join(store, "idle-guard.journal"))
	if r.Coverage != "unknown" || string(before) != string(after) {
		t.Fatal("corrupt read green or rewrote journal")
	}
}

func TestIdleGuardIndependentIntervalOracle(t *testing.T) {
	store := igStore(t)
	o := igOpen(t, store, nil, idleGuardIO{})
	// The independent oracle is the schedule's expected half-open duration in
	// seconds. It does not call the product classifier or aggregation helpers.
	for i := 0; i <= 8; i++ {
		s := igSnapshot(igStart.Add(time.Duration(i) * time.Minute))
		switch {
		case i < 2:
			s.Lanes[0].Capacity = "running"
		case i < 4:
			s.Proofs = []idleGuardProof{igProof("CLAIM_HELD", "DOCS")}
			s.Proofs[0].Observed = s.At
			s.Proofs[0].Expires = s.At.Add(time.Minute)
		case i < 6:
		case i < 8:
			s.Queue.Goals = nil
		default:
			s.Queue.Status = "missing"
		}
		igSample(t, o, s)
	}
	r := readIdleGuard(store, []string{"codex-1"}, igStart.Add(-time.Minute), igStart.Add(10*time.Minute))
	if len(r.Lanes) != 1 {
		t.Fatal("missing lane partition")
	}
	v := r.Lanes[0]
	if v.RunningSeconds != 120 || v.PermittedIdleSeconds != 120 || v.UnexplainedIdleSeconds != 120 || v.BlockedNoDemandSeconds != 120 || v.UnknownSeconds != 180 {
		t.Fatal("independent partition oracle mismatch")
	}
	if v.OpportunitySeconds != 240 || v.UnexplainedSeconds != 120 || v.ObservationRatio == nil || *v.ObservationRatio != 0.5 {
		t.Fatal("opportunity parent/subset mismatch")
	}
	r = readIdleGuard(store, []string{"other"}, igStart, igStart.Add(time.Hour))
	if r.Lanes[0].ObservationRatio != nil || r.Lanes[0].RatioDisplay != "N/A" || r.Lanes[0].UnknownSeconds != 3600 {
		t.Fatal("zero denominator or raw unknown lost")
	}
}

func TestIdleGuardGapRollbackAndTransportFailure(t *testing.T) {
	store := igStore(t)
	n := &igTransport{fail: true}
	o := igOpen(t, store, n, idleGuardIO{})
	igSample(t, o, igSnapshot(igStart))
	igSample(t, o, igSnapshot(igStart.Add(time.Minute)))
	igSample(t, o, igSnapshot(igStart.Add(10*time.Minute)))
	igSample(t, o, igSnapshot(igStart.Add(9*time.Minute)))
	igSample(t, o, igSnapshot(igStart.Add(11*time.Minute)))
	r := readIdleGuard(store, []string{"codex-1"}, igStart, igStart.Add(11*time.Minute))
	if r.Coverage != "unknown" || r.Lanes[0].UnknownSeconds != 600 || r.Lanes[0].UnexplainedIdleSeconds != 60 {
		t.Fatal("sleep or rollback earned known time")
	}
	for i := 12; i <= 18; i++ {
		igSample(t, o, igSnapshot(igStart.Add(time.Duration(i)*time.Minute)))
	}
	if len(n.notices) < 2 {
		t.Fatal("failed transport hid pending incident")
	}
	for _, v := range n.notices {
		if v.IncidentID != n.notices[0].IncidentID {
			t.Fatal("failed transport changed incident identity")
		}
	}
}
