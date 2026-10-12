package nuntius

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

func TestBriefDeliveryExistingSync(t *testing.T) {
	spec, seal, p := deliveryFixture(t, 5000)
	d, err := deliverGoalBrief(spec, seal, p)
	if err != nil {
		t.Fatal(err)
	}
	root, err := openGoalArtifactRoot(p.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	full := seal.Body + seal.Profile.Separator + seal.Standards
	called := false
	_, _, err = readGoalArtifact(root, filepath.Base(d.Artifact), full, func(f *os.File) error {
		called = true
		st, e := f.Stat()
		if e != nil || st.Size() != int64(d.FullBytes) {
			t.Fatal("sync did not receive the complete read descriptor")
		}
		return errors.New("injected sync failure")
	})
	if !called || err == nil {
		t.Fatal("existing artifact returned without confirming file durability")
	}
	// Failure preserves the artifact for review and the prior saved identity.
	if err = validateGoalDelivery(spec, seal, p, d); err != nil {
		t.Fatal("sync failure damaged retained artifact", err)
	}
	called = false
	_, _, err = readGoalArtifact(root, filepath.Base(d.Artifact), full, func(f *os.File) error { called = true; return f.Sync() })
	if !called || err != nil {
		t.Fatal("confirmed existing artifact refused", err)
	}
}

func deliveryRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	return root
}

func deliveryFixture(t *testing.T, points int) (nativeGoalSpec, goalBriefSeal, goalDeliveryProfile) {
	t.Helper()
	spec, in := briefFixture(t)
	in.Body = append(in.Body, []byte(strings.Repeat("界", points))...)
	seal, err := assembleGoalBrief(spec, 1, in)
	if err != nil {
		t.Fatal(err)
	}
	return spec, seal, goalDeliveryProfile{Engine: spec.LaneKind, CodePointCap: 4000, Root: deliveryRoot(t)}
}

func TestBriefDeliveryCodePointsAndWrapper(t *testing.T) {
	for _, engine := range []nativeEngineKind{"claude", "codex", "opencode"} {
		for _, wrapper := range []string{"", "/goal\n"} {
			spec, in := briefFixture(t)
			spec.LaneKind = engine
			spec.Kinds = []nativeEngineKind{engine}
			in.Profile.Wrapper = wrapper
			base, err := assembleGoalBrief(spec, 1, in)
			if err != nil {
				t.Fatal(err)
			}
			cap := 1000
			in.Body = append(in.Body, []byte(strings.Repeat("é", cap-base.DispatchCodePoints))...)
			seal, err := assembleGoalBrief(spec, 1, in)
			if err != nil {
				t.Fatal(err)
			}
			p := goalDeliveryProfile{Engine: engine, CodePointCap: cap, Root: deliveryRoot(t)}
			d, err := deliverGoalBrief(spec, seal, p)
			if err != nil || d.Mode != "direct" || d.PromptCodePoints != cap || d.PromptBytes <= cap || d.PromptSHA256 != seal.DispatchSHA256 {
				t.Fatal("exact code-point boundary refused or bytes altered", err)
			}
			entries, _ := os.ReadDir(p.Root)
			if len(entries) != 0 {
				t.Fatal("direct delivery created an artifact")
			}
			in.Body = append(in.Body, []byte("é")...)
			seal, err = assembleGoalBrief(spec, 1, in)
			if err != nil {
				t.Fatal(err)
			}
			d, err = deliverGoalBrief(spec, seal, p)
			if err != nil || d.Mode != "pointer" || d.PromptCodePoints > cap || d.PromptBytes > 12000 || !strings.HasPrefix(d.Prompt, wrapper+"GOAL:") {
				t.Fatal("wrapper/engine overflow did not use bounded pointer", err)
			}
			full, _ := os.ReadFile(d.Artifact)
			if string(full) != seal.Body+seal.Profile.Separator+seal.Standards || strings.Count(string(full), seal.Profile.Marker) != 1 || d.FullSHA256 != briefDigest(full) || d.FullBytes != len(full) || d.FullCodePoints != utf8.RuneCount(full) || d.PromptSHA256 != briefDigest([]byte(d.Prompt)) {
				t.Fatal("delivery measurements or standards changed")
			}
		}
	}
}

