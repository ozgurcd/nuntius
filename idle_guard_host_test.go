package nuntius

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestIdleGuardModeAndPassiveClients(t *testing.T) {
	c, _ := fixture(t)
	c.IdleGuard = "dispatch_preapproved"
	if _, err := c.Validate(); err == nil {
		t.Error("unsupported execution mode accepted")
	}
	c.IdleGuard = "alert_only"
	store := igStore(t)
	config := filepath.Join(igStore(t), "lanes.json")
	b, _ := json.Marshal(c)
	if os.WriteFile(config, b, 0600) != nil {
		t.Fatal("save fixture config")
	}
	for _, args := range [][]string{{"status", "--json"}, {"agents", "--json"}, {"monitoring", "--json"}, {"mcp"}} {
		var out, errs bytes.Buffer
		if code := CLI(context.Background(), append([]string{"--config", config, "--store", store}, args...), strings.NewReader(""), &out, &errs); code != 0 {
			t.Error("passive CLI failed", args[0])
		}
		if _, err := os.Lstat(filepath.Join(store, idleGuardJournal)); !os.IsNotExist(err) {
			t.Fatal("passive CLI instantiated observer")
		}
	}
	c.OversightPath = filepath.Join(igStore(t), "OVERSIGHT.md")
	m, err := newReadManager(c, store)
	if err != nil {
		t.Fatal("dashboard read manager")
	}
	defer m.Close()
	w := &Web{manager: m}
	if _, err := w.dataContext(context.Background()); err != nil {
		t.Fatal("offline dashboard GET snapshot")
	}
	if _, err := os.Lstat(filepath.Join(store, idleGuardJournal)); !os.IsNotExist(err) {
		t.Fatal("dashboard GET instantiated observer")
	}
	for _, extra := range []string{`,"IDLE_GUARD":"alert_only"`, `,"idle_guard":null`, `,"idle_guard":"alert_only"`} {
		data := append(append([]byte(nil), b[:len(b)-1]...), []byte(extra+"}")...)
		if os.WriteFile(config, data, 0600) != nil {
			t.Fatal("config fault fixture")
		}
		if _, err := LoadConfig(config); err == nil {
			t.Error("ambiguous mode accepted")
		}
	}
}

func TestIdleGuardDaemonLifetimeSilentAndFenced(t *testing.T) {
	c, _ := fixture(t)
	c.IdleGuard = "alert_only"
	store := igStore(t)
	m, err := newReadManager(c, store)
	if err != nil {
		t.Fatal("read manager")
	}
	defer m.Close()
	n := &igTransport{}
	received := make(chan struct{}, 1)
	n.before = func(idleGuardNotice) { received <- struct{}{} }
	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if m.startIdleGuard(ctx, idleGuardHost{now: func() time.Time { return igStart }, ticks: ticks, snapshot: igSnapshot, notifier: n}) != nil {
		t.Fatal("daemon start")
	}
	other, err := newReadManager(c, store)
	if err != nil {
		t.Fatal("second manager")
	}
	defer other.Close()
	if other.startIdleGuard(ctx, idleGuardHost{}) == nil {
		t.Fatal("second daemon acquired observer fence")
	}
	for i := 1; i <= 5; i++ {
		select {
		case ticks <- igStart.Add(time.Duration(i) * time.Minute):
		case <-time.After(3 * time.Second):
			t.Fatal("daemon did not evaluate silent tick")
		}
	}
	select {
	case <-received:
	case <-time.After(3 * time.Second):
		t.Fatal("no daemon incident while model/browser silent")
	}
	cancel()
	_ = m.Close()
	o := igOpen(t, store, nil, idleGuardIO{})
	igSample(t, o, igSnapshot(igStart.Add(6*time.Minute)))
}

