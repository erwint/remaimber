package db

import (
	"database/sql"
	"testing"
)

func seed(t *testing.T, database *sql.DB, id, ended string, tool, plain int) {
	t.Helper()
	if _, err := database.Exec(
		`INSERT INTO sessions (session_id, project_key, project_path, ended_at, message_count)
		 VALUES (?, '-p', '/p', ?, ?)`, id, ended, tool+plain); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < tool+plain; i++ {
		isTool := 0
		role := "assistant"
		if i < tool {
			isTool, role = 1, "user"
		}
		if _, err := database.Exec(
			`INSERT INTO messages (session_id, type, role, content_text, content_json, is_tool_result, is_sidechain, is_compact_summary)
			 VALUES (?, ?, ?, 'text here', '{"x":1}', ?, 0, 0)`, id, role, role, isTool); err != nil {
			t.Fatal(err)
		}
	}
}

func count(t *testing.T, database *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := database.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Tool output is the bulk of an agentic transcript and is already excluded from
// search, so the default mode has to leave everything else — including the
// conversation a summary was built from — untouched.
func TestPruneToolOutputKeepsTheConversation(t *testing.T) {
	database := testDB(t)
	seed(t, database, "old", "2020-01-01T00:00:00Z", 3, 2)
	seed(t, database, "new", "2999-01-01T00:00:00Z", 3, 2)

	candidates, err := PruneCandidates(database, "2021-01-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].SessionID != "old" {
		t.Fatalf("candidates = %+v, want only the old session", candidates)
	}

	stats, err := Prune(database, PruneToolOutput, []string{"old"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Messages != 3 {
		t.Errorf("pruned %d messages, want the 3 tool results", stats.Messages)
	}
	if n := count(t, database, `SELECT COUNT(*) FROM messages WHERE session_id='old'`); n != 2 {
		t.Errorf("%d messages left in the pruned session, want the 2 conversation turns", n)
	}
	if n := count(t, database, `SELECT COUNT(*) FROM messages WHERE session_id='new'`); n != 5 {
		t.Errorf("a session that is not old enough lost %d messages", 5-n)
	}
	if n := count(t, database, `SELECT COUNT(*) FROM sessions WHERE session_id='old'`); n != 1 {
		t.Error("tool-output pruning must keep the session")
	}
}

// The search index is a separate table kept in step by triggers; a prune that
// left it behind would return hits for messages that no longer exist.
func TestPruneKeepsTheSearchIndexInStep(t *testing.T) {
	database := testDB(t)
	seed(t, database, "old", "2020-01-01T00:00:00Z", 2, 2)

	before := count(t, database, `SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'text'`)
	if before != 4 {
		t.Fatalf("index holds %d rows before pruning, want 4", before)
	}
	if _, err := Prune(database, PruneMessages, []string{"old"}, false); err != nil {
		t.Fatal(err)
	}
	if after := count(t, database, `SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'text'`); after != 0 {
		t.Errorf("index still holds %d rows for deleted messages", after)
	}
}

// Removing a session deletes the row that records the import, so without a
// tombstone the next sweep reads the whole transcript back in — "forget this"
// would last until the next hook fired.
func TestPruneTombstonesSoNothingComesBack(t *testing.T) {
	database := testDB(t)
	seed(t, database, "old", "2020-01-01T00:00:00Z", 1, 1)

	if _, err := Prune(database, PruneSessions, []string{"old"}, false); err != nil {
		t.Fatal(err)
	}
	if n := count(t, database, `SELECT COUNT(*) FROM sessions WHERE session_id='old'`); n != 0 {
		t.Error("the session survived a sessions-mode prune")
	}
	if !IsPruned(database, "old") {
		t.Error("a removed session must be tombstoned")
	}
	if n, err := CountPruned(database); err != nil || n != 1 {
		t.Errorf("CountPruned = %d, %v; want 1", n, err)
	}

	// And the tombstone is liftable, or a mistaken prune would be permanent.
	if err := Forget(database, "old"); err != nil {
		t.Fatal(err)
	}
	if IsPruned(database, "old") {
		t.Error("forget must lift the tombstone")
	}
}

// A dry run is what someone checks before letting go of anything.
func TestPruneDryRunMeasuresWithoutDeleting(t *testing.T) {
	database := testDB(t)
	seed(t, database, "old", "2020-01-01T00:00:00Z", 4, 1)

	stats, err := Prune(database, PruneToolOutput, []string{"old"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Messages != 4 || stats.Bytes == 0 {
		t.Errorf("dry run reported %+v, want the 4 tool results and their size", stats)
	}
	if n := count(t, database, `SELECT COUNT(*) FROM messages WHERE session_id='old'`); n != 5 {
		t.Errorf("a dry run deleted %d messages", 5-n)
	}
	if IsPruned(database, "old") {
		t.Error("a dry run must not tombstone")
	}
}
