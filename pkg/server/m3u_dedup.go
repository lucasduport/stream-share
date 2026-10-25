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
	"hash/fnv"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/jamesnetherton/m3u"
	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// dedupRecord is the compact per-track state deduplication needs. Keys are
// 64-bit FNV hashes rather than strings so a 1.45M-track catalogue costs
// ~24 bytes per track; the collision probability at that size is ~1e-7.
type dedupRecord struct {
	// idKey is the exact-identity hash: Xtream stream type + stream ID, or the
	// full URL for plain M3U entries. 0 means "no identity".
	idKey uint64
	// titleKey is the title-fingerprint hash, 0 when the track must never be
	// merged by title (episodes, series, timeshift, plain M3U, empty title).
	titleKey uint64
	// score ranks duplicates; the highest wins, ties go to the earliest track.
	score int32
}

// dedupQualityRank scores video-quality tokens found in titles. Higher wins.
var dedupQualityRank = map[string]int{
	"8k": 90, "4320p": 90,
	"4k": 80, "uhd": 80, "2160p": 80,
	"fhd": 70, "1080p": 70, "1080i": 65,
	"hd": 60, "720p": 60, "hdtv": 55,
	"sd": 40, "576p": 40, "480p": 35,
	"240p": 20, "144p": 10,
}

// dedupLangWords are release tags and language names, matched
// case-insensitively: they are unambiguous as tags.
var dedupLangWords = map[string]string{
	"vf": "fra", "vff": "fra", "vfq": "fra", "vfi": "fra", "vf2": "fra", "truefrench": "fra",
	"french": "fra", "francais": "fra",
	"vostfr": "vostfr", "vost": "vostfr",
	"multi": "multi", "multiaudio": "multi",
	"english": "eng",
	"italian": "ita", "italiano": "ita",
	"german": "deu", "deutsch": "deu",
	"spanish": "spa", "espanol": "spa",
	"dutch": "nld", "nederlands": "nld",
	"russian": "rus", "japanese": "jpn", "korean": "kor",
	"portuguese": "por", "brazilian": "por",
	"turkish": "tur", "arabic": "ara", "hindi": "hin", "chinese": "chi",
	"polish": "pol", "swedish": "swe",
}

// dedupLangCodes are ISO-style codes. Many double as ordinary words ("It",
// "De", "En", "Es"), so they only count as tags when written in upper case,
// or inside brackets.
var dedupLangCodes = map[string]string{
	"FR": "fra", "FRA": "fra",
	"EN": "eng", "ENG": "eng",
	"IT": "ita", "ITA": "ita",
	"DE": "deu", "DEU": "deu", "GER": "deu",
	"ES": "spa", "ESP": "spa", "SPA": "spa",
	"NL": "nld", "NLD": "nld", "DUT": "nld",
	"RU": "rus", "RUS": "rus",
	"JP": "jpn", "JA": "jpn", "JPN": "jpn",
	"KO": "kor", "KOR": "kor",
	"PT": "por", "POR": "por", "PTBR": "por", "PT-BR": "por", "BR": "por",
	"TR": "tur", "TUR": "tur",
	"AR": "ara", "ARA": "ara",
	"HI": "hin", "HIN": "hin",
	"ZH": "chi", "CHI": "chi", "ZHO": "chi",
	"PL": "pol", "POL": "pol",
	"SV": "swe", "SWE": "swe",
}

// dedupAlternation joins keys longest-first so leftmost-first matching
// prefers "PT-BR" over "PT".
func dedupAlternation(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, regexp.QuoteMeta(k))
	}
	sort.Slice(keys, func(i, j int) bool {
		if len(keys[i]) != len(keys[j]) {
			return len(keys[i]) > len(keys[j])
		}
		return keys[i] < keys[j]
	})
	return strings.Join(keys, "|")
}