func TestBriefDeliveryRetention(t *testing.T) {
	spec, seal, p := deliveryFixture(t, 5000)
	d, err := deliverGoalBrief(spec, seal, p)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(d.Artifact)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		again, err := deliverGoalBrief(spec, seal, p, d)
		if err != nil || !reflect.DeepEqual(d, again) {
			t.Fatal("retained delivery changed", err)
		}
		encoded, _ := json.Marshal(d)
		saved, err := decodeGoalDelivery(spec, seal, p, encoded)
		if err != nil || !reflect.DeepEqual(d, saved) {
			t.Fatal("saved delivery did not replay", err)
		}
	}
	again, err := deliverGoalBrief(spec, seal, p)
	if err != nil || !reflect.DeepEqual(d, again) {
		t.Fatal("existing artifact not reused", err)
	}
	after, _ := os.Stat(d.Artifact)
	if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) || after.Mode().Perm() != 0600 {
		t.Fatal("retry replaced or edited artifact")
	}
	// A refreshed revision gets its own artifact and leaves the resume file.
	in := goalBriefInput{Body: []byte(seal.Body + "new revision"), Standards: []byte(seal.Standards), Profile: seal.Profile, Authority: seal.Authority, SourceState: seal.SourceState, ReviewedSourceState: seal.SourceState}
	newSeal, err := assembleGoalBrief(spec, 2, in)
	if err != nil {
		t.Fatal(err)
	}
	newD, err := deliverGoalBrief(spec, newSeal, p)
	if err != nil || newD.Artifact == d.Artifact {
		t.Fatal("new revision overwrote old artifact", err)
	}
	if err = validateGoalDelivery(spec, seal, p, d); err != nil {
		t.Fatal("resume artifact not retained", err)
	}
}

func TestBriefDeliveryRetainedPredicate(t *testing.T) {
	spec, seal, p := deliveryFixture(t, 5000)
	d, err := deliverGoalBrief(spec, seal, p)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*goalBriefDelivery){
		func(d *goalBriefDelivery) { d.FullSHA256 = strings.Repeat("0", 64) },
		func(d *goalBriefDelivery) { d.PromptSHA256 = strings.Repeat("0", 64) },
		func(d *goalBriefDelivery) { d.Prompt += "altered" },
		func(d *goalBriefDelivery) { d.FullBytes++ },
		func(d *goalBriefDelivery) { d.PromptCodePoints++ },
		func(d *goalBriefDelivery) { d.Inode++ },
		func(d *goalBriefDelivery) { d.Artifact = filepath.Join(p.Root, "other") },
		func(d *goalBriefDelivery) { d.Engine = "claude" },
		func(d *goalBriefDelivery) { d.CodePointCap++ },
	} {
		bad := d
		change(&bad)
		if _, err := deliverGoalBrief(spec, seal, p, bad); err == nil {
			t.Fatal("writer accepted retained mismatch")
		}
		b, _ := json.Marshal(bad)
		if _, err := decodeGoalDelivery(spec, seal, p, b); err == nil {
			t.Fatal("replay accepted retained mismatch")
		}
	}
	changed := seal
	changed.Body += "tampered"
	if _, err := deliverGoalBrief(spec, changed, p); err == nil {
		t.Fatal("unsealed text delivered")
	}
	b, _ := json.Marshal(d)
	if _, err := decodeGoalDelivery(spec, changed, p, b); err == nil {
		t.Fatal("replay accepted tampered seal")
	}
}

