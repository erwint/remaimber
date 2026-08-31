package db

import (
	"errors"
	"strings"
	"testing"
)

// Summarization runs from hooks that send stderr to /dev/null, so a failing
// backend has no voice at all unless the reason is written down. Without this,
// "the backlog stopped shrinking" is the only symptom, and it looks identical to
// having nothing left to do.
func TestSummaryFailureIsRecordedAndCleared(t *testing.T) {
	database := testDB(t)
	if _, err := database.Exec(
		`INSERT INTO sessions (session_id, project_key, project_path, message_count)
		 VALUES ('s1','-p','/p',10)`); err != nil {
		t.Fatal(err)
	}

	if err := RecordSummaryError(database, "s1", errors.New("claude: exit status 1")); err != nil {
		t.Fatal(err)
	}
	n, err := CountSummaryFailures(database)
	if err != nil || n != 1 {
		t.Fatalf("CountSummaryFailures = %d, %v; want 1", n, err)
	}
	fails, err := SummaryFailures(database, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(fails) != 1 || fails[0].SessionID != "s1" ||
		!strings.Contains(fails[0].Error, "exit status 1") || fails[0].At == "" {
		t.Fatalf("SummaryFailures = %+v", fails)
	}

	// A summary that lands means the backend recovered; a stale error would keep
	// reporting an outage that is over.
	if err := UpdateSummary(database, "s1", "a summary", 42); err != nil {
		t.Fatal(err)
	}
	if n, _ := CountSummaryFailures(database); n != 0 {
		t.Errorf("failure still recorded after a successful summary (%d)", n)
	}
}

// An LLM backend can fail with a page of output. The archive is not a log.
func TestSummaryErrorIsTruncated(t *testing.T) {
	database := testDB(t)
	if _, err := database.Exec(
		`INSERT INTO sessions (session_id, project_key, project_path, message_count)
		 VALUES ('s1','-p','/p',10)`); err != nil {
		t.Fatal(err)
	}
	if err := RecordSummaryError(database, "s1", errors.New(strings.Repeat("x", 5000))); err != nil {
		t.Fatal(err)
	}
	fails, err := SummaryFailures(database, 1)
	if err != nil || len(fails) != 1 {
		t.Fatalf("SummaryFailures = %+v, %v", fails, err)
	}
	if len(fails[0].Error) > summaryErrorMax+8 {
		t.Errorf("stored %d bytes, want it capped near %d", len(fails[0].Error), summaryErrorMax)
	}
}
