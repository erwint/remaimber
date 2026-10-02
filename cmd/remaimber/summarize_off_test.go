package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/erwint/remaimber/internal/db"
)

// archiveWithBacklog sets up a home holding one Claude Code session long enough
// to be summarized, imports it into a fresh database, and returns that
// database's path.
func archiveWithBacklog(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	dir := filepath.Join(home, ".claude", "projects", "-Users-x-proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	id := "dddddddd-0000-0000-0000-00000000000d"
	var b strings.Builder
	for i := 0; i < 12; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		fmt.Fprintf(&b, `{"type":"%s","uuid":"u%d","timestamp":"2026-10-01T10:%02d:00Z","message":{"role":"%s","content":"turn %d about the relay"},"sessionId":"%s","cwd":"/Users/x/proj"}`+"\n",
			role, i, i, role, i, id)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	dbFile := filepath.Join(t.TempDir(), "r.db")
	run(t, "--db", dbFile, "import")
	return dbFile
}

func run(t *testing.T, args ...string) error {
	t.Helper()
	dbPath = "" // a package-level flag; reset between invocations
	root := newRootCmd()
	root.SetArgs(args)
	root.SetOut(&strings.Builder{})
	root.SetErr(&strings.Builder{})
	return root.Execute()
}

func summaryFailures(t *testing.T, dbFile string) int {
	t.Helper()
	database, err := db.OpenAt(dbFile)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	n, err := db.CountSummaryFailures(database)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// The workaround this replaces: a backend that does not exist. Every background
// run still tries, fails, and records the failure, which doctor then reports
// as a stuck backlog. Kept as a test so the contrast below means something.
func TestUnreachableBackendRecordsFailures(t *testing.T) {
	dbFile := archiveWithBacklog(t)
	t.Setenv("REMAIMBER_LLM", "no-such-summarizer")
	run(t, "--db", dbFile, "summarize-if-stale")
	if summaryFailures(t, dbFile) == 0 {
		t.Fatal("an unreachable backend recorded no failure; the comparison below proves nothing")
	}
}

// Off attempts nothing, so it records nothing — and leaves the throttle stamp
// alone, since no summarization ran.
func TestSummarizationOffAttemptsNothing(t *testing.T) {
	dbFile := archiveWithBacklog(t)
	t.Setenv("REMAIMBER_LLM", "off")
	if err := run(t, "--db", dbFile, "summarize-if-stale"); err != nil {
		t.Fatalf("summarize-if-stale: %v", err)
	}
	if n := summaryFailures(t, dbFile); n != 0 {
		t.Errorf("%d failure(s) recorded with summarization off", n)
	}
	home, _ := os.UserHomeDir()
	if _, err := os.Stat(filepath.Join(home, ".remaimber", ".last-summary")); err == nil {
		t.Error("the summary throttle was stamped although nothing ran")
	}

	// Asked for directly, it says why rather than doing nothing silently...
	err := run(t, "--db", dbFile, "summarize", "--all")
	if err == nil || !strings.Contains(err.Error(), "REMAIMBER_LLM=off") {
		t.Errorf("summarize with summarization off = %v, want it to say it is off", err)
	}
	// ...while reindexing, which calls no model, still works.
	if err := run(t, "--db", dbFile, "summarize", "--reindex"); err != nil {
		t.Errorf("summarize --reindex: %v", err)
	}
}
