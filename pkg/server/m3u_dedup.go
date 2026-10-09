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
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/jamesnetherton/m3u"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// dedupEntry is one M3U track prepared for deduplication: the original track,
// its parsed URL identity, and the metadata extracted from its title and tags.
// order preserves the provider's original sequence so that, all else being
// equal, the entry the provider listed first wins (stable tie-break).
type dedupEntry struct {
	track m3u.Track
	order int

	// basePath is the Xtream stream type ("live", "movie", "series",
	// "timeshift", or "" for plain M3U URLs without such a segment).
	basePath string
	// id is the stream ID: the last URL path segment without its extension.
	id string
	// urlKey is the exact-identity key. For Xtream URLs it is basePath+id;
	// for plain M3U URLs (no recognized type segment) it is the full URL, so
	// two different sources that merely share a trailing filename are not
	// wrongly merged.
	urlKey string

	// titleKey is the normalized title fingerprint used to group
	// same-content entries that carry different stream IDs (typical for
	// per-language or per-quality copies of a VOD movie).
	titleKey string

	quality  int
	language string
	hasLogo  bool
	hasEPG   bool
	hasGroup bool
	episode  bool // title carries an SxxEyy episode marker
	year     int
}

// dedupQualityRank scores video-quality tokens found in titles. Higher wins.
var dedupQualityRank = map[string]int{
	"8k": 90, "uhd": 90, "4320p": 90,
	"4k": 80, "2160p": 80,
	"fhd": 70, "1080p": 70, "1080i": 65,
	"hd": 60, "720p": 60, "hdtv": 55,
	"sd": 40, "576p": 40, "480p": 35,
	"240p": 20, "144p": 10,
}

// dedupLanguageTokens maps language tokens (as they appear in titles, already
// lowercased) to a canonical ISO-ish code. Used both for scoring and for
// stripping language markers out of the title fingerprint.
var dedupLanguageTokens = map[string]string{
	"vf": "fra", "vff": "fra", "vfq": "fra", "vf2": "fra", "french": "fra", "francais": "fra", "français": "fra", "fr": "fra", "fra": "fra",
	"multi": "multi", "multiaudio": "multi",
	"vostfr": "vostfr", "vost": "vostfr",
	"english": "eng", "eng": "eng", "en": "eng",
	"ita": "ita", "it": "ita", "italian": "ita", "italiano": "ita",
	"deu": "deu", "ger": "deu", "german": "deu", "deutsch": "deu", "de": "deu",
	"esp": "spa", "es": "spa", "spanish": "spa", "español": "spa", "spa": "spa",
	"nld": "nld", "dut": "nld", "dutch": "nld", "nederlands": "nld", "nl": "nld",
	"rus": "rus", "russian": "rus", "ru": "rus",
	"jpn": "jpn", "japanese": "jpn", "ja": "jpn",
	"kor": "kor", "korean": "kor", "ko": "kor",
	"por": "por", "pt": "por", "brazilian": "por", "ptbr": "por", "pt-br": "por",
	"tur": "tur", "turkish": "tur", "tr": "tur",
	"ara": "ara", "arabic": "ara", "ar": "ara",
	"hin": "hin", "hindi": "hin", "hi": "hin",
	"chi": "chi", "zho": "chi", "chinese": "chi", "zh": "chi",
	"pol": "pol", "polish": "pol", "pl": "pol",
	"swe": "swe", "swedish": "swe", "sv": "swe",
}

