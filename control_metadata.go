package nuntius

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

const runMetadataEntries = 4096
const runMetadataBytes = 16 << 20

var errRunMetadataLimit = errors.New("run metadata exceeds limit")
var errRunMetadataChanged = errors.New("run metadata changed during read")

// Metadata is private, immutable cache data, never a policy or process-identity
// decision. Every use checks anchored directory/file identity and all stat
// fields except access time, then repeats the caller's full record validation.
type runMetadataStamp struct{ directory, file unix.Stat_t }
type runMetadataEntry struct {
	stamp         runMetadataStamp
	data          []byte
	messageAbsent bool
	validatedAt   time.Time
}
type runMetadataCache struct {
	mu            sync.Mutex
	entries       map[string]runMetadataEntry
	bytes         int
	clearNext     atomic.Bool
	opens         atomic.Uint64 // cache fills; recent-stamp revalidation is separate
	revalidations atomic.Uint64
	hits          atomic.Uint64
	messageOpens  atomic.Uint64
}
type runMetadataScan struct {
	ctx     context.Context
	cache   *runMetadataCache
	history *[]Run // ephemeral output of this one held admission transaction
}

func (m *Manager) metadataCache() *runMetadataCache {
	if m.metadataSource != nil {
		return m.metadataSource
	}
	return &m.metadata
}

func metadataStampAt(ctx context.Context, dir *os.File) (runMetadataStamp, error) {
	var s runMetadataStamp
	if err := ctx.Err(); err != nil {
		return s, err
	}
	if unix.Fstat(int(dir.Fd()), &s.directory) != nil || s.directory.Mode&unix.S_IFMT != unix.S_IFDIR || s.directory.Mode&07777 != 0700 || s.directory.Uid != uint32(os.Geteuid()) {
		return s, errLaneControlUnavailable
	}
	if err := ctx.Err(); err != nil {
		return s, err
	}
	if unix.Fstatat(int(dir.Fd()), "meta.json", &s.file, unix.AT_SYMLINK_NOFOLLOW) != nil || s.file.Mode&unix.S_IFMT != unix.S_IFREG || s.file.Mode&07777 != 0600 || s.file.Uid != uint32(os.Geteuid()) || s.file.Nlink != 1 || s.file.Size < 0 {
		return s, errLaneControlUnavailable
	}
	if s.file.Size > 131072 {
		return s, errRunMetadataLimit
	}
	// ctime stays in the key, but a recent stat identity alone cannot detect
	// same-size rewrites with restored mtime inside one filesystem clock tick.
	s.directory.Atim, s.file.Atim = unix.Timespec{}, unix.Timespec{}
	return s, ctx.Err()
}

func (c *runMetadataCache) clearLocked() {
	if c.clearNext.Swap(false) {
		c.entries, c.bytes = nil, 0
	}
}

// Use the same conservative two-second bound on Darwin and Linux: it exceeds
// both a coarse one-second Unix timestamp tick and its truncation uncertainty.
// This cache supports local Unix stores with timestamp granularity <= 2s;
// network/cloud-synced stores are unsupported. Future stamps also revalidate.
// Judge age at the START of the validating read, never at a later lookup: an
// unsafe entry must not become trusted merely by aging after a hidden rewrite.
func metadataStampReusable(stamp runMetadataStamp, validatedAt time.Time) bool {
	cutoff := validatedAt.Add(-2 * time.Second)
	for _, s := range []unix.Stat_t{stamp.directory, stamp.file} {
		for _, ts := range []unix.Timespec{s.Mtim, s.Ctim} {
			if !time.Unix(ts.Sec, ts.Nsec).Before(cutoff) {
				return false
			}
		}
	}
	return !validatedAt.IsZero()
}

func (c *runMetadataCache) lookup(ctx context.Context, id string, stamp runMetadataStamp, dir *os.File) ([]byte, bool, error) {
	if err := lockContext(ctx, &c.mu, 4); err != nil {
		return nil, false, err
	}
	c.clearLocked()
	entry, ok := c.entries[id]
	c.mu.Unlock()
	if ok && entry.stamp == stamp {
		if !metadataStampReusable(stamp, entry.validatedAt) {
			c.revalidations.Add(1)
			b, after, err := readRunMetadata(ctx, dir)
			if err != nil {
				return nil, false, err
			}
			if after != stamp || !bytes.Equal(b, entry.data) {
				return nil, false, nil
			}
		}
		c.hits.Add(1)
		return entry.data, true, nil
	}
	return nil, false, nil
}

func (c *runMetadataCache) remember(ctx context.Context, id string, stamp runMetadataStamp, b []byte, validatedAt time.Time) error {
	if !runPattern.MatchString(id) || len(b) > 131072 || cap(b) > runMetadataBytes {
		return errLaneControlUnavailable
	}
	if err := lockContext(ctx, &c.mu, 4); err != nil {
		return err
	}
	defer c.mu.Unlock()
	c.clearLocked()
	if old, ok := c.entries[id]; ok {
		c.bytes -= cap(old.data)
		delete(c.entries, id)
	}
	if len(c.entries) >= runMetadataEntries || c.bytes+cap(b) > runMetadataBytes {
		c.entries, c.bytes = nil, 0
	}
	if c.entries == nil {
		c.entries = make(map[string]runMetadataEntry)
	}
	c.entries[id] = runMetadataEntry{stamp: stamp, data: b, validatedAt: validatedAt}
	c.bytes += cap(b)
	return nil
}

