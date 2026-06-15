package segmenter

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/erwin/remaimber/internal/db"
	"github.com/erwin/remaimber/internal/types"
)

// fakeLLM records calls and produces deterministic summaries: a segment summary
// is the comma-joined content texts it folded (so tests can assert exactly which
// messages went in), and the roll-up wraps the parts.
type fakeLLM struct {
	window      int
	amendCalls  int
	reduceCalls int
}

func (f *fakeLLM) WindowSize() int {
	if f.window > 0 {
		return f.window
	}
	return 40
}

func (f *fakeLLM) Amend(_ context.Context, prev string, w []types.Message) (string, error) {
	f.amendCalls++
	var texts []string
	for _, m := range w {
		texts = append(texts, m.ContentText)
	}
	cur := strings.Join(texts, ",")
	if prev != "" {
		return prev + "+" + cur, nil
	}
	return cur, nil
}

func (f *fakeLLM) ReduceSummaries(_ context.Context, _, _ string, parts []string) (string, error) {
	f.reduceCalls++
	return "ROLLUP[" + strings.Join(parts, "|") + "]", nil
}

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := db.OpenAt(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}

// seed inserts a session and a list of messages (in order). Each entry is
// uuid,parent,text and an optional json-flag fragment merged into content_json.
type m struct {
	uuid, parent, text, flag string
}

func seed(t *testing.T, database *sql.DB, sessionID string, msgs []m) {
	t.Helper()
	tx, _ := database.Begin()
	if err := db.UpsertSession(tx, &types.Session{SessionID: sessionID, ProjectKey: "-p"}); err != nil {
		t.Fatal(err)
	}
	for _, x := range msgs {
		json := `{"type":"assistant"` + x.flag + `}`
		if err := db.InsertMessage(tx, &types.Message{
			SessionID: sessionID, UUID: x.uuid, ParentUUID: x.parent,
			Type: "assistant", Role: "assistant", ContentText: x.text, ContentJSON: json,
		}); err != nil {
			t.Fatal(err)
		}
	}
	tx.Commit()
}