var (
	// Quality tokens matched on word boundaries (case-insensitive).
	reDedupQuality = regexp.MustCompile(`(?i)\b(8k|uhd|4320p|4k|2160p|fhd|1080p|1080i|hd|720p|hdtv|sd|576p|480p|240p|144p)\b`)
	// Language tokens matched on word boundaries. Longer alternatives come
	// first so leftmost-first matching prefers them (e.g. "pt-br" over "pt").
	reDedupLang = regexp.MustCompile(`(?i)\b(vf2|vff|vfq|vostfr|vost|multiaudio|multi|french|francais|français|english|italiano|deutsch|spanish|nederlands|russian|japanese|korean|brazilian|turkish|arabic|hindi|chinese|polish|swedish|german|dutch|vf|fra|fr|eng|en|ita|it|deu|ger|de|esp|es|spa|nld|dut|nl|rus|ru|jpn|ja|kor|ko|por|ptbr|pt-br|pt|tur|tr|ara|ar|hin|hi|chi|zho|zh|pol|pl|swe|sv)\b`)
	// SxxEyy episode marker, canonicalized to "s01e02". The trailing group
	// captures the following non-digit (or end of string) so markers like
	// "S01E02V2" still match.
	reDedupEpisode = regexp.MustCompile(`(?i)\bs(\d{1,2})\s?[e](\d{1,3})(\D|$)`)
	// Standalone season marker (S01 without an episode part). Runs after the
	// episode pass, so already-canonicalized "s01e02" does not match (no word
	// boundary between the digits and the "e").
	reDedupSeason = regexp.MustCompile(`(?i)\bs(\d{1,2})\b`)
	// Year in parentheses/brackets/braces, e.g. (2019) or [2019].
	reDedupYear = regexp.MustCompile(`[\(\[\{]((?:19|20)\d{2})[\)\]\}]`)
	// Release-group style trailing dash, e.g. "-GROUP" at end of title.
	reDedupReleaseGroup = regexp.MustCompile(`\s+-\s*[A-Za-z0-9]+\s*$`)
	// Bracketed/parenthesized groups, e.g. "(FR)", "[4K]", "(2019)".
	reDedupBracket = regexp.MustCompile(`[\(\[\{][^\)\]\}]*[\)\]\}]`)
	// Non-alphanumeric runs collapse to a single space.
	reDedupNonAlpha = regexp.MustCompile(`[^a-z0-9]+`)
	// Leading article.
	reDedupArticle = regexp.MustCompile(`^(the|le|la|les|l|el|los|las|der|die|das|il|lo|gli|un|une|una|o|a|os|as)\s+`)
	// tag="value" attribute extractor for raw #EXTINF lines.
	reDedupTag = regexp.MustCompile(`([a-zA-Z0-9-]+)="([^"]*)"`)
)

// dedupAccentFold removes diacritics so "français" and "francais" compare equal.
func dedupAccentFold(s string) string {
	t := transform.Chain(norm.NFD, transform.RemoveFunc(func(r rune) bool {
		return unicode.Is(unicode.Mn, r)
	}), norm.NFC)
	out, _, err := transform.String(t, s)
	if err != nil {
		return s
	}
	return out
}

// dedupNormalizeTitle canonicalizes a track title into a comparison key:
// lowercase, accent-folded, leading article dropped, SxxEyy canonicalized to
// s01e02, quality/language tokens and bracketed groups removed, punctuation
// collapsed. Two titles that differ only by quality, language, or formatting
// map to the same key.
func dedupNormalizeTitle(title string) string {
	s := dedupAccentFold(strings.ToLower(strings.TrimSpace(title)))
	s = reDedupEpisode.ReplaceAllStringFunc(s, func(m string) string {
		parts := reDedupEpisode.FindStringSubmatch(m)
		if len(parts) != 4 {
			return m
		}
		se, _ := strconv.Atoi(parts[1])
		ep, _ := strconv.Atoi(parts[2])
		return "s" + dedupPad2(se) + "e" + dedupPad3(ep) + parts[3]
	})
	s = reDedupSeason.ReplaceAllStringFunc(s, func(m string) string {
		parts := reDedupSeason.FindStringSubmatch(m)
		if len(parts) != 2 {
			return m
		}
		se, _ := strconv.Atoi(parts[1])
		return "s" + dedupPad2(se)
	})
	s = reDedupReleaseGroup.ReplaceAllString(s, "")
	s = reDedupQuality.ReplaceAllString(s, " ")
	s = reDedupLang.ReplaceAllString(s, " ")
	s = reDedupBracket.ReplaceAllString(s, " ")
	s = reDedupNonAlpha.ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)
	for {
		next := reDedupArticle.ReplaceAllString(s, "")
		if next == s {
			break
		}
		s = strings.TrimSpace(next)
	}
	return s
}

