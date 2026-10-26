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
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/utils"
)

// The XMLTV guide is the same for every player and, on big providers, tens to
// hundreds of MB that the provider generates on request. It is downloaded once
// into a disk cache and served from there: players get a plain file (with
// Range and If-Modified-Since support) instead of each one waiting on the
// provider, and a provider outage keeps serving the last good guide.

var (
	// epgHeaderTimeout bounds how long the provider may take to start
	// answering: guides are generated on request and can take minutes.
	epgHeaderTimeout = 5 * time.Minute
	// epgStallTimeout aborts a download that receives no bytes for this long.
	// There is no overall deadline: a big guide on a slow link legitimately
	// takes minutes while still making progress.
	epgStallTimeout = 60 * time.Second
	// epgFailBackoff is how long a failed download is remembered before
	// another one may start, so an outage does not become a retry loop.
	epgFailBackoff = 5 * time.Minute
)

// epgSniffBytes is how much of the guide is inspected to tell an XMLTV
// document from an error page before it may replace the cached guide.
const epgSniffBytes = 64 * 1024

// epgFetch is one in-flight guide download; done closes when it ends.
type epgFetch struct {
	done chan struct{}
	err  error
}

var (
	epgMu       sync.Mutex
	epgInFlight *epgFetch // nil when no download is running
	epgLastErr  error
	epgFailedAt time.Time
)

func epgCachePath() string {
	return filepath.Join(utils.EPGCacheDir(), "xmltv.xml")
}

// xtreamXMLTV serves xmltv.php from the disk cache. A fresh guide is served
// as-is; a stale one is served while a single background download refreshes
// it; with no guide yet, the request waits for the first download.
func (c *Config) xtreamXMLTV(ctx *gin.Context) {
	p, err := c.waitEPGCache(ctx.Request.Context())
	if err != nil {
		if ctx.Request.Context().Err() != nil {
			ctx.Abort() // client went away; the download carries on for the next one
			return
		}
		_ = ctx.AbortWithError(http.StatusBadGateway, utils.PrintErrorAndReturn(err))
		return
	}
	if c.catalogFilter != nil {
		p = c.filteredEPGPath(p)
	}
	ctx.Header("Content-Type", "application/xml")
	ctx.File(p)
}

// ensureEPGCache starts a background guide download when the cached guide is
// missing or older than the configured M3U cache expiration. It never blocks.
func (c *Config) ensureEPGCache() {
	info, err := os.Stat(epgCachePath())
	if err != nil || time.Since(info.ModTime()).Hours() >= float64(c.M3UCacheExpiration) {
		c.startEPGRefresh()
	}
}

// waitEPGCache returns the cached guide's path, waiting for the in-flight
// download (or until rctx ends) only when no guide has been cached yet. The
// download itself is shared by every waiter and is not tied to rctx.
func (c *Config) waitEPGCache(rctx context.Context) (string, error) {
	p := epgCachePath()
	c.ensureEPGCache()
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	f := c.startEPGRefresh()
	if f == nil {
		epgMu.Lock()
		defer epgMu.Unlock()
		return "", fmt.Errorf("XMLTV guide not available yet (last attempt: %v)", epgLastErr)
	}
	select {
	case <-f.done:
		if f.err != nil {
			return "", f.err
		}
		return p, nil
	case <-rctx.Done():
		return "", rctx.Err()
	}
}

// startEPGRefresh starts a background guide download unless one is already
// running, in which case that one is returned. Returns nil while a recent
// failure is in backoff, or when no Xtream provider is configured.
func (c *Config) startEPGRefresh() *epgFetch {
	if c.XtreamBaseURL == "" {
		return nil
	}
	epgMu.Lock()
	defer epgMu.Unlock()
	if epgInFlight != nil {
		return epgInFlight
	}
	if epgLastErr != nil && time.Since(epgFailedAt) < epgFailBackoff {
		return nil
	}
	f := &epgFetch{done: make(chan struct{})}
	epgInFlight = f
	go func() {
		f.err = c.refreshEPG(epgCachePath())
		epgMu.Lock()
		epgInFlight = nil
		epgLastErr = f.err
		if f.err != nil {
			epgFailedAt = time.Now()
			utils.WarnLog("XMLTV guide download failed (next attempt in %v): %v", epgFailBackoff, f.err)
		}
		epgMu.Unlock()
		close(f.done)
	}()
	return f
}

