package nuntius

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"
)

type Run struct {
	Kind                 string     `json:"kind,omitempty"`
	Model                string     `json:"model,omitempty"`
	Effort               string     `json:"effort,omitempty"`
	ID                   string     `json:"run_id"`
	Lane                 string     `json:"lane"`
	GoalID               string     `json:"goal_id"`
	SHA256               string     `json:"sha256"`
	Started              time.Time  `json:"started"`
	Ended                *time.Time `json:"ended,omitempty"`
	Exit                 *int       `json:"exit,omitempty"`
	State                string     `json:"state"`
	SessionID            string     `json:"session_id,omitempty"`
	PID                  int        `json:"pid"`
	ProcessStart         string     `json:"process_start,omitempty"`
	StopReason           string     `json:"stop_reason,omitempty"`
	StopAt               *time.Time `json:"stop_at,omitempty"`
	Resumed              bool       `json:"resumed"`
	ParentRunID          string     `json:"parent_run_id,omitempty"`
	ContinueRunID        string     `json:"continue_run_id,omitempty"`
	StuckSignalled       bool       `json:"stuck_signalled,omitempty"`
	Brief                string     `json:"brief,omitempty"`
	ResumeCount          int        `json:"resume_count,omitempty"`
	RecoveryRun          bool       `json:"recovery_run,omitempty"`
	Integrity            string     `json:"integrity,omitempty"`
	InterruptedSignalled bool       `json:"interrupted_signalled,omitempty"`
	ResumedSignalled     bool       `json:"resumed_signalled,omitempty"`
	WaitingToResume      bool       `json:"waiting_to_resume,omitempty"`
	LaunchdLabel         string     `json:"launchd_label,omitempty"`
	LaunchFallback       bool       `json:"launch_fallback,omitempty"`
	LaunchdCleaned       bool       `json:"launchd_cleaned,omitempty"`
}
type Status struct {
	Kind                string         `json:"kind,omitempty"`
	Model               string         `json:"model,omitempty"`
	Effort              string         `json:"effort"`
	CurrentAction       *CurrentAction `json:"current_action,omitempty"`
	LaunchFallback      bool           `json:"launch_fallback,omitempty"`
	LaunchdLabel        string         `json:"launchd_label,omitempty"`
	Freeze              *FreezeState   `json:"freeze,omitempty"`
	LastEventAgeSeconds *int64         `json:"last_event_age_seconds,omitempty"`
	Lane                string         `json:"lane"`
	State               string         `json:"state"`
	RunID               string         `json:"run_id,omitempty"`
	GoalID              string         `json:"goal_id,omitempty"`
	Started             *time.Time     `json:"started,omitempty"`
	ElapsedSeconds      int64          `json:"elapsed_seconds"`
	LastEvent           *time.Time     `json:"last_event,omitempty"`
	Exit                *int           `json:"exit,omitempty"`
	ReportExists        bool           `json:"report_exists"`
	AskedInteractively  bool           `json:"asked_interactively"`
	Outcome             string         `json:"outcome,omitempty"`
	DisplayState        string         `json:"display_state,omitempty"`
	Badge               string         `json:"badge,omitempty"`
	ETA                 *ETAEstimate   `json:"eta_details,omitempty"`
}
type activeRun struct {
	done chan struct{}
	stop chan string
}
type Manager struct {
	actionClocks map[string]actionObservation
	mu           sync.Mutex
	config       Config
	store        string
	runtime      time.Duration
	Grace        time.Duration
	active       map[string]*activeRun
	closed       bool
	closing      chan struct{}
	watcher      sync.WaitGroup
	recovery     bool
	launcher     *jobLauncher
	webEnsure    *webEnsureRuntime // nil uses owner-process adapters; tests use offline fakes.
}

