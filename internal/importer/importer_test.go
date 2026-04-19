package importer

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/erwin/remaimber/internal/db"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	database, err := db.OpenAt(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}

func writeJSONL(t *testing.T, dir, sessionID string, lines ...string) string {
	t.Helper()
	path := filepath.Join(dir, sessionID+".jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, line := range lines {
		fmt.Fprintln(f, line)
	}
	f.Close()
	return path
}

func TestImportFile_Basic(t *testing.T) {
	database := testDB(t)
	dir := t.TempDir()

	sessionID := "test-session-001"
	writeJSONL(t, dir, sessionID,
		`{"type":"custom-title","customTitle":"Test Session","sessionId":"test-session-001"}`,
		`{"type":"user","uuid":"u1","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"Hello"},"sessionId":"test-session-001","cwd":"/test","gitBranch":"main"}`,
		`{"type":"assistant","uuid":"u2","timestamp":"2026-01-01T00:01:00Z","message":{"role":"assistant","content":[{"type":"text","text":"Hi there!"}]},"sessionId":"test-session-001"}`,
	)

	sf := SessionFile{
		Path:       filepath.Join(dir, sessionID+".jsonl"),
		SessionID:  sessionID,
		ProjectKey: "-test-project",
	}

	imported, newMsgs, _, err := ImportFile(database, sf, false)
	if err != nil {
		t.Fatalf("ImportFile: %v", err)
	}
	if !imported {
		t.Error("expected imported=true")
	}
	if newMsgs != 3 {
		t.Errorf("newMsgs = %d, want 3", newMsgs)
	}

	// Verify session metadata
	sessions, _ := db.ListSessions(database, db.ListFilter{Limit: 10})
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(sessions))
	}
	s := sessions[0]
	if s.CustomTitle != "Test Session" {
		t.Errorf("title = %q, want 'Test Session'", s.CustomTitle)
	}
	if s.FirstPrompt != "Hello" {
		t.Errorf("first_prompt = %q, want 'Hello'", s.FirstPrompt)
	}
	if s.ProjectKey != "-test-project" {
		t.Errorf("project = %q, want '-test-project'", s.ProjectKey)
	}
	if s.MessageCount != 3 {
		t.Errorf("count = %d, want 3", s.MessageCount)
	}
}

func TestImportFile_Incremental_SkipUnchanged(t *testing.T) {
	database := testDB(t)
	dir := t.TempDir()

	sessionID := "test-session-002"
	writeJSONL(t, dir, sessionID,
		`{"type":"user","uuid":"u1","message":{"role":"user","content":"Hello"},"timestamp":"2026-01-01T00:00:00Z"}`,
	)

	sf := SessionFile{
		Path:       filepath.Join(dir, sessionID+".jsonl"),
		SessionID:  sessionID,
		ProjectKey: "-test-project",
	}

	// First import
	imported, _, _, _ := ImportFile(database, sf, false)
	if !imported {
		t.Fatal("first import should import")
	}

	// Second import — same file, should skip
	imported, _, _, _ = ImportFile(database, sf, false)
	if imported {
		t.Error("second import should skip (file unchanged)")
	}
}

func TestImportFile_Incremental_AppendNew(t *testing.T) {
	database := testDB(t)
	dir := t.TempDir()

	sessionID := "test-session-003"
	path := filepath.Join(dir, sessionID+".jsonl")

	// Write initial content
	f, _ := os.Create(path)
	fmt.Fprintln(f, `{"type":"user","uuid":"u1","message":{"role":"user","content":"First message"},"timestamp":"2026-01-01T00:00:00Z"}`)
	f.Close()

	sf := SessionFile{Path: path, SessionID: sessionID, ProjectKey: "-p1"}

	// First import
	ImportFile(database, sf, false)
	count, _ := db.GetMessageCount(database, sessionID)
	if count != 1 {
		t.Fatalf("after first import: count = %d, want 1", count)
	}

	// Append a new line
	f, _ = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	fmt.Fprintln(f, `{"type":"assistant","uuid":"u2","message":{"role":"assistant","content":[{"type":"text","text":"Response"}]},"timestamp":"2026-01-01T00:01:00Z"}`)
	f.Close()

	// Second import — should pick up new line
	imported, _, _, err := ImportFile(database, sf, false)
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if !imported {
		t.Error("second import should detect file change")
	}

	count, _ = db.GetMessageCount(database, sessionID)
	if count != 2 {
		t.Errorf("after append: count = %d, want 2", count)
	}
}

func TestImportFile_Force(t *testing.T) {
	database := testDB(t)
	dir := t.TempDir()

	sessionID := "test-session-004"
	writeJSONL(t, dir, sessionID,
		`{"type":"user","uuid":"u1","message":{"role":"user","content":"Hello"},"timestamp":"2026-01-01T00:00:00Z"}`,
	)

	sf := SessionFile{
		Path:       filepath.Join(dir, sessionID+".jsonl"),
		SessionID:  sessionID,
		ProjectKey: "-p1",
	}

	// First import
	ImportFile(database, sf, false)

	// Force re-import — should re-process but not duplicate (INSERT OR IGNORE)
	imported, _, _, err := ImportFile(database, sf, true)
	if err != nil {
		t.Fatalf("force import: %v", err)
	}
	if !imported {
		t.Error("force import should always import")
	}

	count, _ := db.GetMessageCount(database, sessionID)
	if count != 1 {
		t.Errorf("after force: count = %d, want 1 (dedup should prevent duplicates)", count)
	}
}

