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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lucasduport/stream-share/pkg/utils"
)

const gracePeriod = 60 * time.Second

// Manager owns all active and grace-period DiskBuffers.
type Manager struct {
	enabled  bool
	dir      string
	duration int // hours — controls both advertised window and disk retention

	mu              sync.Mutex
	buffers         map[string]*DiskBuffer   // streamID → buffer (active or in grace period)
	graceCancels    map[string]chan struct{} // streamID → cancel chan for grace goroutine
	upstreamCatchup map[string]bool          // streamID → has native tv_archive=1
}

// New creates a Manager. If !enabled, all methods are no-ops.
func New(enabled bool, dir string, duration int) *Manager {
	if duration <= 0 {
		duration = 4
	}
	m := &Manager{
		enabled:         enabled,
		dir:             dir,
		duration:        duration,
		buffers:         make(map[string]*DiskBuffer),
		graceCancels:    make(map[string]chan struct{}),
		upstreamCatchup: make(map[string]bool),
	}
	return m
}

// IsEnabled returns whether the catchup feature is active.
func (m *Manager) IsEnabled() bool { return m.enabled }

// AdvertisedHours returns the number of hours to advertise in tv_archive_duration.
func (m *Manager) AdvertisedHours() int { return m.duration }

// maxBytes converts duration hours to a byte cap at an assumed 10 Mbps bitrate.
// Returns 0 (unlimited) when duration is 0.
func (m *Manager) maxBytes() int64 {
	if m.duration == 0 {
		return 0
	}
	// 10 Mbps = 10_000_000 bits/sec = 1_250_000 bytes/sec
	return int64(m.duration) * 3600 * 1_250_000
}

// StartBuffer creates a new DiskBuffer for streamID, replacing any existing one.
// Returns nil if not enabled or on error.
func (m *Manager) StartBuffer(streamID string) *DiskBuffer {
	if !m.enabled {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Cancel any pending grace-period deletion for this stream
	if cancel, ok := m.graceCancels[streamID]; ok {
		close(cancel)
		delete(m.graceCancels, streamID)
	}

	// Remove and clean up any existing buffer
	if existing, ok := m.buffers[streamID]; ok {
		existing.Stop()
		_ = existing.Delete()
		delete(m.buffers, streamID)
	}

	buf, err := NewDiskBuffer(m.dir, streamID, time.Now(), m.maxBytes())
	if err != nil {
		utils.ErrorLog("Catchup: failed to create buffer for stream %s: %v", streamID, err)
		return nil
	}
	m.buffers[streamID] = buf
	utils.DebugLog("Catchup: started buffer for stream %s at %s", streamID, buf.FilePath())
	return buf
}

// GetBuffer returns the buffer for streamID (active or in grace period), or nil if none.
func (m *Manager) GetBuffer(streamID string) *DiskBuffer {
	if !m.enabled {
		return nil
	}
	bare := strings.TrimSuffix(streamID, filepath.Ext(streamID))
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.buffers[bare]
}

// StopBuffer stops writing to the buffer but keeps the file alive for gracePeriod,
// allowing in-flight timeshift requests to finish reading.
func (m *Manager) StopBuffer(streamID string) {
	if !m.enabled {
		return
	}

	m.mu.Lock()
	buf, ok := m.buffers[streamID]
	if !ok {
		m.mu.Unlock()
		return
	}

	buf.Stop()

	cancel := make(chan struct{})
	m.graceCancels[streamID] = cancel
	m.mu.Unlock()

	go func() {
		select {
		case <-time.After(gracePeriod):
		case <-cancel:
			return
		}

		m.mu.Lock()
		defer m.mu.Unlock()

		// Only delete if this buffer is still the registered one (StartBuffer may have replaced it)
		if current, exists := m.buffers[streamID]; exists && current == buf {
			if err := buf.Delete(); err != nil && !os.IsNotExist(err) {
				utils.WarnLog("Catchup: failed to delete buffer file %s: %v", buf.FilePath(), err)
			}
			delete(m.buffers, streamID)
			delete(m.graceCancels, streamID)
			utils.DebugLog("Catchup: grace period expired, deleted buffer for stream %s", streamID)
		}
	}()
}

// DeleteBuffer immediately stops and deletes the buffer (used on shutdown).
func (m *Manager) DeleteBuffer(streamID string) {
	if !m.enabled {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if cancel, ok := m.graceCancels[streamID]; ok {
		close(cancel)
		delete(m.graceCancels, streamID)
	}

	if buf, ok := m.buffers[streamID]; ok {
		buf.Stop()
		if err := buf.Delete(); err != nil && !os.IsNotExist(err) {
			utils.WarnLog("Catchup: failed to delete buffer file %s: %v", buf.FilePath(), err)
		}
		delete(m.buffers, streamID)
	}
}

// Cleanup stops and deletes all buffers. Call on server shutdown.
func (m *Manager) Cleanup() {
	if !m.enabled {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, cancel := range m.graceCancels {
		close(cancel)
	}
	m.graceCancels = make(map[string]chan struct{})

	for streamID, buf := range m.buffers {
		buf.Stop()
		if err := buf.Delete(); err != nil && !os.IsNotExist(err) {
			utils.WarnLog("Catchup: cleanup failed to delete %s: %v", buf.FilePath(), err)
		}
		delete(m.buffers, streamID)
	}
	utils.InfoLog("Catchup: all buffers cleaned up")
}

// CleanupOldFiles removes stale *.ts buffer files left from a previous run.
func (m *Manager) CleanupOldFiles() {
	if !m.enabled {
		return
	}

	entries, err := os.ReadDir(m.dir)
	if err != nil {
		if !os.IsNotExist(err) {
			utils.WarnLog("Catchup: failed to read buffer dir %s: %v", m.dir, err)
		}
		return
	}

	count := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".ts") {
			continue
		}
		p := filepath.Join(m.dir, e.Name())
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			utils.WarnLog("Catchup: failed to remove stale file %s: %v", p, err)
		} else {
			count++
		}
	}
	if count > 0 {
		utils.InfoLog("Catchup: removed %d stale buffer file(s) from %s", count, m.dir)
	}
}

// SetUpstreamCatchup stores which stream IDs have native tv_archive support.
// Keys should be bare stream IDs (no extension).
func (m *Manager) SetUpstreamCatchup(supported map[string]bool) {
	if !m.enabled {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.upstreamCatchup = supported
}

// HasUpstreamCatchup returns true if the stream has native upstream catchup support.
// id may include an extension (e.g. "12345.ts"); the extension is stripped before lookup.
func (m *Manager) HasUpstreamCatchup(id string) bool {
	if !m.enabled {
		return true // safe default: always proxy upstream when catchup is disabled
	}
	bare := strings.TrimSuffix(id, filepath.Ext(id))
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.upstreamCatchup[bare]
}
