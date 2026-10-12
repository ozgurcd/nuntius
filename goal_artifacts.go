package nuntius

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const goalDirectMaxBytes = 12000

var errGoalDelivery = errors.New("brief delivery unavailable")

// goalDeliveryProfile is supplied only by a trusted local adapter. The root
// must be visible to the selected lane; it is never inferred from the store.
type goalDeliveryProfile struct {
	Engine       nativeEngineKind
	CodePointCap int
	Root         string
}

type goalBriefDelivery struct {
	Version          int              `json:"version"`
	Engine           nativeEngineKind `json:"engine"`
	CodePointCap     int              `json:"code_point_cap"`
	Mode             string           `json:"mode"`
	Prompt           string           `json:"prompt"`
	Artifact         string           `json:"artifact"`
	Device           uint64           `json:"device"`
	Inode            uint64           `json:"inode"`
	FullSHA256       string           `json:"full_sha256"`
	FullBytes        int              `json:"full_bytes"`
	FullCodePoints   int              `json:"full_code_points"`
	PromptSHA256     string           `json:"prompt_sha256"`
	PromptBytes      int              `json:"prompt_bytes"`
	PromptCodePoints int              `json:"prompt_code_points"`
}

func deliverGoalBrief(spec nativeGoalSpec, seal goalBriefSeal, profile goalDeliveryProfile, retained ...goalBriefDelivery) (goalBriefDelivery, error) {
	if len(retained) > 1 {
		return goalBriefDelivery{}, fmt.Errorf("%w: supply one retained delivery record", errGoalDelivery)
	}
	if len(retained) == 1 {
		if err := validateGoalDelivery(spec, seal, profile, retained[0]); err != nil {
			return goalBriefDelivery{}, err
		}
		return retained[0], nil
	}
	d, full, err := expectedGoalDelivery(spec, seal, profile)
	if err != nil {
		return goalBriefDelivery{}, err
	}
	root, err := openGoalArtifactRoot(profile.Root)
	if err != nil {
		return goalBriefDelivery{}, err
	}
	defer root.Close()
	if d.Mode == "pointer" {
		name := filepath.Base(d.Artifact)
		fd, e := unix.Openat(int(root.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
		if e != nil && !errors.Is(e, unix.EEXIST) {
			return goalBriefDelivery{}, fmt.Errorf("%w: the sealed artifact cannot be created safely in the selected root", errGoalDelivery)
		}
		if e == nil {
			f := os.NewFile(uintptr(fd), "sealed-goal-artifact")
			_, e = io.Copy(f, strings.NewReader(full))
			if e == nil {
				e = f.Sync()
			}
			closeErr := f.Close()
			if e != nil || closeErr != nil {
				return goalBriefDelivery{}, fmt.Errorf("%w: the full artifact write was not confirmed; review the retained file before retrying", errGoalDelivery)
			}
		}
		// Never truncate, chmod, replace or remove an existing artifact. A partial
		// file from an interrupted write remains a visible refusal, not a retry.
		_, st, e := readGoalArtifact(root, name, full, (*os.File).Sync)
		if e != nil {
			return goalBriefDelivery{}, e
		}
		if root.Sync() != nil {
			return goalBriefDelivery{}, fmt.Errorf("%w: artifact directory durability was not confirmed", errGoalDelivery)
		}
		d.Device, d.Inode = uint64(st.Dev), st.Ino
	}
	// The same predicate also checks retained descriptors, even on a no-op
	// retry. Publication is not evidence that a path remains safe or readable.
	if err = validateGoalDelivery(spec, seal, profile, d); err != nil {
		return goalBriefDelivery{}, err
	}
	return d, nil
}

func expectedGoalDelivery(spec nativeGoalSpec, seal goalBriefSeal, p goalDeliveryProfile) (goalBriefDelivery, string, error) {
	var zero goalBriefDelivery
	if !nativeEngine(p.Engine) || p.CodePointCap < 1 || !goalArtifactPath(p.Root) {
		return zero, "", fmt.Errorf("%w: select a supported engine, a positive code-point cap and an absolute one-line artifact root", errGoalDelivery)
	}
	allowed := false
	for _, engine := range spec.Kinds {
		allowed = allowed || engine == p.Engine
	}
	if !allowed {
		return zero, "", fmt.Errorf("%w: the selected engine is outside the sealed goal's allowed engines", errGoalDelivery)
	}
	encoded, err := json.Marshal(seal)
	if err != nil {
		return zero, "", errGoalDelivery
	}
	checked, err := decodeGoalBrief(spec, encoded)
	if err != nil {
		return zero, "", fmt.Errorf("%w: the stored seal does not match the goal, bytes or recorded digests; review and reseal it", errGoalDelivery)
	}
	full, prompt := checked.deliveryText()
	d := goalBriefDelivery{Version: 1, Engine: p.Engine, CodePointCap: p.CodePointCap, Mode: "direct",
		FullSHA256: checked.FinalSHA256, FullBytes: len(full), FullCodePoints: utf8.RuneCountInString(full)}
	if len(prompt) > goalDirectMaxBytes || utf8.RuneCountInString(prompt) > p.CodePointCap {
		d.Mode = "pointer"
		d.Artifact = filepath.Join(p.Root, fmt.Sprintf("goal-%s-r%d-%s.txt", checked.GoalID, checked.Revision, checked.FinalSHA256))
		prompt = checked.Profile.Wrapper + fmt.Sprintf("GOAL: %s (size %s)\nRead the full sealed brief at %s. Follow that file in full; it already contains the shared standards once.\nExpected SHA-256: %s\n", checked.GoalID, checked.Size, strconv.Quote(d.Artifact), checked.FinalSHA256)
	}
	if len(full) > goalBriefMaxBytes || len(prompt) > goalDirectMaxBytes || utf8.RuneCountInString(prompt) > p.CodePointCap {
		return zero, "", fmt.Errorf("%w: the full brief must fit 128 KiB and the complete prompt, including its wrapper or pointer, must fit 12000 UTF-8 bytes and the selected engine's %d code-point cap", errGoalDelivery, p.CodePointCap)
	}
	d.Prompt, d.PromptSHA256, d.PromptBytes, d.PromptCodePoints = prompt, goalBriefDigest([]byte(prompt)), len(prompt), utf8.RuneCountInString(prompt)
	return d, full, nil
}

// validateGoalDelivery is the save/readback predicate for delivery descriptors.
// Later admission must call it again; this component does not launch a lane.
func validateGoalDelivery(spec nativeGoalSpec, seal goalBriefSeal, p goalDeliveryProfile, d goalBriefDelivery) error {
	expected, full, err := expectedGoalDelivery(spec, seal, p)
	if err != nil {
		return err
	}
	root, err := openGoalArtifactRoot(p.Root)
	if err != nil {
		return err
	}
	defer root.Close()
	if expected.Mode == "pointer" {
		_, st, err := readGoalArtifact(root, filepath.Base(expected.Artifact), full, nil)
		if err != nil {
			return err
		}
		expected.Device, expected.Inode = uint64(st.Dev), st.Ino
	}
	if !reflect.DeepEqual(expected, d) {
		return fmt.Errorf("%w: the retained delivery no longer matches the sealed text, selected profile or file identity; review it before resuming", errGoalDelivery)
	}
	// A descriptor-relative open prevents escape but cannot certify the path a
	// sandboxed child will open. Reopen the owner path and check the root inode.
	again, err := openGoalArtifactRoot(p.Root)
	if err != nil {
		return err
	}
	defer again.Close()
	a, e1 := root.Stat()
	b, e2 := again.Stat()
	if e1 != nil || e2 != nil || !os.SameFile(a, b) {
		return fmt.Errorf("%w: the artifact root changed while it was being checked", errGoalDelivery)
	}
	return nil
}

func decodeGoalDelivery(spec nativeGoalSpec, seal goalBriefSeal, p goalDeliveryProfile, b []byte) (goalBriefDelivery, error) {
	var d goalBriefDelivery
	if len(b) > goalSealMaxBytes || !utf8.Valid(b) || !nativeJSONScalars(b) {
		return d, errGoalDelivery
	}
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.UseNumber()
	v, err := queueJSON(decoder, 1)
	if err != nil {
		return d, errGoalDelivery
	}
	if _, err = decoder.Token(); err != io.EOF {
		return d, errGoalDelivery
	}
	m, ok := v.(map[string]any)
	if !ok || !nativeClosedFields(m, []string{"version", "engine", "code_point_cap", "mode", "prompt", "artifact", "device", "inode", "full_sha256", "full_bytes", "full_code_points", "prompt_sha256", "prompt_bytes", "prompt_code_points"}, nil) || json.Unmarshal(b, &d) != nil {
		return goalBriefDelivery{}, errGoalDelivery
	}
	if err = validateGoalDelivery(spec, seal, p, d); err != nil {
		return goalBriefDelivery{}, err
	}
	return d, nil
}

func goalArtifactPath(path string) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" || len(path) > 4096 || !utf8.ValidString(path) {
		return false
	}
	for _, r := range path {
		if unicode.IsControl(r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) || r == '\ufeff' {
			return false
		}
	}
	return true
}

