package nuntius

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"syscall"
)

type AgentPool struct {
	Size     int  `json:"size"`
	Template Lane `json:"template"`
}

func (c Config) expanded() ([]Lane, error) {
	if c.Pools == nil {
		return c.Lanes, nil
	}
	var lanes []Lane
	for _, kind := range []string{"claude", "codex"} {
		p, ok := c.Pools[kind]
		if !ok {
			continue
		}
		if p.Size < 0 || p.Size > 8 || p.Template.Name != "" || (p.Template.Kind != "" && p.Template.Kind != kind) {
			return nil, errors.New("invalid pool size or template")
		}
		for i := 1; i <= p.Size; i++ {
			l := p.Template
			l.Name = fmt.Sprintf("%s-%d", kind, i)
			l.Kind = kind
			lanes = append(lanes, l)
		}
	}
	for kind := range c.Pools {
		if kind != "claude" && kind != "codex" {
			return nil, errors.New("unknown pool kind")
		}
	}
	return lanes, nil
}

func (c Config) poolConfig() (Config, error) {
	if c.Pools != nil {
		c.Lanes = nil
		return c, nil
	}
	pools := map[string]AgentPool{}
	for _, l := range c.Lanes {
		t := l
		t.Name = ""
		t.Kind = ""
		t.StartMode = l.mode()
		t.ContinueText = l.continuation()
		if len(t.ExtraArgs) == 0 {
			t.ExtraArgs = nil
		}
		p, ok := pools[l.Kind]
		if ok && !reflect.DeepEqual(p.Template, t) {
			return Config{}, errors.New("lanes of one kind have different templates; owner must reconcile them before conversion")
		}
		p.Template = t
		p.Size++
		if p.Size > 8 {
			return Config{}, errors.New("pool exceeds eight agents")
		}
		pools[l.Kind] = p
	}
	c.Pools = pools
	c.Lanes = nil
	return c, nil
}

// The store lock serializes dispatch, freeze and pool edits across instances.
func (m *Manager) controlLock() (*os.File, error) {
	f, e := privateFile(filepath.Join(m.store, "locks", "control.lock"), os.O_RDWR|os.O_CREATE)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); e != nil {
		f.Close()
		return nil, e
	}
	return f, nil
}

func atomicPrivate(path string, b []byte) error {
	if fi, e := os.Lstat(path); e == nil && !fi.Mode().IsRegular() {
		return errors.New("destination must be a regular file")
	} else if e != nil && !os.IsNotExist(e) {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".nuntius-")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	return os.Rename(f.Name(), path)
}

func (m *Manager) reload() error {
	if m.config.SourcePath == "" {
		return nil
	}
	c, e := LoadConfig(m.config.SourcePath)
	if e != nil {
		return e
	}
	m.config = c
	return nil
}

func (m *Manager) SetPool(path, kind string, size int, convert bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	guard, e := m.controlLock()
	if e != nil {
		return e
	}
	defer guard.Close()
	c, e := LoadConfig(path)
	if e != nil {
		return e
	}
	next, e := c.poolConfig()
	if e != nil {
		return e
	}
	if !convert {
		p, ok := next.Pools[kind]
		if !ok || size < 0 || size > 8 {
			return errors.New("pool needs an existing template and size 0..8")
		}
		p.Size = size
		next.Pools[kind] = p
	}
	if _, e = next.Validate(); e != nil {
		return e
	}
	expanded, _ := next.expanded()
	retained := map[string]bool{}
	for _, l := range expanded {
		retained[l.Name] = true
	}
	for _, l := range c.Lanes {
		if c.Pools != nil && retained[l.Name] {
			continue
		}
		r, e := m.latest(l.Name)
		if e != nil {
			return e
		}
		busy, err := m.busy(l.Name)
		if err != nil && !errors.Is(err, errBusy) {
			return err
		}
		if busy || (r != nil && r.State == "running" && processExists(r.PID)) {
			return errors.New("cannot remove or rename a busy agent")
		}
	}
	// Legacy names become canonical names in historical records too.
	if c.Pools == nil {
		counts := map[string]int{}
		names := map[string]string{}
		for _, l := range c.Lanes {
			counts[l.Kind]++
			names[l.Name] = fmt.Sprintf("%s-%d", l.Kind, counts[l.Kind])
		}
		runs, e := m.history()
		if e != nil {
			return e
		}
		for _, r := range runs {
			if n, ok := names[r.Lane]; ok && n != r.Lane {
				r.Lane = n
				if e = m.save(&r); e != nil {
					return e
				}
			}
		}
	}
	b, e := json.MarshalIndent(next, "", "  ")
	if e != nil {
		return e
	}
	b = append(b, '\n')
	if e = atomicPrivate(path, b); e != nil {
		return e
	}
	next.Lanes = expanded
	next.SourcePath = path
	m.config = next
	return m.audit("pool_config", "", "", "", "updated")
}

type AgentStatus struct {
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	StartMode string   `json:"start_mode"`
	Flags     []string `json:"flags"`
	State     string   `json:"state"`
	Goal      string   `json:"goal"`
}

func (m *Manager) Agents() ([]AgentStatus, error) {
	s, e := m.Status()
	if e != nil {
		return nil, e
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]AgentStatus, 0, len(s))
	for _, v := range s {
		l, e := m.lane(v.Lane)
		if e != nil {
			return nil, e
		}
		out = append(out, AgentStatus{v.Lane, l.Kind, l.mode(), l.ExtraArgs, v.State, v.GoalID})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
