package nuntius

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

// Fixtures encode the documented wire protocol independently of replay.
func goalReplayFrame(sequence uint64, payload []byte) []byte {
	h := make([]byte, 84)
	copy(h, "NGJF")
	binary.BigEndian.PutUint32(h[4:8], 1)
	binary.BigEndian.PutUint64(h[8:16], sequence)
	binary.BigEndian.PutUint32(h[16:20], uint32(len(payload)))
	goalReplayHeaderSum(h)
	sum := sha256.Sum256(payload)
	copy(h[52:84], sum[:])
	return append(h, payload...)
}

func goalReplayHeaderSum(h []byte) {
	sum := sha256.Sum256(h[:20])
	copy(h[20:52], sum[:])
}

func goalReplayDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "r")
	if err != nil {
		t.Fatal("fixture directory failed")
	}
	t.Cleanup(func() {
		if os.RemoveAll(dir) != nil {
			t.Error("fixture cleanup failed")
		}
	})
	return dir
}

func goalReplayWrite(t *testing.T, dir, name string, b []byte) {
	t.Helper()
	if os.WriteFile(filepath.Join(dir, name), b, 0600) != nil {
		t.Fatal("fixture write failed")
	}
}

func goalReplayBlob(t *testing.T, dir, prefix string, b []byte) string {
	t.Helper()
	sum := sha256.Sum256(b)
	name := prefix + hex.EncodeToString(sum[:])
	goalReplayWrite(t, dir, name, b)
	return name
}

func goalReplayEvent(t *testing.T, dir, id string, revision uint64, spec []byte) []byte {
	t.Helper()
	ref := goalReplayBlob(t, dir, "goal-spec-", spec)
	b, err := json.Marshal(goalJournalEvent{Kind: "goal.spec", GoalID: id, Revision: revision, SpecRef: ref})
	if err != nil {
		t.Fatal("fixture event failed")
	}
	return b
}

func goalReplayInvalid(t *testing.T, dir string, data []byte) {
	t.Helper()
	goalReplayWrite(t, dir, goalJournalName, data)
	p, err := replayGoalJournal(dir)
	if err != errGoalReplay || p != nil {
		t.Fatal("invalid history did not fail closed without a partial projection")
	}
}

