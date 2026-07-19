/*
 * stream-share is a project to efficiently share the use of an IPTV service.
 * Copyright (C) 2025  Lucas Duport
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <https://www.gnu.org/licenses/>.
 */

package catchup

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lucasduport/stream-share/pkg/utils"
)

const (
	indexSampleInterval = 1 << 20 // sample every 1 MB
	writeChanCap        = 2048    // ~256 MB slack at 128 KB chunks before drops
	rotationGracePeriod = 60 * time.Second
)

type indexEntry struct {
	wallTime   time.Time
	byteOffset int64
}

// segment holds the metadata for one on-disk buffer file.
type segment struct {
	filePath  string
	startTime time.Time
	index     []indexEntry
	total     int64 // bytes written to this file
}

// DiskBuffer writes an append-only MPEG-TS file and maintains a time→offset index.
// All disk writes happen in a dedicated goroutine so callers are never blocked.
//
// When maxBytes > 0 and the current file reaches that size it is rotated: current
// becomes prev (still seekable), a new file is opened, and the file that was in prev
// is deleted after rotationGracePeriod so in-flight readers can finish. At most two
// files exist on disk at once (≤ 2×maxBytes), always covering a full window of history.
type DiskBuffer struct {
	streamID string
	maxBytes int64 // 0 = unlimited

	writeCh   chan []byte   // pump goroutine sends slices here
	drainDone chan struct{} // closed by drainLoop when all writes are flushed to disk
	stopped   int32         // accessed via atomic

	stoppedMu sync.Mutex
	stoppedAt time.Time

	indexMu sync.RWMutex // guards current and prev
	current segment      // actively-written file
	prev    segment      // previous file after a rotation; empty filePath means none
}

// NewDiskBuffer creates a buffer that writes to dir/<streamID>_<unix>.ts.
// maxBytes == 0 means unlimited; otherwise the file rotates when it fills,
// keeping the previous segment available for seeking.
func NewDiskBuffer(dir, streamID string, start time.Time, maxBytes int64) (*DiskBuffer, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("catchup: mkdir %s: %w", dir, err)
	}
	b := &DiskBuffer{
		streamID:  streamID,
		maxBytes:  maxBytes,
		current:   segment{filePath: newFilePath(dir, streamID, start), startTime: start},
		writeCh:   make(chan []byte, writeChanCap),
		drainDone: make(chan struct{}),
	}
	go b.drainLoop()
	return b, nil
}

// drainLoop runs in its own goroutine and is the ONLY writer to the file.
func (b *DiskBuffer) drainLoop() {
	b.indexMu.RLock()
	path := b.current.filePath
	b.indexMu.RUnlock()

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		utils.ErrorLog("Catchup: failed to open buffer file %s: %v", path, err)
		for range b.writeCh {
		}
		return
	}

	var written int64
	var sinceLastSample int64

	for chunk := range b.writeCh {
		n, werr := f.Write(chunk)
		if werr != nil {
			utils.WarnLog("Catchup: write error for stream %s: %v (continuing)", b.streamID, werr)
			continue
		}
		written += int64(n)
		sinceLastSample += int64(n)

		if sinceLastSample >= indexSampleInterval {
			b.indexMu.Lock()
			b.current.index = append(b.current.index, indexEntry{wallTime: time.Now(), byteOffset: written})
			b.current.total = written
			b.indexMu.Unlock()
			sinceLastSample = 0

			if b.maxBytes > 0 && written >= b.maxBytes {
				newF := b.rotate(f)
				if newF == nil {
					for range b.writeCh {
					}
					return
				}
				f = newF
				written = 0
			}
		} else {
			b.indexMu.Lock()
			b.current.total = written
			b.indexMu.Unlock()
		}
	}

	_ = f.Close()
	b.indexMu.Lock()
	b.current.total = written
	b.indexMu.Unlock()
	close(b.drainDone)
}

// rotate closes oldFile, promotes current → prev, opens a new current file, and
// schedules deletion of the file evicted from prev. The caller must not hold indexMu.
func (b *DiskBuffer) rotate(oldFile *os.File) *os.File {
	_ = oldFile.Close()

	b.indexMu.Lock()
	evictPath := b.prev.filePath // evict the file that was already in prev
	b.prev = b.current           // promote current to prev
	newTime := time.Now()
	newPath := newFilePath(filepath.Dir(b.current.filePath), b.streamID, newTime)
	b.current = segment{filePath: newPath, startTime: newTime}
	b.indexMu.Unlock()

	if evictPath != "" {
		go func() {
			time.Sleep(rotationGracePeriod)
			if err := os.Remove(evictPath); err != nil && !os.IsNotExist(err) {
				utils.WarnLog("Catchup: failed to remove evicted buffer file %s: %v", evictPath, err)
			}
		}()
	}

	f, err := os.OpenFile(newPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		utils.ErrorLog("Catchup: failed to open rotated buffer file %s: %v", newPath, err)
		return nil
	}
	utils.DebugLog("Catchup: rotated buffer for stream %s → %s", b.streamID, newPath)
	return f
}

