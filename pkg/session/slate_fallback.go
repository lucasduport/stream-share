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

package session

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/lucasduport/stream-share/pkg/slate"
	"github.com/lucasduport/stream-share/pkg/utils"
)

// SlateProvider renders the error clip shown to viewers when the upstream fails.
// It is an interface so tests can supply a fixture without needing ffmpeg.
type SlateProvider interface {
	Available() bool
	Clip(v slate.View) (string, error)
}

const (
	slateRetryInitialBackoff = 2 * time.Second
	slateRetryMaxBackoff     = 30 * time.Second
	// slateBytesPerSec paces delivery so the clip plays in real time instead of
	// being dumped into player buffers all at once.
	slateBytesPerSec = slate.MuxRateBitsPerSec / 8
)

// SetSlateProvider attaches the renderer used for upstream-failure slates.
func (sm *SessionManager) SetSlateProvider(p SlateProvider) { sm.slateProvider = p }

// SetErrorCatalog attaches the code -> message catalog used to build slates.
func (sm *SessionManager) SetErrorCatalog(c *Catalog) { sm.errorCatalog = c }

// SetSlateRetryMax bounds how long a failed stream keeps showing a slate and
// retrying upstream before giving up. 0 disables the slate fallback.
func (sm *SessionManager) SetSlateRetryMax(d time.Duration) { sm.slateRetryMax = d }

// slateEligible reports whether a stream type may be replaced by an error slate.
// Only the continuous MPEG-TS paths qualify: VOD is served over byte ranges of an
// mp4/mkv, where splicing in a TS clip would corrupt the response.
func slateEligible(streamType string) bool {
	return streamType == "live" || streamType == "timeshift"
}

// slateEnabled reports whether a slate can actually be served for this buffer.
func (sm *SessionManager) slateEnabled(buffer *StreamBuffer) bool {
	return buffer != nil && buffer.slateOK &&
		sm.slateRetryMax > 0 &&
		sm.slateProvider != nil && sm.slateProvider.Available()
}

// ServesSlateFor reports whether a failed stream will be shown an error slate
// rather than dropped, so the HTTP handler knows whether to keep the response
// open and stream the slate bytes or to return an error status.
func (sm *SessionManager) ServesSlateFor(streamID string) bool {
	sm.streamLock.RLock()
	buffer, ok := sm.streamBuffers[streamID]
	sm.streamLock.RUnlock()
	if !ok {
		return false
	}
	return sm.slateEnabled(buffer)
}

// dialUpstream performs one upstream attempt, returning either a live response
// or a classified failure. The response body is the caller's to close.
func (sm *SessionManager) dialUpstream(ctx context.Context, upstreamURL *url.URL) (*http.Response, *UpstreamError) {
	req, err := http.NewRequestWithContext(ctx, "GET", upstreamURL.String(), nil)
	if err != nil {
		return nil, classifyUpstream(err, 0)
	}

	// Same headers as the main pump: never inject Range, so the upstream returns
	// a natural 200 with Content-Length.
	req.Header.Set("User-Agent", utils.GetIPTVUserAgent())
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", utils.GetLanguageHeader())
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Connection", "keep-alive")

	resp, err := sm.httpClient.Do(req)
	if err != nil {
		return nil, classifyUpstream(err, 0)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		status := resp.StatusCode
		_ = resp.Body.Close()
		return nil, classifyUpstream(nil, status)
	}
	return resp, nil
}

// ProbeUpstream performs a single upstream attempt against target and reports
// the outcome for health checks: nil when the provider served a live response
// (200/206), or a classified UpstreamError otherwise — notably StatusCode 456
// when the provider is blocking our egress IP. The response body is closed here
// since the probe only cares about reachability, not the stream itself.
func (sm *SessionManager) ProbeUpstream(ctx context.Context, target *url.URL) *UpstreamError {
	resp, uerr := sm.dialUpstream(ctx, target)
	if uerr != nil {
		return uerr
	}
	_ = resp.Body.Close()
	return nil
}

// slateViewFor resolves what the slate should say for a failure.
func (sm *SessionManager) slateViewFor(buffer *StreamBuffer, uerr *UpstreamError) slate.View {
	code, meaning, message := sm.errorCatalog.Lookup(uerr)

	channel := ""
	if sm.nameResolver != nil {
		if name, ok := sm.nameResolver(buffer.streamID); ok {
			channel = name
		}
	}

	return slate.View{Code: code, Meaning: meaning, Message: message, Channel: channel}
}

