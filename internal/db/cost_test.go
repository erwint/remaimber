package db

import (
	"database/sql"
	"testing"

	"github.com/erwin/remaimber/internal/types"
)

func seedCosts(t *testing.T) *sql.DB {
	t.Helper()
	database := testDB(t)
	insertSession(t, database, &types.Session{SessionID: "a", ProjectKey: "-infra"})
	insertSession(t, database, &types.Session{SessionID: "b", ProjectKey: "-web"})

	// UpsertSegment stamps updated_at with now, so set the dates directly.
	rows := []struct {
		sid  string
		seq  int
		usd  float64
		call int
		day  string
	}{
		{"a", 0, 1.00, 10, "2026-08-10"},
		{"a", 1, 2.00, 20, "2026-08-11"},
		{"b", 0, 0.50, 5, "2026-08-11"},
		{"b", 1, 0.00, 0, "2026-08-12"}, // summarized before cost tracking
	}
	for _, r := range rows {
		seg := Segment{SessionID: r.sid, Seq: r.seq, StartID: 1, EndID: 2,
			Summary: "did work", MsgCount: 2, CostUSD: r.usd, LLMCalls: r.call}
		if err := UpsertSegment(database, &seg); err != nil {
			t.Fatal(err)
		}
		if _, err := database.Exec(`UPDATE session_segments SET updated_at=? WHERE session_id=? AND seq=?`,
			r.day+"T12:00:00Z", r.sid, r.seq); err != nil {
			t.Fatal(err)
		}
	}
	return database
}

func TestGetCostTotals(t *testing.T) {
	database := seedCosts(t)

	got, err := GetCostTotals(database, "", "")
	if err != nil {
		t.Fatal(err)
	}
	// The unpriced segment must not dilute the rate: it could never have a price.
	if got.USD != 3.50 || got.Calls != 35 || got.Segments != 3 {
		t.Errorf("totals = $%.2f/%d calls/%d segs, want $3.50/35/3", got.USD, got.Calls, got.Segments)
	}
	if got.FirstDay != "2026-08-10" || got.LastDay != "2026-08-11" || got.DaysSpan != 2 {
		t.Errorf("span = %s..%s (%d days), want 08-10..08-11 (2)", got.FirstDay, got.LastDay, got.DaysSpan)
	}
	if got.PerDay != 1.75 {
		t.Errorf("per-day = %.4f, want 1.75", got.PerDay)
	}
	if got.PerCall != 0.10 {
		t.Errorf("per-call = %.4f, want 0.10", got.PerCall)
	}

	// A window restricts the total and the span it is averaged over.
	got, err = GetCostTotals(database, "2026-08-11", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.USD != 2.50 || got.DaysSpan != 1 {
		t.Errorf("windowed = $%.2f over %d days, want $2.50 over 1", got.USD, got.DaysSpan)
	}
}

func TestGetCostTotalsEmpty(t *testing.T) {
	database := testDB(t)
	got, err := GetCostTotals(database, "", "")
	if err != nil {
		t.Fatalf("totals over an archive with no cost failed: %v", err)
	}
	if got.USD != 0 || got.Segments != 0 || got.DaysSpan != 1 {
		t.Errorf("empty totals = %+v", got)
	}
}

func TestGetCostBreakdown(t *testing.T) {
	database := seedCosts(t)

	// Days read as a series, so they stay chronological rather than sorted by size.
	days, err := GetCostBreakdown(database, CostByDay, "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 2 || days[0].Key != "2026-08-10" || days[1].Key != "2026-08-11" {
		t.Fatalf("days = %+v, want chronological 08-10 then 08-11", days)
	}
	if days[1].USD != 2.50 {
		t.Errorf("08-11 = $%.2f, want $2.50 (both sessions that day)", days[1].USD)
	}

	// Sessions and projects rank by spend.
	sess, err := GetCostBreakdown(database, CostBySession, "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(sess) != 2 || sess[0].Key != "a" || sess[0].USD != 3.00 {
		t.Errorf("sessions = %+v, want session a first at $3.00", sess)
	}
	if sess[0].Label != "-infra" {
		t.Errorf("session label = %q, want the project key", sess[0].Label)
	}

	proj, err := GetCostBreakdown(database, CostByProject, "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(proj) != 2 || proj[0].Key != "-infra" {
		t.Errorf("projects = %+v, want -infra first", proj)
	}

	if _, err := GetCostBreakdown(database, CostDimension("wallet"), "", "", 0); err == nil {
		t.Error("an unknown breakdown should be rejected, not silently grouped")
	}

	if rows, _ := GetCostBreakdown(database, CostBySession, "", "", 1); len(rows) != 1 {
		t.Errorf("limit ignored: got %d rows", len(rows))
	}
}

// The total understates the work behind it while older summaries carry no price;
// saying how many is what keeps it from reading as the full picture.
func TestUnpricedSegments(t *testing.T) {
	database := seedCosts(t)
	n, err := UnpricedSegments(database)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("unpriced = %d, want 1", n)
	}
}
