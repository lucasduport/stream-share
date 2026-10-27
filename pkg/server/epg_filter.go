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
	"fmt"
	"html"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/lucasduport/stream-share/pkg/filter"
	"github.com/lucasduport/stream-share/pkg/utils"
)

// The guide players get holds only the channels the catalogue filter keeps
// (and their programmes). The provider's guide stays cached as downloaded;
// a filtered copy is derived from it next to it, named after the filter rules
// and the live catalogue it was computed from, and rebuilt when either the
// guide or that catalogue changes.

var (
	// epgFilterMu serialises building the filtered guide.
	epgFilterMu sync.Mutex
	// epgFilterUnavailableOnce logs once that the guide is served unfiltered
	// because no live catalogue is cached yet.
	epgFilterUnavailableOnce sync.Once
)

// filteredEPGPath returns the guide to serve for raw (the cached provider
// guide): raw itself when the live filter is off or the kept channel set is
// not known yet (no live catalogue cached), else a filtered copy, built on
// first use. A failure to build it serves raw rather than nothing.
func (c *Config) filteredEPGPath(raw string) string {
	if !c.catalogFilter.ActiveFor(filter.Live) {
		return raw
	}
	set := c.keptLiveIDs()
	if set == nil {
		epgFilterUnavailableOnce.Do(func() {
			utils.InfoLog("XMLTV filter: live catalogue not loaded yet, serving the unfiltered guide")
		})
		return raw
	}
	info, err := os.Stat(raw)
	if err != nil {
		return raw
	}
	// Named after what decides its content — the rules, the guide version and
	// the set of kept channels — rather than when the live catalogue was
	// fetched: the catalogue is refetched hourly, and naming the file after
	// the fetch rebuilt the whole guide inside a player's request each time
	// even though the kept channels had not changed.
	dest := filepath.Join(filepath.Dir(raw), fmt.Sprintf("xmltv.%s.%d.%s.xml",
		c.catalogFilter.Fingerprint(), info.ModTime().UnixNano(), set.epgKey()))
	if _, err := os.Stat(dest); err == nil {
		return dest
	}

	epgFilterMu.Lock()
	defer epgFilterMu.Unlock()
	if _, err := os.Stat(dest); err == nil {
		return dest
	}
	start := time.Now()
	kept, dropped, err := writeFilteredEPG(raw, dest, set.epgIDs)
	if err != nil {
		utils.WarnLog("XMLTV filter failed, serving the unfiltered guide: %v", err)
		return raw
	}
	utils.InfoLog("XMLTV filter: kept %d of %d channels in %v", kept, kept+dropped, time.Since(start).Round(time.Millisecond))
	removeStaleFilteredEPGs(filepath.Dir(raw), dest)
	return dest
}

// removeStaleFilteredEPGs deletes filtered guides other than keep, with their
// compressed siblings.
func removeStaleFilteredEPGs(dir, keep string) {
	matches, _ := filepath.Glob(filepath.Join(dir, "xmltv.*.*.xml"))
	gz, _ := filepath.Glob(filepath.Join(dir, "xmltv.*.*.xml.gz"))
	for _, m := range append(matches, gz...) {
		if m != keep && m != keep+".gz" {
			_ = os.Remove(m)
		}
	}
}

// writeFilteredEPG copies the guide at src to dst, keeping only the <channel>
// elements whose id is in keep and the <programme> elements whose channel is.
// It streams: memory stays at one element, whatever the guide's size. dst is
// written through a .part file and only appears once complete.
func writeFilteredEPG(src, dst string, keep map[string]bool) (kept, dropped int, err error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = in.Close() }()
	tmp := dst + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return 0, 0, err
	}
	w := bufio.NewWriterSize(out, 256*1024)
	kept, dropped, err = filterXMLTV(bufio.NewReaderSize(in, 256*1024), w, keep)
	if err == nil {
		err = w.Flush()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return kept, dropped, err
}