func TestIdleGuardAdapterEndedFreeButControlsRetain(t *testing.T) {
	c, _ := fixture(t)
	c.IdleGuard = "alert_only"
	store := igStore(t)
	m, err := NewManager(c, store)
	if err != nil {
		t.Fatal("manager")
	}
	defer m.Close()
	at := time.Now().UTC().Truncate(time.Second)
	snap := igSnapshot(at)
	snap.Queue.Goals[0].LaneKinds = []string{"codex"}
	b, _ := json.Marshal(map[string]any{"version": 1, "updated": at.Format(time.RFC3339), "goals": []map[string]any{{"id": "DOCS", "title": "Documentation", "size": "S", "lane_kinds": []string{"codex"}, "order": 1, "depends_on": []string{}, "needs_owner": false, "state": "ready", "created": at.Format(time.RFC3339), "updated": at.Format(time.RFC3339)}}})
	if os.WriteFile(filepath.Join(store, "queue.json"), b, 0600) != nil {
		t.Fatal("portable fixture")
	}
	end := at.Add(-time.Minute)
	r := Run{ID: strings.Repeat("a", 32), Lane: "codex", Kind: "codex", GoalID: "DOCS", SHA256: strings.Repeat("a", 64), Started: end.Add(-time.Minute), Ended: &end, State: "ended"}
	// save uses the repository's native metadata writer, no child is launched.
	if privateDir(filepath.Join(store, "runs", r.ID)) != nil {
		t.Fatal("ended run directory")
	}
	if err := m.save(&r); err != nil {
		t.Fatal("ended fixture")
	}
	s := m.idleGuardSnapshot(at)
	var lane idleGuardLane
	for _, v := range s.Lanes {
		if v.ID == "codex" {
			lane = v
		}
	}
	if s.Fault != "" || lane.Capacity != "free" {
		t.Fatal("ended lane was not free")
	}
	for _, hold := range []string{"recovery", "successor", "native-held"} {
		r.WaitingToResume = hold == "recovery"
		r.ContinueRunID = ""
		r.NativeEnd = nil
		if hold == "successor" {
			r.ContinueRunID = strings.Repeat("b", 32)
		}
		if hold == "native-held" {
			r.PID = 12345
			r.ProcessStart = "fixture"
			r.NativeEnd = &goalAttemptEnd{RunID: r.ID, Lane: r.Lane, PID: r.PID, ProcessStart: r.ProcessStart, Started: r.Started, Ended: *r.Ended, Outcome: "completed", Held: true, Pending: "recovery_pending"}
		}
		if m.save(&r) != nil {
			t.Fatal("held ended fixture")
		}
		s = m.idleGuardSnapshot(at)
		for _, v := range s.Lanes {
			if v.ID == "codex" && v.Capacity == "free" {
				t.Fatal("held ended lane was released", hold)
			}
		}
	}
	r.WaitingToResume = false
	r.ContinueRunID = ""
	r.NativeEnd = nil
	if m.save(&r) != nil {
		t.Fatal("settled fixture")
	}
	if _, err := m.setLaneControl(laneControlChange{Lane: "codex", Paused: true}, "cli"); err != nil {
		t.Fatal("pause fixture")
	}
	s = m.idleGuardSnapshot(at)
	for _, p := range classifyIdleGuard(s) {
		if p.Lane == "codex" && p.Category != idleGuardUnknown {
			t.Fatal("unattributed pause became exemption or cleared")
		}
	}
	controls, err := m.Status()
	if err != nil {
		t.Fatal("passive controls")
	}
	for _, v := range controls {
		if v.Lane == "codex" && !v.LaneControl.Paused {
			t.Fatal("observer cleared pause")
		}
	}
}

func TestIdleGuardNativeReservationRetainsNoChild(t *testing.T) {
	m, req, ctx := dispatchFixture(t)
	if _, err := m.dispatchGoal(req, ctx, dispatchOneFault("publication")); err == nil {
		t.Fatal("reservation fault was not reached")
	}
	_, data := dispatchRecord(t, m, req.GoalID)
	if len(data.Dispatches) != 1 {
		t.Fatal("no durable native reservation")
	}
	s := m.idleGuardSnapshot(time.Now())
	for _, l := range s.Lanes {
		if l.ID == req.Lane && l.Capacity == "free" {
			t.Fatal("durable reservation became free")
		}
	}
	dispatchStarts(t, m, 0)
}