func openGoalArtifactRoot(path string) (*os.File, error) {
	refusal := fmt.Errorf("%w: the selected artifact root is unreadable or not protected; use an existing owner-owned 0700 directory without linked or unprotected ancestors", errGoalDelivery)
	if !goalArtifactPath(path) {
		return nil, refusal
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, refusal
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, part := range parts {
		next, e := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if e != nil {
			return nil, refusal
		}
		fd = next
		var st unix.Stat_t
		if unix.Fstat(fd, &st) != nil || (st.Uid != 0 && st.Uid != uint32(os.Geteuid())) ||
			(st.Mode&0022 != 0 && st.Mode&unix.S_ISVTX == 0) ||
			(i == len(parts)-1 && (st.Uid != uint32(os.Geteuid()) || st.Mode&07777 != 0700)) {
			unix.Close(fd)
			return nil, refusal
		}
	}
	return os.NewFile(uintptr(fd), "goal-artifact-root"), nil
}

func readGoalArtifact(root *os.File, name, full string, confirm func(*os.File) error) ([]byte, unix.Stat_t, error) {
	var zero unix.Stat_t
	refusal := fmt.Errorf("%w: the artifact is missing, linked, not a private regular file, changed during reading or differs from the sealed full text; preserve it for review", errGoalDelivery)
	fd, err := unix.Openat(int(root.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, zero, refusal
	}
	f := os.NewFile(uintptr(fd), "sealed-goal-artifact")
	defer f.Close()
	var before, after, named unix.Stat_t
	if checkLaneControlFile(fd) != nil || unix.Fstat(fd, &before) != nil || before.Size != int64(len(full)) || before.Size > goalBriefMaxBytes {
		return nil, zero, refusal
	}
	beforeInfo, err := f.Stat()
	if err != nil {
		return nil, zero, refusal
	}
	b, err := io.ReadAll(io.LimitReader(f, goalBriefMaxBytes+1))
	// Another publisher may have written every byte but not yet synced the
	// file. A new delivery confirms it through this same checked descriptor;
	// retained readback remains read-only and relies on its prior publication.
	if err == nil && confirm != nil {
		err = confirm(f)
	}
	afterInfo, statErr := f.Stat()
	if err != nil || !bytes.Equal(b, []byte(full)) || checkLaneControlFile(fd) != nil || unix.Fstat(fd, &after) != nil ||
		statErr != nil || !beforeInfo.ModTime().Equal(afterInfo.ModTime()) || before.Size != after.Size ||
		unix.Fstatat(int(root.Fd()), name, &named, unix.AT_SYMLINK_NOFOLLOW) != nil || named.Dev != after.Dev || named.Ino != after.Ino {
		return nil, zero, refusal
	}
	return b, after, nil
}