func dedupPad2(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

func dedupPad3(n int) string {
	switch {
	case n < 10:
		return "00" + strconv.Itoa(n)
	case n < 100:
		return "0" + strconv.Itoa(n)
	default:
		return strconv.Itoa(n)
	}
}

// dedupDetectQuality returns the best quality score found in the text.
func dedupDetectQuality(text string) int {
	best := 0
	for _, m := range reDedupQuality.FindAllString(strings.ToLower(text), -1) {
		if q, ok := dedupQualityRank[m]; ok && q > best {
			best = q
		}
	}
	return best
}

// dedupDetectLanguage returns the canonical language code of the first
// language token found in the text, or "" when none is found.
func dedupDetectLanguage(text string) string {
	m := reDedupLang.FindString(strings.ToLower(text))
	if m == "" {
		return ""
	}
	return dedupLanguageTokens[m]
}

// dedupDetectYear extracts a 19xx/20xx year from bracketed/parenthesized text.
func dedupDetectYear(title string) int {
	m := reDedupYear.FindStringSubmatch(title)
	if len(m) != 2 {
		return 0
	}
	y, _ := strconv.Atoi(m[1])
	return y
}

// dedupTagValue returns the value of the first tag with the given name
// (case-insensitive), or "".
func dedupTagValue(tags []m3u.Tag, name string) string {
	for _, t := range tags {
		if strings.EqualFold(t.Name, name) {
			return t.Value
		}
	}
	return ""
}

// parseDedupEntry builds a dedupEntry from a track.
func parseDedupEntry(track m3u.Track, order int) dedupEntry {
	e := dedupEntry{track: track, order: order}

	if u, err := url.Parse(track.URI); err == nil {
		e.basePath = extractBasePath(u.Path)
		last := path.Base(u.Path)
		id := last
		if ext := path.Ext(last); ext != "" {
			id = strings.TrimSuffix(last, ext)
		}
		e.id = id
		if e.basePath != "" {
			e.urlKey = e.basePath + "\x00" + e.id
		} else {
			// Plain M3U without an Xtream type segment: the exact identity is
			// the full URL, so same-named files from different sources are not
			// merged by identity.
			e.urlKey = track.URI
		}
	}

	// Metadata detection looks at the title and the group-title: providers
	// often put the language/quality in group-title (e.g. "FR | 4K") and the
	// clean name in the title.
	group := dedupTagValue(track.Tags, "group-title")
	detectSource := track.Name
	if group != "" {
		detectSource = track.Name + " " + group
	}

	e.titleKey = dedupNormalizeTitle(track.Name)
	e.quality = dedupDetectQuality(detectSource)
	e.language = dedupDetectLanguage(detectSource)
	e.hasLogo = dedupTagValue(track.Tags, "tvg-logo") != ""
	e.hasEPG = dedupTagValue(track.Tags, "tvg-id") != ""
	e.hasGroup = group != ""
	e.episode = reDedupEpisode.MatchString(track.Name)
	e.year = dedupDetectYear(track.Name)

	return e
}

// dedupScore computes the preference score of an entry. Higher wins.
// Order of preference:
//  1. Video quality (4K > 1080p > 720p > ...)
//  2. Preferred language (configured list, in order)
//  3. Rich metadata (logo, EPG id, group-title)
//
// Ties are broken by the caller on the original playlist order.
func dedupScore(e dedupEntry, preferredLangs []string) int {
	score := e.quality * 1000

	if e.language != "" {
		rank := len(preferredLangs) + 1 // detected but not preferred
		for i, p := range preferredLangs {
			if strings.EqualFold(p, e.language) {
				rank = i
				break
			}
		}
		// Invert so the most preferred language scores highest.
		score += (len(preferredLangs) + 1 - rank) * 100
	}

	if e.hasLogo {
		score += 10
	}
	if e.hasEPG {
		score += 5
	}
	if e.hasGroup {
		score += 2
	}
	return score
}

// dedupTitleGroupKey returns the title-fingerprint key used to merge
// same-content entries that have different stream IDs. Entries whose title
// carries an episode marker (SxxEyy) are never merged by title, and neither
// are "series" or "timeshift" entries: there, distinct stream IDs are usually
// distinct episodes, and collapsing them would lose content. The year is part
// of the key so remakes (Dune 1984 vs Dune 2021) stay separate.
func dedupTitleGroupKey(e dedupEntry) string {
	if e.titleKey == "" || e.episode {
		return ""
	}
	switch e.basePath {
	case "movie", "live", "":
		// merge allowed
	default:
		return ""
	}
	return e.basePath + "\x00" + strconv.Itoa(e.year) + "\x00" + e.titleKey
}

// dedupUnionFind is a minimal union-find over integer indices.
type dedupUnionFind struct {
	parent []int
}

func newDedupUnionFind(n int) *dedupUnionFind {
	p := make([]int, n)
	for i := range p {
		p[i] = i
	}
	return &dedupUnionFind{parent: p}
}

func (uf *dedupUnionFind) find(x int) int {
	for uf.parent[x] != x {
		uf.parent[x] = uf.parent[uf.parent[x]]
		x = uf.parent[x]
	}
	return x
}

func (uf *dedupUnionFind) union(a, b int) {
	ra, rb := uf.find(a), uf.find(b)
	if ra != rb {
		uf.parent[rb] = ra
	}
}

// deduplicateTracks removes redundant tracks from the playlist, keeping the
// best representative of each duplicate group. The relative order of the
// survivors matches the provider's original ordering.
//
// Duplicate detection has two layers:
//   - Exact identity: same Xtream base path + stream ID (the same source
//     listed twice, e.g. in two categories), or the exact same URL for plain
//     M3U entries.
//   - Title fingerprint: same normalized title (quality, language, accents,
//     punctuation, year and SxxEyy formatting ignored) within a mergeable
//     stream type. This collapses per-language / per-quality copies of a movie
//     or channel into one entry. Series and timeshift entries, and any entry
//     with an episode marker, are exempt so distinct episodes are never lost.
//
// preferredLangs is the ordered list of preferred audio languages (e.g.
// ["fra"]); among equals, an entry in a preferred language wins.
//
// Returns the deduplicated track slice and the number of removed tracks.
func deduplicateTracks(tracks []m3u.Track, preferredLangs []string) ([]m3u.Track, int) {
	keep := deduplicateTrackIndices(tracks, preferredLangs)
	out := make([]m3u.Track, 0, len(keep))
	for _, i := range keep {
		out = append(out, tracks[i])
	}
	return out, len(tracks) - len(out)
}

// deduplicateTrackIndices returns the indices of the tracks to keep, in their
// original relative order. It is the index-level core of deduplicateTracks, so
// callers that already hold parallel per-track data (e.g. byte offsets in a
// temp file) can map the decision back to their own structures without
// matching on title/URI.
func deduplicateTrackIndices(tracks []m3u.Track, preferredLangs []string) []int {
	if len(tracks) < 2 {
		keep := make([]int, len(tracks))
		for i := range keep {
			keep[i] = i
		}
		return keep
	}

	entries := make([]dedupEntry, len(tracks))
	for i, tr := range tracks {
		entries[i] = parseDedupEntry(tr, i)
	}

	uf := newDedupUnionFind(len(entries))

	// Layer 1: exact identity.
	byKey := make(map[string]int, len(entries))
	for i, e := range entries {
		if e.urlKey == "" {
			continue
		}
		if j, ok := byKey[e.urlKey]; ok {
			uf.union(i, j)
		} else {
			byKey[e.urlKey] = i
		}
	}

	// Layer 2: title fingerprint.
	byTitle := make(map[string]int, len(entries))
	for i, e := range entries {
		k := dedupTitleGroupKey(e)
		if k == "" {
			continue
		}
		if j, ok := byTitle[k]; ok {
			uf.union(i, j)
		} else {
			byTitle[k] = i
		}
	}

	// Pick the best representative per group.
	groups := make(map[int][]int, len(entries))
	for i := range entries {
		root := uf.find(i)
		groups[root] = append(groups[root], i)
	}

	keep := make([]bool, len(entries))
	for _, members := range groups {
		if len(members) == 1 {
			keep[members[0]] = true
			continue
		}
		best := members[0]
		bestScore := dedupScore(entries[best], preferredLangs)
		for _, m := range members[1:] {
			s := dedupScore(entries[m], preferredLangs)
			if s > bestScore || (s == bestScore && entries[m].order < entries[best].order) {
				best, bestScore = m, s
			}
		}
		keep[best] = true
	}

	indices := make([]int, 0, len(entries))
	for i := range entries {
		if keep[i] {
			indices = append(indices, i)
		}
	}
	return indices
}

// dedupPreferredLanguages parses a comma-separated list of preferred language
// codes into a normalized slice. Empty entries are dropped.
func dedupPreferredLanguages(raw string) []string {
	var out []string
	for _, p := range strings.Split(raw, ",") {
		p = strings.ToLower(strings.TrimSpace(p))
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