// filterXMLTV copies an XMLTV document from r to w, dropping the <channel>
// and <programme> elements of channels not in keep. Everything else (prolog,
// the <tv> element, whitespace) is copied byte for byte. These two elements
// do not nest, so each is read through its closing tag (or "/>").
func filterXMLTV(r *bufio.Reader, w io.Writer, keep map[string]bool) (kept, dropped int, err error) {
	for {
		// Copy up to (not including) the next '<'.
		chunk, rerr := r.ReadSlice('<')
		atTag := rerr == nil
		if atTag {
			chunk = chunk[:len(chunk)-1]
		}
		if len(chunk) > 0 {
			if _, err = w.Write(chunk); err != nil {
				return kept, dropped, err
			}
		}
		switch {
		case rerr == io.EOF:
			return kept, dropped, nil
		case rerr == bufio.ErrBufferFull:
			continue
		case rerr != nil:
			return kept, dropped, rerr
		}

		var tag, attr string
		switch {
		case peekTag(r, "channel"):
			tag, attr = "channel", "id"
		case peekTag(r, "programme"):
			tag, attr = "programme", "channel"
		default:
			if _, err = w.Write([]byte{'<'}); err != nil {
				return kept, dropped, err
			}
			continue
		}

		// The element: its opening tag through '>', then (unless
		// self-closing) through its closing tag.
		elem, rerr := readThrough(r, []byte(">"))
		if rerr != nil {
			return kept, dropped, fmt.Errorf("truncated <%s> tag: %w", tag, rerr)
		}
		open := elem
		if !bytes.HasSuffix(bytes.TrimRight(open[:len(open)-1], " \t\r\n"), []byte("/")) {
			rest, rerr := readThrough(r, []byte("</"+tag+">"))
			if rerr != nil {
				return kept, dropped, fmt.Errorf("unterminated <%s> element: %w", tag, rerr)
			}
			elem = append(elem, rest...)
		}

		if !keep[html.UnescapeString(xmlAttr(open, attr))] {
			if tag == "channel" {
				dropped++
			}
			continue
		}
		if tag == "channel" {
			kept++
		}
		if _, err = w.Write([]byte{'<'}); err != nil {
			return kept, dropped, err
		}
		if _, err = w.Write(elem); err != nil {
			return kept, dropped, err
		}
	}
}

// peekTag tells whether the reader (positioned just after a '<') is at the
// start tag of element name: the name followed by whitespace, '>' or '/'.
func peekTag(r *bufio.Reader, name string) bool {
	b, _ := r.Peek(len(name) + 1)
	if len(b) < len(name)+1 || string(b[:len(name)]) != name {
		return false
	}
	switch b[len(name)] {
	case ' ', '\t', '\r', '\n', '>', '/':
		return true
	}
	return false
}

// readThrough reads from r up to and including delim, returning a copy.
func readThrough(r *bufio.Reader, delim []byte) ([]byte, error) {
	var out []byte
	last := delim[len(delim)-1]
	for {
		chunk, err := r.ReadSlice(last)
		out = append(out, chunk...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			return out, err
		}
		if bytes.HasSuffix(out, delim) {
			return out, nil
		}
	}
}

// xmlAttr returns the raw (still escaped) value of attribute name in an
// opening tag, "" when absent.
func xmlAttr(tag []byte, name string) string {
	for i := 0; i+len(name)+2 <= len(tag); i++ {
		// The name must start after whitespace and be followed by ="/=' .
		if !bytes.HasPrefix(tag[i:], []byte(name)) || i == 0 {
			continue
		}
		switch tag[i-1] {
		case ' ', '\t', '\r', '\n':
		default:
			continue
		}
		rest := tag[i+len(name):]
		if len(rest) < 2 || rest[0] != '=' || (rest[1] != '"' && rest[1] != '\'') {
			continue
		}
		q := rest[1]
		v := rest[2:]
		if j := bytes.IndexByte(v, q); j >= 0 {
			return string(v[:j])
		}
		return ""
	}
	return ""
}
