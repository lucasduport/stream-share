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

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lucasduport/stream-share/pkg/utils"
)

// Technical stream info never costs an extra connection to the provider:
//   - Live streams are probed from a small sample of bytes already flowing
//     through the channel's existing shared upstream connection (see
//     SessionManager.CaptureStreamSample).
//   - Cached VOD/series (once fully downloaded) are probed straight off the
//     local file — no network involved at all, and since the whole file is
//     available, every audio track can be listed (not just whichever one a
//     live sample happened to catch).
//
// Either way the sample/file is analyzed locally with ffprobe.
const (
	techProbeSampleBytes    = 2 * 1024 * 1024 // ~2MB, enough for ffprobe to see a keyframe + audio frames on typical bitrates
	techProbeCaptureTimeout = 6 * time.Second
	techProbeFFprobeTimeout = 6 * time.Second
	techProbeCacheTTL       = 5 * time.Minute
	techProbeFailureTTL     = time.Minute // retry failed probes sooner than successful ones
)

// AudioTrackInfo describes one audio stream found by ffprobe. A live sample
// only ever sees whichever track(s) happen to be multiplexed into the sample;
// a fully-downloaded VOD/series file reports every track it contains.
type AudioTrackInfo struct {
	Index        int    `json:"index"`
	Codec        string `json:"codec,omitempty"`
	Channels     int    `json:"channels,omitempty"`
	SampleRateHz int    `json:"sample_rate_hz,omitempty"`
	Language     string `json:"language,omitempty"`
	BitrateKbps  int    `json:"bitrate_kbps,omitempty"`
}

// SubtitleTrackInfo describes one embedded subtitle stream found by ffprobe
// (e.g. SRT/ASS/PGS muxed into the container, or DVB subtitles in a TS). Like
// audio tracks, a live sample only sees whichever ones happen to be
// multiplexed into the sample; a fully-downloaded VOD/series file reports all
// of them.
type SubtitleTrackInfo struct {
	Index    int    `json:"index"`
	Codec    string `json:"codec,omitempty"`
	Language string `json:"language,omitempty"`
}

// StreamTechInfo describes the audio/video/subtitle technical characteristics
// of an active stream, as last observed by ffprobe.
type StreamTechInfo struct {
	ContainerFormat  string              `json:"container_format,omitempty"`
	VideoCodec       string              `json:"video_codec,omitempty"`
	Width            int                 `json:"width,omitempty"`
	Height           int                 `json:"height,omitempty"`
	FrameRate        float64             `json:"frame_rate,omitempty"`
	VideoBitrateKbps int                 `json:"video_bitrate_kbps,omitempty"`
	AudioTracks      []AudioTrackInfo    `json:"audio_tracks,omitempty"`
	SubtitleTracks   []SubtitleTrackInfo `json:"subtitle_tracks,omitempty"`
	DurationSec      float64             `json:"duration_sec,omitempty"` // VOD/series only; live streams have no fixed duration
	ProbedAt         time.Time           `json:"probed_at,omitempty"`
	Error            string              `json:"error,omitempty"`
}

// formatTechSummary renders tech as a compact one-line string for human-
// readable output (e.g. the Discord /status text), or "" if there's nothing
// usable to show (no probe yet, or the last probe failed).
func formatTechSummary(t *StreamTechInfo) string {
	if t == nil || t.Error != "" {
		return ""
	}

	var parts []string
	if t.VideoCodec != "" {
		video := t.VideoCodec
		if t.Width > 0 && t.Height > 0 {
			video += fmt.Sprintf(" %dx%d", t.Width, t.Height)
		}
		if t.FrameRate > 0 {
			video += fmt.Sprintf("@%gfps", t.FrameRate)
		}
		if t.VideoBitrateKbps > 0 {
			video += fmt.Sprintf(" %dkbps", t.VideoBitrateKbps)
		}
		parts = append(parts, video)
	}
	if len(t.AudioTracks) > 0 {
		audioParts := make([]string, 0, len(t.AudioTracks))
		for _, a := range t.AudioTracks {
			seg := a.Codec
			if a.Channels > 0 {
				seg += fmt.Sprintf(" %dch", a.Channels)
			}
			if a.Language != "" {
				seg += " " + a.Language
			}
			audioParts = append(audioParts, strings.TrimSpace(seg))
		}
		parts = append(parts, "audio: "+strings.Join(audioParts, ", "))
	}
	if len(t.SubtitleTracks) > 0 {
		langs := make([]string, 0, len(t.SubtitleTracks))
		for _, sub := range t.SubtitleTracks {
			if sub.Language != "" {
				langs = append(langs, sub.Language)
			}
		}
		if len(langs) > 0 {
			parts = append(parts, "subs: "+strings.Join(langs, ", "))
		} else {
			parts = append(parts, fmt.Sprintf("subs: %d", len(t.SubtitleTracks)))
		}
	}

	return strings.Join(parts, " · ")
}

type techCacheEntry struct {
	info      *StreamTechInfo
	expiresAt time.Time
}

