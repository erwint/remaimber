package db

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/erwin/remaimber/internal/types"
)

func seedSummaries(t *testing.T) *sql.DB {
	t.Helper()
	database := testDB(t)
	insertSession(t, database, &types.Session{SessionID: "s1", ProjectKey: "-infra"})
	insertSession(t, database, &types.Session{SessionID: "s2", ProjectKey: "-web"})

	for _, s := range []Segment{
		{SessionID: "s1", Seq: 0, StartID: 1, EndID: 9, MsgCount: 9,
			Summary: "Deployed a postfix mail relay on the NAS so the printer can send through the upstream server."},
		{SessionID: "s1", Seq: 1, StartID: 10, EndID: 19, MsgCount: 10,
			Summary: "Rotated database credentials and purged attacker accounts."},
		{SessionID: "s2", Seq: 0, StartID: 20, EndID: 29, MsgCount: 10,
			Summary: "Reworked the login form validation and cookie handling."},
	} {
		seg := s
		if err := UpsertSegment(database, &seg); err != nil {
			t.Fatal(err)
		}
		if err := IndexSegmentSummary(database, seg.SessionID, seg.Seq, seg.Summary); err != nil {
			t.Fatal(err)
		}
	}
	return database
}

// A summary is a few sentences, so AND-ing every word of a remembered phrase
// matches nothing. Terms are OR-ed and ranked instead.
func TestSearchSummariesMatchesRememberedPhrasing(t *testing.T) {
	database := seedSummaries(t)

	hits, err := SearchSummaries(database, "smtp relay on the nas", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("a remembered phrase matched nothing; filler words must not be required")
	}
	if hits[0].SessionID != "s1" || hits[0].Seq != 0 {
		t.Errorf("best hit = %s seg %d, want s1 seg 0", hits[0].SessionID, hits[0].Seq)
	}
	if hits[0].ProjectKey != "-infra" {
		t.Errorf("project not joined: %q", hits[0].ProjectKey)
	}

	if hits, _ := SearchSummaries(database, "kubernetes ingress", 10); len(hits) != 0 {
		t.Errorf("unrelated topic returned %d hits", len(hits))
	}
}

// Re-summarizing rewrites a summary; the index must follow rather than
// accumulate both versions.
func TestIndexSegmentSummaryReplaces(t *testing.T) {
	database := seedSummaries(t)

	if err := IndexSegmentSummary(database, "s1", 0, "Now about DNS resolution instead."); err != nil {
		t.Fatal(err)
	}
	if hits, _ := SearchSummaries(database, "postfix relay", 10); len(hits) != 0 {
		t.Errorf("stale summary still indexed: %+v", hits)
	}
	hits, _ := SearchSummaries(database, "dns resolution", 10)
	if len(hits) != 1 || hits[0].Seq != 0 {
		t.Errorf("replacement not indexed: %+v", hits)
	}

	var n int
	database.QueryRow(`SELECT COUNT(*) FROM segments_fts_map WHERE session_id='s1' AND seq=0`).Scan(&n)
	if n != 1 {
		t.Errorf("map rows for one segment = %d, want 1", n)
	}
}

func TestReindexAndCoverage(t *testing.T) {
	database := seedSummaries(t)

	// A summary written without indexing (e.g. by an older version) is invisible
	// until reindexed — which is exactly what doctor reports on.
	seg := Segment{SessionID: "s2", Seq: 1, StartID: 30, EndID: 39, MsgCount: 10,
		Summary: "Migrated the build to a pinned toolchain."}
	if err := UpsertSegment(database, &seg); err != nil {
		t.Fatal(err)
	}
	if hits, _ := SearchSummaries(database, "pinned toolchain", 10); len(hits) != 0 {
		t.Fatal("unindexed summary should not be searchable yet")
	}

	c, err := GetSummaryCoverage(database)
	if err != nil {
		t.Fatal(err)
	}
	if c.SegmentsWithSum != 4 || c.IndexedSummaries != 3 {
		t.Errorf("coverage = %d summarized / %d indexed, want 4/3", c.SegmentsWithSum, c.IndexedSummaries)
	}

	n, err := ReindexSummaries(database)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("reindexed %d, want 4", n)
	}
	if hits, _ := SearchSummaries(database, "pinned toolchain", 10); len(hits) != 1 {
		t.Error("reindex did not make the summary searchable")
	}
	if c, _ := GetSummaryCoverage(database); c.IndexedSummaries != 4 {
		t.Errorf("indexed count after reindex = %d, want 4", c.IndexedSummaries)
	}
}