func TestBriefDeliveryStrictDecoder(t *testing.T) {
	spec, seal, p := deliveryFixture(t, 5000)
	d, err := deliverGoalBrief(spec, seal, p)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(d)
	for _, bad := range [][]byte{
		bytes.Replace(b, []byte(`"mode"`), []byte(`"Mode"`), 1),
		bytes.Replace(b, []byte(`"mode":"pointer"`), []byte(`"mode":"pointer","Mode":"pointer"`), 1),
		bytes.Replace(b, []byte(`"mode":"pointer"`), []byte(`"mode":"pointer","\u006dode":"pointer"`), 1),
		bytes.Replace(b, []byte(`"mode":"pointer"`), []byte(`"mode":null`), 1),
		append(append([]byte(nil), b...), []byte(` {}`)...),
	} {
		if _, err := decodeGoalDelivery(spec, seal, p, bad); err == nil {
			t.Fatal("noncanonical saved descriptor accepted")
		}
	}
	escaped := bytes.Replace(b, []byte(`"mode"`), []byte(`"\u006dode"`), 1)
	if _, err := decodeGoalDelivery(spec, seal, p, escaped); err != nil {
		t.Fatal("canonical unescaped key refused", err)
	}
}

func TestBriefDeliveryUnsafeFiles(t *testing.T) {
	for _, kind := range []string{"symlink", "fifo", "directory", "hardlink", "mode", "corrupt", "same bytes swap", "root swap", "ancestor symlink", "ancestor writable"} {
		t.Run(kind, func(t *testing.T) {
			spec, seal, p := deliveryFixture(t, 5000)
			d, err := deliverGoalBrief(spec, seal, p)
			if err != nil {
				t.Fatal(err)
			}
			full, _ := os.ReadFile(d.Artifact)
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			switch kind {
			case "symlink":
				must(os.Rename(d.Artifact, d.Artifact+"-original"))
				must(os.Symlink(d.Artifact+"-original", d.Artifact))
			case "fifo":
				must(os.Remove(d.Artifact))
				must(unix.Mkfifo(d.Artifact, 0600))
			case "directory":
				must(os.Remove(d.Artifact))
				must(os.Mkdir(d.Artifact, 0600))
			case "hardlink":
				must(os.Link(d.Artifact, d.Artifact+"-link"))
			case "mode":
				must(os.Chmod(d.Artifact, 0644))
			case "corrupt":
				must(os.WriteFile(d.Artifact, []byte(strings.Repeat("z", len(full))), 0600))
			case "same bytes swap":
				must(os.Rename(d.Artifact, d.Artifact+"-original"))
				must(os.WriteFile(d.Artifact, full, 0600))
			case "root swap":
				must(os.Rename(p.Root, p.Root+"-original"))
				t.Cleanup(func() { os.RemoveAll(p.Root + "-original") })
				must(os.Mkdir(p.Root, 0700))
				must(os.WriteFile(d.Artifact, full, 0600))
			case "ancestor symlink":
				must(os.Rename(p.Root, p.Root+"-original"))
				t.Cleanup(func() { os.RemoveAll(p.Root + "-original") })
				must(os.Symlink(p.Root+"-original", p.Root))
			case "ancestor writable":
				must(os.Chmod(filepath.Dir(p.Root), 0777))
				t.Cleanup(func() { os.Chmod(filepath.Dir(p.Root), 0755) })
			}
			if _, err := deliverGoalBrief(spec, seal, p, d); err == nil {
				t.Fatal("retained writer accepted unsafe artifact")
			}
			b, _ := json.Marshal(d)
			if _, err := decodeGoalDelivery(spec, seal, p, b); err == nil {
				t.Fatal("replay accepted unsafe artifact")
			}
			if kind != "same bytes swap" && kind != "root swap" {
				if _, err := deliverGoalBrief(spec, seal, p); err == nil {
					t.Fatal("new delivery accepted unsafe path")
				}
			}
		})
	}
}

