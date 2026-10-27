// Postgres integration tests for live share links; skipped unless SS_TEST_DSN
// points at a throwaway database (see stream_names_pg_test.go). Note it
// TRUNCATEs live_shares.

package database

import (
	"testing"
	"time"

	"github.com/lucasduport/stream-share/pkg/types"
)

func freshShareDB(t *testing.T) *DBManager {
	t.Helper()
	m := testDB(t)
	if _, err := m.db.Exec("TRUNCATE live_shares"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return m
}

func TestLiveSharesCreateListRevokeDelete(t *testing.T) {
	m := freshShareDB(t)
	now := time.Now().Truncate(time.Second)
	active := &types.LiveShare{Token: "tok-active", StreamID: "11", Title: "Channel", CreatedBy: "alice",
		StartsAt: now.Add(-time.Hour), EndsAt: now.Add(time.Hour), CreatedAt: now}
	old := &types.LiveShare{Token: "tok-old", StreamID: "12", Title: "Old", CreatedBy: "bob",
		StartsAt: now.Add(-72 * time.Hour), EndsAt: now.Add(-48 * time.Hour), CreatedAt: now.Add(-72 * time.Hour)}
	for _, s := range []*types.LiveShare{active, old} {
		if err := m.CreateLiveShare(s); err != nil {
			t.Fatal(err)
		}
	}

	got, err := m.ListLiveShares(now.Add(-24 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Token != "tok-active" || got[0].CreatedBy != "alice" || got[0].RevokedAt != nil {
		t.Fatalf("list = %+v", got)
	}
	if !got[0].StartsAt.Equal(active.StartsAt) || !got[0].EndsAt.Equal(active.EndsAt) {
		t.Fatalf("window round-trip: got %v–%v, want %v–%v", got[0].StartsAt, got[0].EndsAt, active.StartsAt, active.EndsAt)
	}
	if got[0].State(now) != types.ShareActive {
		t.Fatalf("state = %s", got[0].State(now))
	}

	if err := m.RevokeLiveShare("tok-active", now); err != nil {
		t.Fatal(err)
	}
	if err := m.RevokeLiveShare("tok-active", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, _ = m.ListLiveShares(now.Add(-24 * time.Hour))
	if len(got) != 1 || got[0].RevokedAt == nil || !got[0].RevokedAt.Equal(now) {
		t.Fatalf("revoked = %+v", got[0])
	}

	n, err := m.DeleteLiveSharesEndedBefore(now.Add(-24 * time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("delete = %d, %v", n, err)
	}
}