// Write enqueues a copy of p for async disk write. Never blocks; drops if channel full.
func (b *DiskBuffer) Write(p []byte) {
	if atomic.LoadInt32(&b.stopped) != 0 {
		return
	}
	chunk := make([]byte, len(p))
	copy(chunk, p)
	select {
	case b.writeCh <- chunk:
	default:
		utils.WarnLog("Catchup: write channel full for stream %s, dropping %d bytes", b.streamID, len(p))
	}
}

// Stop signals the drain goroutine to finish. Safe to call multiple times.
func (b *DiskBuffer) Stop() {
	if atomic.CompareAndSwapInt32(&b.stopped, 0, 1) {
		b.stoppedMu.Lock()
		b.stoppedAt = time.Now()
		b.stoppedMu.Unlock()
		close(b.writeCh)
	}
}

// Delete removes all buffer files from disk. Call only after Stop().
func (b *DiskBuffer) Delete() error {
	b.Stop()
	b.indexMu.RLock()
	cur, prev := b.current.filePath, b.prev.filePath
	b.indexMu.RUnlock()
	_ = os.Remove(prev)
	return os.Remove(cur)
}

func (b *DiskBuffer) IsStopped() bool            { return atomic.LoadInt32(&b.stopped) != 0 }
func (b *DiskBuffer) DrainDone() <-chan struct{} { return b.drainDone }
func (b *DiskBuffer) StreamID() string           { return b.streamID }

func (b *DiskBuffer) StoppedAt() time.Time {
	b.stoppedMu.Lock()
	defer b.stoppedMu.Unlock()
	return b.stoppedAt
}

func (b *DiskBuffer) FilePath() string {
	b.indexMu.RLock()
	defer b.indexMu.RUnlock()
	return b.current.filePath
}

func (b *DiskBuffer) StartTime() time.Time {
	b.indexMu.RLock()
	defer b.indexMu.RUnlock()
	return b.current.startTime
}

func (b *DiskBuffer) BytesBuffered() int64 {
	b.indexMu.RLock()
	defer b.indexMu.RUnlock()
	return b.current.total
}

// OffsetForTime returns the byte offset within the current file for t.
// Use FileForTime when a rewind may span a rotation boundary.
func (b *DiskBuffer) OffsetForTime(t time.Time) int64 {
	b.indexMu.RLock()
	defer b.indexMu.RUnlock()
	return searchIndex(b.current.index, t)
}

// FileForTime returns the file path and byte offset that best cover t.
// When t falls before the current file's start, the previous file is returned.
// The caller should continue into the current file from offset 0 after reaching EOF.
func (b *DiskBuffer) FileForTime(t time.Time) (filePath string, offset int64) {
	b.indexMu.RLock()
	defer b.indexMu.RUnlock()
	if b.prev.filePath != "" && t.Before(b.current.startTime) {
		return b.prev.filePath, searchIndex(b.prev.index, t)
	}
	return b.current.filePath, searchIndex(b.current.index, t)
}

// searchIndex binary-searches idx and returns the byte offset for t.
func searchIndex(idx []indexEntry, t time.Time) int64 {
	if len(idx) == 0 {
		return 0
	}
	pos := sort.Search(len(idx), func(i int) bool { return idx[i].wallTime.After(t) })
	pos-- // first entry AFTER t → pos-1 is what we want
	if pos < 0 {
		return 0
	}
	return idx[pos].byteOffset
}

func newFilePath(dir, streamID string, t time.Time) string {
	return filepath.Join(dir, fmt.Sprintf("%s_%d.ts", sanitizeID(streamID), t.Unix()))
}

func sanitizeID(id string) string {
	safe := make([]byte, 0, len(id))
	for i := 0; i < len(id); i++ {
		c := id[i]
		if c == '/' || c == '\\' || c == ':' || c == '*' || c == '?' || c == '"' || c == '<' || c == '>' || c == '|' || c == '.' {
			safe = append(safe, '_')
		} else {
			safe = append(safe, c)
		}
	}
	return string(safe)
}