func NewManager(c Config, store string) (*Manager, error) {
	d, e := c.Validate()
	if e != nil {
		return nil, e
	}
	c.Lanes, e = c.expanded()
	if e != nil {
		return nil, e
	}
	if !filepath.IsAbs(store) {
		return nil, errors.New("run store must be absolute")
	}
	for _, p := range []string{store, filepath.Join(store, "runs"), filepath.Join(store, "locks")} {
		if e := privateDir(p); e != nil {
			return nil, e
		}
	}
	if e = syncDir(store); e != nil {
		return nil, e
	}
	if e = syncDir(filepath.Dir(store)); e != nil {
		return nil, e
	}
	m := &Manager{config: c, store: store, runtime: d, Grace: 30 * time.Second, active: map[string]*activeRun{}, closing: make(chan struct{}), launcher: newJobLauncher()}
	if _, e := m.Status(); e != nil {
		return nil, e
	}
	m.watcher.Go(func() {
		tick := time.NewTicker(250 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-m.closing:
				return
			case <-tick.C:
				_, _ = m.Status()
				m.mu.Lock()
				recovery := m.recovery
				m.mu.Unlock()
				if recovery {
					_ = m.Recover()
				}
			}
		}
	})
	return m, nil
}
func (m *Manager) lane(name string) (Lane, error) {
	for _, l := range m.config.Lanes {
		if l.Name == name {
			return l, nil
		}
	}
	return Lane{}, errors.New("unknown lane")
}
func (m *Manager) Dispatch(lane, goal, text string) (string, error) {
	return m.start(lane, goal, text, false)
}
func (m *Manager) Reply(lane, text string) (string, error) { return m.start(lane, "", text, true) }
func (m *Manager) start(name, goal, text string, resume bool) (id string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	guard, e := m.controlLock()
	if e != nil {
		return "", e
	}
	defer guard.Close()
	if e = m.reload(); e != nil {
		return "", e
	}
	tool := "nuntius_dispatch"
	if resume {
		tool = "nuntius_reply"
	}
	sum := sha256.Sum256([]byte(text))
	digest := hex.EncodeToString(sum[:])
	defer func() {
		if err != nil {
			_ = m.audit(tool, name, goal, digest, "refused", err.Error())
		}
	}()
	if e = m.checkFrozen(); e != nil {
		return "", e
	}
	if m.closed {
		return "", errors.New("server is closing")
	}
	lane, e := m.lane(name)
	if e != nil {
		return "", e
	}
	if len(text) < 1 || len(text) > 12000 {
		return "", errors.New("text must contain 1..12000 bytes")
	}
	if !resume && !goalPattern.MatchString(goal) {
		return "", errors.New("invalid goal_id")
	}
	lock, e := m.lock(name)
	if e != nil {
		return "", errors.New("lane is busy or lock unavailable")
	}
	keep := false
	defer func() {
		if !keep {
			_ = lock.Close()
		}
	}()
	session := ""
	runs, readErr := m.history()
	if readErr != nil {
		return "", readErr
	}
	for _, r := range runs {
		if r.State == "running" {
			if err := m.pendingJob(&r); err != nil {
				return "", err
			}
		}
		if r.Lane == name && r.State == "running" && (sameProcess(r) || (r.ProcessStart == "" && processExists(r.PID))) {
			return "", errors.New("lane has a surviving process")
		}
	}
	if resume {
		last, e := m.latest(name)
		if e != nil || last == nil {
			return "", errors.New("reply needs an ended run with a session id")
		}
		if last.State == "running" {
			if e = m.recoverEnded(last); e != nil {
				return "", e
			}
		}
		if !sessionPattern.MatchString(last.SessionID) {
			return "", errors.New("reply needs an ended run with a session id")
		}
		goal = last.GoalID
		session = last.SessionID
	}
	id, err = m.launch(lane, goal, text, session, lock, nil)
	keep = err == nil
	return id, err
}

