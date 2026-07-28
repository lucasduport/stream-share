package database

import (
	"testing"
	"time"

	"github.com/lucasduport/stream-share/pkg/types"
)

func freshVODDB(t *testing.T) *DBManager {
	t.Helper()
	m := testDB(t)
	if _, err := m.db.Exec("TRUNCATE vod_cache"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return m
}

func progress(t *testing.T, m *DBManager, id string) (int64, int64) {
	t.Helper()
	var d, tot int64
	if err := m.db.QueryRow("SELECT downloaded_bytes, total_bytes FROM vod_cache WHERE stream_id=$1", id).Scan(&d, &tot); err != nil {
		t.Fatalf("read progress: %v", err)
	}
	return d, tot
}

// TestUpsertVODCacheDoesNotClobberProgress is the regression test: a proxied
// Range request re-runs the "not cached yet" upsert with zeroed counters while a
// download is in flight, and that must not wipe the downloader's progress.
func TestUpsertVODCacheDoesNotClobberProgress(t *testing.T) {
	m := freshVODDB(t)
	exp := time.Now().Add(24 * time.Hour)

	if err := m.UpsertVODCache(&types.VODCacheEntry{
		StreamID: "1", Type: "movie", FilePath: "/x.mp4", Status: "downloading", ExpiresAt: exp, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.UpdateVODProgress("1", 5_000_000, 90_000_000); err != nil {
		t.Fatal(err)
	}
	if d, tot := progress(t, m, "1"); d != 5_000_000 || tot != 90_000_000 {
		t.Fatalf("after progress update got (%d,%d)", d, tot)
	}

	// The offending call: same shape a Range request produces, counters zeroed.
	if err := m.UpsertVODCache(&types.VODCacheEntry{
		StreamID: "1", Type: "movie", FilePath: "/x.mp4", Status: "downloading", ExpiresAt: exp, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	d, tot := progress(t, m, "1")
	if d != 5_000_000 || tot != 90_000_000 {
		t.Fatalf("progress clobbered by a zero-valued upsert: got (%d,%d), want (5000000,90000000)", d, tot)
	}
}

// TestUpsertVODCacheStillAppliesRealProgress guards the other direction: the
// guard must not block a genuine update.
func TestUpsertVODCacheStillAppliesRealProgress(t *testing.T) {
	m := freshVODDB(t)
	exp := time.Now().Add(24 * time.Hour)
	if err := m.UpsertVODCache(&types.VODCacheEntry{StreamID: "2", Status: "downloading", FilePath: "/y.mp4", ExpiresAt: exp}); err != nil {
		t.Fatal(err)
	}
	if err := m.UpsertVODCache(&types.VODCacheEntry{
		StreamID: "2", Status: "ready", FilePath: "/y.mp4", DownloadedBytes: 42, TotalBytes: 99, ExpiresAt: exp,
	}); err != nil {
		t.Fatal(err)
	}
	if d, tot := progress(t, m, "2"); d != 42 || tot != 99 {
		t.Fatalf("real progress not applied: got (%d,%d), want (42,99)", d, tot)
	}
}

// TestUpdateVODProgressKeepsTotalWhenUnknown: the downloader passes total=0 when
// the upstream sent no Content-Length; that must not erase a known total.
func TestUpdateVODProgressKeepsTotalWhenUnknown(t *testing.T) {
	m := freshVODDB(t)
	exp := time.Now().Add(24 * time.Hour)
	if err := m.UpsertVODCache(&types.VODCacheEntry{StreamID: "3", Status: "downloading", FilePath: "/z.mp4", TotalBytes: 500, ExpiresAt: exp}); err != nil {
		t.Fatal(err)
	}
	if err := m.UpdateVODProgress("3", 100, 0); err != nil {
		t.Fatal(err)
	}
	if d, tot := progress(t, m, "3"); d != 100 || tot != 500 {
		t.Fatalf("got (%d,%d), want (100,500)", d, tot)
	}
}
