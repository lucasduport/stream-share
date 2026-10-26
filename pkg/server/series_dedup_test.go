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
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/lucasduport/stream-share/pkg/config"
	"github.com/lucasduport/stream-share/pkg/utils"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// epJSON renders one get_series_info episode entry. info is a JSON object or
// "" for none.
func epJSON(id string, num interface{}, title, ext, info string) string {
	numJSON, _ := json.Marshal(num)
	s := fmt.Sprintf(`{"id":%q,"episode_num":%s,"season":1,"title":%q,"container_extension":%q`, id, numJSON, title, ext)
	if info != "" {
		s += `,"info":` + info
	}
	return s + "}"
}

func infoJSON(duration int, vcodec, pixFmt string, height int, acodec string) string {
	return fmt.Sprintf(`{"duration_secs":%d,"bitrate":3000,"video":{"codec_name":%q,"profile":"100","pix_fmt":%q,"height":%d},"audio":{"codec_name":%q}}`,
		duration, vcodec, pixFmt, height, acodec)
}

var (
	h264Info = infoJSON(1309, "h264", "yuv420p", 1080, "aac")
	hevcInfo = infoJSON(1309, "hevc", "yuv420p10le", 1080, "aac")
)

func seriesInfoAnswer(episodes ...string) string {
	return `{"info":{"name":"Some Show"},"seasons":[{"season_number":1,"episode_count":"` +
		fmt.Sprint(len(episodes)) + `"}],"episodes":{"1":[` + strings.Join(episodes, ",") + `]}}`
}

func episodeIDs(t *testing.T, info map[string]interface{}) []string {
	t.Helper()
	eps, _ := info["episodes"].(map[string]interface{})["1"].([]interface{})
	ids := make([]string, 0, len(eps))
	for _, e := range eps {
		ids = append(ids, stringField(e.(map[string]interface{})["id"]))
	}
	return ids
}

