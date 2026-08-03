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

// Package slate renders short MPEG-TS "error slate" clips with ffmpeg, so a
// viewer whose provider failed sees the reason on screen instead of a frozen
// picture. Clips are cached on disk keyed by their rendered text, because the
// same handful of errors recur constantly.
package slate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/lucasduport/stream-share/pkg/utils"
)

const (
	// ClipSeconds is the length of a generated clip. Longer clips mean the loop
	// boundary (where TS continuity counters restart) is hit less often.
	ClipSeconds = 15
	// MuxRateBitsPerSec must match the -muxrate below; callers pace playback
	// against it so the clip is delivered in real time rather than all at once.
	MuxRateBitsPerSec = 2_000_000

	genTimeout    = 60 * time.Second
	messageWidth  = 44 // characters per wrapped message line
	maxMsgLines   = 2
	maxFieldChars = 96
)

// fontCandidates are the usual locations of DejaVu Sans. The Alpine path comes
// first because that is what the shipped image installs (ttf-dejavu), but the
// binary is also run directly during development, where the distro layout
// differs — hardcoding one path would silently disable slates there.
var fontCandidates = []string{
	"/usr/share/fonts/ttf-dejavu/DejaVuSans.ttf",      // Alpine: ttf-dejavu
	"/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf", // Debian/Ubuntu: fonts-dejavu-core
	"/usr/share/fonts/dejavu/DejaVuSans.ttf",          // Fedora/RHEL
	"/usr/share/fonts/TTF/DejaVuSans.ttf",             // Arch
}

var (
	fontOnce     sync.Once
	resolvedFont string
)

// fontFile returns the first DejaVu Sans found on this system, or "" if none.
func fontFile() string {
	fontOnce.Do(func() {
		for _, p := range fontCandidates {
			if _, err := os.Stat(p); err == nil {
				resolvedFont = p
				return
			}
		}
	})
	return resolvedFont
}

// View is the resolved, ready-to-render content of a slate.
type View struct {
	Code    string // "403"; empty for transport failures
	Meaning string // "Forbidden"; may be empty
	Message string // custom operator-authored message
	Channel string // channel name, for context
}

// Generator renders and caches slate clips.
type Generator struct {
	dir       string
	available bool
	// inFlight serializes renders per cache key so duplicate requests spawn one
	// ffmpeg, mirroring the inProgressDownloads guard in pkg/server. Entries are
	// never evicted, which is fine: the key space is the set of distinct error
	// messages, not something user input can grow without bound.
	inFlight sync.Map // cache key -> *sync.Mutex
}

// lockKey serializes work on a cache key and returns the unlock func.
func (g *Generator) lockKey(key string) func() {
	actual, _ := g.inFlight.LoadOrStore(key, &sync.Mutex{})
	mu := actual.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// New probes for a usable ffmpeg and returns a Generator. When ffmpeg is missing
// or cannot render, Available() reports false and callers fall back to their
// previous behavior.
func New(dir string) *Generator {
	g := &Generator{dir: dir}

	if _, err := exec.LookPath("ffmpeg"); err != nil {
		utils.WarnLog("Error slate: ffmpeg not found on PATH; error slates are disabled")
		return g
	}
	if fontFile() == "" {
		utils.WarnLog("Error slate: no DejaVu Sans font found in %v; error slates are disabled. Install the ttf-dejavu package.", fontCandidates)
		return g
	}

	g.available = true
	return g
}

// Available reports whether slates can actually be rendered.
func (g *Generator) Available() bool { return g != nil && g.available }

// Clip returns the path to a cached clip for v, rendering it if needed.
// Concurrent callers for the same content wait for a single render rather than
// each spawning ffmpeg.
func (g *Generator) Clip(v View) (string, error) {
	if !g.Available() {
		return "", fmt.Errorf("slate: ffmpeg unavailable")
	}

	code := sanitize(v.Code)
	meaning := sanitize(v.Meaning)
	message := sanitize(v.Message)
	channel := sanitize(v.Channel)

	key := hashView(code, meaning, message, channel)
	out := filepath.Join(g.dir, key+".ts")

	// Serialize per-key so duplicate requests render once.
	defer g.lockKey(key)()

	if fi, err := os.Stat(out); err == nil && fi.Size() > 0 {
		return out, nil
	}

	if err := os.MkdirAll(g.dir, 0o755); err != nil {
		return "", fmt.Errorf("slate: mkdir %s: %w", g.dir, err)
	}

	if err := g.render(out, code, meaning, message, channel); err != nil {
		// Never leave a partial file behind — it would be served as a valid clip.
		_ = os.Remove(out)
		return "", err
	}
	utils.DebugLog("Error slate: rendered %s", out)
	return out, nil
}

// PruneCache removes cached slate clips whose modification time is older than
// maxAge. Slates are keyed by error and re-rendered on demand, so pruning is
// always safe — a still-relevant clip is simply regenerated the next time that
// error occurs. A missing directory is not an error.
func PruneCache(dir string, maxAge time.Duration) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	cutoff := time.Now().Add(-maxAge)
	removed := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".ts") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			if err := os.Remove(filepath.Join(dir, e.Name())); err == nil {
				removed++
			}
		}
	}
	return removed, nil
}

