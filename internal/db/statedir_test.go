package db

import (
	"os"
	"path/filepath"
	"testing"
)

// The archive used to live under ~/.claude, which misdescribes a database that
// now holds Codex and pi conversations too. Moving it must not lose it, and must
// not strand the processes already running against the old path.
func TestStateDirMigratesLegacyArchive(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	legacy := filepath.Join(home, ".claude", "remaimber")
	if err := os.MkdirAll(legacy, 0755); err != nil {
		t.Fatal(err)
	}

	// A legacy archive, with a throttle stamp beside it.
	legacyDB := filepath.Join(legacy, DBFile)
	database, err := OpenAt(legacyDB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(
		`INSERT INTO sessions (session_id, project_key, project_path, message_count)
		 VALUES ('before-move','-p','/p',1)`); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "import.stamp"), []byte("123"), 0644); err != nil {
		t.Fatal(err)
	}
	// Deliberately left open: an MCP server holds the archive for as long as its
	// agent session lasts, which is easily longer than the move.
	defer database.Close()

	dir, err := StateDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".remaimber"); dir != want {
		t.Fatalf("StateDir() = %q, want %q", dir, want)
	}
	for _, name := range []string{DBFile, "import.stamp"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s did not move: %v", name, err)
		}
	}

	// The old path still resolves to the moved archive, so a process that opens
	// it after the move — a connection from a pool that was created before it —
	// reaches the same database instead of creating an empty one.
	target, err := filepath.EvalSymlinks(legacyDB)
	if err != nil {
		t.Fatalf("legacy path no longer resolves: %v", err)
	}
	moved, err := filepath.EvalSymlinks(filepath.Join(dir, DBFile))
	if err != nil {
		t.Fatal(err)
	}
	if target != moved {
		t.Errorf("legacy path resolves to %q, want the moved archive %q", target, moved)
	}

	// A writer on the new path and the handle opened before the move must be
	// looking at one database, not two.
	after, err := OpenAt(filepath.Join(dir, DBFile))
	if err != nil {
		t.Fatal(err)
	}
	defer after.Close()
	if _, err := after.Exec(
		`INSERT INTO sessions (session_id, project_key, project_path, message_count)
		 VALUES ('after-move','-p','/p',1)`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n); err != nil {
		t.Fatalf("pre-move handle broke after the move: %v", err)
	}
	if n != 2 {
		t.Errorf("pre-move handle sees %d sessions, want both", n)
	}

	// Idempotent: a second call is not a second migration.
	again, err := StateDir()
	if err != nil || again != dir {
		t.Fatalf("StateDir() again = %q, %v", again, err)
	}
}

// A fresh install goes straight to the neutral path.
func TestStateDirFreshInstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir, err := StateDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".remaimber"); dir != want {
		t.Fatalf("StateDir() = %q, want %q", dir, want)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "remaimber")); !os.IsNotExist(err) {
		t.Error("a fresh install must not create the legacy directory")
	}
}