func TestImportFile_UUIDDedup_Concurrent(t *testing.T) {
	database := testDB(t)
	dir := t.TempDir()

	sessionID := "test-session-005"
	writeJSONL(t, dir, sessionID,
		`{"type":"user","uuid":"u1","message":{"role":"user","content":"Hello"},"timestamp":"2026-01-01T00:00:00Z"}`,
		`{"type":"assistant","uuid":"u2","message":{"role":"assistant","content":[{"type":"text","text":"Hi"}]},"timestamp":"2026-01-01T00:01:00Z"}`,
	)

	sf := SessionFile{
		Path:       filepath.Join(dir, sessionID+".jsonl"),
		SessionID:  sessionID,
		ProjectKey: "-p1",
	}

	// Simulate concurrent imports by force-importing twice
	ImportFile(database, sf, true)
	ImportFile(database, sf, true)

	count, _ := db.GetMessageCount(database, sessionID)
	if count != 2 {
		t.Errorf("after concurrent imports: count = %d, want 2 (should not duplicate)", count)
	}
}

func TestImportFile_ContentHashDedup(t *testing.T) {
	database := testDB(t)
	dir := t.TempDir()

	sessionID := "test-session-006"
	writeJSONL(t, dir, sessionID,
		`{"type":"custom-title","customTitle":"My Title","sessionId":"test-session-006"}`,
	)

	sf := SessionFile{
		Path:       filepath.Join(dir, sessionID+".jsonl"),
		SessionID:  sessionID,
		ProjectKey: "-p1",
	}

	// Import twice with force — content hash should prevent duplicates
	ImportFile(database, sf, true)
	ImportFile(database, sf, true)

	count, _ := db.GetMessageCount(database, sessionID)
	if count != 1 {
		t.Errorf("after double import of uuid-less line: count = %d, want 1", count)
	}
}

func TestImportFile_LargeLineBuffer(t *testing.T) {
	database := testDB(t)
	dir := t.TempDir()

	// Create a message with content larger than default buffer
	bigContent := ""
	for i := 0; i < 100000; i++ {
		bigContent += "x"
	}

	sessionID := "test-session-007"
	writeJSONL(t, dir, sessionID,
		fmt.Sprintf(`{"type":"user","uuid":"u1","message":{"role":"user","content":"%s"},"timestamp":"2026-01-01T00:00:00Z"}`, bigContent),
	)

	sf := SessionFile{
		Path:       filepath.Join(dir, sessionID+".jsonl"),
		SessionID:  sessionID,
		ProjectKey: "-p1",
	}

	imported, _, _, err := ImportFile(database, sf, false)
	if err != nil {
		t.Fatalf("import large line: %v", err)
	}
	if !imported {
		t.Error("should import large line")
	}
}

func TestImportFile_EmptyFile(t *testing.T) {
	database := testDB(t)
	dir := t.TempDir()

	sessionID := "test-session-008"
	path := filepath.Join(dir, sessionID+".jsonl")
	os.WriteFile(path, []byte{}, 0644)

	sf := SessionFile{Path: path, SessionID: sessionID, ProjectKey: "-p1"}

	// Should handle gracefully — no messages, no session created
	imported, newMsgs, _, err := ImportFile(database, sf, false)
	if err != nil {
		t.Fatalf("import empty: %v", err)
	}
	// Empty file with no prior offset: 0 messages parsed, no session upsert needed
	_ = imported
	_ = newMsgs
}

func TestImportFile_MalformedLines(t *testing.T) {
	database := testDB(t)
	dir := t.TempDir()

	sessionID := "test-session-009"
	writeJSONL(t, dir, sessionID,
		`not valid json`,
		`{"type":"user","uuid":"u1","message":{"role":"user","content":"Good line"},"timestamp":"2026-01-01T00:00:00Z"}`,
		`{also bad`,
	)

	sf := SessionFile{
		Path:       filepath.Join(dir, sessionID+".jsonl"),
		SessionID:  sessionID,
		ProjectKey: "-p1",
	}

	imported, newMsgs, _, err := ImportFile(database, sf, false)
	if err != nil {
		t.Fatalf("import with bad lines: %v", err)
	}
	if !imported {
		t.Error("should import despite bad lines")
	}
	// Only the valid line should be imported
	count, _ := db.GetMessageCount(database, sessionID)
	if count != 1 {
		t.Errorf("count = %d, want 1 (only valid line)", count)
	}
	_ = newMsgs
}

func TestImportAll_WithTestDir(t *testing.T) {
	// This test would require mocking ScanProjects or setting HOME
	// Just verify ImportAll handles an empty scan gracefully
	database := testDB(t)

	// Set HOME to a temp dir with no .claude/projects
	origHome := os.Getenv("HOME")
	tmpHome := t.TempDir()
	os.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	stats, err := ImportAll(database, false)
	if err != nil {
		t.Fatalf("ImportAll empty: %v", err)
	}
	if stats.FilesScanned != 0 {
		t.Errorf("scanned = %d, want 0", stats.FilesScanned)
	}
}
