package db

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/erwin/remaimber/internal/types"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	db, err := OpenAt(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestOpenAt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	db, err := OpenAt(path)
	if err != nil {
		t.Fatalf("OpenAt: %v", err)
	}
	defer db.Close()

	var mode string
	db.QueryRow("PRAGMA journal_mode").Scan(&mode)
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}

	for _, table := range []string{"sessions", "messages"} {
		var name string
		err := db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&name)
		if err != nil {
			t.Errorf("table %s not found: %v", table, err)
		}
	}

	var name string
	err = db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='messages_fts'").Scan(&name)
	if err != nil {
		t.Errorf("FTS5 table not found: %v", err)
	}
}

func TestOpenAt_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	db1, _ := OpenAt(path)
	db1.Close()
	db2, err := OpenAt(path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	db2.Close()
}

func TestDBPath(t *testing.T) {
	path, err := DBPath()
	if err != nil {
		t.Fatalf("DBPath: %v", err)
	}
	home, _ := os.UserHomeDir()
	expected := filepath.Join(home, ".claude", "remaimber", "remaimber.db")
	if path != expected {
		t.Errorf("DBPath = %q, want %q", path, expected)
	}
}

func TestOpenPath_CustomPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom.db")
	db, err := OpenPath(path)
	if err != nil {
		t.Fatalf("OpenPath: %v", err)
	}
	db.Close()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("custom db not created at %s", path)
	}
}

func TestOpenPath_Empty(t *testing.T) {
	// Should use default path
	db, err := OpenPath("")
	if err != nil {
		t.Fatalf("OpenPath empty: %v", err)
	}
	db.Close()
}

func TestUpsertSession(t *testing.T) {
	db := testDB(t)
	tx, _ := db.Begin()
	sess := &types.Session{
		SessionID:  "test-session-1",
		ProjectKey: "-test-project",
		ProjectPath: "/test/project",
		CustomTitle: "Test Title",
		FirstPrompt: "Hello world",
		GitBranch:  "main",
		CWD:        "/test",
		StartedAt:  "2026-01-01T00:00:00Z",
		EndedAt:    "2026-01-01T01:00:00Z",
		MessageCount: 5,
		FileMtime:  1234567890.0,
		FileSize:   1024,
		LastByteOffset: 512,
	}
	if err := UpsertSession(tx, sess); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	tx.Commit()

	mtime, size, offset, found, err := GetSessionMeta(db, "test-session-1")
	if err != nil {
		t.Fatalf("GetSessionMeta: %v", err)
	}
	if !found {
		t.Fatal("session not found")
	}
	if mtime != 1234567890.0 || size != 1024 || offset != 512 {
		t.Errorf("meta = (%v, %v, %v), want (1234567890, 1024, 512)", mtime, size, offset)
	}
}

func TestUpsertSession_Update(t *testing.T) {
	db := testDB(t)
	tx, _ := db.Begin()
	UpsertSession(tx, &types.Session{SessionID: "s1", ProjectKey: "-p1", CustomTitle: "Old"})
	tx.Commit()

	tx, _ = db.Begin()
	UpsertSession(tx, &types.Session{SessionID: "s1", ProjectKey: "-p1", CustomTitle: "New", MessageCount: 10})
	tx.Commit()

	sessions, _ := ListSessions(db, ListFilter{Limit: 10})
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if sessions[0].CustomTitle != "New" {
		t.Errorf("title = %q, want New", sessions[0].CustomTitle)
	}
}

func TestInsertMessage_UUIDDedup(t *testing.T) {
	db := testDB(t)
	tx, _ := db.Begin()
	UpsertSession(tx, &types.Session{SessionID: "s1", ProjectKey: "-p1"})
	tx.Commit()

	msg := &types.Message{SessionID: "s1", UUID: "uuid-1", Type: "user", ContentJSON: `{}`}
	tx, _ = db.Begin()
	InsertMessage(tx, msg)
	tx.Commit()

	tx, _ = db.Begin()
	InsertMessage(tx, msg) // dup
	tx.Commit()

	count, _ := GetMessageCount(db, "s1")
	if count != 1 {
		t.Errorf("count = %d, want 1", count)
	}
}

func TestInsertMessage_ContentHashDedup(t *testing.T) {
	db := testDB(t)
	tx, _ := db.Begin()
	UpsertSession(tx, &types.Session{SessionID: "s1", ProjectKey: "-p1"})
	msg := &types.Message{SessionID: "s1", Type: "custom-title", ContentJSON: `{}`, ContentHash: "abc"}
	InsertMessage(tx, msg)
	InsertMessage(tx, msg) // dup
	tx.Commit()

	count, _ := GetMessageCount(db, "s1")
	if count != 1 {
		t.Errorf("count = %d, want 1", count)
	}
}