// launch requires the manager mutex and the lane's process-shared lock.
func (m *Manager) launch(lane Lane, goal, text, session string, lock *os.File, parent *Run) (string, error) {
	name := lane.Name
	sum := sha256.Sum256([]byte(text))
	digest := hex.EncodeToString(sum[:])
	tool := "nuntius_dispatch"
	if session != "" {
		tool = "nuntius_reply"
	}
	if parent != nil {
		tool = "automatic_continue"
	}
	random := make([]byte, 16)
	if _, e := rand.Read(random); e != nil {
		return "", errors.New("cannot allocate run id")
	}
	id := hex.EncodeToString(random)
	dir := filepath.Join(m.store, "runs", ".pending-"+id)
	if e := os.Mkdir(dir, 0700); e != nil {
		return "", errors.New("cannot create run directory")
	}
	model, effort := effectiveIdentity(lane)
	r := Run{ID: id, Lane: name, Kind: lane.Kind, Model: model, Effort: effort, GoalID: goal, SHA256: digest, Brief: text, Started: time.Now().UTC(), State: "running"}
	r.Resumed = session != ""
	r.SessionID = session
	if parent != nil {
		r.ParentRunID = parent.ID
		r.Brief = parent.Brief
		r.ResumeCount = parent.ResumeCount
		if parent.State == "interrupted" {
			r.ResumeCount++
			r.RecoveryRun = true
			r.Resumed = true
		}
	}
	if e := putPrivate(filepath.Join(dir, "goal.txt"), []byte(text)); e != nil {
		return "", e
	}
	stdout, e := privateFile(filepath.Join(dir, "events.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if e != nil {
		return "", e
	}
	defer stdout.Close()
	stderr, e := privateFile(filepath.Join(dir, "stderr.log"), os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if e != nil {
		return "", e
	}
	defer stderr.Close()
	input, e := privateFile(filepath.Join(dir, "goal.txt"), os.O_RDONLY)
	if e != nil {
		return "", e
	}
	defer input.Close()
	if e = saveAt(dir, &r); e != nil {
		return "", e
	}
	if e = os.Rename(dir, filepath.Join(m.store, "runs", id)); e != nil {
		return "", errors.New("cannot publish run directory")
	}
	if e = syncDir(filepath.Join(m.store, "runs")); e != nil {
		return "", e
	}
	if parent != nil {
		parent.ContinueRunID = id
		if e = m.save(parent); e != nil {
			return "", e
		}
	}
	if e = m.audit(tool, name, goal, digest, "accepted"); e != nil {
		return "", e
	}
	if e = m.startAgent(filepath.Join(m.store, "runs", id), lane, session, input, stdout, stderr, lock); e != nil {
		return "", e
	}
	rp, e := loadRun(filepath.Join(m.store, "runs", id), id)
	if e != nil || rp == nil {
		return "", errors.New("cannot read detached identity")
	}
	r = *rp
	a := &activeRun{done: make(chan struct{}), stop: make(chan string, 1)}
	supervisor, e := m.lock("supervisor-" + r.ID)
	if e != nil {
		_ = signalRun(r, syscall.SIGKILL)
		return "", e
	}
	m.active[name] = a
	go m.follow(r, lock, supervisor, a, m.Grace)
	return id, nil
}
func agentArgs(l Lane, session string) []string {
	if l.Kind == "opencode" {
		args := []string{"run", "--format", "json", "--model", l.Model}
		args = append(args, l.ExtraArgs...)
		if session != "" {
			args = append(args, "--session", session)
		}
		return args
	}
	if l.Kind == "claude" {
		args := []string{"-p", "--output-format", "stream-json", "--verbose"}
		if session != "" {
			args = append(args, "--resume", session)
		}
		return append(args, l.ExtraArgs...)
	}
	args := []string{"exec"}
	args = append(args, "--json")
	args = append(args, l.ExtraArgs...)
	if session != "" {
		args = append(args, "resume", session)
	}
	return append(args, "-")
}
func (m *Manager) follow(r Run, lock, supervisor *os.File, a *activeRun, grace time.Duration) {
	launchAllowed := lock != nil
	defer supervisor.Close()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	finished := false
	var lastStuckCheck time.Time
	for !finished {
		select {
		case <-m.closing:
			if lock != nil {
				_ = lock.Close()
			}
			close(a.done)
			return
		case <-tick.C:
			m.mu.Lock()
			guard, e := m.controlLock()
			if e == nil {
				current, err := loadRun(filepath.Join(m.store, "runs", r.ID), r.ID)
				if err == nil && current != nil && current.ID == r.ID {
					if current.ProcessStart != r.ProcessStart {
						e = errors.New("process identity changed")
					} else {
						r = *current
					}
				} else {
					e = errors.New("run metadata unavailable")
				}
				if e == nil {
					if !sameProcess(r) {
						finished = true
					}
					if sameProcess(r) {
						if now := time.Now(); now.Sub(lastStuckCheck) >= time.Second {
							lastStuckCheck = now
							if _, err := m.checkStuck(&r, now); err != nil {
								_ = m.audit("run_stuck_signal", r.Lane, r.GoalID, r.SHA256, "persistence_failed")
							}
						}
						if r.StopAt == nil && (time.Since(r.Started) >= m.runtime || m.checkFrozen() != nil) {
							reason := "timed_out"
							if time.Since(r.Started) < m.runtime {
								reason = "cancelled"
							}
							_ = m.requestStop(&r, reason)
						}
						if r.StopAt != nil && time.Since(*r.StopAt) >= grace {
							_ = signalRun(r, syscall.SIGKILL)
						}
					}
				}
				_ = guard.Close()
			}
			m.mu.Unlock()
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	guard, e := m.controlLock()
	if e != nil {
		close(a.done)
		if lock != nil {
			_ = lock.Close()
		}
		delete(m.active, r.Lane)
		return
	}
	defer guard.Close()
	if current, err := loadRun(filepath.Join(m.store, "runs", r.ID), r.ID); err == nil && current != nil && current.ID == r.ID {
		r = *current
	}
	now := time.Now().UTC()
	r.Ended = &now
	r.State = "ended"
	if r.StopReason == "timed_out" {
		r.State = "timed_out"
	}
	if r.StopReason == "" {
		r.StopReason = "ended_no_report"
		if m.reportExists(r) {
			r.StopReason = "completed"
		}
	}
	if session := m.session(r.ID); session != "" {
		r.SessionID = session
	}
	saved := m.endRun(&r) == nil
	if !saved {
		_ = m.audit("run_end_signal", r.Lane, r.GoalID, r.SHA256, "persistence_failed")
	}
	_ = m.audit("run_end", r.Lane, r.GoalID, r.SHA256, "ended")
	if lock == nil {
		lock, _ = m.lock(r.Lane)
	}
	continued := false
	lane, laneErr := m.lane(r.Lane)
	pending, queueErr := m.pendingMessage(r.ID)
	if saved && laneErr == nil && (launchAllowed || m.recovery) && !m.closed && m.checkFrozen() == nil && lock != nil && r.SessionID != "" {
		text := ""
		if queueErr == nil && pending != "" {
			text = pending
		} else if lock != nil && !r.Resumed && (r.StopReason == "ended_no_report" || r.StopReason == "completed") && m.noWork(r.ID) && lane.Kind == "claude" && lane.mode() == "goal_then_continue" {
			text = lane.continuation()
		}
		if text != "" {
			_, e = m.launch(lane, r.GoalID, text, r.SessionID, lock, &r)
			continued = e == nil
			if continued && pending != "" {
				_ = os.Remove(filepath.Join(m.store, "runs", r.ID, "message.json"))
			}
		}
	}
	if !continued {
		delete(m.active, r.Lane)
		if lock != nil {
			_ = lock.Close()
		}
	}
	close(a.done)
	if m.config.Notifications == nil || *m.config.Notifications {
		go notify(r)
	}
}
func (m *Manager) Cancel(name string) (err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	goal := ""
	defer func() {
		if err != nil {
			_ = m.audit("nuntius_cancel", name, goal, "", "refused", err.Error())
		}
	}()
	if _, e := m.lane(name); e != nil {
		return e
	}
	guard, e := m.controlLock()
	if e != nil {
		return e
	}
	defer guard.Close()
	r, e := m.latest(name)
	if e != nil || r == nil || r.State != "running" {
		return errors.New("lane is not running")
	}
	goal = r.GoalID
	if e = m.requestStop(r, "cancelled"); e != nil {
		return e
	}
	return m.audit("nuntius_cancel", name, r.GoalID, "", "accepted")
}
func (m *Manager) Status() ([]Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	guard, e := m.controlLock()
	if e != nil {
		return nil, e
	}
	defer guard.Close()
	if e = m.reload(); e != nil {
		return nil, e
	}
	frozen, e := m.freezeState()
	if e != nil {
		return nil, e
	}
	runs, e := m.history()
	if e != nil {
		return nil, e
	}
	for _, r := range runs {
		if r.State != "running" && r.LaunchdLabel != "" && !r.LaunchdCleaned {
			if err := m.cleanupJob(&r); err != nil {
				_ = m.audit("launchd_cleanup", r.Lane, r.GoalID, r.SHA256, "failed")
			} else if err := m.save(&r); err != nil {
				return nil, err
			}
		}
		if r.State == "running" {
			if err := m.pendingJob(&r); err != nil {
				return nil, err
			}
		}
		if r.State != "running" || sameProcess(r) || (r.ProcessStart == "" && processExists(r.PID)) {
			continue
		}
		lock, err := m.lock("supervisor-" + r.ID)
		if errors.Is(err, errBusy) {
			continue
		}
		if err != nil {
			return nil, err
		}
		err = m.recoverEnded(&r)
		_ = lock.Close()
		if err != nil {
			return nil, err
		}
	}
	out := make([]Status, 0, len(m.config.Lanes))
	for _, l := range m.config.Lanes {
		model, effort := effectiveIdentity(l)
		s := Status{Lane: l.Name, Kind: l.Kind, Model: model, Effort: effort, State: "idle", Freeze: frozen}
		r, e := m.latest(l.Name)
		if e != nil {
			return nil, e
		}
		if r != nil {
			if m.captureEffort(r) {
				if err := m.save(r); err != nil {
					return nil, err
				}
			}
			s.Model, s.Effort = identityValue(r.Model), identityValue(r.Effort)
			if r.Model == "" && r.Effort == "" {
				s.Model, s.Effort = model+" (configured)", effort+" (configured)"
			}
			if r.State == "running" && m.active[l.Name] == nil {
				lock, e := m.lock("supervisor-" + r.ID)
				if e == nil {
					if sameProcess(*r) {
						a := &activeRun{done: make(chan struct{}), stop: make(chan string, 1)}
						m.active[l.Name] = a
						go m.follow(*r, nil, lock, a, m.Grace)
						lock = nil
					} else if !processExists(r.PID) || r.ProcessStart != "" {
						e = m.recoverEnded(r)
					} else {
						r.StopReason = "surviving_process_unverified"
					}
					if lock != nil {
						_ = lock.Close()
					}
					if e != nil {
						return nil, e
					}
				} else if !errors.Is(e, errBusy) {
					return nil, e
				}
			}
			if r.State == "running" && r.SessionID == "" {
				if session := m.session(r.ID); session != "" {
					r.SessionID = session
					if err := m.save(r); err != nil {
						return nil, err
					}
				}
			}
			s.State = r.State
			if r.State == "timed_out" {
				s.State = "ended"
			}
			s.RunID = r.ID
			s.LaunchFallback = r.LaunchFallback
			s.LaunchdLabel = r.LaunchdLabel
			s.GoalID = r.GoalID
			s.Started = &r.Started
			s.Exit = r.Exit
			s.Outcome = r.StopReason
			end := time.Now()
			if r.Ended != nil {
				end = *r.Ended
			}
			s.ElapsedSeconds = max(0, int64(end.Sub(r.Started).Seconds()))
			if r.State == "running" {
				s.ETA = m.etaDetails(runs, *r, l.Kind, m.runSize(r.ID), time.Duration(s.ElapsedSeconds)*time.Second)
			}
			if fi, e := os.Lstat(filepath.Join(m.store, "runs", r.ID, "events.jsonl")); e == nil && fi.Mode().IsRegular() {
				v := fi.ModTime().UTC()
				s.LastEvent = &v
				age := max(0, int64(time.Since(v).Seconds()))
				s.LastEventAgeSeconds = &age
			}
			s.ReportExists = m.reportExists(*r)
			if s.State == "ended" && !s.ReportExists {
				s.State = "ended_no_report"
			}
			s.DisplayState = terminalDisplay(*r, s.State)
			s.Badge = badgeTone(*r, s.State)
			if stuck, err := m.checkStuck(r, time.Now()); err != nil {
				return nil, err
			} else if stuck {
				s.DisplayState, s.Badge = "stuck?", "amber"
			}
			if s.DisplayState == s.State {
				s.DisplayState = ""
			}
			if b, err := readWindow(filepath.Join(m.store, "runs", r.ID, "events.jsonl"), true); err == nil {
				_, s.AskedInteractively = questionSummary(b)
				if s.State == "running" && s.LastEvent != nil {
					s.CurrentAction = m.currentAction(r.ID, b, *s.LastEvent, time.Now())
				}
			}
		}
		out = append(out, s)
	}
	return out, nil
}
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	close(m.closing)
	var pending []chan struct{}
	for _, a := range m.active {
		pending = append(pending, a.done)
	}
	m.mu.Unlock()
	for _, d := range pending {
		<-d
	}
	m.watcher.Wait()
	return nil
}
func notify(r Run) {
	if !notificationsSupported(runtime.GOOS) || r.Exit == nil {
		return
	}
	if _, e := os.Stat("/usr/bin/osascript"); e != nil {
		return
	}
	text := fmt.Sprintf("nuntius: %s finished %s (exit %d)", r.Lane, r.GoalID, *r.Exit)
	cmd := exec.Command("/usr/bin/osascript", "-e", "on run argv\ndisplay notification (item 1 of argv)\nend run", text)
	if e := cmd.Start(); e == nil {
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}
}
