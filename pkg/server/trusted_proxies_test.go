/*
 * stream-share is a project to efficiently share the use of an IPTV service.
 * Copyright (C) 2025  Lucas Duport
 * Copyright (C) 2026  x3n0n10
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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/config"
)

func clientIPFor(t *testing.T, proxy bool, remote, xff string) string {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	c := &Config{ProxyConfig: &config.ProxyConfig{ReverseProxyEnabled: proxy}}
	if err := c.applyTrustedProxies(r); err != nil {
		t.Fatal(err)
	}
	r.GET("/", func(ctx *gin.Context) { ctx.String(http.StatusOK, ctx.ClientIP()) })
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = remote
	req.Header.Set("X-Forwarded-For", xff)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Body.String()
}

func TestClientIPIgnoresForwardedForWithoutReverseProxy(t *testing.T) {
	if got := clientIPFor(t, false, "192.0.2.10:5000", "1.2.3.4"); got != "192.0.2.10" {
		t.Fatalf("got %q", got)
	}
}

func TestClientIPFromTrustedProxy(t *testing.T) {
	for _, tc := range []struct{ remote, xff, want string }{
		{"10.0.0.5:5000", "1.2.3.4", "1.2.3.4"},
		{"203.0.113.7:5000", "1.2.3.4", "203.0.113.7"},
		{"10.0.0.5:5000", "6.6.6.6, 1.2.3.4", "1.2.3.4"},
		{"[fd00::5]:5000", "2001:db8::1", "2001:db8::1"},
	} {
		if got := clientIPFor(t, true, tc.remote, tc.xff); got != tc.want {
			t.Errorf("%s xff=%s: got %q want %q", tc.remote, tc.xff, got, tc.want)
		}
	}
}