func TestIdleGuardIncompleteRunInventoryIsUnknown(t *testing.T) {
	c, _ := fixture(t)
	store := igStore(t)
	m, err := NewManager(c, store)
	if err != nil {
		t.Fatal("manager")
	}
	defer m.Close()
	path := filepath.Join(store, "runs", strings.Repeat("c", 32))
	if privateDir(path) != nil || os.WriteFile(filepath.Join(path, "meta.json"), []byte("invalid"), 0600) != nil {
		t.Fatal("partial inventory fixture")
	}
	s := m.idleGuardSnapshot(time.Now())
	if s.Fault == "" {
		t.Fatal("display omission became a complete capacity inventory")
	}
	for _, l := range s.Lanes {
		if l.Capacity == "free" {
			t.Fatal("corrupt metadata freed a lane")
		}
	}
}

func TestIdleGuardFenceReplacementRefusesOldWriter(t *testing.T) {
	store := igStore(t)
	o := igOpen(t, store, nil, idleGuardIO{})
	igSample(t, o, igSnapshot(igStart))
	if os.Rename(filepath.Join(store, "idle-guard.lock"), filepath.Join(store, "old-fence")) != nil {
		t.Fatal("displaced inode fixture")
	}
	other := igOpen(t, store, nil, idleGuardIO{})
	if o.sample(context.Background(), igSnapshot(igStart.Add(time.Minute))) == nil {
		t.Fatal("displaced controller wrote through old fence")
	}
	igSample(t, other, igSnapshot(igStart.Add(time.Minute)))
}

func TestIdleGuardBoundedReaderContention(t *testing.T) {
	store := igStore(t)
	o := igOpen(t, store, nil, idleGuardIO{})
	f, err := os.Open(filepath.Join(store, idleGuardJournal))
	if err != nil {
		t.Fatal("reader fixture")
	}
	defer f.Close()
	if unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB) != nil {
		t.Fatal("reader lock")
	}
	defer unix.Flock(int(f.Fd()), unix.LOCK_UN)
	done := make(chan error, 1)
	go func() { done <- o.sample(context.Background(), igSnapshot(igStart)) }()
	select {
	case <-done:
		t.Fatal("brief passive reader contention stopped observer")
	case <-time.After(20 * time.Millisecond):
	}
	if unix.Flock(int(f.Fd()), unix.LOCK_UN) != nil {
		t.Fatal("reader release")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("observer did not recover after bounded reader")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("observer blocked after reader release")
	}
}

// RV3: hold a passive reader past the writer's entire lock budget. The
// failed sample must not earn grace, notify, or stop the next sample.
func TestIdleGuardLongReaderRecovery(t *testing.T) {
	store := igStore(t)
	n := &igTransport{}
	o := igOpen(t, store, n, idleGuardIO{})
	igSample(t, o, igSnapshot(igStart))
	interval := o.frame.Lanes[0].IntervalID
	f, err := os.Open(filepath.Join(store, idleGuardJournal))
	if err != nil {
		t.Fatal("reader fixture")
	}
	defer f.Close()
	if unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB) != nil {
		t.Fatal("reader lock")
	}
	defer unix.Flock(int(f.Fd()), unix.LOCK_UN)
	start := time.Now()
	err = o.sample(context.Background(), igSnapshot(igStart.Add(time.Minute)))
	if err == nil || time.Since(start) < time.Second {
		t.Fatal("fixture did not hold the reader past the lock budget")
	}
	if unix.Flock(int(f.Fd()), unix.LOCK_UN) != nil {
		t.Fatal("reader release")
	}
	if err = o.sample(context.Background(), igSnapshot(igStart.Add(2*time.Minute))); err != nil {
		t.Fatal("RV3: observer permanently failed after passive reader released")
	}
	if o.failed || !o.frame.GapUnknown || o.frame.Sequence != 2 || o.frame.Lanes[0].IdleNanos != 0 || o.frame.Lanes[0].IntervalID != interval || len(n.notices) != 0 {
		t.Fatal("contention gap was counted or changed incident delivery")
	}
	igSample(t, o, igSnapshot(igStart.Add(3*time.Minute)))
	if o.frame.GapUnknown || o.frame.Lanes[0].IdleNanos != int64(time.Minute) {
		t.Fatal("normal observation did not resume after unknown gap")
	}
	r := readIdleGuard(store, []string{"codex-1"}, igStart, igStart.Add(3*time.Minute))
	if r.Lanes[0].UnknownSeconds != 120 || r.Lanes[0].UnexplainedIdleSeconds != 60 {
		t.Fatal("reader timeout gap was not reported as unknown")
	}
}