var (
	dedupLangPattern = `(?:(?i:` + dedupAlternation(dedupLangWords) + `)|` + dedupAlternation(dedupLangCodes) + `)`

	// Any language tag, used to detect the language for scoring.
	reDedupLang = regexp.MustCompile(`\b` + dedupLangPattern + `\b`)
	// A language tag at the start of the title, followed by a separator:
	// "FR: TF1", "[FR] Dune", "|FR| Dune", "FR - Dune".
	reDedupLangPrefix = regexp.MustCompile(`^\s*[\[\(|]?\s*` + dedupLangPattern + `\s*[\]\)|:\-]+\s*`)
	// Quality tokens are stripped anywhere; they are not ordinary words.
	reDedupQuality = regexp.MustCompile(`(?i)\b(8k|4320p|uhd|4k|2160p|fhd|1080p|1080i|hdtv|hd|720p|sd|576p|480p|240p|144p)\b`)
	// SxxEyy episode marker, canonicalized to "s01e002".
	reDedupEpisode = regexp.MustCompile(`(?i)\bs(\d{1,2})\s?e(\d{1,3})(\D|$)`)
	// Year in parentheses/brackets/braces, e.g. (2019) or [2019].
	reDedupYear = regexp.MustCompile(`[\(\[\{]((?:19|20)\d{2})[\)\]\}]`)
	// Unbracketed trailing year on an already-normalized title: "dune 2021".
	reDedupTrailingYear = regexp.MustCompile(`^(.+) ((?:19|20)\d{2})$`)
	// A bracketed group, e.g. "(FR)", "[4K]", "(2019)", "(US)".
	reDedupBracket = regexp.MustCompile(`[\(\[\{]([^\(\)\[\]\{\}]*)[\)\]\}]`)
	// Non-alphanumeric runs collapse to a single space.
	reDedupNonAlnum = regexp.MustCompile(`[^a-z0-9]+`)
	// Leading article, stripped once. Kept to unambiguous articles: "die",
	// "das", "a" etc. start too many real titles.
	reDedupArticle = regexp.MustCompile(`^(the|le|la|les|l) `)
)

// dedupAccentFolder is reused across calls: building a transform chain
// allocates ~20 KB, which dominated the per-track cost on large playlists.
var dedupAccentFolder = sync.Pool{New: func() interface{} {
	return transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
}}

// dedupAccentFold removes diacritics so "français" and "francais" compare equal.
func dedupAccentFold(s string) string {
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			ascii = false
			break
		}
	}
	if ascii {
		return s
	}
	t := dedupAccentFolder.Get().(transform.Transformer)
	defer dedupAccentFolder.Put(t)
	t.Reset()
	out, _, err := transform.String(t, s)
	if err != nil {
		return s
	}
	return out
}

// dedupIsTagGroup reports whether a bracketed group holds only language,
// quality or year tokens, e.g. "FR", "4K UHD", "2021", "vostfr". Inside
// brackets codes are unambiguous, so case is ignored.
func dedupIsTagGroup(inner string) bool {
	fields := strings.FieldsFunc(inner, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-'
	})
	if len(fields) == 0 {
		return false
	}
	for _, f := range fields {
		lower := strings.ToLower(f)
		if _, ok := dedupLangWords[lower]; ok {
			continue
		}
		if _, ok := dedupLangCodes[strings.ToUpper(f)]; ok {
			continue
		}
		if _, ok := dedupQualityRank[lower]; ok {
			continue
		}
		if reDedupYear.MatchString("(" + f + ")") {
			continue
		}
		return false
	}
	return true
}

