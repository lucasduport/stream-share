/*
 * stream-share is a project to efficiently share the use of an IPTV service.
 * Copyright (C) 2025  Lucas Duport
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License, version 3 or later.
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
	"bufio"
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/utils"
)

// Catalogue responses (get.php playlists, XMLTV guides, player_api lists) are
// tens to hundreds of MB of text that compresses about tenfold. They are
// compressed once per version — a .gz sibling next to a cached file, or a
// memoised gzip body for a cached catalogue — and served compressed to every
// client that accepts gzip, so the per-request cost is a plain file or byte
// copy either way.

// acceptsGzip reports whether the request accepts a gzip-encoded answer.
func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		fields := strings.Split(part, ";")
		if !strings.EqualFold(strings.TrimSpace(fields[0]), "gzip") {
			continue
		}
		for _, p := range fields[1:] {
			if v := strings.ReplaceAll(strings.TrimSpace(p), " ", ""); v == "q=0" || v == "q=0.0" || v == "q=0.00" || v == "q=0.000" {
				return false
			}
		}
		return true
	}
	return false
}

// gzipBuilds tracks .gz siblings being written (source path -> struct{}), so
// concurrent requests start one compression per file.
var gzipBuilds sync.Map

// serveFileCompressed serves the file at p, sending its gzip sibling instead
// when the client accepts gzip and asks for the whole file. The sibling is
// written in the background the first time the file is served after it
// changed, and the plain file is served until it is ready; it carries the
// source's mtime, which is how a sibling of an older version is recognised.
// Range requests always get the plain file.
func serveFileCompressed(ctx *gin.Context, p, contentType string) {
	ctx.Header("Vary", "Accept-Encoding")
	r := ctx.Request
	if acceptsGzip(r) && r.Header.Get("Range") == "" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		if info, err := os.Stat(p); err == nil {
			gz := p + ".gz"
			if f, err := os.Open(gz); err == nil {
				gi, serr := f.Stat()
				if serr == nil && gi.ModTime().Equal(info.ModTime()) {
					defer func() { _ = f.Close() }()
					ctx.Header("Content-Type", contentType)
					ctx.Header("Content-Encoding", "gzip")
					http.ServeContent(ctx.Writer, r, "", info.ModTime(), f)
					return
				}
				_ = f.Close()
			}
			startGzipSibling(p)
		}
	}
	ctx.Header("Content-Type", contentType)
	ctx.File(p)
}

// startGzipSibling writes p+".gz" in the background unless that is already
// under way.
func startGzipSibling(p string) {
	if _, busy := gzipBuilds.LoadOrStore(p, struct{}{}); busy {
		return
	}
	go func() {
		defer gzipBuilds.Delete(p)
		if err := writeGzipSibling(p); err != nil {
			utils.WarnLog("Compressing %s failed; serving it uncompressed: %v", p, err)
		}
	}()
}

// writeGzipSibling compresses p into p+".gz" through a .part file and stamps
// it with the mtime of the version it was made from.
func writeGzipSibling(p string) error {
	in, err := os.Open(p)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	tmp := p + ".gz.part"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(out, 256*1024)
	// The fastest level already shrinks playlist and guide text ~8x, and a
	// several-hundred-MB file is recompressed on every refresh.
	zw, _ := gzip.NewWriterLevel(bw, gzip.BestSpeed)
	_, err = io.Copy(zw, bufio.NewReaderSize(in, 256*1024))
	if err == nil {
		err = zw.Close()
	}
	if err == nil {
		err = bw.Flush()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chtimes(tmp, info.ModTime(), info.ModTime())
	}
	if err == nil {
		err = os.Rename(tmp, p+".gz")
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

// gzipBytes compresses b once; used for memoised catalogue bodies.
func gzipBytes(b []byte) ([]byte, error) {
	var buf bytes.Buffer
	buf.Grow(len(b) / 6)
	zw, _ := gzip.NewWriterLevel(&buf, gzip.DefaultCompression)
	if _, err := zw.Write(b); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// renderedGzip memoises the gzip form of a cached catalogue's rendered JSON,
// one slot per cache key, replaced when the key's entry is replaced.
type renderedGzip struct {
	entry *catalogEntry
	once  sync.Once
	body  []byte
	err   error
}

var (
	renderedGzipMu sync.Mutex
	renderedGzips  = map[string]*renderedGzip{}
)

// catalogGzip returns the gzip form of body, the rendered JSON of entry e
// cached under key, compressing it once per entry.
func catalogGzip(key string, e *catalogEntry, body []byte) ([]byte, error) {
	renderedGzipMu.Lock()
	g := renderedGzips[key]
	if g == nil || g.entry != e {
		g = &renderedGzip{entry: e}
		renderedGzips[key] = g
	}
	renderedGzipMu.Unlock()
	g.once.Do(func() { g.body, g.err = gzipBytes(body) })
	return g.body, g.err
}