// serveSlateAndRetry shows the error slate to every viewer of a failed stream
// while retrying the provider in the background.
//
// It returns a live upstream response when the provider recovers — the caller
// resumes normal pumping with it — or nil when the retry budget is exhausted,
// every viewer left, or the stream was stopped.
func (sm *SessionManager) serveSlateAndRetry(
	ctx context.Context,
	buffer *StreamBuffer,
	upstreamURL *url.URL,
	uerr *UpstreamError,
) *http.Response {

	clipPath, err := sm.slateProvider.Clip(sm.slateViewFor(buffer, uerr))
	if err != nil {
		utils.WarnLog("Error slate: cannot render for %s: %v; dropping stream as before",
			sm.streamLabel(buffer.streamID), err)
		return nil
	}

	utils.InfoLog("Error slate: %s failed (%s); showing slate and retrying for up to %s",
		sm.streamLabel(buffer.streamID), uerr.Error(), utils.HumanDuration(sm.slateRetryMax))

	deadline := time.Now().Add(sm.slateRetryMax)
	backoff := slateRetryInitialBackoff
	nextRetry := time.Now().Add(backoff)

	for {
		if sm.stopRequested(ctx, buffer) {
			return nil
		}
		if sm.viewerCount(buffer) == 0 {
			utils.DebugLog("Error slate: no viewers left on %s; stopping", sm.streamLabel(buffer.streamID))
			return nil
		}

		// Play one pass of the clip. Cutting over to real video only ever happens
		// at this boundary, where the next clip loop would have restarted with a
		// fresh PAT/PMT anyway, which is the least disruptive splice point.
		if !sm.playSlateOnce(ctx, buffer, clipPath) {
			return nil
		}

		if time.Now().After(deadline) {
			utils.InfoLog("Error slate: retry budget exhausted for %s; stopping",
				sm.streamLabel(buffer.streamID))
			return nil
		}
		if time.Now().Before(nextRetry) {
			continue
		}

		resp, retryErr := sm.dialUpstream(ctx, upstreamURL)
		if retryErr == nil {
			utils.InfoLog("Error slate: %s recovered; resuming live video", sm.streamLabel(buffer.streamID))
			return resp
		}

		utils.DebugLog("Error slate: %s still failing (%s); next retry in %s",
			sm.streamLabel(buffer.streamID), retryErr.Error(), utils.HumanDuration(backoff))

		backoff *= 2
		if backoff > slateRetryMaxBackoff {
			backoff = slateRetryMaxBackoff
		}
		nextRetry = time.Now().Add(backoff)
	}
}

// playSlateOnce fans one pass of the clip out to all viewers, paced in real
// time. It reports false when the stream was stopped mid-pass.
func (sm *SessionManager) playSlateOnce(ctx context.Context, buffer *StreamBuffer, clipPath string) bool {
	f, err := os.Open(clipPath)
	if err != nil {
		utils.WarnLog("Error slate: cannot open %s: %v", clipPath, err)
		return false
	}
	defer func() { _ = f.Close() }()

	buf := make([]byte, streamChunkSize)
	for {
		if sm.stopRequested(ctx, buffer) {
			return false
		}

		n, rerr := f.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			sm.fanOut(buffer, chunk)

			// Pace against the clip's mux rate so the player receives it at
			// playback speed rather than as fast as the disk can serve it.
			delay := time.Duration(float64(n) / float64(slateBytesPerSec) * float64(time.Second))
			select {
			case <-time.After(delay):
			case <-buffer.stopChan:
				return false
			case <-ctx.Done():
				return false
			}
		}
		if rerr == io.EOF {
			return true
		}
		if rerr != nil {
			utils.WarnLog("Error slate: read error on %s: %v", clipPath, rerr)
			return false
		}
	}
}

// stopRequested reports whether this stream should stop pumping.
func (sm *SessionManager) stopRequested(ctx context.Context, buffer *StreamBuffer) bool {
	select {
	case <-buffer.stopChan:
		return true
	case <-ctx.Done():
		return true
	case <-sm.stopChan:
		return true
	default:
		return false
	}
}

// viewerCount returns how many clients are still attached to the buffer.
func (sm *SessionManager) viewerCount(buffer *StreamBuffer) int {
	buffer.clientsLock.RLock()
	defer buffer.clientsLock.RUnlock()
	return len(buffer.clients)
}