// refreshEPG downloads the provider's guide into dest. The body goes to a
// sibling .part file renamed into place only once complete and recognised as
// XMLTV, so an aborted download or an error page never replaces a good guide.
func (c *Config) refreshEPG(dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	q := url.Values{}
	q.Set("username", c.XtreamUser.String())
	q.Set("password", c.XtreamPassword.String())
	base := strings.TrimRight(c.XtreamBaseURL, "/") + "/xmltv.php"
	utils.InfoLog("Refreshing XMLTV guide from %s", base) // no query: it holds the credentials
	start := time.Now()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", base+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", utils.GetIPTVUserAgent())
	req.Header.Set("Accept", "application/xml, text/xml, */*")

	// Compression stays on: the transport asks for gzip and inflates it, and
	// XML shrinks ~10x on the wire.
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ResponseHeaderTimeout: epgHeaderTimeout,
		IdleConnTimeout:       90 * time.Second,
	}
	if os.Getenv("XTREAM_INSECURE_TLS_ENABLED") == "true" {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // #nosec G402 -- operator opt-in
	}
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return stripURLError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("provider returned %d for xmltv.php", resp.StatusCode)
	}

	stall := time.AfterFunc(epgStallTimeout, cancel)
	defer stall.Stop()
	counted := &countingReader{r: &stallReader{r: resp.Body, timer: stall, idle: epgStallTimeout}}
	n, err := writeEPG(dest, counted)
	if err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("stalled for %v: %w", epgStallTimeout, err)
		}
		return fmt.Errorf("XMLTV download aborted after %s (%v): %w",
			utils.HumanBytes(counted.n), time.Since(start).Round(time.Second), stripURLError(err))
	}
	utils.InfoLog("Stored XMLTV guide to %s (%s, %s on the wire, in %v)", dest,
		utils.HumanBytes(n), utils.HumanBytes(counted.n), time.Since(start).Round(time.Second))
	return nil
}

// writeEPG writes the guide read from r to a .part sibling of dest and renames
// it into place once complete; on any error dest is left untouched.
func writeEPG(dest string, r io.Reader) (int64, error) {
	body, err := xmltvBody(r)
	if err != nil {
		return 0, err
	}
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, dest)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return n, err
}

// xmltvBody returns the guide's XML stream, inflating a body that is a gzip
// file (some providers serve xmltv.xml.gz without Content-Encoding), and
// rejects anything that does not look like an XMLTV document.
func xmltvBody(r io.Reader) (io.Reader, error) {
	br := bufio.NewReaderSize(r, epgSniffBytes)
	if magic, _ := br.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		zr, err := gzip.NewReader(br)
		if err != nil {
			return nil, fmt.Errorf("invalid gzip XMLTV guide: %w", err)
		}
		br = bufio.NewReaderSize(zr, epgSniffBytes)
	}
	head, err := br.Peek(epgSniffBytes)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull) {
		return nil, stripURLError(err)
	}
	if !bytes.Contains(head, []byte("<tv")) {
		return nil, fmt.Errorf("provider answer is not an XMLTV guide (%d bytes, no <tv> element)", len(head))
	}
	return br, nil
}

// countingReader counts the bytes read through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// stripURLError drops the request URL from a *url.Error: it holds the
// provider credentials in its query and would otherwise reach the logs.
func stripURLError(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return fmt.Errorf("%s xmltv.php: %w", uerr.Op, uerr.Err)
	}
	return err
}
