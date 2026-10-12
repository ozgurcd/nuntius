package nuntius

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// goalWriteEvidence is a private, immutable observation made from trusted root
// configuration before reservation. Replay checks correspondence, not today's
// filesystem or the authenticity of a coherently rewritten private store.
type goalWriteEvidence struct {
	Write      string `json:"write"`
	Root       string `json:"root"`
	Ancestor   string `json:"ancestor"`
	Resolved   string `json:"resolved"`
	RootDevice uint64 `json:"root_device"`
	RootInode  uint64 `json:"root_inode"`
	Device     uint64 `json:"device"`
	Inode      uint64 `json:"inode"`
}

// goalDispatchScopeValid is the one retained-scope predicate. It runs before
// equality, end/report and release shortcuts, including failed receipts.
func goalDispatchScopeValid(d goalDispatchRecord) bool {
	spec, err := decodeNativeGoalSpec([]byte(d.Spec))
	if err != nil || spec.ID != d.Request.GoalID || d.Scope == nil || d.Writes == nil || len(d.Scope) != len(spec.Writes) || len(d.Writes) != len(spec.Writes) {
		return false
	}
	for i, e := range d.Scope {
		_, rest, _ := strings.Cut(spec.Writes[i], "/")
		rest = filepath.FromSlash(rest)
		if e.Write != spec.Writes[i] || !goalArtifactPath(e.Root) || !goalArtifactPath(e.Resolved) || e.RootDevice == 0 || e.RootInode == 0 || e.Device == 0 || e.Inode == 0 || !goalAdmissionContains(e.Root, e.Resolved) {
			return false
		}
		if e.Ancestor != "" && (!goalArtifactPath(filepath.Join(e.Root, e.Ancestor)) || filepath.Clean(e.Ancestor) != e.Ancestor || filepath.IsAbs(e.Ancestor) || e.Ancestor == ".." || strings.HasPrefix(e.Ancestor, ".."+string(filepath.Separator)) || rest != e.Ancestor && !strings.HasPrefix(rest, e.Ancestor+string(filepath.Separator))) {
			return false
		}
		if e.Ancestor == "" && (e.Resolved != e.Root || e.Device != e.RootDevice || e.Inode != e.RootInode) {
			return false
		}
		suffix := rest
		if e.Ancestor != "" {
			suffix = strings.TrimPrefix(strings.TrimPrefix(rest, e.Ancestor), string(filepath.Separator))
		}
		if want := filepath.Join(e.Resolved, suffix); !goalArtifactPath(want) || want != d.Writes[i] {
			return false
		}
	}
	return true
}

func goalDispatchScopesValid(data goalRecordData) bool {
	for _, d := range data.Dispatches {
		if !goalDispatchScopeValid(d) {
			return false
		}
	}
	return true
}

// Resolve links before normalizing trusted root spellings, then recover actual
// directory-entry case. Never lowercase distinct paths on a sensitive volume.
func goalScopePhysical(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	current := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(resolved, current), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		entries, err := os.ReadDir(current)
		if err != nil {
			return "", err
		}
		name := ""
		for _, entry := range entries {
			if entry.Name() == part {
				name = part
				break
			}
		}
		if name == "" {
			info, err := os.Stat(filepath.Join(current, part))
			if err != nil {
				return "", err
			}
			for _, entry := range entries {
				other, err := entry.Info()
				if err == nil && os.SameFile(info, other) {
					name = entry.Name()
					break
				}
			}
		}
		if name == "" {
			return "", os.ErrNotExist
		}
		current = filepath.Join(current, name)
	}
	return current, nil
}

func goalScopeIdentity(path string) (uint64, uint64, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, 0, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Dev == 0 || stat.Ino == 0 {
		return 0, 0, false
	}
	return uint64(stat.Dev), uint64(stat.Ino), true
}

func goalCanonicalScope(writes []string, roots map[string]string) ([]string, []string, []goalWriteEvidence, bool) {
	if !nativeStringSet(writes, nativeGoalMaxList, nativeWrite) {
		return nil, nil, nil, false
	}
	out, repos, scope := []string{}, []string{}, []goalWriteEvidence{}
	seen := map[string]bool{}
	for _, write := range writes {
		name, rest, _ := strings.Cut(write, "/")
		base := roots[name]
		if !filepath.IsAbs(base) {
			return nil, nil, nil, false
		}
		base, err := goalScopePhysical(base)
		if err != nil || !goalArtifactPath(base) {
			return nil, nil, nil, false
		}
		info, err := os.Stat(base)
		if err != nil || !info.IsDir() {
			return nil, nil, nil, false
		}
		e := goalWriteEvidence{Write: write, Root: base}
		var ok bool
		if e.RootDevice, e.RootInode, ok = goalScopeIdentity(base); !ok {
			return nil, nil, nil, false
		}
		ancestor := filepath.Join(base, filepath.FromSlash(rest))
		for {
			_, err := os.Lstat(ancestor)
			if err == nil {
				break
			}
			if !os.IsNotExist(err) || ancestor == base {
				return nil, nil, nil, false
			}
			ancestor = filepath.Dir(ancestor)
		}
		e.Ancestor, err = filepath.Rel(base, ancestor)
		if err != nil {
			return nil, nil, nil, false
		}
		if e.Ancestor == "." {
			e.Ancestor = ""
		}
		e.Resolved, err = goalScopePhysical(ancestor)
		if err != nil || !goalAdmissionContains(base, e.Resolved) {
			return nil, nil, nil, false
		}
		if e.Device, e.Inode, ok = goalScopeIdentity(e.Resolved); !ok {
			return nil, nil, nil, false
		}
		suffix, err := filepath.Rel(ancestor, filepath.Join(base, filepath.FromSlash(rest)))
		if err != nil {
			return nil, nil, nil, false
		}
		out = append(out, filepath.Join(e.Resolved, suffix))
		scope = append(scope, e)
		if !seen[base] {
			seen[base] = true
			repos = append(repos, base)
		}
	}
	sort.Strings(repos)
	return out, repos, scope, true
}