func TestBriefDeliveryRefusalsAndFullLimit(t *testing.T) {
	spec, seal, p := deliveryFixture(t, 1)
	for _, root := range []string{"", "relative", p.Root + "/../escape", p.Root + "\u2028", p.Root + "\u2029", p.Root + "\u0085", filepath.Join(p.Root, "missing")} {
		bad := p
		bad.Root = root
		if _, err := deliverGoalBrief(spec, seal, bad); err == nil {
			t.Fatal("unsafe or unreadable profile root accepted")
		}
	}
	for _, cap := range []int{0, -1, 1} {
		bad := p
		bad.CodePointCap = cap
		if _, err := deliverGoalBrief(spec, seal, bad); err == nil {
			t.Fatal("invalid cap or oversized pointer accepted")
		}
	}
	bad := p
	bad.Engine = "opencode"
	if _, err := deliverGoalBrief(spec, seal, bad); err == nil {
		t.Fatal("engine outside sealed spec accepted")
	}
	in := goalBriefInput{Body: []byte(seal.Body), Standards: []byte(seal.Standards), Profile: seal.Profile, Authority: seal.Authority, SourceState: seal.SourceState, ReviewedSourceState: seal.SourceState}
	in.Body = append(in.Body, []byte(strings.Repeat("x", goalBriefMaxBytes-seal.Bytes))...)
	boundary, err := assembleGoalBrief(spec, 1, in)
	if err != nil {
		t.Fatal(err)
	}
	d, err := deliverGoalBrief(spec, boundary, p)
	if err != nil || d.FullBytes != goalBriefMaxBytes {
		t.Fatal("full artifact boundary refused", err)
	}
	in.Body = append(in.Body, 'x')
	if _, err := assembleGoalBrief(spec, 1, in); err == nil {
		t.Fatal("oversized full body assembled")
	}
	boundary.Body += "x"
	if _, err := deliverGoalBrief(spec, boundary, p); err == nil {
		t.Fatal("oversized full body delivered")
	}
}

func TestBriefDeliveryFakeLane(t *testing.T) {
	spec, seal, p := deliveryFixture(t, 8000)
	d, err := deliverGoalBrief(spec, seal, p)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(fakeBinary, "brief-pointer")
	cmd.Stdin = strings.NewReader(d.Prompt)
	// Run waits/reaps the offline child before any assertion or test return.
	out, err := cmd.Output()
	if err != nil {
		t.Fatal("offline artifact reader failed", err)
	}
	if cmd.ProcessState == nil || !cmd.ProcessState.Exited() || !cmd.ProcessState.Success() {
		t.Fatal("offline child not ended and reaped")
	}
	want := d.FullSHA256 + " " + strconv.Itoa(d.FullBytes) + "\n"
	if string(out) != want || strings.Contains(string(out), "SHARED STANDARDS") {
		t.Fatal("fake did not read the entire sealed file privately")
	}
}

func TestBriefDelivery(t *testing.T) {
	spec, in := briefFixture(t)
	for _, tc := range []struct {
		name    string
		fill    string
		cap     int
		pointer bool
	}{
		{"byte limit", "x", 12000, false},
		{"byte overflow", "x", 12000, true},
		{"unicode byte overflow", "界", 12000, true},
		{"engine point overflow", "x", 4000, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := in
			n := 12000 - len(in.Profile.Wrapper) - len(in.Body) - len(in.Profile.Separator) - len(in.Standards)
			if tc.pointer {
				n++
			}
			input.Body = append(append([]byte(nil), in.Body...), []byte(strings.Repeat(tc.fill, n))...)
			seal, err := assembleGoalBrief(spec, 1, input)
			if err != nil {
				t.Fatal(err)
			}
			d, err := deliverGoalBrief(spec, seal, goalDeliveryProfile{Engine: spec.LaneKind, CodePointCap: tc.cap, Root: deliveryRoot(t)})
			if err != nil {
				t.Fatal(err)
			}
			want := "direct"
			if tc.pointer {
				want = "pointer"
			}
			if d.Mode != want {
				t.Fatalf("delivery mode = %q, want %q", d.Mode, want)
			}
		})
	}
}
