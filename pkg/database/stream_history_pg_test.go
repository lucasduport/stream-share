// Integration test for UpdateStreamHistoryTitle. Needs a real Postgres and is
// skipped unless SS_TEST_DSN points at a throwaway database (see
// stream_names_pg_test.go). Note it TRUNCATEs stream_history.

package database

import "testing"

func storedHistoryTitle(t *testing.T, m *DBManager, id int64) string {
	t.Helper()
	var title string
	if err := m.db.QueryRow("SELECT COALESCE(stream_title, '') FROM stream_history WHERE id=$1", id).Scan(&title); err != nil {
		t.Fatalf("read title: %v", err)
	}
	return title
}

// A VOD view's history row is inserted by its first request, possibly with the
// raw stream id as title; UpdateStreamHistoryTitle must correct it in place
// (ported from x3n0n10/stream-share#46).
func TestUpdateStreamHistoryTitleCorrectsRow(t *testing.T) {
	m := testDB(t)
	if _, err := m.db.Exec("TRUNCATE stream_history"); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	id, err := m.AddStreamHistory("alice", "327914", "movie", "327914", "1.2.3.4", "TestAgent")
	if err != nil {
		t.Fatal(err)
	}
	if got := storedHistoryTitle(t, m, id); got != "327914" {
		t.Fatalf("expected fallback title 327914, got %q", got)
	}

	if err := m.UpdateStreamHistoryTitle(id, "The Real Movie Title"); err != nil {
		t.Fatal(err)
	}
	if got := storedHistoryTitle(t, m, id); got != "The Real Movie Title" {
		t.Fatalf("expected corrected title, got %q", got)
	}
}