func TestGoalReplay(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		dir := goalReplayDir(t)
		before, _ := os.ReadDir(dir)
		p, err := replayGoalJournal(dir)
		after, _ := os.ReadDir(dir)
		if err != nil || p == nil || p.sequence != 0 || p.committedBytes != 0 || p.tornTail || len(p.records) != 0 || !reflect.DeepEqual(before, after) {
			t.Fatal("absent journal was not an empty read-only projection")
		}
		goalReplayWrite(t, dir, goalJournalName, nil)
		p, err = replayGoalJournal(dir)
		if err != nil || p.sequence != 0 || p.tornTail {
			t.Fatal("empty journal refused")
		}
	})
	t.Run("complete", func(t *testing.T) {
		dir := goalReplayDir(t)
		doc := nativeSchemaFixture()
		doc["owner_hold"] = map[string]any{"categories": []string{"scope"}, "reason": "Review scope"}
		doc["coverage"] = map[string]any{"covered_by": "OTHER-GOAL", "reason": "Related work"}
		b := nativeSchemaBytes(t, doc)
		first := goalReplayEvent(t, dir, "EXAMPLE-DOCS", 1, b)
		doc["title"] = "Updated documentation"
		doc["writes"] = []string{"repo/docs"}
		e := goalJournalEvent{Kind: "goal.spec", GoalID: "EXAMPLE-DOCS", Revision: 2, SpecRef: goalReplayBlob(t, dir, "goal-spec-", nativeSchemaBytes(t, doc)), SealRef: goalReplayBlob(t, dir, "goal-seal-", []byte("private sealed fixture"))}
		second, _ := json.Marshal(e)
		data := append(goalReplayFrame(1, first), goalReplayFrame(2, second)...)
		goalReplayWrite(t, dir, goalJournalName, data)
		before := goalReplayTree(t, dir)
		p, err := replayGoalJournal(dir)
		q, again := replayGoalJournal(dir)
		if err != nil || again != nil || !reflect.DeepEqual(p, q) || p.sequence != 2 || p.committedBytes != int64(len(data)) || p.tornTail || len(p.records) != 1 {
			t.Fatal("complete history changed or replay was not deterministic")
		}
		s, ok := p.spec("EXAMPLE-DOCS")
		if !ok || s.Title != "Updated documentation" || p.records[s.ID].revision != 2 || p.records[s.ID].sealRef != e.SealRef {
			t.Fatal("replayed record or private reference changed")
		}
		s.Kinds[0] = "claude"
		s.Writes[0] = "changed"
		s.After[0] = "CHANGED"
		s.OwnerHold.Categories[0] = "changed"
		s.OwnerHold.Reason = "Changed"
		s.Coverage.CoveredBy = "CHANGED"
		fresh, _ := p.spec("EXAMPLE-DOCS")
		if fresh.Kinds[0] != "codex" || fresh.Writes[0] != "repo/docs" || fresh.After[0] != "EXAMPLE-REVIEW" || fresh.OwnerHold.Categories[0] != "scope" || fresh.OwnerHold.Reason != "Review scope" || fresh.Coverage.CoveredBy != "OTHER-GOAL" {
			t.Fatal("caller mutated immutable projection")
		}
		if _, ok := p.spec("MISSING"); ok {
			t.Fatal("missing goal invented")
		}
		var readers sync.WaitGroup
		for range 8 {
			readers.Go(func() {
				for range 10 {
					copySpec, _ := p.spec("EXAMPLE-DOCS")
					copySpec.OwnerHold.Categories[0] = "changed"
					if !reflect.DeepEqual(p, q) {
						t.Error("concurrent reader changed projection")
					}
				}
			})
		}
		readers.Wait()
		if !reflect.DeepEqual(before, goalReplayTree(t, dir)) {
			t.Fatal("replay changed private history")
		}
	})
	t.Run("torn-tail", func(t *testing.T) {
		dir := goalReplayDir(t)
		payload := goalReplayEvent(t, dir, "EXAMPLE-DOCS", 1, nativeSchemaBytes(t, nativeSchemaFixture()))
		first := goalReplayFrame(1, payload)
		// The next event intentionally has no blob: no incomplete event may
		// load references or publish a revision.
		payload = bytes.Replace(payload, []byte(`"revision":1`), []byte(`"revision":2`), 1)
		var event goalJournalEvent
		if json.Unmarshal(payload, &event) != nil {
			t.Fatal("fixture event decode failed")
		}
		event.SpecRef = "goal-spec-" + strings.Repeat("0", 64)
		payload, _ = json.Marshal(event)
		second := goalReplayFrame(2, payload)
		for cut := 1; cut < len(second); cut++ {
			data := append(append([]byte{}, first...), second[:cut]...)
			goalReplayWrite(t, dir, goalJournalName, data)
			p, err := replayGoalJournal(dir)
			q, err2 := replayGoalJournal(dir)
			if err != nil || err2 != nil || p.sequence != 1 || p.committedBytes != int64(len(first)) || !p.tornTail || p.records["EXAMPLE-DOCS"].revision != 1 || !reflect.DeepEqual(p, q) {
				t.Fatalf("incomplete frame published or replay changed at cut %d", cut)
			}
			got, _ := os.ReadFile(filepath.Join(dir, goalJournalName))
			if !bytes.Equal(data, got) {
				t.Fatal("read repaired or truncated tail")
			}
		}
	})
	t.Run("interior-corruption", func(t *testing.T) {
		dir := goalReplayDir(t)
		p := goalReplayEvent(t, dir, "EXAMPLE-DOCS", 1, nativeSchemaBytes(t, nativeSchemaFixture()))
		for _, offset := range []int{0, 8, 16, 20, 52, 84, len(goalReplayFrame(1, p)) - 1} {
			bad := goalReplayFrame(1, p)
			bad[offset] ^= 0x40
			goalReplayInvalid(t, dir, append(bad, goalReplayFrame(2, p)...))
		}
		bad := goalReplayFrame(1, p)
		binary.BigEndian.PutUint32(bad[16:20], 4000) // cannot disguise corruption as a torn final payload
		goalReplayInvalid(t, dir, append(bad, goalReplayFrame(2, p)...))
		goalReplayInvalid(t, dir, append(goalReplayFrame(1, p), []byte("junk")...))
		// A valid prefix and valid suffix must not hide a corrupt middle event.
		middle := goalReplayFrame(2, bytes.Replace(p, []byte(`"revision":1`), []byte(`"revision":2`), 1))
		middle[84] ^= 1
		data := append(goalReplayFrame(1, p), middle...)
		data = append(data, goalReplayFrame(3, bytes.Replace(p, []byte(`"revision":1`), []byte(`"revision":3`), 1))...)
		goalReplayInvalid(t, dir, data)
	})
	t.Run("unsupported-version", func(t *testing.T) {
		dir := goalReplayDir(t)
		p := goalReplayEvent(t, dir, "EXAMPLE-DOCS", 1, nativeSchemaBytes(t, nativeSchemaFixture()))
		b := goalReplayFrame(1, p)
		binary.BigEndian.PutUint32(b[4:8], 2)
		goalReplayHeaderSum(b)
		goalReplayInvalid(t, dir, b)
	})
	t.Run("repeated-sequence", func(t *testing.T) {
		dir := goalReplayDir(t)
		p := goalReplayEvent(t, dir, "EXAMPLE-DOCS", 1, nativeSchemaBytes(t, nativeSchemaFixture()))
		for _, seqs := range [][]uint64{{0}, {2}, {1, 1}, {1, 3}, {1, 2, 1}} {
			var b []byte
			for _, seq := range seqs {
				b = append(b, goalReplayFrame(seq, p)...)
			}
			goalReplayInvalid(t, dir, b)
		}
	})
}

func goalReplayTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal("fixture census failed")
	}
	result := map[string]string{}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		info, statErr := e.Info()
		if err != nil || statErr != nil {
			t.Fatal("fixture read failed")
		}
		sum := sha256.Sum256(b)
		result[e.Name()] = fmt.Sprintf("%x:%o:%d", sum, info.Mode(), info.ModTime().UnixNano())
	}
	return result
}

func TestGoalReplaySchema(t *testing.T) {
	base := string(nativeSchemaBytes(t, nativeSchemaFixture()))
	for name, raw := range map[string]string{
		"unknown":         strings.TrimSuffix(base, "}") + `,"state":"approved"}`,
		"duplicate":       strings.Replace(base, `"version":1`, `"version":1,"\u0076ersion":1`, 1),
		"null":            strings.Replace(base, `"writes":[]`, `"writes":null`, 1),
		"coercion":        strings.Replace(base, `"order":4`, `"order":"4"`, 1),
		"exponent":        strings.Replace(base, `"order":4`, `"order":4e0`, 1),
		"missing-scope":   strings.Replace(base, `,"writes":[]`, "", 1),
		"unknown-version": strings.Replace(base, `"version":1`, `"version":2`, 1),
		"traversal":       strings.Replace(base, `"writes":[]`, `"writes":["repo/../other"]`, 1),
		"nested-unknown":  strings.TrimSuffix(base, "}") + `,"owner_hold":{"categories":["scope"],"reason":"Ask","approved":true}}`,
		"trailing":        base + " {}",
		"utf8":            strings.Replace(base, "Review", string([]byte{255}), 1),
		"oversize":        base + strings.Repeat(" ", nativeGoalMaxBytes),
		"high-surrogate":  strings.Replace(base, "Review", `\ud800`, 1),
		"low-surrogate":   strings.Replace(base, "Review", `\udc00`, 1),
		"split-surrogate": strings.Replace(base, "Review", `\ud800x\udc00`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			dir := goalReplayDir(t)
			payload := goalReplayEvent(t, dir, "EXAMPLE-DOCS", 1, []byte(raw))
			goalReplayInvalid(t, dir, goalReplayFrame(1, payload))
		})
	}
	for name, text := range map[string]string{"pair": `\ud83d\ude00`, "literal-backslash": `\\ud800`, "replacement-character": "�"} {
		t.Run(name, func(t *testing.T) {
			dir := goalReplayDir(t)
			raw := strings.Replace(base, "Review", text, 1)
			p := goalReplayEvent(t, dir, "EXAMPLE-DOCS", 1, []byte(raw))
			goalReplayWrite(t, dir, goalJournalName, goalReplayFrame(1, p))
			if _, err := replayGoalJournal(dir); err != nil {
				t.Fatal("valid Unicode spec refused")
			}
		})
	}
}