func TestListSessions_WithDateFilters(t *testing.T) {
	db := testDB(t)
	tx, _ := db.Begin()
	UpsertSession(tx, &types.Session{SessionID: "s1", ProjectKey: "-p1",
		StartedAt: "2026-01-01T00:00:00Z", EndedAt: "2026-01-02T00:00:00Z"})
	UpsertSession(tx, &types.Session{SessionID: "s2", ProjectKey: "-p1",
		StartedAt: "2026-02-01T00:00:00Z", EndedAt: "2026-02-02T00:00:00Z"})
	UpsertSession(tx, &types.Session{SessionID: "s3", ProjectKey: "-p2",
		StartedAt: "2026-03-01T00:00:00Z", EndedAt: "2026-03-02T00:00:00Z"})
	tx.Commit()

	// All
	sessions, _ := ListSessions(db, ListFilter{Limit: 10})
	if len(sessions) != 3 {
		t.Errorf("all = %d, want 3", len(sessions))
	}

	// Since
	sessions, _ = ListSessions(db, ListFilter{Since: "2026-02-01T00:00:00Z", Limit: 10})
	if len(sessions) != 2 {
		t.Errorf("since = %d, want 2", len(sessions))
	}

	// Until
	sessions, _ = ListSessions(db, ListFilter{Until: "2026-01-31T00:00:00Z", Limit: 10})
	if len(sessions) != 1 {
		t.Errorf("until = %d, want 1", len(sessions))
	}

	// Project filter
	sessions, _ = ListSessions(db, ListFilter{Project: "p2", Limit: 10})
	if len(sessions) != 1 {
		t.Errorf("project = %d, want 1", len(sessions))
	}

	// Limit
	sessions, _ = ListSessions(db, ListFilter{Limit: 1})
	if len(sessions) != 1 {
		t.Errorf("limit = %d, want 1", len(sessions))
	}
}

func TestSearchMessages_WithFilters(t *testing.T) {
	db := testDB(t)
	tx, _ := db.Begin()
	UpsertSession(tx, &types.Session{SessionID: "s1", ProjectKey: "-project-alpha"})
	InsertMessage(tx, &types.Message{
		SessionID: "s1", UUID: "u1", Type: "user", Role: "user",
		ContentText: "How do I configure SQLite?",
		ContentJSON: `{}`, Timestamp: "2026-01-15T00:00:00Z",
	})
	InsertMessage(tx, &types.Message{
		SessionID: "s1", UUID: "u2", Type: "assistant", Role: "assistant",
		ContentText: "You can use SQLite with the fts5 module.",
		ContentJSON: `{}`, Timestamp: "2026-01-15T00:01:00Z",
	})
	tx.Commit()

	// Basic search
	results, err := SearchMessages(db, SearchFilter{Query: "SQLite", Limit: 10})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected results for 'SQLite'")
	}

	// Role filter
	results, _ = SearchMessages(db, SearchFilter{Query: "SQLite", Role: "user", Limit: 10})
	if len(results) != 1 {
		t.Errorf("role filter = %d, want 1", len(results))
	}

	// Date filters
	results, _ = SearchMessages(db, SearchFilter{Query: "SQLite", Since: "2026-02-01T00:00:00Z", Limit: 10})
	if len(results) != 0 {
		t.Errorf("since future = %d, want 0", len(results))
	}

	// Porter stemming
	results, _ = SearchMessages(db, SearchFilter{Query: "configuring", Limit: 10})
	if len(results) == 0 {
		t.Error("porter stemming should match 'configuring' -> 'configure'")
	}
}

func TestResolveSessionID(t *testing.T) {
	db := testDB(t)
	tx, _ := db.Begin()
	UpsertSession(tx, &types.Session{SessionID: "abc12345-6789-0000-1111-222233334444", ProjectKey: "-p1"})
	UpsertSession(tx, &types.Session{SessionID: "abc12345-6789-0000-1111-555566667777", ProjectKey: "-p1"})
	UpsertSession(tx, &types.Session{SessionID: "def99999-0000-1111-2222-333344445555", ProjectKey: "-p1"})
	tx.Commit()

	// Exact match
	id, err := ResolveSessionID(db, "def99999-0000-1111-2222-333344445555")
	if err != nil {
		t.Fatalf("exact: %v", err)
	}
	if id != "def99999-0000-1111-2222-333344445555" {
		t.Errorf("exact = %q", id)
	}

	// Unique prefix
	id, err = ResolveSessionID(db, "def9")
	if err != nil {
		t.Fatalf("prefix: %v", err)
	}
	if id != "def99999-0000-1111-2222-333344445555" {
		t.Errorf("prefix = %q", id)
	}

	// Ambiguous prefix
	_, err = ResolveSessionID(db, "abc1")
	if err == nil {
		t.Error("expected error for ambiguous prefix")
	}

	// Not found
	_, err = ResolveSessionID(db, "zzz")
	if err == nil {
		t.Error("expected error for not found")
	}
}

