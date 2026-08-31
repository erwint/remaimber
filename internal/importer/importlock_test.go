package importer

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/erwin/remaimber/internal/db"
)

// Hooks fire from every agent at once and three MCP tools refresh the archive
// before reading it, so importers overlap routinely. They must queue rather than
// contend inside SQLite — and rather than pile up waiting, since each one runs
// inside a hook or a tool call that an agent is blocked on.
func TestImportAllDefersToARunningImport(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, "absent"))

	database, err := db.OpenAt(filepath.Join(home, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	// Stand in for an import already running. flock conflicts between two
	// descriptors even in one process, so this is the real contention.
	held := AcquireLock(ImportLockName)
	if held == nil {
		t.Fatal("could not take the import lock")
	}

	start := time.Now()
	stats, err := ImportAllWithin(database, false, 200*time.Millisecond)
	waited := time.Since(start)
	if err != nil {
		t.Fatalf("a busy lock is not an error: %v", err)
	}
	if !stats.Deferred {
		t.Error("import ran while another held the lock")
	}
	if waited > 2*time.Second {
		t.Errorf("waited %v for a 200ms deadline", waited)
	}

	// Once the holder is done, the next import proceeds normally.
	Release(held)
	stats, err = ImportAllWithin(database, false, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Deferred {
		t.Error("import still deferred after the lock was released")
	}
}

// The lock has to be its own file: a throttled sweep holds its stamp for the
// whole run, so sharing it would have the sweep wait for itself.
func TestImportLockIsNotTheThrottleStamp(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	sweep := AcquireImportLock() // what import-if-stale holds
	if sweep == nil {
		t.Fatal("could not take the throttle stamp")
	}
	defer TouchAndRelease(sweep)

	lock := AcquireLockWait(ImportLockName, 200*time.Millisecond)
	if lock == nil {
		t.Fatal("the import mutex must be free while a sweep holds its stamp")
	}
	Release(lock)
}
