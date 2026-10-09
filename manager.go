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
	"sync"
	"syscall"
	"time"
)

type Run struct {
	ID            string     `json:"run_id"`
	Lane          string     `json:"lane"`
	GoalID        string     `json:"goal_id"`
	SHA256        string     `json:"sha256"`
	Started       time.Time  `json:"started"`
	Ended         *time.Time `json:"ended,omitempty"`
	Exit          *int       `json:"exit,omitempty"`
	State         string     `json:"state"`
	SessionID     string     `json:"session_id,omitempty"`
	PID           int        `json:"pid"`
	StopReason    string     `json:"stop_reason,omitempty"`
	StopAt        *time.Time `json:"stop_at,omitempty"`
	Resumed       bool       `json:"resumed"`
	ParentRunID   string     `json:"parent_run_id,omitempty"`
	ContinueRunID string     `json:"continue_run_id,omitempty"`
}
type Status struct {
	Lane               string     `json:"lane"`
	State              string     `json:"state"`
	RunID              string     `json:"run_id,omitempty"`
	GoalID             string     `json:"goal_id,omitempty"`
	Started            *time.Time `json:"started,omitempty"`
	ElapsedSeconds     int64      `json:"elapsed_seconds"`
	LastEvent          *time.Time `json:"last_event,omitempty"`
	Exit               *int       `json:"exit,omitempty"`
	ReportExists       bool       `json:"report_exists"`
	AskedInteractively bool       `json:"asked_interactively"`
	Outcome            string     `json:"outcome,omitempty"`
}
type activeRun struct {
	done chan struct{}
	stop chan string
}
type Manager struct {
	mu      sync.Mutex
	config  Config
	store   string
	runtime time.Duration
	Grace   time.Duration
	active  map[string]*activeRun
	closed  bool
	closing chan struct{}
	watcher sync.WaitGroup
}

