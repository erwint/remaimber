package mover

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func setupTestProjects(t *testing.T) (home string, cleanup func()) {
	t.Helper()
	home = t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", home)

	projectsDir := filepath.Join(home, ".claude", "projects")

	// Create source project with a session
	srcDir := filepath.Join(projectsDir, "-src-project-a")
	os.MkdirAll(srcDir, 0755)
	os.WriteFile(
		filepath.Join(srcDir, "session-1.jsonl"),
		[]byte(`{"type":"user","uuid":"u1","message":{"role":"user","content":"Hello"}}`+"\n"),
		0644,
	)

	// Create session subdirectory
	os.MkdirAll(filepath.Join(srcDir, "session-1"), 0755)
	os.WriteFile(filepath.Join(srcDir, "session-1", "subagent.jsonl"), []byte("data"), 0644)

	return home, func() { os.Setenv("HOME", origHome) }
}

func TestMove_Copy(t *testing.T) {
	_, cleanup := setupTestProjects(t)
	defer cleanup()

	err := Move("session-1", "-src-project-b", true)
	if err != nil {
		t.Fatalf("Move(copy): %v", err)
	}

	home, _ := os.UserHomeDir()
	projectsDir := filepath.Join(home, ".claude", "projects")

	// Source should still exist
	srcPath := filepath.Join(projectsDir, "-src-project-a", "session-1.jsonl")
	if _, err := os.Stat(srcPath); err != nil {
		t.Error("source file should still exist after copy")
	}

	// Target should exist
	dstPath := filepath.Join(projectsDir, "-src-project-b", "session-1.jsonl")
	if _, err := os.Stat(dstPath); err != nil {
		t.Error("target file should exist after copy")
	}

	// Verify content matches
	srcData, _ := os.ReadFile(srcPath)
	dstData, _ := os.ReadFile(dstPath)
	if string(srcData) != string(dstData) {
		t.Error("copied file content doesn't match source")
	}
}

func TestMove_Move(t *testing.T) {
	_, cleanup := setupTestProjects(t)
	defer cleanup()

	err := Move("session-1", "-src-project-b", false)
	if err != nil {
		t.Fatalf("Move: %v", err)
	}

	home, _ := os.UserHomeDir()
	projectsDir := filepath.Join(home, ".claude", "projects")

	// Source should be gone
	srcPath := filepath.Join(projectsDir, "-src-project-a", "session-1.jsonl")
	if _, err := os.Stat(srcPath); !os.IsNotExist(err) {
		t.Error("source file should be removed after move")
	}

	// Source subdirectory should be gone
	srcSubdir := filepath.Join(projectsDir, "-src-project-a", "session-1")
	if _, err := os.Stat(srcSubdir); !os.IsNotExist(err) {
		t.Error("source subdirectory should be removed after move")
	}

	// Target should exist
	dstPath := filepath.Join(projectsDir, "-src-project-b", "session-1.jsonl")
	if _, err := os.Stat(dstPath); err != nil {
		t.Error("target file should exist after move")
	}
}

func TestMove_NotFound(t *testing.T) {
	_, cleanup := setupTestProjects(t)
	defer cleanup()

	err := Move("nonexistent-session", "-src-project-b", false)
	if err == nil {
		t.Error("expected error for nonexistent session")
	}
}

func TestMove_AlreadyExists(t *testing.T) {
	_, cleanup := setupTestProjects(t)
	defer cleanup()

	home, _ := os.UserHomeDir()
	projectsDir := filepath.Join(home, ".claude", "projects")

	// Create target with same session
	dstDir := filepath.Join(projectsDir, "-src-project-b")
	os.MkdirAll(dstDir, 0755)
	os.WriteFile(filepath.Join(dstDir, "session-1.jsonl"), []byte("existing"), 0644)

	err := Move("session-1", "-src-project-b", true)
	if err == nil {
		t.Error("expected error when session already exists in target")
	}
}

func TestMove_UpdatesSessionsIndex(t *testing.T) {
	_, cleanup := setupTestProjects(t)
	defer cleanup()

	err := Move("session-1", "-src-project-b", true)
	if err != nil {
		t.Fatalf("Move: %v", err)
	}

	home, _ := os.UserHomeDir()
	projectsDir := filepath.Join(home, ".claude", "projects")

	// Check target sessions-index.json
	data, err := os.ReadFile(filepath.Join(projectsDir, "-src-project-b", "sessions-index.json"))
	if err != nil {
		t.Fatalf("read sessions-index: %v", err)
	}

	var idx sessionsIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		t.Fatalf("parse sessions-index: %v", err)
	}

	found := false
	for _, s := range idx.Sessions {
		if s.ID == "session-1" {
			found = true
		}
	}
	if !found {
		t.Error("session-1 not found in target sessions-index.json")
	}
}

func TestMove_RemovesFromSourceIndex(t *testing.T) {
	_, cleanup := setupTestProjects(t)
	defer cleanup()

	home, _ := os.UserHomeDir()
	projectsDir := filepath.Join(home, ".claude", "projects")
	srcDir := filepath.Join(projectsDir, "-src-project-a")

	// Create source sessions-index.json
	idx := sessionsIndex{Sessions: []sessionEntry{{ID: "session-1"}, {ID: "session-2"}}}
	data, _ := json.Marshal(idx)
	os.WriteFile(filepath.Join(srcDir, "sessions-index.json"), data, 0644)

	err := Move("session-1", "-src-project-b", false) // move, not copy
	if err != nil {
		t.Fatalf("Move: %v", err)
	}

	// Source index should no longer contain session-1
	data, _ = os.ReadFile(filepath.Join(srcDir, "sessions-index.json"))
	var updated sessionsIndex
	json.Unmarshal(data, &updated)

	for _, s := range updated.Sessions {
		if s.ID == "session-1" {
			t.Error("session-1 should be removed from source index after move")
		}
	}
	if len(updated.Sessions) != 1 {
		t.Errorf("source index should have 1 entry, got %d", len(updated.Sessions))
	}
}

func TestAddToSessionsIndex_Idempotent(t *testing.T) {
	dir := t.TempDir()

	addToSessionsIndex(dir, "s1")
	addToSessionsIndex(dir, "s1") // should not duplicate

	data, _ := os.ReadFile(filepath.Join(dir, "sessions-index.json"))
	var idx sessionsIndex
	json.Unmarshal(data, &idx)

	if len(idx.Sessions) != 1 {
		t.Errorf("expected 1 entry after duplicate add, got %d", len(idx.Sessions))
	}
}