// Writes never wait for a cache lock. Contention invalidates the entire cache
// on the next access; uncontended writes discard only their own record.
func (c *runMetadataCache) invalidate(id string) {
	if !c.mu.TryLock() {
		c.clearNext.Store(true)
		return
	}
	defer c.mu.Unlock()
	c.clearLocked()
	if old, ok := c.entries[id]; ok {
		delete(c.entries, id)
		c.bytes -= cap(old.data)
	}
}

func (c *runMetadataCache) invalidateAll() {
	c.clearNext.Store(true)
	if c.mu.TryLock() {
		c.clearLocked()
		c.mu.Unlock()
	}
}

type metadataContextReader struct {
	ctx  context.Context
	file *os.File
}

func (r metadataContextReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.file.Read(b)
}

func (c *runMetadataCache) read(ctx context.Context, dir *os.File, id string) ([]byte, error) {
	if !runPattern.MatchString(id) {
		return nil, errLaneControlUnavailable
	}
	for range 3 {
		b, err := c.readOnce(ctx, dir, id)
		if !errors.Is(err, errRunMetadataChanged) {
			return b, err
		}
	}
	return nil, errLaneControlUnavailable
}

func (c *runMetadataCache) readOnce(ctx context.Context, dir *os.File, id string) ([]byte, error) {
	started := time.Now()
	before, err := metadataStampAt(ctx, dir)
	if err != nil {
		return nil, err
	}
	if b, ok, err := c.lookup(ctx, id, before, dir); err != nil || ok {
		return b, err
	}
	c.opens.Add(1)
	b, after, err := readRunMetadata(ctx, dir)
	if err != nil {
		return nil, err
	}
	if err := c.remember(ctx, id, after, b, started); err != nil {
		return nil, err
	}
	return b, nil
}

// The revalidation path uses the same bounded, anchored, no-follow read as a
// cache fill. No cache lock is held across I/O; check cancellation between the
// OS-bounded local-file syscalls and on every reader call.
func readRunMetadata(ctx context.Context, dir *os.File) ([]byte, runMetadataStamp, error) {
	if err := ctx.Err(); err != nil {
		return nil, runMetadataStamp{}, err
	}
	fd, err := unix.Openat(int(dir.Fd()), "meta.json", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, runMetadataStamp{}, errLaneControlUnavailable
	}
	f := os.NewFile(uintptr(fd), "private-run-metadata")
	defer f.Close()
	if err := ctx.Err(); err != nil {
		return nil, runMetadataStamp{}, err
	}
	var opened unix.Stat_t
	if unix.Fstat(fd, &opened) != nil || opened.Mode&unix.S_IFMT != unix.S_IFREG || opened.Mode&07777 != 0600 || opened.Uid != uint32(os.Geteuid()) || opened.Nlink != 1 || opened.Size < 0 {
		return nil, runMetadataStamp{}, errLaneControlUnavailable
	}
	if opened.Size > 131072 {
		return nil, runMetadataStamp{}, errRunMetadataLimit
	}
	opened.Atim = unix.Timespec{}
	b, err := io.ReadAll(io.LimitReader(metadataContextReader{ctx, f}, 131073))
	if err != nil {
		return nil, runMetadataStamp{}, err
	}
	after, err := metadataStampAt(ctx, dir)
	if err != nil {
		return nil, runMetadataStamp{}, err
	}
	if opened != after.file || int64(len(b)) != after.file.Size {
		return nil, runMetadataStamp{}, errRunMetadataChanged
	}
	return b, after, ctx.Err()
}

// Cache only absence. Directory ctime/mtime and identity are rechecked before
// reuse, including when a caller restores mtime after creating a message.
// Positive contents, receipts and hold records always remain fresh reads.
func (c *runMetadataCache) readMessage(ctx context.Context, dir *os.File, id string) ([]byte, error) {
	if !runPattern.MatchString(id) {
		return nil, errLaneControlUnavailable
	}
	before, err := metadataStampAt(ctx, dir)
	if err != nil {
		return nil, err
	}
	if err := lockContext(ctx, &c.mu, 4); err != nil {
		return nil, err
	}
	c.clearLocked()
	entry, ok := c.entries[id]
	absent := ok && entry.stamp == before && entry.messageAbsent
	c.mu.Unlock()
	if absent {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if metadataStampReusable(before, entry.validatedAt) {
			return nil, os.ErrNotExist
		}
		// Only absence is cached. A fresh anchored existence check is enough
		// to disprove it; positive content still follows the full read below.
		var message unix.Stat_t
		err := unix.Fstatat(int(dir.Fd()), "message.json", &message, unix.AT_SYMLINK_NOFOLLOW)
		if errors.Is(err, unix.ENOENT) {
			return nil, os.ErrNotExist
		}
		if err != nil {
			return nil, errLaneControlUnavailable
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.messageOpens.Add(1)
	fd, err := unix.Openat(int(dir.Fd()), "message.json", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		after, stampErr := metadataStampAt(ctx, dir)
		if stampErr != nil {
			return nil, stampErr
		}
		if before == after {
			if err := lockContext(ctx, &c.mu, 4); err != nil {
				return nil, err
			}
			c.clearLocked()
			if entry, ok := c.entries[id]; ok && entry.stamp == after {
				entry.messageAbsent = true
				c.entries[id] = entry
			}
			c.mu.Unlock()
		}
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, errLaneControlUnavailable
	}
	f := os.NewFile(uintptr(fd), "private-run-message")
	defer f.Close()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := checkLaneControlFile(fd); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(metadataContextReader{ctx, f}, 16385))
	if err != nil || len(b) > 16384 {
		return nil, errLaneControlUnavailable
	}
	return b, ctx.Err()
}