// Cost is recorded per segment so spend attributes to the work that caused it.
func TestSegmentCostRoundTrips(t *testing.T) {
	database := testDB(t)
	insertSession(t, database, &types.Session{SessionID: "s", ProjectKey: "-p"})
	seg := Segment{SessionID: "s", Seq: 0, StartID: 1, EndID: 9, MsgCount: 9,
		Summary: "did a thing", CostUSD: 0.0123, LLMCalls: 3}
	if err := UpsertSegment(database, &seg); err != nil {
		t.Fatal(err)
	}
	got, err := GetSegments(database, "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].CostUSD != 0.0123 || got[0].LLMCalls != 3 {
		t.Errorf("cost round-trip = %+v, want 0.0123/3", got)
	}
	c, _ := GetSummaryCoverage(database)
	if c.TotalCostUSD != 0.0123 || c.TotalLLMCalls != 3 {
		t.Errorf("coverage totals = $%v over %d calls", c.TotalCostUSD, c.TotalLLMCalls)
	}
}

// Most sessions without a summary are slash-command invocations a few messages
// long. Counting those as a backlog is a false alarm that sends someone off to
// spend model calls describing nothing.
func TestCoverageSeparatesBacklogFromTrivia(t *testing.T) {
	database := testDB(t)
	insertSession(t, database, &types.Session{SessionID: "real", ProjectKey: "-p"})
	insertSession(t, database, &types.Session{SessionID: "slash", ProjectKey: "-p"})
	insertSession(t, database, &types.Session{SessionID: "empty", ProjectKey: "-p"})

	tx, _ := database.Begin()
	for i := 0; i < 8; i++ {
		InsertMessage(tx, &types.Message{SessionID: "real", UUID: fmt.Sprintf("r%d", i),
			Type: "user", Role: "user", ContentText: "substantive work here", ContentJSON: `{"type":"user"}`})
	}
	for i := 0; i < 2; i++ {
		InsertMessage(tx, &types.Message{SessionID: "slash", UUID: fmt.Sprintf("s%d", i),
			Type: "user", Role: "user", ContentText: "/model", ContentJSON: `{"type":"user"}`})
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	c, err := GetSummaryCoverage(database)
	if err != nil {
		t.Fatal(err)
	}
	if c.Backlog != 1 {
		t.Errorf("backlog = %d, want 1 (only the session with real content)", c.Backlog)
	}
	if c.TooSmall != 2 {
		t.Errorf("trivial = %d, want 2 (the slash command and the empty session)", c.TooSmall)
	}
}

// An archive with nothing unsummarized must not error: SUM over no rows is NULL.
func TestCoverageWithNoUnsummarizedSessions(t *testing.T) {
	database := testDB(t)
	insertSession(t, database, &types.Session{SessionID: "s", ProjectKey: "-p", Summary: "done"})
	if _, err := database.Exec(`UPDATE sessions SET summary='done' WHERE session_id='s'`); err != nil {
		t.Fatal(err)
	}
	c, err := GetSummaryCoverage(database)
	if err != nil {
		t.Fatalf("coverage over a fully summarized archive failed: %v", err)
	}
	if c.Backlog != 0 || c.TooSmall != 0 {
		t.Errorf("got backlog=%d trivial=%d, want 0/0", c.Backlog, c.TooSmall)
	}
}
