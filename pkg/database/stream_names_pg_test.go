// Integration tests for the stream_names write path. They need a real Postgres
// because the batching builds SQL by hand — the thing worth testing is exactly
// what a mock would paper over. Skipped unless SS_TEST_DSN points at a
// throwaway database:
//
//	SS_TEST_DSN="postgres://user:pass@localhost/sstest?sslmode=disable" go test ./pkg/database/
//
// Note these TRUNCATE stream_names, so never point them at a real deployment.

package database

import (
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

func testDB(t *testing.T) *DBManager {
	t.Helper()
	dsn := os.Getenv("SS_TEST_DSN")
	if dsn == "" {
		t.Skip("SS_TEST_DSN not set; skipping Postgres integration tests")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Skipf("no db: %v", err)
	}
	m := &DBManager{db: db}
	if err := m.initSchema(); err != nil {
		t.Fatalf("schema: %v", err)
	}
	if _, err := db.Exec("TRUNCATE stream_names"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return m
}

// TestUpsertStreamNamesBatchLarge is the regression test for the stall: a
// realistic channel count must complete quickly and in few round trips.
func TestUpsertStreamNamesBatchLarge(t *testing.T) {
	m := testDB(t)
	const n = 5000
	names := make(map[string]string, n)
	epg := make(map[string]string, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("%d", i)
		names[id] = fmt.Sprintf("Channel %d", i)
		if i%2 == 0 {
			epg[id] = fmt.Sprintf("chan%d.tv", i)
		}
	}

	start := time.Now()
	if err := m.UpsertStreamNames(names, epg, "api"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	elapsed := time.Since(start)
	t.Logf("upserted %d rows in %s", n, elapsed)
	if elapsed > 10*time.Second {
		t.Fatalf("too slow: %s", elapsed)
	}

	var count int
	if err := m.db.QueryRow("SELECT COUNT(*) FROM stream_names WHERE source='api'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != n {
		t.Fatalf("row count = %d, want %d", count, n)
	}

	// Spot-check content and the EPG column.
	var name, epgID string
	if err := m.db.QueryRow("SELECT name, epg_channel_id FROM stream_names WHERE stream_id='42' AND source='api'").Scan(&name, &epgID); err != nil {
		t.Fatal(err)
	}
	if name != "Channel 42" || epgID != "chan42.tv" {
		t.Fatalf("got (%q,%q)", name, epgID)
	}
	// Odd ids have no EPG id and must store empty, not NULL.
	if err := m.db.QueryRow("SELECT name, epg_channel_id FROM stream_names WHERE stream_id='43' AND source='api'").Scan(&name, &epgID); err != nil {
		t.Fatal(err)
	}
	if name != "Channel 43" || epgID != "" {
		t.Fatalf("got (%q,%q)", name, epgID)
	}
}

// TestUpsertStreamNamesConflictUpdates verifies re-running updates in place
// rather than erroring or duplicating.
func TestUpsertStreamNamesConflictUpdates(t *testing.T) {
	m := testDB(t)
	if err := m.UpsertStreamNames(map[string]string{"1": "Old"}, map[string]string{"1": "old.tv"}, "api"); err != nil {
		t.Fatal(err)
	}
	if err := m.UpsertStreamNames(map[string]string{"1": "New"}, map[string]string{"1": "new.tv"}, "api"); err != nil {
		t.Fatal(err)
	}

	var count int
	if err := m.db.QueryRow("SELECT COUNT(*) FROM stream_names").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("row count = %d, want 1 (upsert should replace)", count)
	}
	var name, epgID string
	if err := m.db.QueryRow("SELECT name, epg_channel_id FROM stream_names WHERE stream_id='1'").Scan(&name, &epgID); err != nil {
		t.Fatal(err)
	}
	if name != "New" || epgID != "new.tv" {
		t.Fatalf("got (%q,%q), want (New,new.tv)", name, epgID)
	}
}

// TestUpsertStreamNamesExactBatchBoundary exercises the chunking arithmetic.
func TestUpsertStreamNamesExactBatchBoundary(t *testing.T) {
	for _, n := range []int{1, upsertBatchRows - 1, upsertBatchRows, upsertBatchRows + 1, upsertBatchRows * 2} {
		m := testDB(t)
		names := make(map[string]string, n)
		for i := 0; i < n; i++ {
			names[fmt.Sprintf("%d", i)] = fmt.Sprintf("C%d", i)
		}
		if err := m.UpsertStreamNames(names, nil, "m3u"); err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		var count int
		if err := m.db.QueryRow("SELECT COUNT(*) FROM stream_names").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != n {
			t.Fatalf("n=%d: stored %d", n, count)
		}
	}
}

// TestLoadStreamNamesRoundTrip verifies the read path still matches the write.
func TestLoadStreamNamesRoundTrip(t *testing.T) {
	m := testDB(t)
	if err := m.UpsertStreamNames(map[string]string{"7": "Seven"}, map[string]string{"7": "seven.tv"}, "api"); err != nil {
		t.Fatal(err)
	}
	if err := m.UpsertStreamNames(map[string]string{"8": "Eight"}, nil, "m3u"); err != nil {
		t.Fatal(err)
	}

	bySource, epg, err := m.LoadStreamNames()
	if err != nil {
		t.Fatal(err)
	}
	if bySource["api"]["7"] != "Seven" {
		t.Fatalf("api index = %#v", bySource["api"])
	}
	if bySource["m3u"]["8"] != "Eight" {
		t.Fatalf("m3u index = %#v", bySource["m3u"])
	}
	if epg["7"] != "seven.tv" {
		t.Fatalf("epg index = %#v", epg)
	}
}
