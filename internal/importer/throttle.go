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

func remaimberDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "remaimber")
}

func lockFilePath() string {
	return filepath.Join(remaimberDir(), ".last-import")
}

// ShouldImport checks if enough time has passed since the last import
// by reading the timestamp from the lock file.
func ShouldImport() bool {
	data, err := os.ReadFile(lockFilePath())
	if err != nil {
		return true
	}
	// First line is unix timestamp
	line := strings.SplitN(string(data), "\n", 2)[0]
	ts, err := strconv.ParseInt(strings.TrimSpace(line), 10, 64)
	if err != nil {
		return true
	}
	return time.Since(time.Unix(ts, 0)) >= ThrottledImportInterval
}

// AcquireImportLock tries to acquire an exclusive lock for import.
// Returns the lock file (caller must call TouchAndRelease) or nil if already locked.
func AcquireImportLock() *os.File {
	os.MkdirAll(remaimberDir(), 0755)
	f, err := os.OpenFile(lockFilePath(), os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil
	}
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		f.Close()
		return nil
	}
	return f
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