func TestIdleGuardDaemonReaderTimeoutRecovery(t *testing.T) {
	c, _ := fixture(t)
	c.IdleGuard = "alert_only"
	store := igStore(t)
	m, err := newReadManager(c, store)
	if err != nil {
		t.Fatal("manager")
	}
	defer m.Close()
	ticks := make(chan time.Time)
	faults := make(chan struct{}, 1)
	blockedSample := make(chan struct{})
	proceed := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if m.startIdleGuard(ctx, idleGuardHost{now: func() time.Time { return igStart }, ticks: ticks, snapshot: func(at time.Time) idleGuardSnapshot {
		if at.Equal(igStart.Add(2 * time.Minute)) {
			close(blockedSample)
			<-proceed
		}
		return igSnapshot(at)
	}, onFault: func() { faults <- struct{}{} }}) != nil {
		t.Fatal("daemon")
	}
	// Acceptance of the next tick establishes completion of the first append.
	select {
	case ticks <- igStart.Add(time.Minute):
	case <-time.After(3 * time.Second):
		t.Fatal("first sample did not complete")
	}
	select {
	case ticks <- igStart.Add(2 * time.Minute):
	case <-time.After(3 * time.Second):
		t.Fatal("second sample did not complete")
	}
	<-blockedSample
	f, err := os.Open(filepath.Join(store, idleGuardJournal))
	if err != nil {
		t.Fatal("reader")
	}
	defer f.Close()
	if unix.Flock(int(f.Fd()), unix.LOCK_SH) != nil {
		t.Fatal("reader lock")
	}
	defer unix.Flock(int(f.Fd()), unix.LOCK_UN)
	close(proceed)
	select {
	case <-faults:
	case <-time.After(3 * time.Second):
		t.Fatal("contention fault hidden")
	}
	m.mu.Lock()
	fault := m.idleGuardFault
	m.mu.Unlock()
	if fault != "reader_contention" {
		t.Errorf("reader timeout fault = %s, want reader_contention", fault)
	}
	if unix.Flock(int(f.Fd()), unix.LOCK_UN) != nil {
		t.Fatal("reader release")
	}
	select {
	case ticks <- igStart.Add(3 * time.Minute):
	case <-time.After(3 * time.Second):
		t.Fatal("daemon stopped permanently after reader timeout")
	}
	// The following accepted tick proves recovery completed, without polling.
	select {
	case ticks <- igStart.Add(4 * time.Minute):
	case <-time.After(3 * time.Second):
		t.Fatal("daemon failed to finish recovery")
	}
	m.mu.Lock()
	fault = m.idleGuardFault
	m.mu.Unlock()
	if fault != "" {
		t.Fatal("transient contention fault did not clear")
	}
}

