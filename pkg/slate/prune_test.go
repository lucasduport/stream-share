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

package slate

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPruneCache(t *testing.T) {
	dir := t.TempDir()

	write := func(name string, age time.Duration) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		mt := time.Now().Add(-age)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
		return p
	}

	old := write("old.ts", 48*time.Hour)
	fresh := write("fresh.ts", 1*time.Hour)
	keep := write("keep.txt", 48*time.Hour) // non-.ts must be left alone

	removed, err := PruneCache(dir, 24*time.Hour)
	if err != nil {
		t.Fatalf("PruneCache: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("old.ts should have been pruned")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("fresh.ts should have been kept")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Error("non-.ts file should have been kept")
	}
}

func TestPruneCacheMissingDir(t *testing.T) {
	// A missing directory is not an error (nothing to prune yet).
	if n, err := PruneCache(filepath.Join(t.TempDir(), "does-not-exist"), time.Hour); err != nil || n != 0 {
		t.Errorf("PruneCache(missing) = (%d, %v), want (0, nil)", n, err)
	}
}
