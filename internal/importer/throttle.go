package importer

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/erwint/remaimber/internal/db"
)

// ThrottledImportInterval is the minimum time between background imports.
const ThrottledImportInterval = 5 * time.Minute

// UpdateCheckInterval is the minimum time between release checks. Daily: a
// missed release costs a day, while asking GitHub on every session start would
// be a request per session for something that changes weekly at most.
const UpdateCheckInterval = 24 * time.Hour

// ThrottledSummarizeInterval is the minimum time between background summary
// sweeps. Larger than imports because summarization is heavier (an LLM call).
const ThrottledSummarizeInterval = 15 * time.Minute

// remaimberDir is where the throttle stamps and locks live — the same directory
// as the archive, so the two move together.
func remaimberDir() string {
	dir, err := db.StateDir()
	if err != nil {
		return ""
	}
	return dir
}

func stampPath(name string) string {
	return filepath.Join(remaimberDir(), name)
}

// ShouldRun reports whether at least interval has elapsed since the timestamp
// recorded in the named stamp file. Missing/unreadable stamp means "run".
func ShouldRun(name string, interval time.Duration) bool {
	data, err := os.ReadFile(stampPath(name))
	if err != nil {
		return true
	}
	line := strings.SplitN(string(data), "\n", 2)[0]
	ts, err := strconv.ParseInt(strings.TrimSpace(line), 10, 64)
	if err != nil {
		return true
	}
	return time.Since(time.Unix(ts, 0)) >= interval
}

// AcquireLock tries to take an exclusive, non-blocking lock on the named stamp
// file. Returns the file (caller must call TouchAndRelease) or nil if held.
func AcquireLock(name string) *os.File {
	os.MkdirAll(remaimberDir(), 0755)
	f, err := os.OpenFile(stampPath(name), os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil
	}
	return f
}

// ShouldImport reports whether a throttled background import is due.
func ShouldImport() bool {
	return ShouldRun(".last-import", ThrottledImportInterval)
}

// AcquireImportLock takes the import throttle lock.
func AcquireImportLock() *os.File {
	return AcquireLock(".last-import")
}

// ShouldCheckUpdate reports whether a release check is due.
func ShouldCheckUpdate() bool {
	return ShouldRun(".last-update-check", UpdateCheckInterval)
}

// AcquireUpdateLock takes the update-check throttle lock.
func AcquireUpdateLock() *os.File {
	return AcquireLock(".last-update-check")
}

// ShouldSummarize reports whether a throttled background summary sweep is due.
func ShouldSummarize() bool {
	return ShouldRun(".last-summary", ThrottledSummarizeInterval)
}

// AcquireSummarizeLock takes the summary throttle lock.
func AcquireSummarizeLock() *os.File {
	return AcquireLock(".last-summary")
}

// ImportLockName is the mutex every importer takes, throttled sweep and one-off
// alike. It is deliberately not one of the throttle stamps: a sweep holds its
// stamp for the whole run, and flock conflicts between two descriptors in the
// same process, so reusing it would have the sweep deadlock against itself.
const ImportLockName = ".import.lock"

// AcquireLockWait takes the named lock, waiting up to wait for a holder to
// finish. Returns nil if it could not be taken in time — a caller that cannot
// get in is expected to skip its work, not to force it.
func AcquireLockWait(name string, wait time.Duration) *os.File {
	deadline := time.Now().Add(wait)
	for {
		if f := AcquireLock(name); f != nil {
			return f
		}
		if !time.Now().Before(deadline) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Release drops a lock without stamping it. The import mutex records no time —
// it says who is running, not when a sweep last ran.
func Release(f *os.File) {
	if f == nil {
		return
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	f.Close()
}

// TouchAndRelease writes the current timestamp into the lock file and releases it.
func TouchAndRelease(f *os.File) {
	if f == nil {
		return
	}
	now := time.Now()
	f.Truncate(0)
	f.Seek(0, 0)
	fmt.Fprintf(f, "%d\n%s\n", now.Unix(), now.Format(time.RFC3339))
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	f.Close()
}