// render shells out to ffmpeg. Arguments are passed as a slice (never through a
// shell) and every interpolated string has been sanitized by the caller.
func (g *Generator) render(out, code, meaning, message, channel string) error {
	ctx, cancel := context.WithTimeout(context.Background(), genTimeout)
	defer cancel()

	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", fmt.Sprintf("color=c=0x101010:s=1280x720:r=25:d=%d", ClipSeconds),
		"-f", "lavfi", "-i", "anullsrc=channel_layout=stereo:sample_rate=48000",
		"-vf", buildFilter(code, meaning, message, channel),
		"-c:v", "libx264", "-preset", "ultrafast", "-tune", "stillimage",
		"-pix_fmt", "yuv420p", "-g", "25", "-b:v", "800k",
		"-c:a", "aac", "-b:a", "64k", "-ar", "48000", "-ac", "2",
		"-shortest", "-t", fmt.Sprintf("%d", ClipSeconds),
		"-f", "mpegts", "-muxrate", fmt.Sprintf("%d", MuxRateBitsPerSec),
		out,
	}

	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("slate: ffmpeg failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// buildFilter assembles the drawtext chain. The layout is computed from the
// lines that actually have content, so a missing code or meaning does not leave
// a gap and the block stays vertically centred.
//
// Every field is sanitized here rather than trusting the caller: this is the
// only place text becomes filter syntax, so making it safe by construction means
// no future caller can introduce an injection. sanitize is idempotent, so the
// caller sanitizing first (for the cache key) costs nothing.
func buildFilter(code, meaning, message, channel string) string {
	code, meaning = sanitize(code), sanitize(meaning)
	message, channel = sanitize(message), sanitize(channel)

	type line struct {
		text  string
		size  int
		color string
	}

	var lines []line

	if headline := joinHeadline(code, meaning); headline != "" {
		lines = append(lines, line{text: headline, size: 48, color: "white"})
	}
	for _, msgLine := range wrap(message, messageWidth, maxMsgLines) {
		lines = append(lines, line{text: msgLine, size: 32, color: "0xBBBBBB"})
	}
	if channel != "" {
		lines = append(lines, line{text: channel, size: 26, color: "0x777777"})
	}
	if len(lines) == 0 {
		lines = append(lines, line{text: "Stream unavailable", size: 48, color: "white"})
	}

	// Vertical layout: stack the lines around the centre with a little extra
	// padding after the headline and before the channel.
	const lineGap = 58
	totalHeight := (len(lines) - 1) * lineGap
	y := -totalHeight / 2

	font := fontFile()
	filters := make([]string, 0, len(lines))
	for _, l := range lines {
		filters = append(filters, fmt.Sprintf(
			"drawtext=fontfile=%s:text='%s':fontcolor=%s:fontsize=%d:x=(w-text_w)/2:y=(h-text_h)/2%+d",
			font, l.text, l.color, l.size, y,
		))
		y += lineGap
	}
	return strings.Join(filters, ",")
}

// joinHeadline renders "Error 403 - Forbidden", dropping either half when empty.
func joinHeadline(code, meaning string) string {
	switch {
	case code != "" && meaning != "":
		return "Error " + code + " - " + meaning
	case code != "":
		return "Error " + code
	default:
		return meaning
	}
}

// safePunct are ASCII punctuation characters that carry no meaning to ffmpeg's
// filter parser. Deliberately absent: ':' (option separator), '\” (string
// delimiter), '\\' (escape), '%' (expansion), and '[' / ']' (filter pad labels).
const safePunct = " .,()-_/?!&+#@"

// safeSymbols are decorative characters common in IPTV channel names that DejaVu
// Sans is known to contain. Symbols outside this set are dropped rather than
// passed through, because a glyph the font lacks renders as a placeholder box
// (most emoji, e.g. the football and clapperboard, do exactly that) which looks
// broken on screen.
const safeSymbols = "★☆●○◆◇■□▲▼▶◀•·|"

// sanitize reduces text to characters that are safe inside an ffmpeg filter
// string and that the font can actually draw. Unsafe characters are dropped
// rather than escaped, since escaping rules vary between ffmpeg versions. This
// applies to operator-authored catalog text as much as to provider strings.
//
// Letters, digits and combining marks from any script are kept: channel names
// are routinely non-ASCII ("Télé Monte Carlo", "Спорт 1"), and DejaVu Sans
// covers Latin, Cyrillic and Greek, so stripping them would mangle real names.
func sanitize(s string) string {
	var b strings.Builder
	lastSpace := false
	for _, r := range s {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), unicode.IsMark(r),
			strings.ContainsRune(safeSymbols, r):
			b.WriteRune(r)
			lastSpace = false
		case strings.ContainsRune(safePunct, r):
			if r == ' ' {
				if lastSpace || b.Len() == 0 {
					continue
				}
				lastSpace = true
			} else {
				lastSpace = false
			}
			b.WriteRune(r)
		default:
			// Filter syntax, control characters, and glyphs the font lacks all
			// collapse to a space so words do not run together.
			if !lastSpace && b.Len() > 0 {
				b.WriteRune(' ')
				lastSpace = true
			}
		}
	}

	// Truncate by runes, not bytes: slicing a multi-byte character in half would
	// emit invalid UTF-8 into the filter string.
	return truncateRunes(strings.TrimSpace(b.String()), maxFieldChars)
}