var (
	techCacheMu sync.Mutex
	techCache   = map[string]*techCacheEntry{} // streamID -> cached result
	techProbing = map[string]bool{}            // streamID -> probe in flight

	ffprobeCheckOnce sync.Once
	ffprobeOK        bool
)

// ffprobeAvailable reports whether the ffprobe binary is on PATH, checked once.
func ffprobeAvailable() bool {
	ffprobeCheckOnce.Do(func() {
		path, err := exec.LookPath("ffprobe")
		ffprobeOK = err == nil
		if ffprobeOK {
			utils.InfoLog("Stream tech probe: ffprobe found at %s", path)
		} else {
			utils.WarnLog("Stream tech probe: ffprobe not found on PATH; technical stream info will be unavailable")
		}
	})
	return ffprobeOK
}

// getCachedTechInfo returns a previously probed result without triggering any
// work. Staleness is not considered here — callers decide whether to also warm.
func getCachedTechInfo(streamID string) (*StreamTechInfo, bool) {
	techCacheMu.Lock()
	defer techCacheMu.Unlock()
	entry, ok := techCache[streamID]
	if !ok {
		return nil, false
	}
	return entry.info, true
}

func cacheTechInfo(streamID string, info *StreamTechInfo, ttl time.Duration) {
	techCacheMu.Lock()
	techCache[streamID] = &techCacheEntry{info: info, expiresAt: time.Now().Add(ttl)}
	techCacheMu.Unlock()
}

// warmTechInfo kicks off a background probe for streamID via prober if there
// isn't already a fresh cache entry or an in-flight probe for it. Never
// blocks the caller. Assumes the feature is enabled and ffprobe is available
// (callers check that first, since the reason differs by source).
func warmTechInfo(streamID string, prober func() (*StreamTechInfo, error)) {
	techCacheMu.Lock()
	if techProbing[streamID] {
		techCacheMu.Unlock()
		return
	}
	if entry, ok := techCache[streamID]; ok && time.Now().Before(entry.expiresAt) {
		techCacheMu.Unlock()
		return
	}
	techProbing[streamID] = true
	techCacheMu.Unlock()

	go func() {
		defer func() {
			techCacheMu.Lock()
			delete(techProbing, streamID)
			techCacheMu.Unlock()
		}()
		runProbe(streamID, prober)
	}()
}

// forceProbeTechInfo runs a probe synchronously via prober (used for single-
// stream lookups, where a caller can afford to wait a couple of seconds for a
// fresh result) and returns whatever ends up cached, including the last-good
// result if this attempt fails.
func forceProbeTechInfo(streamID string, prober func() (*StreamTechInfo, error)) *StreamTechInfo {
	techCacheMu.Lock()
	alreadyProbing := techProbing[streamID]
	if !alreadyProbing {
		techProbing[streamID] = true
	}
	techCacheMu.Unlock()

	if !alreadyProbing {
		defer func() {
			techCacheMu.Lock()
			delete(techProbing, streamID)
			techCacheMu.Unlock()
		}()
		runProbe(streamID, prober)
	}

	info, _ := getCachedTechInfo(streamID)
	return info
}

// runProbe executes prober and stores the outcome (success or failure) in the
// cache. Callers are responsible for the techProbing in-flight guard.
func runProbe(streamID string, prober func() (*StreamTechInfo, error)) {
	info, err := prober()
	if err != nil {
		utils.DebugLog("Stream tech probe: failed for %s: %v", streamID, err)
		cacheTechInfo(streamID, &StreamTechInfo{Error: err.Error()}, techProbeFailureTTL)
		return
	}
	info.ProbedAt = time.Now()
	cacheTechInfo(streamID, info, techProbeCacheTTL)
}

// warmLiveTechInfo probes an active live stream by sampling bytes already
// flowing through its shared upstream connection.
func (c *Config) warmLiveTechInfo(streamID string) {
	if !c.StreamTechProbeEnabled || !ffprobeAvailable() {
		return
	}
	warmTechInfo(streamID, func() (*StreamTechInfo, error) { return c.probeLiveSample(streamID) })
}

// forceProbeLiveTechInfo is the blocking counterpart of warmLiveTechInfo.
func (c *Config) forceProbeLiveTechInfo(streamID string) *StreamTechInfo {
	if !c.StreamTechProbeEnabled || !ffprobeAvailable() {
		return nil
	}
	return forceProbeTechInfo(streamID, func() (*StreamTechInfo, error) { return c.probeLiveSample(streamID) })
}

func (c *Config) probeLiveSample(streamID string) (*StreamTechInfo, error) {
	sample, err := c.sessionManager.CaptureStreamSample(streamID, techProbeSampleBytes, techProbeCaptureTimeout)
	if err != nil {
		return nil, err
	}
	return runFFprobe(techProbeFFprobeTimeout, func(ctx context.Context) *exec.Cmd {
		cmd := exec.CommandContext(ctx, "ffprobe",
			"-v", "error",
			"-print_format", "json",
			"-show_streams",
			"-show_format",
			"-analyzeduration", "3000000",
			"-probesize", strconv.Itoa(len(sample)),
			"-i", "pipe:0",
		)
		cmd.Stdin = bytes.NewReader(sample)
		return cmd
	})
}