func TestGoalReplayEvents(t *testing.T) {
	t.Run("closed-event", func(t *testing.T) {
		dir := goalReplayDir(t)
		base := string(goalReplayEvent(t, dir, "EXAMPLE-DOCS", 1, nativeSchemaBytes(t, nativeSchemaFixture())))
		for _, raw := range []string{
			strings.TrimSuffix(base, "}") + `,"body":"freeform"}`,
			strings.TrimSuffix(base, "}") + `,"kind":"goal.spec"}`,
			strings.Replace(base, `"revision":1`, `"revision":1e0`, 1),
			strings.Replace(base, `"revision":1`, `"revision":0`, 1),
			strings.Replace(base, `"revision":1`, `"revision":null`, 1),
			strings.Replace(base, `"goal.spec"`, `"goal.approved"`, 1),
			strings.Replace(base, `"EXAMPLE-DOCS"`, `"OTHER-GOAL"`, 1),
			strings.TrimSuffix(base, "}") + `,"seal_ref":null}`,
			base + " {}",
		} {
			goalReplayInvalid(t, dir, goalReplayFrame(1, []byte(raw)))
		}
	})
	t.Run("revision", func(t *testing.T) {
		dir := goalReplayDir(t)
		b := nativeSchemaBytes(t, nativeSchemaFixture())
		for _, revs := range [][]uint64{{2}, {1, 1}, {1, 3}} {
			var data []byte
			for i, rev := range revs {
				data = append(data, goalReplayFrame(uint64(i+1), goalReplayEvent(t, dir, "EXAMPLE-DOCS", rev, b))...)
			}
			goalReplayInvalid(t, dir, data)
		}
	})
	t.Run("references", func(t *testing.T) {
		for _, ref := range []string{"../blob", "/blob", "goal-spec-" + strings.Repeat("A", 64), "goal-spec-" + strings.Repeat("0", 64), "goal-seal-" + strings.Repeat("0", 64)} {
			dir := goalReplayDir(t)
			p, _ := json.Marshal(goalJournalEvent{Kind: "goal.spec", GoalID: "EXAMPLE-DOCS", Revision: 1, SpecRef: ref})
			goalReplayInvalid(t, dir, goalReplayFrame(1, p))
		}
		dir := goalReplayDir(t)
		b := nativeSchemaBytes(t, nativeSchemaFixture())
		ref := goalReplayBlob(t, dir, "goal-spec-", b)
		goalReplayWrite(t, dir, ref, append(b, ' ')) // schema valid, digest invalid
		p, _ := json.Marshal(goalJournalEvent{Kind: "goal.spec", GoalID: "EXAMPLE-DOCS", Revision: 1, SpecRef: ref})
		goalReplayInvalid(t, dir, goalReplayFrame(1, p))
		p, _ = json.Marshal(goalJournalEvent{Kind: "goal.spec", GoalID: "EXAMPLE-DOCS", Revision: 1, SpecRef: goalReplayBlob(t, dir, "goal-spec-", b), SealRef: goalReplayBlob(t, dir, "goal-seal-", bytes.Repeat([]byte{'s'}, goalSealMaxBytes+1))})
		goalReplayInvalid(t, dir, goalReplayFrame(1, p))
	})
}

