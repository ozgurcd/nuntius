package nuntius

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"

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
}
type runMetadataCache struct {
	mu           sync.Mutex
	entries      map[string]runMetadataEntry
	bytes        int
	clearNext    atomic.Bool
	opens        atomic.Uint64
	hits         atomic.Uint64
	messageOpens atomic.Uint64
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
	// ctime stays in the key: same-size rewrites with a restored mtime, chmod,
	// link changes and atomic replacements cannot reuse the previous bytes.
	s.directory.Atim, s.file.Atim = unix.Timespec{}, unix.Timespec{}
	return s, ctx.Err()
}

func (c *runMetadataCache) clearLocked() {
	if c.clearNext.Swap(false) {
		c.entries, c.bytes = nil, 0
	}
}

func (c *runMetadataCache) lookup(ctx context.Context, id string, stamp runMetadataStamp) ([]byte, bool, error) {
	if err := lockContext(ctx, &c.mu, 4); err != nil {
		return nil, false, err
	}
	defer c.mu.Unlock()
	c.clearLocked()
	if entry, ok := c.entries[id]; ok && entry.stamp == stamp {
		c.hits.Add(1)
		return entry.data, true, nil
	}
	return nil, false, nil
}

func (c *runMetadataCache) remember(ctx context.Context, id string, stamp runMetadataStamp, b []byte) error {
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
	c.entries[id] = runMetadataEntry{stamp: stamp, data: b}
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
	before, err := metadataStampAt(ctx, dir)
	if err != nil {
		return nil, err
	}
	if b, ok, err := c.lookup(ctx, id, before); err != nil || ok {
		return b, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(int(dir.Fd()), "meta.json", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errLaneControlUnavailable
	}
	c.opens.Add(1)
	f := os.NewFile(uintptr(fd), "private-run-metadata")
	defer f.Close()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var opened unix.Stat_t
	if unix.Fstat(fd, &opened) != nil || opened.Mode&unix.S_IFMT != unix.S_IFREG || opened.Mode&07777 != 0600 || opened.Uid != uint32(os.Geteuid()) || opened.Nlink != 1 || opened.Size < 0 {
		return nil, errLaneControlUnavailable
	}
	if opened.Size > 131072 {
		return nil, errRunMetadataLimit
	}
	opened.Atim = unix.Timespec{}
	b, err := io.ReadAll(io.LimitReader(metadataContextReader{ctx, f}, 131073))
	if err != nil {
		return nil, err
	}
	after, err := metadataStampAt(ctx, dir)
	if err != nil {
		return nil, err
	}
	if opened != after.file || int64(len(b)) != after.file.Size {
		return nil, errRunMetadataChanged
	}
	if err := c.remember(ctx, id, after, b); err != nil {
		return nil, err
	}
	return b, nil
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
		return nil, os.ErrNotExist
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
