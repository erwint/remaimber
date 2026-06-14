package importer

import (
	"testing"
	"time"
)

func TestShouldRunMissingStamp(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if !ShouldRun(".nope", time.Minute) {
		t.Error("missing stamp should mean 'run'")
	}
}

func TestThrottleGate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// Acquire the lock and stamp it now.
	lock := AcquireLock(".last-summary")
	if lock == nil {
		t.Fatal("expected to acquire lock")
	}
	TouchAndRelease(lock)

	// Immediately after stamping, a long interval should gate it off.
	if ShouldRun(".last-summary", time.Hour) {
		t.Error("should be throttled right after a touch")
	}
	// A zero interval should always allow running.
	if !ShouldRun(".last-summary", 0) {
		t.Error("zero interval should always run")
	}
}

func TestImportAndSummarizeUseDistinctStamps(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// Stamp the import throttle; the summary throttle must be unaffected.
	lock := AcquireImportLock()
	if lock == nil {
		t.Fatal("acquire import lock")
	}
	TouchAndRelease(lock)

	if ShouldImport() {
		t.Error("import should be throttled after touch")
	}
	if !ShouldSummarize() {
		t.Error("summary throttle should be independent of import throttle")
	}

	// Their lock files are different, so both can be held at once.
	il := AcquireImportLock()
	sl := AcquireSummarizeLock()
	if il == nil || sl == nil {
		t.Error("import and summary locks should be independently acquirable")
	}
	if il != nil {
		TouchAndRelease(il)
	}
	if sl != nil {
		TouchAndRelease(sl)
	}
}
