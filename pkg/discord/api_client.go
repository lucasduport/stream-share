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
 
package discord

import (
    "bytes"
    "encoding/json"
    "fmt"
    "net/http"
)

// makeAPIRequest centralizes internal API calls with auth headers and JSON handling.
func (b *Bot) makeAPIRequest(method, endpoint string, body interface{}) (bool, interface{}, error) {
    url := b.apiURL + "/api/internal" + endpoint

    var reqBody []byte
    var err error
    if body != nil {
        reqBody, err = json.Marshal(body)
        if err != nil {
            return false, nil, err
        }
    }

    req, err := http.NewRequest(method, url, bytes.NewBuffer(reqBody))
    if err != nil {
        return false, nil, err
    }
    req.Header.Set("Content-Type", "application/json")
    req.Header.Set("X-API-Key", b.apiKey)

    resp, err := b.client.Do(req)
    if err != nil {
        return false, nil, err
    }
    defer resp.Body.Close()

    var apiResp map[string]interface{}
    if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
        return false, nil, err
    }
    if ok, _ := apiResp["success"].(bool); !ok {
        return false, apiResp["data"], fmt.Errorf("%v", apiResp["error"])
    }
    return true, apiResp["data"], nil
}
