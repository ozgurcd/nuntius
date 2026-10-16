package nuntius

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func p0MetadataFixture(t *testing.T) (context.Context, *Manager, Run) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	c, store := fixture(t)
	for _, path := range []string{store, filepath.Join(store, "runs"), filepath.Join(store, "locks")} {
		if ctx.Err() != nil || privateDir(path) != nil {
			t.Fatal("metadata fixture directory")
		}
	}
	m, err := newReadManager(c, store)
	if err != nil || m.prepareControlStore() != nil || ctx.Err() != nil || m.recoverLaneControls(nil) != nil {
		t.Fatal("metadata fixture manager")
	}
	t.Cleanup(func() { _ = m.Close() })
	r := seedCrashRun(t, m, strings.Repeat("a", 32), "Q9-METADATA", "completed")
	r.State = "ended"
	if m.save(&r) != nil {
		t.Fatal("metadata fixture seed")
	}
	return ctx, m, r
}

func TestP0MetadataReuse(t *testing.T) {
	ctx, m, r := p0MetadataFixture(t)
	for range 2 {
		runs, err := m.historyContext(ctx)
		if err != nil || len(runs) != 1 || runs[0].ID != r.ID {
			t.Fatal("metadata history identity")
		}
		// Mutating a projection must never mutate retained private bytes.
		runs[0].Brief = "changed projection"
	}
	if m.metadata.opens.Load() != 1 {
		t.Fatal("unchanged history reopened private metadata")
	}
	guard, err := m.admissionControlLockWithin(dispatchControlLockWait, ctx)
	if err != nil {
		t.Fatal("metadata admission guard")
	}
	defer guard.Close()
	if m.admitLaneLocked("codex", ctx) != nil || m.metadata.opens.Load() != 1 {
		t.Fatal("fresh admission did not reuse validated metadata")
	}
	runs, err := m.historyContext(ctx)
	if err != nil || runs[0].Brief != r.Brief || m.metadata.hits.Load() < 3 {
		t.Fatal("metadata projection isolation")
	}
	r.Brief = "new stored briefing"
	if m.save(&r) != nil {
		t.Fatal("metadata own write")
	}
	if lockContext(ctx, &m.metadata.mu) != nil {
		t.Fatal("metadata invalidation lock")
	}
	_, retained := m.metadata.entries[r.ID]
	m.metadata.mu.Unlock()
	if retained {
		t.Fatal("own write did not explicitly invalidate its entry")
	}
	runs, err = m.historyContext(ctx)
	if err != nil || len(runs) != 1 || runs[0].Brief != r.Brief || m.metadata.opens.Load() != 2 {
		t.Fatal("metadata own write freshness")
	}
}