// dedupTitleFingerprint canonicalizes a track title into a comparison key
// plus its release year (0 when none). Language tags at the edges of the
// title, quality tokens, tag-only bracket groups, accents, case, punctuation,
// a leading article and SxxEyy formatting are ignored, so two titles that
// differ only by those map to the same key. Bracketed text that is not a tag
// ("(US)", "(Director's Cut)") stays in the key, so it never causes a merge.
func dedupTitleFingerprint(title string) (key string, year int) {
	s := dedupAccentFold(strings.TrimSpace(title))

	if m := reDedupYear.FindStringSubmatch(s); len(m) == 2 {
		year, _ = strconv.Atoi(m[1])
	}
	s = reDedupBracket.ReplaceAllStringFunc(s, func(m string) string {
		inner := m[1 : len(m)-1]
		if dedupIsTagGroup(inner) {
			return " "
		}
		return " " + inner + " "
	})
	for {
		next := reDedupLangPrefix.ReplaceAllString(s, "")
		if next == s || strings.TrimSpace(next) == "" {
			break
		}
		s = next
	}
	s = reDedupQuality.ReplaceAllString(s, " ")
	s = dedupTrimLangSuffix(s)

	s = strings.ToLower(s)
	s = reDedupEpisode.ReplaceAllStringFunc(s, func(m string) string {
		parts := reDedupEpisode.FindStringSubmatch(m)
		se, _ := strconv.Atoi(parts[1])
		ep, _ := strconv.Atoi(parts[2])
		return " s" + dedupPad(se, 2) + "e" + dedupPad(ep, 3) + parts[3]
	})
	s = strings.TrimSpace(reDedupNonAlnum.ReplaceAllString(s, " "))
	s = reDedupArticle.ReplaceAllString(s, "")

	// "Dune 2021" carries its year unbracketed; treat it like "Dune (2021)".
	if m := reDedupTrailingYear.FindStringSubmatch(s); len(m) == 3 {
		if y, _ := strconv.Atoi(m[2]); year == 0 || y == year {
			year = y
			s = m[1]
		}
	}
	return s, year
}

// dedupTrimLangSuffix drops trailing language tags ("Dune FR", "Dune - VF"),
// keeping at least one word. A hand-rolled scan: the equivalent end-anchored
// regexp backtracks at every separator and dominated the per-track cost.
func dedupTrimLangSuffix(s string) string {
	isSep := func(r rune) bool { return unicode.IsSpace(r) || r == '-' || r == '|' || r == ':' }
	for {
		s = strings.TrimRightFunc(s, unicode.IsSpace)
		i := strings.LastIndexFunc(s, isSep)
		if i < 0 {
			return s
		}
		word := s[i+1:]
		_, isCode := dedupLangCodes[word]
		_, isWord := dedupLangWords[strings.ToLower(word)]
		head := strings.TrimRightFunc(s[:i], isSep)
		if (!isCode && !isWord) || head == "" {
			return s
		}
		s = head
	}
}

func dedupPad(n, width int) string {
	s := strconv.Itoa(n)
	for len(s) < width {
		s = "0" + s
	}
	return s
}

// dedupDetectQuality returns the best quality score found in the text.
func dedupDetectQuality(text string) int {
	best := 0
	for _, m := range reDedupQuality.FindAllString(text, -1) {
		if q := dedupQualityRank[strings.ToLower(m)]; q > best {
			best = q
		}
	}
	return best
}

// dedupDetectLanguage returns the canonical language code of the first
// language tag found in the text, or "" when none is found.
func dedupDetectLanguage(text string) string {
	m := reDedupLang.FindString(dedupAccentFold(text))
	if m == "" {
		return ""
	}
	if code, ok := dedupLangCodes[m]; ok {
		return code
	}
	return dedupLangWords[strings.ToLower(m)]
}

// dedupScore ranks a track among its duplicates. Order of preference:
//  1. Video quality (4K > 1080p > 720p > ...)
//  2. Preferred language (configured list, in order)
//  3. Rich metadata (logo, EPG id, group-title)
//
// Ties are broken by the caller on the original playlist order.
func dedupScore(quality int, language string, hasLogo, hasEPG, hasGroup bool, preferredLangs []string) int32 {
	score := quality * 1000
	for i, p := range preferredLangs {
		if language != "" && p == language {
			score += (len(preferredLangs) - i) * 100
			break
		}
	}
	if hasLogo {
		score += 10
	}
	if hasEPG {
		score += 5
	}
	if hasGroup {
		score += 2
	}
	return int32(score)
}

func dedupHash(parts ...string) uint64 {
	h := fnv.New64a()
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
	}
	if v := h.Sum64(); v != 0 {
		return v
	}
	return 1 // 0 is reserved for "no key"
}