// truncateRunes limits s to n characters, counting runes rather than bytes.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n]))
}

// wrap splits text into at most maxLines lines of at most width characters,
// breaking on spaces. The final line is ellipsised if text remains. Widths are
// measured in runes so accented and Cyrillic names wrap at the right place
// instead of far too early.
func wrap(text string, width, maxLines int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	words := strings.Fields(text)
	lines := make([]string, 0, maxLines)
	current := ""

	for _, w := range words {
		candidate := w
		if current != "" {
			candidate = current + " " + w
		}
		if utf8.RuneCountInString(candidate) <= width {
			current = candidate
			continue
		}

		// Word does not fit on the current line.
		if current != "" {
			lines = append(lines, current)
		}
		if len(lines) == maxLines {
			return ellipsise(lines, width)
		}
		// A single word longer than the width would loop forever otherwise.
		current = truncateRunes(w, width)
	}

	if current != "" && len(lines) < maxLines {
		lines = append(lines, current)
	} else if current != "" {
		return ellipsise(lines, width)
	}
	return lines
}

// ellipsise marks the last line as truncated, making room for the ellipsis so
// the line still fits. Measured in runes, like wrap.
func ellipsise(lines []string, width int) []string {
	if len(lines) == 0 {
		return lines
	}
	last := lines[len(lines)-1]
	if utf8.RuneCountInString(last)+3 > width && width > 3 {
		last = truncateRunes(last, width-3)
	}
	lines[len(lines)-1] = last + "..."
	return lines
}

// hashView keys the cache on exactly what will be rendered.
func hashView(code, meaning, message, channel string) string {
	sum := sha256.Sum256([]byte(code + "|" + meaning + "|" + message + "|" + channel))
	return hex.EncodeToString(sum[:])[:16]
}