func TestGoalReplaySafeFiles(t *testing.T) {
	for _, target := range []string{"journal", "spec", "seal"} {
		for _, kind := range []string{"symlink", "fifo", "directory", "public", "hardlink"} {
			t.Run(target+"-"+kind, func(t *testing.T) {
				dir := goalReplayDir(t)
				b := nativeSchemaBytes(t, nativeSchemaFixture())
				ref := goalReplayBlob(t, dir, "goal-spec-", b)
				seal := goalReplayBlob(t, dir, "goal-seal-", []byte("private fixture"))
				p, _ := json.Marshal(goalJournalEvent{Kind: "goal.spec", GoalID: "EXAMPLE-DOCS", Revision: 1, SpecRef: ref, SealRef: seal})
				goalReplayWrite(t, dir, goalJournalName, goalReplayFrame(1, p))
				name := map[string]string{"journal": goalJournalName, "spec": ref, "seal": seal}[target]
				path := filepath.Join(dir, name)
				if os.Rename(path, path+"-original") != nil {
					t.Fatal("fixture move failed")
				}
				var err error
				switch kind {
				case "symlink":
					err = os.Symlink(path+"-original", path)
				case "fifo":
					err = unix.Mkfifo(path, 0600)
				case "directory":
					err = os.Mkdir(path, 0700)
				case "public":
					err = os.Rename(path+"-original", path)
					if err == nil {
						err = os.Chmod(path, 0644)
					}
				case "hardlink":
					err = os.Link(path+"-original", path)
				}
				if err != nil {
					t.Fatal("unsafe file fixture failed")
				}
				got, e := replayGoalJournal(dir)
				if e != errGoalReplay || got != nil {
					t.Fatal("unsafe private file accepted")
				}
			})
		}
	}
	t.Run("root", func(t *testing.T) {
		dir := goalReplayDir(t)
		alias := filepath.Join(goalReplayDir(t), "alias")
		if os.Symlink(dir, alias) != nil {
			t.Fatal("fixture alias failed")
		}
		for _, path := range []string{alias, alias + "/child", dir + "/../" + filepath.Base(dir), "relative", dir + "/missing"} {
			if p, e := replayGoalJournal(path); e != errGoalReplay || p != nil {
				t.Fatal("unsafe or missing root accepted")
			}
		}
		if os.Chmod(dir, 0755) != nil {
			t.Fatal("fixture mode failed")
		}
		if p, e := replayGoalJournal(dir); e != errGoalReplay || p != nil {
			t.Fatal("nonprivate root accepted")
		}
	})
}

func TestGoalReplayBounds(t *testing.T) {
	dir := goalReplayDir(t)
	p := goalReplayEvent(t, dir, "EXAMPLE-DOCS", 1, nativeSchemaBytes(t, nativeSchemaFixture()))
	// Pin both sides of the encoded payload bound, including whitespace.
	full := append(append([]byte{}, p...), bytes.Repeat([]byte{' '}, goalJournalMaxFrameBytes-len(p))...)
	goalReplayWrite(t, dir, goalJournalName, goalReplayFrame(1, full))
	if _, err := replayGoalJournal(dir); err != nil {
		t.Fatal("exact frame byte bound refused")
	}
	goalReplayInvalid(t, dir, goalReplayFrame(1, append(full, ' ')))
	goalReplayInvalid(t, dir, goalReplayFrame(1, nil))
	// Oversized sparse history is refused without reading it into memory.
	f, err := os.OpenFile(filepath.Join(dir, goalJournalName), os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		t.Fatal("fixture open failed")
	}
	err = f.Truncate(goalJournalMaxBytes + 1)
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		t.Fatal("fixture size failed")
	}
	if got, e := replayGoalJournal(dir); e != errGoalReplay || got != nil {
		t.Fatal("oversize history accepted")
	}
}