func TestP0MetadataMissingMessage(t *testing.T) {
	ctx, m, r := p0MetadataFixture(t)
	if _, err := m.historyContext(ctx); err != nil {
		t.Fatal("message absence fixture history")
	}
	root, err := openLaneControlRoot(m.store)
	if err != nil {
		t.Fatal("message absence fixture root")
	}
	defer root.Close()
	runs, err := heldDirectory(root, "runs")
	if err != nil {
		t.Fatal("message absence fixture runs")
	}
	defer runs.Close()
	dir, err := heldDirectory(runs, r.ID)
	if err != nil {
		t.Fatal("message absence fixture run")
	}
	defer dir.Close()
	for range 2 {
		if _, err := m.metadata.readMessage(ctx, dir, r.ID); !os.IsNotExist(err) {
			t.Fatal("missing message did not preserve absence")
		}
	}
	if m.metadata.messageOpens.Load() != 1 {
		t.Fatal("unchanged missing message reopened")
	}
	path := filepath.Join(m.store, "runs", r.ID)
	info, err := os.Stat(path)
	if err != nil || os.WriteFile(filepath.Join(path, "message.json"), []byte("first"), 0600) != nil || os.Chtimes(path, info.ModTime(), info.ModTime()) != nil {
		t.Fatal("message creation fixture")
	}
	if b, err := m.metadata.readMessage(ctx, dir, r.ID); err != nil || string(b) != "first" {
		t.Fatal("message creation with restored directory mtime was missed")
	}
	if os.WriteFile(filepath.Join(path, "message.json"), []byte("other"), 0600) != nil {
		t.Fatal("positive message rewrite fixture")
	}
	if b, err := m.metadata.readMessage(ctx, dir, r.ID); err != nil || string(b) != "other" {
		t.Fatal("positive message content was cached")
	}
	if os.Rename(filepath.Join(path, "message.json"), filepath.Join(path, "archived-message.json")) != nil {
		t.Fatal("message removal fixture")
	}
	if _, err := m.historyContext(ctx); err != nil {
		t.Fatal("message removal metadata refresh")
	}
	for range 2 {
		if _, err := m.metadata.readMessage(ctx, dir, r.ID); !os.IsNotExist(err) {
			t.Fatal("removed message did not become absent")
		}
	}
	if m.metadata.messageOpens.Load() != 4 {
		t.Fatal("removed message absence was not reused")
	}
	if m.save(&r) != nil {
		t.Fatal("own write invalidation fixture")
	}
	if _, ok := m.metadata.entries[r.ID]; ok {
		t.Fatal("own write retained message absence")
	}
	if os.Symlink(filepath.Join(path, "archived-message.json"), filepath.Join(path, "message.json")) != nil {
		t.Fatal("message symlink fixture")
	}
	if _, err := m.metadata.readMessage(ctx, dir, r.ID); err == nil || os.IsNotExist(err) {
		t.Fatal("message symlink bypassed fresh checks")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	opens := m.metadata.messageOpens.Load()
	if _, err := m.metadata.readMessage(canceled, dir, r.ID); err != context.Canceled || m.metadata.messageOpens.Load() != opens {
		t.Fatal("canceled message read performed I/O")
	}
	if _, err := m.metadata.readMessage(ctx, dir, "../outside"); err == nil || m.metadata.messageOpens.Load() != opens {
		t.Fatal("message name guard performed I/O")
	}
}

func TestP0MetadataRewriteAndIntegrity(t *testing.T) {
	ctx, m, r := p0MetadataFixture(t)
	if _, err := m.historyContext(ctx); err != nil {
		t.Fatal("metadata warmup")
	}
	meta := filepath.Join(m.store, "runs", r.ID, "meta.json")
	before, err := os.Stat(meta)
	if err != nil {
		t.Fatal("metadata timestamp")
	}
	r.Brief = "note no-session" // same length as the seeded brief
	r.Integrity = ""
	b, _ := json.Marshal(r)
	r.Integrity = laneControlDigest(b)
	b, _ = json.Marshal(r)
	if int64(len(b)) != before.Size() || os.WriteFile(meta, b, 0600) != nil || os.Chtimes(meta, before.ModTime(), before.ModTime()) != nil {
		t.Fatal("same-size restored-time fixture")
	}
	runs, err := m.historyContext(ctx)
	if err != nil || len(runs) != 1 || runs[0].Brief != r.Brief || m.metadata.opens.Load() != 2 {
		t.Fatal("same-size restored-mtime rewrite reused stale bytes")
	}
	// An external corrupt rewrite must be revalidated even when it has the
	// original byte length and mtime. Admission must fail closed.
	b = bytes.Replace(b, []byte("note no-session"), []byte("fake no-session"), 1)
	if os.WriteFile(meta, b, 0600) != nil || os.Chtimes(meta, before.ModTime(), before.ModTime()) != nil {
		t.Fatal("metadata integrity fixture")
	}
	runs, err = m.historyContext(ctx)
	if err != nil || len(runs) != 0 {
		t.Fatal("corrupt metadata bypassed record validation")
	}
	guard, err := m.admissionControlLockWithin(dispatchControlLockWait, ctx)
	if err != nil {
		t.Fatal("metadata integrity guard")
	}
	defer guard.Close()
	if m.admitLaneLocked("codex", ctx) == nil {
		t.Fatal("cached admission accepted a corrupt record")
	}
}

func TestP0MetadataFilesystemGuards(t *testing.T) {
	for _, mode := range []string{"permissions", "hardlink", "symlink", "directory-symlink", "replacement"} {
		t.Run(mode, func(t *testing.T) {
			ctx, m, r := p0MetadataFixture(t)
			if _, err := m.historyContext(ctx); err != nil {
				t.Fatal("metadata guard warmup")
			}
			dir := filepath.Join(m.store, "runs", r.ID)
			meta := filepath.Join(dir, "meta.json")
			original, err := os.ReadFile(meta)
			if err != nil {
				t.Fatal("metadata guard fixture")
			}
			archive := filepath.Join(filepath.Dir(m.store), "metadata-archive")
			switch mode {
			case "permissions":
				if os.Chmod(meta, 0644) != nil {
					t.Fatal("metadata mode fixture")
				}
				defer func() { _ = os.Chmod(meta, 0600) }()
			case "hardlink":
				if os.Link(meta, archive) != nil {
					t.Fatal("metadata link fixture")
				}
				defer func() {
					if m.save(&r) != nil {
						t.Error("metadata link fixture restore")
					}
				}()
			case "symlink":
				if os.Rename(meta, archive) != nil || os.Symlink(archive, meta) != nil {
					t.Fatal("metadata symlink fixture")
				}
				defer func() {
					if os.Rename(archive, meta) != nil {
						t.Error("metadata symlink fixture restore")
					}
				}()
			case "directory-symlink":
				if os.Rename(dir, archive) != nil || os.Symlink(archive, dir) != nil {
					t.Fatal("metadata directory fixture")
				}
				defer func() {
					link := filepath.Join(filepath.Dir(m.store), "metadata-link-archive")
					if os.Rename(dir, link) != nil || os.Rename(archive, dir) != nil {
						t.Error("metadata directory fixture restore")
					}
				}()
			case "replacement":
				if os.Rename(dir, archive) != nil || privateDir(dir) != nil || putPrivate(meta, original) != nil {
					t.Fatal("metadata replacement fixture")
				}
			}
			runs, err := m.historyContext(ctx)
			if mode == "replacement" {
				if err != nil || len(runs) != 1 || m.metadata.opens.Load() != 2 {
					t.Fatal("directory identity replacement reused old entry")
				}
			} else if err == nil {
				t.Fatal("unsafe filesystem shape reused cached bytes")
			}
		})
	}
}

func TestP0MetadataOversize(t *testing.T) {
	ctx, m, r := p0MetadataFixture(t)
	if _, err := m.historyContext(ctx); err != nil {
		t.Fatal("oversized metadata warmup")
	}
	meta := filepath.Join(m.store, "runs", r.ID, "meta.json")
	if os.WriteFile(meta, make([]byte, 131073), 0600) != nil {
		t.Fatal("oversized metadata fixture")
	}
	runs, err := m.historyContext(ctx)
	if err != nil || len(runs) != 0 {
		t.Fatal("oversized metadata blocked otherwise intact history")
	}
	guard, err := m.admissionControlLockWithin(dispatchControlLockWait, ctx)
	if err != nil {
		t.Fatal("oversized metadata guard")
	}
	defer guard.Close()
	if m.admitLaneLocked("codex", ctx) == nil {
		t.Fatal("oversized metadata did not fail admission closed")
	}
}

func TestP0MetadataNamesAndCancellation(t *testing.T) {
	ctx, m, r := p0MetadataFixture(t)
	dir, err := os.Open(filepath.Join(m.store, "runs", r.ID))
	if err != nil {
		t.Fatal("metadata name fixture")
	}
	defer dir.Close()
	for _, id := range []string{"", "a b", "a'b", "$(id)", "`id`", "a;b", "a|b", "a\nb", "a\x00b", "-a", "../a", "/a", strings.Repeat("a", 33), strings.Repeat("A", 32), "а" + strings.Repeat("a", 31)} {
		if _, err := m.metadata.read(ctx, dir, id); err == nil || m.save(&Run{ID: id}) == nil {
			t.Fatal("hostile run name reached metadata access")
		}
	}
	if m.metadata.opens.Load() != 0 {
		t.Fatal("invalid names opened metadata")
	}
	if lockContext(ctx, &m.metadata.mu) != nil {
		t.Fatal("metadata cancellation fixture lock")
	}
	wait, stop := context.WithTimeout(ctx, 20*time.Millisecond)
	start := time.Now()
	_, _, err = m.metadata.lookup(wait, r.ID, runMetadataStamp{})
	stop()
	m.metadata.mu.Unlock()
	if err == nil || time.Since(start) > 100*time.Millisecond {
		t.Fatal("metadata lock ignored deadline")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := m.metadata.read(canceled, dir, r.ID); err == nil || m.metadata.opens.Load() != 0 {
		t.Fatal("canceled metadata read performed file access")
	}
}

func TestP0MetadataBudgetAndInvalidation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var c runMetadataCache
	retained := make([]byte, 1, 131072)
	if c.remember(ctx, strings.Repeat("a", 32), runMetadataStamp{}, retained) != nil || c.bytes != cap(retained) {
		t.Fatal("metadata budget omitted retained buffer capacity")
	}
	c.invalidateAll()
	for i := range runMetadataEntries + 1 {
		if c.remember(ctx, fmt.Sprintf("%032x", i), runMetadataStamp{}, []byte{1}) != nil || len(c.entries) > runMetadataEntries || c.bytes > runMetadataBytes {
			t.Fatal("metadata entry budget")
		}
	}
	large := make([]byte, 131072)
	for i := range runMetadataBytes/len(large) + 1 {
		if c.remember(ctx, fmt.Sprintf("%032x", i), runMetadataStamp{}, large) != nil || c.bytes > runMetadataBytes {
			t.Fatal("metadata byte budget")
		}
	}
	if lockContext(ctx, &c.mu) != nil {
		t.Fatal("metadata invalidation fixture lock")
	}
	c.invalidate(strings.Repeat("a", 32))
	c.mu.Unlock()
	if !c.clearNext.Load() {
		t.Fatal("contended invalidation was lost")
	}
	if _, _, err := c.lookup(ctx, strings.Repeat("a", 32), runMetadataStamp{}); err != nil || len(c.entries) != 0 || c.bytes != 0 {
		t.Fatal("deferred invalidation did not clear retained data")
	}
	c.invalidateAll()
}