// warmVODTechInfo probes a fully-downloaded cached VOD/series file directly —
// no sampling needed and no network involved at all.
func (c *Config) warmVODTechInfo(streamID, filePath string) {
	if !c.StreamTechProbeEnabled || !ffprobeAvailable() {
		return
	}
	warmTechInfo(streamID, func() (*StreamTechInfo, error) { return probeLocalFile(filePath) })
}

// forceProbeVODTechInfo is the blocking counterpart of warmVODTechInfo.
func (c *Config) forceProbeVODTechInfo(streamID, filePath string) *StreamTechInfo {
	if !c.StreamTechProbeEnabled || !ffprobeAvailable() {
		return nil
	}
	return forceProbeTechInfo(streamID, func() (*StreamTechInfo, error) { return probeLocalFile(filePath) })
}

func probeLocalFile(filePath string) (*StreamTechInfo, error) {
	return runFFprobe(techProbeFFprobeTimeout, func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, "ffprobe",
			"-v", "error",
			"-print_format", "json",
			"-show_streams",
			"-show_format",
			"-i", filePath,
		)
	})
}

// ffprobeStream is the subset of ffprobe's per-stream JSON output we care about.
type ffprobeStream struct {
	Index        int    `json:"index"`
	CodecType    string `json:"codec_type"`
	CodecName    string `json:"codec_name"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	AvgFrameRate string `json:"avg_frame_rate"`
	RFrameRate   string `json:"r_frame_rate"`
	BitRate      string `json:"bit_rate"`
	Channels     int    `json:"channels"`
	SampleRate   string `json:"sample_rate"`
	Tags         struct {
		Language string `json:"language"`
	} `json:"tags"`
}

type ffprobeFormat struct {
	FormatName string `json:"format_name"`
	BitRate    string `json:"bit_rate"`
	Duration   string `json:"duration"`
}

type ffprobeOutput struct {
	Streams []ffprobeStream `json:"streams"`
	Format  ffprobeFormat   `json:"format"`
}

// runFFprobe runs the *exec.Cmd built by build (stdout/stderr wired up by this
// function) under a bounded timeout and parses its JSON output.
func runFFprobe(timeout time.Duration, build func(ctx context.Context) *exec.Cmd) (*StreamTechInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := build(ctx)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("ffprobe: %s", msg)
	}

	return parseFFprobeJSON(stdout.Bytes())
}

// parseFFprobeJSON extracts the first video stream and every audio stream's
// technical characteristics from ffprobe's JSON output.
func parseFFprobeJSON(data []byte) (*StreamTechInfo, error) {
	var out ffprobeOutput
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("ffprobe: failed to parse output: %w", err)
	}

	info := &StreamTechInfo{ContainerFormat: out.Format.FormatName}
	if d, err := strconv.ParseFloat(out.Format.Duration, 64); err == nil && d > 0 {
		info.DurationSec = d
	}

	for _, s := range out.Streams {
		switch s.CodecType {
		case "video":
			if info.VideoCodec != "" {
				continue // keep the first video stream only
			}
			info.VideoCodec = s.CodecName
			info.Width = s.Width
			info.Height = s.Height
			info.FrameRate = parseFrameRate(s.AvgFrameRate)
			if info.FrameRate == 0 {
				info.FrameRate = parseFrameRate(s.RFrameRate)
			}
			info.VideoBitrateKbps = kbps(s.BitRate)
		case "audio":
			track := AudioTrackInfo{
				Index:       s.Index,
				Codec:       s.CodecName,
				Channels:    s.Channels,
				Language:    s.Tags.Language,
				BitrateKbps: kbps(s.BitRate),
			}
			if sr, err := strconv.Atoi(s.SampleRate); err == nil {
				track.SampleRateHz = sr
			}
			info.AudioTracks = append(info.AudioTracks, track)
		case "subtitle":
			info.SubtitleTracks = append(info.SubtitleTracks, SubtitleTrackInfo{
				Index:    s.Index,
				Codec:    s.CodecName,
				Language: s.Tags.Language,
			})
		}
	}

	if info.VideoCodec == "" && len(info.AudioTracks) == 0 {
		return nil, fmt.Errorf("ffprobe: no audio or video stream detected")
	}
	return info, nil
}

// parseFrameRate converts an ffprobe "num/den" rational frame rate into a float.
func parseFrameRate(rate string) float64 {
	parts := strings.SplitN(rate, "/", 2)
	if len(parts) != 2 {
		return 0
	}
	num, errN := strconv.ParseFloat(parts[0], 64)
	den, errD := strconv.ParseFloat(parts[1], 64)
	if errN != nil || errD != nil || den == 0 {
		return 0
	}
	fps := num / den
	// Round to 2 decimals for a clean display value.
	return float64(int(fps*100+0.5)) / 100
}

// kbps converts an ffprobe bits-per-second string into whole kbps.
func kbps(bitRate string) int {
	bps, err := strconv.ParseInt(bitRate, 10, 64)
	if err != nil || bps <= 0 {
		return 0
	}
	return int(bps / 1000)
}