// Identical copies of an episode collapse to the lowest id, in place of the
// first copy, and the season count follows.
func TestDedupSeriesEpisodesMergesIdenticalCopies(t *testing.T) {
	in := decodeLikeXtream(t, seriesInfoAnswer(
		epJSON("200", 1, "Some Show - S01E01", "mp4", h264Info),
		epJSON("150", 2, "Some Show - S01E02", "mp4", h264Info),
		epJSON("120", 2, "some show -  S01E02", "mp4", infoJSON(1310, "h264", "yuv420p", 1080, "aac")), // same file, rounding + case
		epJSON("300", 3, "Some Show - S01E03", "mp4", h264Info),
	))
	out, removed := dedupSeriesEpisodes(in, false, nil)
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if got, want := episodeIDs(t, out), []string{"200", "120", "300"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ids = %v, want %v (lowest id kept, at the first copy's position)", got, want)
	}
	season := out["seasons"].([]interface{})[0].(map[string]interface{})
	if season["episode_count"] != "3" {
		t.Fatalf("episode_count = %#v, want \"3\" (string kept)", season["episode_count"])
	}
	// The input answer is left untouched.
	if got := episodeIDs(t, in); len(got) != 4 {
		t.Fatalf("input was modified: %v", got)
	}
	if in["seasons"].([]interface{})[0].(map[string]interface{})["episode_count"] != "4" {
		t.Fatal("input season count was modified")
	}
}

// Different encodes of one episode are all kept; with tagging they get told
// apart by what differs.
func TestDedupSeriesEpisodesKeepsDifferentEncodes(t *testing.T) {
	answer := seriesInfoAnswer(
		epJSON("10", 1, "Some Show - S01E01", "mp4", h264Info),
		epJSON("11", 1, "Some Show - S01E01", "mkv", hevcInfo),
	)
	out, removed := dedupSeriesEpisodes(decodeLikeXtream(t, answer), false, nil)
	if removed != 0 || len(episodeIDs(t, out)) != 2 {
		t.Fatalf("different encodes merged: removed=%d ids=%v", removed, episodeIDs(t, out))
	}

	out, _ = dedupSeriesEpisodes(decodeLikeXtream(t, answer), true, nil)
	eps := out["episodes"].(map[string]interface{})["1"].([]interface{})
	got := []string{
		stringField(eps[0].(map[string]interface{})["title"]),
		stringField(eps[1].(map[string]interface{})["title"]),
	}
	want := []string{"Some Show - S01E01 [H.264 / MP4]", "Some Show - S01E01 [HEVC 10-bit / MKV]"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("titles = %q, want %q", got, want)
	}
}

// Entries without comparable technical info are never merged, even with the
// same title.
func TestDedupSeriesEpisodesNeedsComparableInfo(t *testing.T) {
	for name, answer := range map[string]string{
		"no info":           seriesInfoAnswer(epJSON("1", 1, "S01E01", "mkv", ""), epJSON("2", 1, "S01E01", "mkv", "")),
		"no duration":       seriesInfoAnswer(epJSON("1", 1, "S01E01", "mkv", `{"video":{"codec_name":"h264"}}`), epJSON("2", 1, "S01E01", "mkv", `{"video":{"codec_name":"h264"}}`)),
		"one side unknown":  seriesInfoAnswer(epJSON("1", 1, "S01E01", "mp4", h264Info), epJSON("2", 1, "S01E01", "mp4", "")),
		"different title":   seriesInfoAnswer(epJSON("1", 1, "S01E01 Part 1", "mp4", h264Info), epJSON("2", 1, "S01E01 Part 2", "mp4", h264Info)),
		"duration too far":  seriesInfoAnswer(epJSON("1", 1, "S01E01", "mp4", h264Info), epJSON("2", 1, "S01E01", "mp4", infoJSON(1400, "h264", "yuv420p", 1080, "aac"))),
		"no episode number": seriesInfoAnswer(epJSON("1", 0, "Extra", "mp4", h264Info), epJSON("2", 0, "Extra", "mp4", h264Info)),
	} {
		t.Run(name, func(t *testing.T) {
			in := decodeLikeXtream(t, answer)
			out, removed := dedupSeriesEpisodes(in, true, nil)
			if removed != 0 || len(episodeIDs(t, out)) != 2 {
				t.Fatalf("merged: removed=%d ids=%v", removed, episodeIDs(t, out))
			}
		})
	}
}

// Episode numbers and durations may come as JSON numbers or strings.
func TestDedupSeriesEpisodesStringAndNumberFields(t *testing.T) {
	strInfo := `{"duration_secs":"1309","video":{"codec_name":"h264","profile":"100","pix_fmt":"yuv420p","height":"1080"},"audio":{"codec_name":"aac"}}`
	in := decodeLikeXtream(t, seriesInfoAnswer(
		epJSON("5", "2", "S01E02", "mp4", strInfo),
		epJSON("4", 2, "S01E02", "mp4", h264Info),
	))
	out, removed := dedupSeriesEpisodes(in, false, nil)
	if removed != 1 || !reflect.DeepEqual(episodeIDs(t, out), []string{"4"}) {
		t.Fatalf("removed=%d ids=%v, want one entry with id 4", removed, episodeIDs(t, out))
	}
}

// A season count given as a JSON number stays a number.
func TestAdjustSeasonCountsKeepsNumberType(t *testing.T) {
	in := decodeLikeXtream(t, `{"seasons":[{"season_number":1,"episode_count":3},{"season_number":2,"episode_count":5}],"episodes":{"1":[`+
		epJSON("1", 1, "S01E01", "mp4", h264Info)+","+epJSON("2", 1, "S01E01", "mp4", h264Info)+","+epJSON("3", 2, "S01E02", "mp4", h264Info)+`]}}`)
	out, _ := dedupSeriesEpisodes(in, false, nil)
	b, _ := json.Marshal(out["seasons"])
	if want := `[{"episode_count":2,"season_number":1},{"episode_count":5,"season_number":2}]`; string(b) != want {
		t.Fatalf("seasons = %s, want %s", b, want)
	}
}

func seriesInfoProvider(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

var dupAnswer = seriesInfoAnswer(
	epJSON("101", 1, "Some Show - S01E01", "mkv", hevcInfo),
	epJSON("102", 1, "Some Show - S01E01", "mkv", hevcInfo),
)

// Disabled, the player_api answer is exactly what it was before the option
// existed; enabled, players get one copy while title harvesting still sees
// every id.
func TestPlayerAPISeriesInfoDedup(t *testing.T) {
	srv := seriesInfoProvider(t, dupAnswer)

	off := playerAPITestConfig(srv.URL)
	got := callPlayerAPI(t, off, "action=get_series_info&series_id=7")
	wantBody, _ := json.Marshal(decodeLikeXtream(t, dupAnswer))
	if !bytes.Equal(got.Body.Bytes(), wantBody) {
		t.Fatalf("disabled answer changed:\n got %s\nwant %s", got.Body.Bytes(), wantBody)
	}

	resetVODNameState(t)
	on := playerAPITestConfig(srv.URL)
	on.SeriesEpisodeDedupEnabled = true
	w := callPlayerAPI(t, on, "action=get_series_info&series_id=7")
	var out map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if ids := episodeIDs(t, out); !reflect.DeepEqual(ids, []string{"101"}) {
		t.Fatalf("enabled ids = %v, want [101]", ids)
	}
	for _, id := range []string{"101", "102"} {
		if _, ok := vodNameCached(id); !ok {
			t.Errorf("title of dropped/kept copy %s not harvested", id)
		}
	}
}

// A copy already in the VOD cache is kept over a lower-id twin, so players are
// not sent to an uncached copy of a file that is already on disk.
func TestDedupPrefersCachedCopy(t *testing.T) {
	t.Setenv("CACHE_FOLDER", t.TempDir())
	ep := func(id string) map[string]interface{} {
		return map[string]interface{}{
			"id": id, "episode_num": "24", "season": "7", "title": "Some Show - S07E24", "container_extension": "mkv",
			"info": map[string]interface{}{"duration_secs": "1330", "video": map[string]interface{}{"codec_name": "hevc", "profile": "2", "pix_fmt": "yuv420p10le", "height": "1080"}, "audio": map[string]interface{}{"codec_name": "aac"}},
		}
	}
	answer := func() map[string]interface{} {
		return map[string]interface{}{"episodes": map[string]interface{}{"7": []interface{}{ep("100"), ep("101")}}}
	}
	c := &Config{ProxyConfig: &config.ProxyConfig{VODCacheEnabled: true, SeriesEpisodeDedupEnabled: true}}
	keptID := func(out interface{}) string {
		eps := out.(map[string]interface{})["episodes"].(map[string]interface{})["7"].([]interface{})
		if len(eps) != 1 {
			t.Fatalf("want 1 episode, got %d", len(eps))
		}
		return eps[0].(map[string]interface{})["id"].(string)
	}

	if got := keptID(c.seriesEpisodeDedup(answer())); got != "100" {
		t.Fatalf("nothing cached: kept %s, want lowest id 100", got)
	}

	dir := utils.VODCacheDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A download in progress (.part) does not count.
	if err := os.WriteFile(filepath.Join(dir, "101.mkv.part"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := keptID(c.seriesEpisodeDedup(answer())); got != "100" {
		t.Fatalf("only a .part on disk: kept %s, want 100", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "101.mkv"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := keptID(c.seriesEpisodeDedup(answer())); got != "101" {
		t.Fatalf("101 cached: kept %s, want 101", got)
	}
	c.VODCacheEnabled = false
	if got := keptID(c.seriesEpisodeDedup(answer())); got != "100" {
		t.Fatalf("cache disabled: kept %s, want 100", got)
	}
}
