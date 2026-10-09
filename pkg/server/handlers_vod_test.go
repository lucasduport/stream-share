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

import "testing"

// TestDefaultVODExt pins the per-type defaults used when no extension can be
// resolved synchronously.
func TestDefaultVODExt(t *testing.T) {
	if got := defaultVODExt("movie"); got != ".mp4" {
		t.Fatalf("movie default = %q, want .mp4", got)
	}
	if got := defaultVODExt("series"); got != ".mkv" {
		t.Fatalf("series default = %q, want .mkv", got)
	}
}

// TestVODFallbackTitle pins the placeholder-title precedence: series metadata
// first, then the caller-supplied title, then a constant.
func TestVODFallbackTitle(t *testing.T) {
	if got := vodFallbackTitle("series", "Ep title", "My Series", 2, 4); got != "My Series — S02E04" {
		t.Fatalf("series fallback = %q", got)
	}
	if got := vodFallbackTitle("movie", "  Some Movie  ", "", 0, 0); got != "Some Movie" {
		t.Fatalf("movie fallback = %q", got)
	}
	if got := vodFallbackTitle("movie", "", "", 0, 0); got != "Unknown title" {
		t.Fatalf("empty fallback = %q", got)
	}
	// A series without episode context falls through to the plain title.
	if got := vodFallbackTitle("series", "Ep title", "My Series", 0, 0); got != "Ep title" {
		t.Fatalf("series no-context fallback = %q", got)
	}
}