func TestDeleteSession(t *testing.T) {
	db := testDB(t)
	tx, _ := db.Begin()
	UpsertSession(tx, &types.Session{SessionID: "s1", ProjectKey: "-p1"})
	InsertMessage(tx, &types.Message{SessionID: "s1", UUID: "u1", Type: "user", ContentText: "Hello", ContentJSON: `{}`})
	InsertMessage(tx, &types.Message{SessionID: "s1", UUID: "u2", Type: "assistant", ContentText: "Hi", ContentJSON: `{}`})
	tx.Commit()

	if err := DeleteSession(db, "s1"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	count, _ := GetMessageCount(db, "s1")
	if count != 0 {
		t.Errorf("messages remaining = %d, want 0", count)
	}

	_, err := GetSession(db, "s1")
	if err == nil {
		t.Error("session should be deleted")
	}
}

func TestDeleteSession_NotFound(t *testing.T) {
	db := testDB(t)
	err := DeleteSession(db, "nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent session")
	}
}

func TestVerify(t *testing.T) {
	db := testDB(t)
	tx, _ := db.Begin()
	UpsertSession(tx, &types.Session{SessionID: "s1", ProjectKey: "-p1"})
	InsertMessage(tx, &types.Message{SessionID: "s1", UUID: "u1", Type: "user", Role: "user", ContentText: "Hello", ContentJSON: `{}`})
	InsertMessage(tx, &types.Message{SessionID: "s1", UUID: "u2", Type: "assistant", Role: "assistant", ContentText: "Hi", ContentJSON: `{}`})
	tx.Commit()

	r, err := Verify(db)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if r.SessionCount != 1 {
		t.Errorf("sessions = %d", r.SessionCount)
	}
	if r.MessageCount != 2 {
		t.Errorf("messages = %d", r.MessageCount)
	}
	if !r.FTSMatch {
		t.Errorf("FTS mismatch: messages=%d fts=%d", r.MessageCount, r.FTSCount)
	}
	if r.DuplicateUUIDs != 0 {
		t.Errorf("duplicate UUIDs = %d", r.DuplicateUUIDs)
	}
	if r.MessagesByRole["user"] != 1 {
		t.Errorf("user messages = %d, want 1", r.MessagesByRole["user"])
	}
	if len(r.TopProjects) != 1 {
		t.Errorf("top projects = %d, want 1", len(r.TopProjects))
	}
}

func TestGetSession(t *testing.T) {
	db := testDB(t)
	tx, _ := db.Begin()
	UpsertSession(tx, &types.Session{SessionID: "s1", ProjectKey: "-p1", CustomTitle: "Test"})
	tx.Commit()

	s, err := GetSession(db, "s1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if s.CustomTitle != "Test" {
		t.Errorf("title = %q", s.CustomTitle)
	}

	_, err = GetSession(db, "nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent")
	}
}

func TestGetNthLastSession(t *testing.T) {
	db := testDB(t)
	tx, _ := db.Begin()
	UpsertSession(tx, &types.Session{SessionID: "s1", ProjectKey: "-p1", EndedAt: "2026-01-01T00:00:00Z"})
	UpsertSession(tx, &types.Session{SessionID: "s2", ProjectKey: "-p1", EndedAt: "2026-01-02T00:00:00Z"})
	UpsertSession(tx, &types.Session{SessionID: "s3", ProjectKey: "-p1", EndedAt: "2026-01-03T00:00:00Z"})
	tx.Commit()

	s, err := GetNthLastSession(db, 1)
	if err != nil {
		t.Fatalf("1st: %v", err)
	}
	if s.SessionID != "s3" {
		t.Errorf("1st = %s, want s3", s.SessionID)
	}

	s, _ = GetNthLastSession(db, 2)
	if s.SessionID != "s2" {
		t.Errorf("2nd = %s, want s2", s.SessionID)
	}

	_, err = GetNthLastSession(db, 99)
	if err == nil {
		t.Error("expected error for out of range")
	}
}

func TestGetSessionMeta_NotFound(t *testing.T) {
	db := testDB(t)
	_, _, _, found, err := GetSessionMeta(db, "nonexistent")
	if err != nil {
		t.Fatalf("GetSessionMeta: %v", err)
	}
	if found {
		t.Error("expected not found")
	}
}

func TestGetStats(t *testing.T) {
	db := testDB(t)
	tx, _ := db.Begin()
	UpsertSession(tx, &types.Session{SessionID: "s1", ProjectKey: "-p1"})
	UpsertSession(tx, &types.Session{SessionID: "s2", ProjectKey: "-p2"})
	InsertMessage(tx, &types.Message{SessionID: "s1", UUID: "u1", Type: "user", ContentJSON: `{}`})
	InsertMessage(tx, &types.Message{SessionID: "s2", UUID: "u2", Type: "user", ContentJSON: `{}`})
	tx.Commit()

	sc, mc, projects, err := GetStats(db)
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}
	if sc != 2 || mc != 2 || len(projects) != 2 {
		t.Errorf("stats = (%d, %d, %d)", sc, mc, len(projects))
	}
}
