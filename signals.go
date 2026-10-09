package nuntius

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var signalPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{1,129}\.(ended|stuck|interrupted|resumed)$`)

func (m *Manager) signalRoot() (*os.Root, error) {
	dir := filepath.Join(m.store, "signals")
	if err := privateDir(dir); err != nil {
		return nil, err
	}
	return os.OpenRoot(dir)
}

func (m *Manager) endRun(r *Run) error {
	if err := m.save(r); err != nil {
		return err
	}
	return m.emitSignal(*r, "ended")
}

func (m *Manager) emitSignal(r Run, suffix string) error {
	root, err := m.signalRoot()
	if err != nil {
		return err
	}
	defer root.Close()
	name := r.Lane + "." + r.GoalID + "." + suffix
	if !signalPattern.MatchString(name) {
		return errors.New("invalid signal name")
	}
	st, err := root.Lstat(name)
	if err == nil {
		if !st.Mode().IsRegular() || st.Size() != 0 || st.Mode().Perm() != 0600 {
			return errors.New("invalid existing signal")
		}
		return syncDir(filepath.Join(m.store, "signals"))
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return atomicPrivate(filepath.Join(m.store, "signals", name), nil)
}

func (m *Manager) Signals() ([]string, error) {
	if _, err := m.Status(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	root, err := m.signalRoot()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(10001)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(entries) > 10000 {
		return nil, errors.New("too many end signals; acknowledge them")
	}
	names := []string{}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".nuntius-") && entry.Type().IsRegular() {
			continue
		}
		if !signalPattern.MatchString(entry.Name()) || !entry.Type().IsRegular() {
			return nil, errors.New("invalid end signal")
		}
		st, err := root.Lstat(entry.Name())
		if err != nil {
			return nil, err
		}
		if st.Size() != 0 {
			continue
		} // A torn signal is never delivered.
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names, nil
}

func (m *Manager) AckSignals(names []string) error {
	if len(names) == 0 || len(names) > 10000 {
		return errors.New("names must contain 1..10000 signal names")
	}
	for _, name := range names {
		if !signalPattern.MatchString(name) {
			return errors.New("invalid signal name; use a name from nuntius_signals")
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	root, err := m.signalRoot()
	if err != nil {
		return err
	}
	defer root.Close()
	// Validate the whole request before removing anything. Root confinement also
	// protects against races and linked ancestors.
	for _, name := range names {
		st, err := root.Lstat(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !st.Mode().IsRegular() {
			return errors.New("invalid end signal")
		}
	}
	for _, name := range names {
		if err := root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			return errors.New("cannot acknowledge end signal")
		}
	}
	return syncDir(filepath.Join(m.store, "signals"))
}