func TestIdleGuardCLIRepeatedReaderContention(t *testing.T) {
	c, _ := fixture(t)
	c.IdleGuard = "alert_only"
	store := igStore(t)
	m, err := newReadManager(c, store)
	if err != nil {
		t.Fatal("manager")
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		closed := make(chan struct{})
		go func() { _ = m.Close(); close(closed) }()
		select {
		case <-closed:
		case <-time.After(5 * time.Second):
			t.Error("Close did not return after repeated contention")
		}
	})
	ticks := make(chan time.Time)
	result := make(chan int, 1)
	var messages bytes.Buffer
	go func() {
		result <- m.observeCLI(ctx, idleGuardHost{now: func() time.Time { return igStart }, ticks: ticks, snapshot: igSnapshot}, &messages, &messages)
	}()
	tick := func(minute int) bool {
		t.Helper()
		select {
		case ticks <- igStart.Add(time.Duration(minute) * time.Minute):
			return true
		case <-time.After(5 * time.Second):
			t.Errorf("sample %d was not accepted", minute)
			return false
		}
	}
	if !tick(1) || !tick(2) {
		return
	}
	// The next sample cannot begin until we hold the passive reader lock.
	f, err := os.Open(filepath.Join(store, idleGuardJournal))
	if err != nil {
		t.Fatal("reader")
	}
	defer f.Close()
	fd := int(f.Fd())
	if unix.Flock(fd, unix.LOCK_SH) != nil {
		t.Fatal("reader lock")
	}
	defer unix.Flock(fd, unix.LOCK_UN)
	for minute := 3; minute <= 6; minute++ {
		if !tick(minute) {
			return
		}
	}
	if unix.Flock(fd, unix.LOCK_UN) != nil {
		t.Fatal("reader release")
	}
	if !tick(7) || !tick(8) {
		return
	}
	m.mu.Lock()
	fault := m.idleGuardFault
	m.mu.Unlock()
	if fault != "" {
		t.Error("successful recovery did not clear contention")
	}
	select {
	case <-result:
		t.Error("transient contention ended observe")
	default:
	}
	cancel()
	select {
	case code := <-result:
		if code != 0 {
			t.Error("cancelled observer returned failure")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("observe did not return on cancellation")
	}
	if !strings.Contains(messages.String(), "reader contention") || strings.Contains(messages.String(), "persistence unavailable") || strings.Contains(messages.String(), "preserve records") {
		t.Error("transient contention was mislabeled as a persistence failure")
	}
}

func TestIdleGuardCLITerminalFault(t *testing.T) {
	c, _ := fixture(t)
	c.IdleGuard = "alert_only"
	m, err := newReadManager(c, igStore(t))
	if err != nil {
		t.Fatal("manager")
	}
	defer m.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan int, 1)
	var messages bytes.Buffer
	go func() {
		result <- m.observeCLI(ctx, idleGuardHost{now: func() time.Time { return igStart }, snapshot: func(time.Time) idleGuardSnapshot { return idleGuardSnapshot{} }}, &messages, &messages)
	}()
	select {
	case code := <-result:
		if code != 1 || !strings.Contains(messages.String(), "persistence unavailable") || !strings.Contains(messages.String(), "preserve records") {
			t.Error("terminal persistence fault lost its failure status or cause")
		}
	case <-time.After(5 * time.Second):
		cancel()
		<-result
		t.Error("terminal fault did not end observe")
	}
}

