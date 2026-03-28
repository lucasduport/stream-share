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
    "fmt"
    "net/url"

    "github.com/gin-gonic/gin"
    "github.com/jamesnetherton/m3u"
    "github.com/lucasduport/stream-share/pkg/utils"
    xtreamapi "github.com/lucasduport/stream-share/pkg/xtream"
)

// xtreamGenerateM3u constructs an M3U playlist by calling Xtream categories
// and streams endpoints and rewriting URIs to this proxy.
func (c *Config) xtreamGenerateM3u(ctx *gin.Context, extension string) (*m3u.Playlist, error) {
    client, err := xtreamapi.New(c.XtreamUser.String(), c.XtreamPassword.String(), c.XtreamBaseURL, ctx.Request.UserAgent())
    if err != nil {
        return nil, utils.PrintErrorAndReturn(err)
    }

    utils.DebugLog("========== GENERATING M3U PLAYLIST ==========")
    utils.DebugLog("Requesting live categories...")

    // Use the robust Action method to get categories
    catResp, httpCode, contentType, err := client.Action(c.ProxyConfig, "get_live_categories", url.Values{})
    if err != nil {
        utils.DebugLog("Failed to get live categories: %v", err)
        return nil, utils.PrintErrorAndReturn(err)
    }

    utils.DebugLog("Live categories response - HTTP Status: %d, Content-Type: %s", httpCode, contentType)
    utils.DumpStructToLog("live_categories", catResp)

    // Type assert the response to the expected format
    catData, ok := catResp.([]interface{})
    if !ok {
        utils.DebugLog("Unexpected format for live categories: %T - %+v", catResp, catResp)
        return nil, utils.PrintErrorAndReturn(fmt.Errorf("unexpected format for live categories: %T", catResp))
    }

    utils.DebugLog("Found %d live categories", len(catData))

    // this is specific to xtream API,
    // prefix with "live" if there is an extension.
    var prefix string
    if extension != "" {
        extension = "." + extension
        prefix = "live/"
    }

    var playlist = new(m3u.Playlist)
    playlist.Tracks = make([]m3u.Track, 0)

    for i, categoryItem := range catData {
        categoryMap, ok := categoryItem.(map[string]interface{})
        if !ok {
            utils.DebugLog("WARNING: Category item #%d is not a map: %T - %+v", i, categoryItem, categoryItem)
            continue
        }

        categoryID := fmt.Sprintf("%v", categoryMap["category_id"])
        categoryName := fmt.Sprintf("%v", categoryMap["category_name"])
        utils.DebugLog("Processing category: %s (ID: %s)", categoryName, categoryID)

        // Use the robust Action method to get live streams for each category
        utils.DebugLog("Requesting streams for category %s...", categoryID)
        liveResp, httpCode, contentType, err := client.Action(c.ProxyConfig, "get_live_streams", url.Values{"category_id": {categoryID}})
        if err != nil {
            utils.DebugLog("Failed to get live streams for category %s: %v", categoryID, err)
            return nil, utils.PrintErrorAndReturn(err)
        }

        utils.DebugLog("Streams response - HTTP Status: %d, Content-Type: %s", httpCode, contentType)
        utils.DumpStructToLog(fmt.Sprintf("streams_cat_%s", categoryID), liveResp)

        liveData, ok := liveResp.([]interface{})
        if !ok {
            utils.DebugLog("WARNING: Unexpected format for streams in category '%s': %T", categoryName, liveResp)
            continue
        }

        utils.DebugLog("Found %d streams in category: %s", len(liveData), categoryName)

        for j, streamItem := range liveData {
            streamMap, ok := streamItem.(map[string]interface{})
            if !ok {
                utils.DebugLog("WARNING: Stream #%d in category '%s' is not a map: %T", j, categoryName, streamItem)
                continue
            }

            // Validate required fields
            streamName, hasName := streamMap["name"].(string)
            _, hasID := streamMap["stream_id"].(string)
            var streamID string

            if !hasName || !hasID {
                utils.DebugLog("WARNING: Stream missing required fields - Name: %v, ID: %v", streamMap["name"], streamMap["stream_id"])
                continue
            }

            track := m3u.Track{
                Name:   streamName,
                Length: -1,
                URI:    "",
                Tags:   nil,
            }

            //TODO: Add more tag if needed.
            if epgID, ok := streamMap["epg_channel_id"].(string); ok && epgID != "" {
                track.Tags = append(track.Tags, m3u.Tag{Name: "tvg-id", Value: epgID})
            }
            if name, ok := streamMap["name"].(string); ok && name != "" {
                track.Tags = append(track.Tags, m3u.Tag{Name: "tvg-name", Value: name})
            }
            if logo, ok := streamMap["stream_icon"].(string); ok && logo != "" {
                track.Tags = append(track.Tags, m3u.Tag{Name: "tvg-logo", Value: logo})
            }
            if categoryName != "" {
                track.Tags = append(track.Tags, m3u.Tag{Name: "group-title", Value: categoryName})
            }

            streamID = fmt.Sprintf("%v", streamMap["stream_id"])
            track.URI = fmt.Sprintf("%s/%s%s/%s/%s%s", c.XtreamBaseURL, prefix, c.XtreamUser, c.XtreamPassword, streamID, extension)

            utils.DebugLog("Added stream: %s (ID: %s)", track.Name, streamID)
            playlist.Tracks = append(playlist.Tracks, track)
        }
    }

    utils.DebugLog("Playlist generation complete: %d total tracks", len(playlist.Tracks))
    return playlist, nil
}