func TestGoalReplayCapacity(t *testing.T) {
	t.Run("events", func(t *testing.T) {
		dir := goalReplayDir(t)
		ref := goalReplayBlob(t, dir, "goal-spec-", nativeSchemaBytes(t, nativeSchemaFixture()))
		var data []byte
		for i := 1; i <= goalJournalMaxEvents; i++ {
			data = append(data, goalReplayFrame(uint64(i), goalReplayPayload(t, ref, uint64(i), 0))...)
		}
		goalReplayWrite(t, dir, goalJournalName, data)
		if p, e := replayGoalJournal(dir); e != nil || p.sequence != goalJournalMaxEvents {
			t.Fatal("exact event bound refused")
		}
		data = append(data, goalReplayFrame(goalJournalMaxEvents+1, goalReplayPayload(t, ref, goalJournalMaxEvents+1, 0))...)
		goalReplayInvalid(t, dir, data)
	})
	t.Run("journal-bytes", func(t *testing.T) {
		dir := goalReplayDir(t)
		ref := goalReplayBlob(t, dir, "goal-spec-", nativeSchemaBytes(t, nativeSchemaFixture()))
		var data []byte
		var seq uint64
		for len(data) < goalJournalMaxBytes {
			seq++
			size := min(goalJournalMaxFrameBytes, goalJournalMaxBytes-len(data)-goalJournalHeaderBytes)
			data = append(data, goalReplayFrame(seq, goalReplayPayload(t, ref, seq, size))...)
		}
		goalReplayWrite(t, dir, goalJournalName, data)
		if p, e := replayGoalJournal(dir); e != nil || p.committedBytes != goalJournalMaxBytes {
			t.Fatal("exact history byte bound refused")
		}
		goalReplayInvalid(t, dir, append(data, 'N'))
	})
	t.Run("blob-work", func(t *testing.T) {
		dir := goalReplayDir(t)
		spec := nativeSchemaBytes(t, nativeSchemaFixture())
		spec = append(spec, bytes.Repeat([]byte{' '}, nativeGoalMaxBytes-len(spec))...)
		ref := goalReplayBlob(t, dir, "goal-spec-", spec)
		var data []byte
		for i := 1; i <= goalJournalMaxBlobBytes/nativeGoalMaxBytes; i++ {
			data = append(data, goalReplayFrame(uint64(i), goalReplayPayload(t, ref, uint64(i), 0))...)
		}
		goalReplayWrite(t, dir, goalJournalName, data)
		p, e := replayGoalJournal(dir)
		if e != nil || p.sequence != goalJournalMaxBlobBytes/nativeGoalMaxBytes {
			t.Fatal("exact blob work budget refused")
		}
		next := p.sequence + 1
		goalReplayInvalid(t, dir, append(data, goalReplayFrame(next, goalReplayPayload(t, ref, next, 0))...))
	})
	t.Run("goals", func(t *testing.T) {
		dir := goalReplayDir(t)
		var data []byte
		for i := 1; i <= goalJournalMaxGoals; i++ {
			doc := nativeSchemaFixture()
			id := fmt.Sprintf("G-%04d", i)
			doc["id"] = id
			data = append(data, goalReplayFrame(uint64(i), goalReplayEvent(t, dir, id, 1, nativeSchemaBytes(t, doc)))...)
		}
		goalReplayWrite(t, dir, goalJournalName, data)
		if p, e := replayGoalJournal(dir); e != nil || len(p.records) != goalJournalMaxGoals {
			t.Fatal("exact record bound refused")
		}
		doc := nativeSchemaFixture()
		doc["id"] = "EXTRA-GOAL"
		goalReplayInvalid(t, dir, append(data, goalReplayFrame(goalJournalMaxGoals+1, goalReplayEvent(t, dir, "EXTRA-GOAL", 1, nativeSchemaBytes(t, doc)))...))
	})
}

func goalReplayPayload(t *testing.T, ref string, revision uint64, size int) []byte {
	t.Helper()
	b, err := json.Marshal(goalJournalEvent{Kind: "goal.spec", GoalID: "EXAMPLE-DOCS", Revision: revision, SpecRef: ref})
	if err != nil || size > 0 && size < len(b) {
		t.Fatal("fixture event size failed")
	}
	if size > 0 {
		b = append(b, bytes.Repeat([]byte{' '}, size-len(b))...)
	}
	return b
}

func TestNativeGoalUnicodeScalars(t *testing.T) {
	base := string(nativeSchemaBytes(t, nativeSchemaFixture()))
	for _, text := range []string{`\ud800`, `\udfff`, `\ud800\ud800`, `\ud800x\udc00`, `\ud800\\udc00`, `\ud800\u0041`} {
		if _, err := decodeNativeGoalSpec([]byte(strings.Replace(base, "Review", text, 1))); err != errNativeGoalInvalid {
			t.Fatal("unpaired escaped surrogate accepted by shared decoder")
		}
	}
	for _, text := range []string{`\ud83d\ude00`, `\uD83D\uDE00`, `\\ud800`, "�"} {
		if _, err := decodeNativeGoalSpec([]byte(strings.Replace(base, "Review", text, 1))); err != nil {
			t.Fatal("valid scalar or literal escape refused")
		}
	}
}