// newDedupRecord builds the record for one track from its display name, URI
// and the few EXTINF attributes scoring looks at.
//
// Title merging is restricted to "movie" and "live" entries without an
// episode marker: series and timeshift IDs are usually distinct episodes, and
// plain M3U entries carry no type, so they are only merged on an exact URL.
// The year is part of the title key so remakes (Dune 1984 / 2021) stay apart.
func newDedupRecord(name, uri, group string, hasLogo, hasEPG bool, preferredLangs []string) dedupRecord {
	var rec dedupRecord

	basePath := ""
	if u, err := url.Parse(uri); err == nil {
		basePath = extractBasePath(u.Path)
		if basePath != "" {
			last := path.Base(u.Path)
			rec.idKey = dedupHash(basePath, strings.TrimSuffix(last, path.Ext(last)))
		}
	}
	if rec.idKey == 0 && uri != "" {
		rec.idKey = dedupHash(uri)
	}

	if (basePath == "movie" || basePath == "live") && !reDedupEpisode.MatchString(name) {
		if key, year := dedupTitleFingerprint(name); key != "" {
			rec.titleKey = dedupHash(basePath, strconv.Itoa(year), key)
		}
	}

	// Providers often put the language/quality in group-title ("FR | 4K")
	// and the clean name in the title, so detection looks at both.
	detect := name
	if group != "" {
		detect = name + " " + group
	}
	rec.score = dedupScore(dedupDetectQuality(detect), dedupDetectLanguage(detect), hasLogo, hasEPG, group != "", preferredLangs)
	return rec
}

// dedupKeep decides which records survive: records sharing an identity or a
// title key form a group (transitively), and the best-scoring record of each
// group is kept, the earliest one on ties.
func dedupKeep(recs []dedupRecord) []bool {
	n := len(recs)
	parent := make([]int32, n)
	for i := range parent {
		parent[i] = int32(i)
	}
	find := func(x int32) int32 {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	union := func(index map[uint64]int32, key uint64, i int32) {
		if key == 0 {
			return
		}
		j, ok := index[key]
		if !ok {
			index[key] = i
			return
		}
		if ri, rj := find(i), find(j); ri != rj {
			parent[ri] = rj
		}
	}

	byID := make(map[uint64]int32, n)
	byTitle := make(map[uint64]int32, n)
	for i := range recs {
		union(byID, recs[i].idKey, int32(i))
		union(byTitle, recs[i].titleKey, int32(i))
	}

	best := make([]int32, n)
	for i := range best {
		best[i] = -1
	}
	for i := range recs {
		r := find(int32(i))
		if b := best[r]; b == -1 || recs[i].score > recs[b].score {
			best[r] = int32(i)
		}
	}

	keep := make([]bool, n)
	for _, b := range best {
		if b >= 0 {
			keep[b] = true
		}
	}
	return keep
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

// deduplicateTracks removes redundant tracks from an in-memory playlist,
// keeping the best representative of each duplicate group in the provider's
// original order. Returns the survivors and the number of removed tracks.
//
// Duplicates are the same source listed twice (same Xtream stream type + ID,
// or the exact same URL), or same-title movie/live copies that differ only by
// quality or language tags. See newDedupRecord for what is never merged.
func deduplicateTracks(tracks []m3u.Track, preferredLangs []string) ([]m3u.Track, int) {
	if len(tracks) < 2 {
		return tracks, 0
	}
	recs := make([]dedupRecord, len(tracks))
	for i, tr := range tracks {
		recs[i] = newDedupRecord(tr.Name, tr.URI,
			dedupTagValue(tr.Tags, "group-title"),
			dedupTagValue(tr.Tags, "tvg-logo") != "",
			dedupTagValue(tr.Tags, "tvg-id") != "",
			preferredLangs)
	}
	keep := dedupKeep(recs)
	out := make([]m3u.Track, 0, len(tracks))
	for i, tr := range tracks {
		if keep[i] {
			out = append(out, tr)
		}
	}
	return out, len(tracks) - len(out)
}

// extinfName returns the display name of a raw #EXTINF line: the text after
// the first comma that follows the attributes, so commas inside attribute
// values or inside the title itself are handled.
func extinfName(line string) string {
	rest := line
	if q := strings.LastIndex(rest, `"`); q != -1 {
		rest = rest[q+1:]
	}
	if i := strings.Index(rest, ","); i != -1 {
		return strings.TrimSpace(rest[i+1:])
	}
	return ""
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
