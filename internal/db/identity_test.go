package db

import (
	"testing"

	"github.com/erwin/remaimber/internal/types"
)

func TestUpsertIdentityBeforeSessionExists(t *testing.T) {
	database := testDB(t)

	// Identity can be recorded before the session row exists (no FK).
	id := &types.SessionIdentity{
		SessionID:    "sess-1",
		RepoID:       "/repo/.git",
		Subpath:      "packages/foo",
		WorktreeRoot: "/wt/a",
		CWD:          "/wt/a/packages/foo",
		CapturedAt:   "2026-01-01T00:00:00Z",
		PID:          1234,
	}
	if err := UpsertIdentity(database, id); err != nil {
		t.Fatalf("UpsertIdentity: %v", err)
	}

	got, err := GetIdentity(database, "sess-1")
	if err != nil {
		t.Fatalf("GetIdentity: %v", err)
	}
	if got == nil {
		t.Fatal("expected identity, got nil")
	}
	if got.RepoID != "/repo/.git" || got.Subpath != "packages/foo" || got.PID != 1234 {
		t.Errorf("unexpected identity: %+v", got)
	}
}

func TestUpsertIdentityCoalescePreservesValues(t *testing.T) {
	database := testDB(t)

	full := &types.SessionIdentity{
		SessionID: "sess-2", RepoID: "/repo/.git", Subpath: "sub", WorktreeRoot: "/wt",
		CWD: "/wt/sub", CapturedAt: "2026-01-01T00:00:00Z",
	}
	if err := UpsertIdentity(database, full); err != nil {
		t.Fatal(err)
	}

	// A later capture with empty repo fields must not wipe the known identity.
	partial := &types.SessionIdentity{SessionID: "sess-2", CapturedAt: "2026-02-02T00:00:00Z"}
	if err := UpsertIdentity(database, partial); err != nil {
		t.Fatal(err)
	}

	got, _ := GetIdentity(database, "sess-2")
	if got.RepoID != "/repo/.git" || got.Subpath != "sub" {
		t.Errorf("COALESCE did not preserve identity: %+v", got)
	}
	if got.CapturedAt != "2026-02-02T00:00:00Z" {
		t.Errorf("captured_at should update, got %q", got.CapturedAt)
	}
}

func TestMarkEnded(t *testing.T) {
	database := testDB(t)

	// MarkEnded works even with no prior identity row.
	if err := MarkEnded(database, "sess-3", "2026-03-03T00:00:00Z"); err != nil {
		t.Fatalf("MarkEnded: %v", err)
	}
	got, _ := GetIdentity(database, "sess-3")
	if got == nil || got.EndedAt != "2026-03-03T00:00:00Z" {
		t.Errorf("expected ended_at set, got %+v", got)
	}

	// Re-recording identity (a resume/new run) clears ended_at again.
	if err := UpsertIdentity(database, &types.SessionIdentity{
		SessionID: "sess-3", RepoID: "/r/.git", CapturedAt: "2026-03-04T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	got, _ = GetIdentity(database, "sess-3")
	if got.EndedAt != "" {
		t.Errorf("ended_at should be cleared on re-capture, got %q", got.EndedAt)
	}
}

func TestGetIdentityMissing(t *testing.T) {
	database := testDB(t)
	got, err := GetIdentity(database, "nope")
	if err != nil {
		t.Fatalf("GetIdentity: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for missing identity, got %+v", got)
	}
}