func NewManager(c Config, store string) (*Manager, error) {
	d, e := c.Validate()
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
	m := &Manager{config: c, store: store, runtime: d, Grace: 30 * time.Second, active: map[string]*activeRun{}, closing: make(chan struct{})}
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
	tool := "nuntius_dispatch"
	if resume {
		tool = "nuntius_reply"
	}
	sum := sha256.Sum256([]byte(text))
	digest := hex.EncodeToString(sum[:])
	defer func() {
		if err != nil {
			_ = m.audit(tool, name, goal, digest, "refused")
		}
	}()
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
	previous, readErr := m.latest(name)
	if readErr != nil {
		return "", readErr
	}
	if previous != nil && previous.State == "running" && processExists(previous.PID) {
		return "", errors.New("lane has a surviving process; v1 will not reuse it")
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
	r := Run{ID: id, Lane: name, GoalID: goal, SHA256: digest, Started: time.Now().UTC(), State: "running"}
	r.Resumed = session != ""
	if parent != nil {
		r.ParentRunID = parent.ID
		parent.ContinueRunID = id
		if e := m.save(parent); e != nil {
			return "", e
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
	if e = m.audit(tool, name, goal, digest, "accepted"); e != nil {
		return "", e
	}
	cmd := exec.Command(lane.Binary, agentArgs(lane, session)...)
	cmd.Dir = lane.Workdir
	cmd.Stdin = input
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// The child inherits the lane lock so a server crash cannot admit another run.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.ExtraFiles = []*os.File{lock}
	if e = cmd.Start(); e != nil {
		r.State = "ended"
		now := time.Now().UTC()
		r.Ended = &now
		n := 127
		r.Exit = &n
		_ = m.save(&r)
		return "", errors.New("agent could not start")
	}
	r.PID = cmd.Process.Pid
	if e = m.save(&r); e != nil {
		_ = syscall.Kill(-r.PID, syscall.SIGKILL)
		_ = cmd.Wait()
		return "", e
	}
	a := &activeRun{done: make(chan struct{}), stop: make(chan string, 1)}
	m.active[name] = a
	go m.follow(r, cmd, lock, a, m.Grace)
	return id, nil
}
func agentArgs(l Lane, session string) []string {
	if l.Kind == "claude" {
		args := []string{"-p", "--output-format", "stream-json", "--verbose"}
		if session != "" {
			args = append(args, "--resume", session)
		}
		return append(args, l.ExtraArgs...)
	}
	args := []string{"exec"}
	if session != "" {
		args = append(args, "resume", session)
	}
	args = append(args, "--json")
	args = append(args, l.ExtraArgs...)
	return append(args, "-")
}
func (m *Manager) follow(r Run, cmd *exec.Cmd, lock *os.File, a *activeRun, grace time.Duration) {
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	timer := time.NewTimer(m.runtime)
	defer timer.Stop()
	var stop string
	select {
	case <-waited:
	case <-timer.C:
		stop = "timed_out"
	case stop = <-a.stop:
	}
	if stop != "" {
		_ = syscall.Kill(-r.PID, syscall.SIGINT)
		killTimer := time.NewTimer(grace)
		select {
		case <-waited:
		case <-killTimer.C:
			_ = syscall.Kill(-r.PID, syscall.SIGKILL)
			<-waited
		}
		killTimer.Stop()
	}
	_ = syscall.Kill(-r.PID, syscall.SIGKILL)
	now := time.Now().UTC()
	r.Ended = &now
	r.State = "ended"
	r.StopReason = stop
	if stop == "timed_out" {
		r.State = "timed_out"
	}
	n := cmd.ProcessState.ExitCode()
	r.Exit = &n
	r.SessionID = m.session(r.ID)
	canContinue := !r.Resumed && stop == "" && n == 0 && r.SessionID != "" && m.noWork(r.ID)
	m.mu.Lock()
	saved := true
	if e := m.save(&r); e != nil {
		saved = false
		_ = m.audit("run_end", r.Lane, r.GoalID, r.SHA256, "store_error")
	} else {
		_ = m.audit("run_end", r.Lane, r.GoalID, r.SHA256, "ended")
	}
	continued := false
	lane, _ := m.lane(r.Lane)
	if saved && canContinue && !m.closed && lane.Kind == "claude" && lane.mode() == "goal_then_continue" {
		_, e := m.launch(lane, r.GoalID, lane.continuation(), r.SessionID, lock, &r)
		continued = e == nil
		if e != nil {
			_ = m.audit("automatic_continue", r.Lane, r.GoalID, r.SHA256, "failed")
		}
	}
	if !continued {
		delete(m.active, r.Lane)
		_ = lock.Close()
	}
	close(a.done)
	m.mu.Unlock()
	if m.config.Notifications == nil || *m.config.Notifications {
		notify(r)
	}
}
func (m *Manager) Cancel(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, e := m.lane(name); e != nil {
		return e
	}
	if a := m.active[name]; a != nil {
		select {
		case a.stop <- "cancelled":
		default:
		}
		return m.audit("nuntius_cancel", name, "", "", "accepted")
	}
	r, e := m.latest(name)
	busy, lockErr := m.busy(name)
	if e != nil || r == nil || r.State != "running" || !busy || !errors.Is(lockErr, errBusy) {
		return errors.New("lane is not running")
	}
	return errors.New("v1 cannot safely signal a run owned by another server; use its original server or inspect it locally")
}
func (m *Manager) Status() ([]Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Status, 0, len(m.config.Lanes))
	for _, l := range m.config.Lanes {
		s := Status{Lane: l.Name, State: "idle"}
		r, e := m.latest(l.Name)
		if e != nil {
			return nil, e
		}
		if r != nil {
			if r.State == "running" && m.active[l.Name] == nil {
				lock, e := m.lock(l.Name)
				if e == nil {
					if !processExists(r.PID) {
						e = m.recoverEnded(r)
					} else {
						r.StopReason = "surviving_process_unverified"
					}
					_ = lock.Close()
					if e != nil {
						return nil, e
					}
				} else if !errors.Is(e, errBusy) {
					return nil, e
				}
			}
			s.State = r.State
			if r.State == "timed_out" {
				s.State = "ended"
			}
			s.RunID = r.ID
			s.GoalID = r.GoalID
			s.Started = &r.Started
			s.Exit = r.Exit
			s.Outcome = r.StopReason
			end := time.Now()
			if r.Ended != nil {
				end = *r.Ended
			}
			s.ElapsedSeconds = max(0, int64(end.Sub(r.Started).Seconds()))
			if fi, e := os.Lstat(filepath.Join(m.store, "runs", r.ID, "events.jsonl")); e == nil && fi.Mode().IsRegular() {
				v := fi.ModTime().UTC()
				s.LastEvent = &v
			}
			s.ReportExists = m.reportExists(r.GoalID)
			if s.State == "ended" && !s.ReportExists {
				s.State = "ended_no_report"
			}
			if b, err := readWindow(filepath.Join(m.store, "runs", r.ID, "events.jsonl"), true); err == nil {
				_, s.AskedInteractively = questionSummary(b)
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
		select {
		case a.stop <- "cancelled":
		default:
		}
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
