package importer

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ThrottledImportInterval is the minimum time between background imports.
const ThrottledImportInterval = 5 * time.Minute

// ThrottledSummarizeInterval is the minimum time between background summary
// sweeps. Larger than imports because summarization is heavier (an LLM call).
const ThrottledSummarizeInterval = 15 * time.Minute

func remaimberDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "remaimber")
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

// ShouldSummarize reports whether a throttled background summary sweep is due.
func ShouldSummarize() bool {
	return ShouldRun(".last-summary", ThrottledSummarizeInterval)
}

// AcquireSummarizeLock takes the summary throttle lock.
func AcquireSummarizeLock() *os.File {
	return AcquireLock(".last-summary")
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