func segs(t *testing.T, database *sql.DB, sessionID string) []db.Segment {
	t.Helper()
	s, err := db.GetSegments(database, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestReconcileSingleSegment(t *testing.T) {
	database := testDB(t)
	seed(t, database, "s", []m{
		{"a", "", "asked-to-build-flag", ""},
		{"b", "a", "added-the-flag", ""},
		{"c", "b", "wrote-a-test", ""},
	})
	llm := &fakeLLM{}
	summary, _, err := Reconcile(context.Background(), llm, database, "s", "goal", 60)
	if err != nil {
		t.Fatal(err)
	}
	if summary != "asked-to-build-flag,added-the-flag,wrote-a-test" {
		t.Errorf("unexpected summary: %q", summary)
	}
	if llm.reduceCalls != 0 {
		t.Errorf("single segment should not roll up, reduceCalls=%d", llm.reduceCalls)
	}
	sg := segs(t, database, "s")
	if len(sg) != 1 || sg[0].Closed {
		t.Errorf("want 1 open segment, got %+v", sg)
	}
}

func TestReconcileClosedSegmentsCachedAcrossRuns(t *testing.T) {
	database := testDB(t)
	// 7 messages, cap 3 -> segments of 3,3,1 (two closed, one open).
	seed(t, database, "s", []m{
		{"a", "", "1", ""}, {"b", "a", "2", ""}, {"c", "b", "3", ""},
		{"d", "c", "4", ""}, {"e", "d", "5", ""}, {"f", "e", "6", ""},
		{"g", "f", "7", ""},
	})
	llm := &fakeLLM{}
	if _, _, err := Reconcile(context.Background(), llm, database, "s", "goal", 3); err != nil {
		t.Fatal(err)
	}
	if got := len(segs(t, database, "s")); got != 3 {
		t.Fatalf("want 3 segments, got %d", got)
	}
	first := llm.amendCalls

	// Re-run with no new messages: nothing should be re-folded.
	if _, _, err := Reconcile(context.Background(), llm, database, "s", "goal", 3); err != nil {
		t.Fatal(err)
	}
	if llm.amendCalls != first {
		t.Errorf("closed/unchanged segments were re-folded: amendCalls %d -> %d", first, llm.amendCalls)
	}
}

func TestReconcileGrowAmendsOpenSegment(t *testing.T) {
	database := testDB(t)
	seed(t, database, "s", []m{{"a", "", "1", ""}, {"b", "a", "2", ""}})
	llm := &fakeLLM{}
	Reconcile(context.Background(), llm, database, "s", "g", 60)
	calls := llm.amendCalls

	// Append a message; the open segment should be amended (folded incrementally),
	// not rebuilt: prior summary "+", and only the new message folded.
	seed(t, database, "s", []m{{"c", "b", "3", ""}})
	summary, _, _ := Reconcile(context.Background(), llm, database, "s", "g", 60)
	if llm.amendCalls != calls+1 {
		t.Errorf("growing the open segment should add exactly one amend, got +%d", llm.amendCalls-calls)
	}
	if summary != "1,2+3" {
		t.Errorf("expected incremental amend '1,2+3', got %q", summary)
	}
}

func TestReconcileCompactionCommitsAndRollsUp(t *testing.T) {
	database := testDB(t)
	seed(t, database, "s", []m{
		{"a", "", "pre1", ""},
		{"b", "a", "pre2", ""},
		{"comp", "", "DIGEST", `,"isCompactSummary":true`}, // boundary, excluded from content
		{"d", "comp", "post1", ""},
		{"e", "d", "post2", ""},
	})
	llm := &fakeLLM{}
	summary, _, err := Reconcile(context.Background(), llm, database, "s", "g", 60)
	if err != nil {
		t.Fatal(err)
	}
	sg := segs(t, database, "s")
	if len(sg) != 2 {
		t.Fatalf("want 2 segments split at compaction, got %d: %+v", len(sg), sg)
	}
	if !sg[0].Closed || sg[0].Reason != "compaction" {
		t.Errorf("seg0 should be closed by compaction: %+v", sg[0])
	}
	if sg[1].Closed {
		t.Errorf("seg1 should be open: %+v", sg[1])
	}
	if !strings.HasPrefix(summary, "ROLLUP[") {
		t.Errorf("multi-segment session should roll up, got %q", summary)
	}
	if !strings.Contains(summary, "pre1,pre2") || !strings.Contains(summary, "post1,post2") {
		t.Errorf("roll-up should contain both segments: %q", summary)
	}
}

// The regression test the extraction was for: a rewind that abandons content
// inside the open segment must NOT leave that content in the summary.
func TestReconcileRewindInvalidatesOpenSegment(t *testing.T) {
	database := testDB(t)
	// Linear a->b->c, summarized once.
	seed(t, database, "s", []m{
		{"a", "", "first", ""},
		{"b", "a", "second", ""},
		{"c", "b", "THIRD-abandoned", ""},
	})
	llm := &fakeLLM{}
	first, _, _ := Reconcile(context.Background(), llm, database, "s", "g", 60)
	if !strings.Contains(first, "THIRD-abandoned") {
		t.Fatalf("setup: first summary should contain c: %q", first)
	}

	// Rewind: branch d from b, abandoning c.
	seed(t, database, "s", []m{{"d", "b", "fourth", ""}})
	summary, _, err := Reconcile(context.Background(), llm, database, "s", "g", 60)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(summary, "THIRD-abandoned") {
		t.Errorf("rewound-away content must be dropped, got %q", summary)
	}
	if !strings.Contains(summary, "first") || !strings.Contains(summary, "second") || !strings.Contains(summary, "fourth") {
		t.Errorf("active content missing from summary: %q", summary)
	}
}

func TestReconcileEmpty(t *testing.T) {
	database := testDB(t)
	seed(t, database, "s", nil)
	summary, _, err := Reconcile(context.Background(), &fakeLLM{}, database, "s", "g", 60)
	if err != nil {
		t.Fatal(err)
	}
	if summary != "" {
		t.Errorf("empty session should yield empty summary, got %q", summary)
	}
}
