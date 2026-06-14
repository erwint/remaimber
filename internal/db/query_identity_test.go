package db

import (
	"database/sql"
	"testing"

	"github.com/erwin/remaimber/internal/types"
)

func insertSession(t *testing.T, db *sql.DB, s *types.Session) {
	t.Helper()
	tx, _ := db.Begin()
	if err := UpsertSession(tx, s); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	tx.Commit()
}

// Two worktrees of one repo (same repo_id, different worktree_root/project_key)
// must both surface under a single --repo filter.
func TestListSessions_RepoSpansWorktrees(t *testing.T) {
	database := testDB(t)
	const repoID = "/Volumes/Data/src/mono/.git"

	insertSession(t, database, &types.Session{SessionID: "a", ProjectKey: "-wt-1", EndedAt: "2026-01-02"})
	insertSession(t, database, &types.Session{SessionID: "b", ProjectKey: "-wt-2", EndedAt: "2026-01-01"})
	insertSession(t, database, &types.Session{SessionID: "c", ProjectKey: "-other", EndedAt: "2026-01-03"})

	UpsertIdentity(database, &types.SessionIdentity{SessionID: "a", RepoID: repoID, Subpath: "pkg/api", WorktreeRoot: "/wt/1"})
	UpsertIdentity(database, &types.SessionIdentity{SessionID: "b", RepoID: repoID, Subpath: "pkg/web", WorktreeRoot: "/wt/2"})
	UpsertIdentity(database, &types.SessionIdentity{SessionID: "c", RepoID: "/other/.git", Subpath: ""})

	got, err := ListSessions(database, ListFilter{Repo: repoID})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 sessions for repo, got %d", len(got))
	}
	// Newest-first ordering: a (01-02) before b (01-01).
	if got[0].SessionID != "a" || got[1].SessionID != "b" {
		t.Errorf("unexpected order: %s, %s", got[0].SessionID, got[1].SessionID)
	}
	// Identity fields populated via LEFT JOIN.
	if got[0].RepoID != repoID || got[0].Subpath != "pkg/api" {
		t.Errorf("identity not joined: %+v", got[0])
	}
}

// --subpath narrows within a monorepo; verbatim, no collapsing.
func TestListSessions_SubpathNarrows(t *testing.T) {
	database := testDB(t)
	const repoID = "/repo/.git"

	insertSession(t, database, &types.Session{SessionID: "a", ProjectKey: "-1", EndedAt: "2026-01-01"})
	insertSession(t, database, &types.Session{SessionID: "b", ProjectKey: "-2", EndedAt: "2026-01-02"})
	UpsertIdentity(database, &types.SessionIdentity{SessionID: "a", RepoID: repoID, Subpath: "crates/integration"})
	UpsertIdentity(database, &types.SessionIdentity{SessionID: "b", RepoID: repoID, Subpath: "crates/client"})

	got, err := ListSessions(database, ListFilter{Repo: repoID, Subpath: "crates/integration"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SessionID != "a" {
		t.Fatalf("subpath filter wrong: %+v", got)
	}
}

func TestSearchSessionsBySummary(t *testing.T) {
	database := testDB(t)
	insertSession(t, database, &types.Session{SessionID: "a", ProjectKey: "-1", EndedAt: "2026-01-01"})
	insertSession(t, database, &types.Session{SessionID: "b", ProjectKey: "-2", EndedAt: "2026-01-02"})

	database.Exec(`UPDATE sessions SET summary = ? WHERE session_id = 'a'`, "Fixed recipe import validation bug")
	database.Exec(`UPDATE sessions SET summary = ? WHERE session_id = 'b'`, "Refactored auth middleware")

	got, err := SearchSessionsBySummary(database, "recipe import", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SessionID != "a" {
		t.Fatalf("summary search wrong: %+v", got)
	}
	if got[0].Summary == "" {
		t.Error("summary should be populated in result")
	}
}