func TestIdleGuardByteLimitStatus(t *testing.T) {
	store := igStore(t)
	var journal bytes.Buffer
	frame := idleGuardFrame{Version: 1, Epoch: "boot-" + strings.Repeat("0", 32)}
	for i := 0; i < 6; i++ {
		frame.Lanes = append(frame.Lanes, idleGuardPoint{Lane: "codex-" + string(rune('1'+i)), Category: idleGuardUnexplained, Candidates: 1, IntervalID: "ig-" + strings.Repeat("b", 64), IncidentID: "ig-" + strings.Repeat("b", 64), OpenedAt: igStart, Open: true})
	}
	for seq := 1; seq <= idleGuardMaxFrames; seq++ {
		frame.Sequence, frame.At = uint64(seq), igStart.Add(time.Duration(seq)*time.Minute)
		payload, err := json.Marshal(frame)
		if err != nil {
			t.Fatal("frame fixture")
		}
		if journal.Len()+36+len(payload) > idleGuardMaxBytes-512 {
			break
		}
		hdr := make([]byte, 36)
		binary.BigEndian.PutUint32(hdr, uint32(len(payload)))
		digest := sha256.Sum256(payload)
		copy(hdr[4:], digest[:])
		journal.Write(hdr)
		journal.Write(payload)
	}
	if os.WriteFile(filepath.Join(store, idleGuardJournal), journal.Bytes(), 0600) != nil {
		t.Fatal("journal fixture")
	}
	o := igOpen(t, store, nil, idleGuardIO{})
	at := o.frame.At
	limited := false
	for i := 1; i <= 20; i++ {
		at = at.Add(time.Minute)
		if o.sample(context.Background(), igSnapshot(at)) != nil {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("fixture never reached byte saturation")
	}
	_ = o.close()
	h := readIdleGuardHealth(store, at.Add(time.Hour))
	if h.Coverage != "unknown" || h.Fault != "storage_limit" || h.OpenIncidents == 0 {
		t.Fatalf("byte cap fault = %s, want storage_limit", h.Fault)
	}
	r := readIdleGuard(store, []string{"codex-1"}, at.Add(-time.Minute), at.Add(time.Hour))
	if r.Fault != "storage_limit" || r.Coverage != "unknown" {
		t.Fatal("monitoring report hid byte saturation")
	}
	st, err := os.Stat(filepath.Join(store, idleGuardJournal))
	if err != nil || st.Size() > idleGuardMaxBytes {
		t.Fatal("journal exceeded byte cap")
	}
	if other, err := newIdleGuard(store, nil, idleGuardIO{}); err != errIdleGuardLimit {
		if other != nil {
			_ = other.close()
		}
		t.Fatal("restart lost durable storage-limit cause")
	}
}

func TestIdleGuardGeneratedWindowOracle(t *testing.T) {
	store := igStore(t)
	o := igOpen(t, store, nil, idleGuardIO{})
	for i := 0; i <= 200; i++ {
		s := igSnapshot(igStart.Add(time.Duration(i) * time.Minute))
		switch i % 5 {
		case 0:
			s.Lanes[0].Capacity = "running"
		case 1:
			s.Proofs = []idleGuardProof{igProof("FILE_CONFLICT", "DOCS")}
			s.Proofs[0].Observed = s.At
			s.Proofs[0].Expires = s.At.Add(time.Minute)
		case 2:
		case 3:
			s.Queue.Goals = nil
		case 4:
			s.Queue.Status = "missing"
		}
		igSample(t, o, s)
	}
	for window := 0; window < 50; window++ {
		left := (window * 73) % 11000
		right := left + 1 + (window*19)%999
		var want [5]float64
		// Independent elementary interval intersection, in integer seconds.
		for minute := 0; minute < 200; minute++ {
			lo, hi := minute*60, (minute+1)*60
			if lo < left {
				lo = left
			}
			if hi > right {
				hi = right
			}
			if hi > lo {
				want[minute%5] += float64(hi - lo)
			}
		}
		r := readIdleGuard(store, []string{"codex-1"}, igStart.Add(time.Duration(left)*time.Second), igStart.Add(time.Duration(right)*time.Second))
		v := r.Lanes[0]
		got := [5]float64{v.RunningSeconds, v.PermittedIdleSeconds, v.UnexplainedIdleSeconds, v.BlockedNoDemandSeconds, v.UnknownSeconds}
		if got != want {
			t.Fatal("generated half-open partition disagrees with independent oracle", window)
		}
		if v.OpportunitySeconds != want[0]+want[2] || v.UnexplainedSeconds != want[2] {
			t.Fatal("generated opportunity parent or subset wrong")
		}
	}
}

func TestIdleGuardStaleStatusAndFaultLiveness(t *testing.T) {
	store := igStore(t)
	o := igOpen(t, store, nil, idleGuardIO{})
	igSample(t, o, igSnapshot(igStart))
	_ = o.close()
	if h := readIdleGuardHealth(store, igStart.Add(121*time.Second)); h.Coverage != "unknown" || h.Fault != "heartbeat_unavailable" {
		t.Fatal("stale heartbeat was healthy")
	}
	c, _ := fixture(t)
	c.IdleGuard = "alert_only"
	m, err := newReadManager(c, igStore(t))
	if err != nil {
		t.Fatal("manager")
	}
	defer m.Close()
	failed := make(chan struct{}, 1)
	if m.startIdleGuard(context.Background(), idleGuardHost{now: func() time.Time { return igStart }, snapshot: func(at time.Time) idleGuardSnapshot { return idleGuardSnapshot{} }, onFault: func() { failed <- struct{}{} }}) != nil {
		t.Fatal("fault host")
	}
	select {
	case <-failed:
	case <-time.After(3 * time.Second):
		t.Fatal("persistence fault was not surfaced")
	}
	m.mu.Lock()
	fault, closed := m.idleGuardFault, m.closed
	m.mu.Unlock()
	if fault == "" || closed {
		t.Fatal("fault was hidden or terminated manager")
	}
}

func TestIdleGuardJournalTruncationLinksAndLimits(t *testing.T) {
	store := igStore(t)
	o := igOpen(t, store, nil, idleGuardIO{})
	igSample(t, o, igSnapshot(igStart))
	_ = o.close()
	b, err := os.ReadFile(filepath.Join(store, idleGuardJournal))
	if err != nil {
		t.Fatal("journal fixture")
	}
	corrupt := bytes.Replace(b, []byte(`"Candidates":1`), []byte(`"Candidates":2`), 1)
	if bytes.Equal(b, corrupt) {
		t.Fatal("checksum fixture mutation missed")
	}
	payloadStore := igStore(t)
	if os.WriteFile(filepath.Join(payloadStore, idleGuardJournal), corrupt, 0600) != nil {
		t.Fatal("checksum fixture")
	}
	if other, e := newIdleGuard(payloadStore, nil, idleGuardIO{}); e == nil {
		_ = other.close()
		t.Error("valid-JSON payload corruption was accepted")
	}
	for _, cut := range []int{1, 4, 35, len(b) - 1} {
		p := igStore(t)
		if os.WriteFile(filepath.Join(p, idleGuardJournal), b[:cut], 0600) != nil {
			t.Fatal("torn fixture")
		}
		if other, e := newIdleGuard(p, nil, idleGuardIO{}); e == nil {
			_ = other.close()
			t.Error("torn journal accepted")
		}
	}
	for _, name := range []string{"idle-guard.lock", idleGuardJournal} {
		p := igStore(t)
		target := filepath.Join(igStore(t), "target")
		if os.WriteFile(target, nil, 0600) != nil || os.Symlink(target, filepath.Join(p, name)) != nil {
			t.Fatal("link fixture")
		}
		if other, e := newIdleGuard(p, nil, idleGuardIO{}); e == nil {
			_ = other.close()
			t.Error("linked monitoring file accepted")
		}
	}
	p := igStore(t)
	f, err := os.OpenFile(filepath.Join(p, idleGuardJournal), os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal("limit fixture")
	}
	if f.Truncate(idleGuardMaxBytes+1) != nil {
		t.Fatal("bounded sparse fixture")
	}
	_ = f.Close()
	if other, e := newIdleGuard(p, nil, idleGuardIO{}); e == nil {
		_ = other.close()
		t.Fatal("oversized journal accepted")
	}
}

func TestIdleGuardFractionalWindowOracle(t *testing.T) {
	store := igStore(t)
	o := igOpen(t, store, nil, idleGuardIO{})
	for i := 0; i <= 4; i++ {
		s := igSnapshot(igStart.Add(time.Duration(i) * time.Minute))
		s.Lanes[0].Capacity = "running"
		igSample(t, o, s)
	}
	from := igStart.Add(123 * time.Millisecond)
	until := igStart.Add(120*time.Second + 789*time.Millisecond)
	r := readIdleGuard(store, []string{"codex-1"}, from, until)
	if r.Lanes[0].UnknownSeconds != 0 {
		t.Fatal("fully known fractional window has nonzero unknown seconds", r.Lanes[0].UnknownSeconds)
	}
	if r.Coverage != "known" {
		t.Fatal("fully known fractional window failed coverage")
	}
}
