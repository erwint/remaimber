package db

import (
	"database/sql"

	"github.com/erwint/remaimber/internal/types"
)

// UpsertIdentity records (or updates) a session's durable identity. It is safe
// to call before the session row exists. COALESCE preserves previously-captured
// values when a later call passes empty fields (e.g. a re-capture after the
// worktree is gone), so identity is never downgraded to null once known.
func UpsertIdentity(db *sql.DB, id *types.SessionIdentity) error {
	_, err := db.Exec(`
		INSERT INTO session_identity
			(session_id, repo_id, subpath, worktree_root, cwd, captured_at, pid, ended_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, NULL)
		ON CONFLICT(session_id) DO UPDATE SET
			repo_id       = COALESCE(excluded.repo_id, session_identity.repo_id),
			subpath       = COALESCE(excluded.subpath, session_identity.subpath),
			worktree_root = COALESCE(excluded.worktree_root, session_identity.worktree_root),
			cwd           = COALESCE(excluded.cwd, session_identity.cwd),
			captured_at   = excluded.captured_at,
			pid           = excluded.pid,
			ended_at      = NULL`,
		id.SessionID, nullStr(id.RepoID), nullStr(id.Subpath), nullStr(id.WorktreeRoot),
		nullStr(id.CWD), nullStr(id.CapturedAt), nullInt(id.PID),
	)
	return err
}

// FillIdentity fills in the repo identity of a session after the fact, without
// touching what a live capture owns: when it was captured, by which process,
// and whether the session has ended. A backfill that cleared ended_at would
// make a finished session look live again to resume's liveness guard.
func FillIdentity(db *sql.DB, id *types.SessionIdentity) error {
	_, err := db.Exec(`
		INSERT INTO session_identity (session_id, repo_id, subpath, worktree_root, cwd, captured_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET
			repo_id       = COALESCE(NULLIF(session_identity.repo_id,''), excluded.repo_id),
			subpath       = COALESCE(session_identity.subpath, excluded.subpath),
			worktree_root = COALESCE(NULLIF(session_identity.worktree_root,''), excluded.worktree_root),
			cwd           = COALESCE(NULLIF(session_identity.cwd,''), excluded.cwd),
			captured_at   = COALESCE(session_identity.captured_at, excluded.captured_at)`,
		id.SessionID, nullStr(id.RepoID), nullStr(id.Subpath), nullStr(id.WorktreeRoot),
		nullStr(id.CWD), nullStr(id.CapturedAt),
	)
	return err
}

// MarkEnded clears the liveness marker for a session by stamping ended_at.
// It inserts a row if none exists so a SessionEnd that races ahead still records.
func MarkEnded(db *sql.DB, sessionID, endedAt string) error {
	_, err := db.Exec(`
		INSERT INTO session_identity (session_id, ended_at)
		VALUES (?, ?)
		ON CONFLICT(session_id) DO UPDATE SET ended_at = excluded.ended_at`,
		sessionID, endedAt,
	)
	return err
}

// GetIdentity returns the identity row for a session, or nil if none exists.
func GetIdentity(db *sql.DB, sessionID string) (*types.SessionIdentity, error) {
	var id types.SessionIdentity
	var pid sql.NullInt64
	err := db.QueryRow(`
		SELECT session_id, COALESCE(repo_id,''), COALESCE(subpath,''),
			COALESCE(worktree_root,''), COALESCE(cwd,''), COALESCE(captured_at,''),
			pid, COALESCE(ended_at,'')
		FROM session_identity WHERE session_id = ?`, sessionID).
		Scan(&id.SessionID, &id.RepoID, &id.Subpath, &id.WorktreeRoot, &id.CWD,
			&id.CapturedAt, &pid, &id.EndedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if pid.Valid {
		id.PID = int(pid.Int64)
	}
	return &id, nil
}

// SessionsNeedingIdentity returns this machine's sessions that have a known cwd
// but no repo identity — the backfill work list. That includes an identity row
// holding only an end time: SessionEnd writes one when the SessionStart capture
// never ran, and a list of only missing rows would pass those over for good.
// Sessions synced from another machine are left out, since their directories
// are not on this one.
func SessionsNeedingIdentity(db *sql.DB) (map[string]string, error) {
	rows, err := db.Query(`
		SELECT s.session_id, COALESCE(NULLIF(si.cwd,''), s.cwd)
		FROM sessions s
		LEFT JOIN session_identity si USING(session_id)
		WHERE COALESCE(si.repo_id,'') = '' AND COALESCE(s.origin,'') = ''
		  AND COALESCE(NULLIF(si.cwd,''), s.cwd, '') != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]string)
	for rows.Next() {
		var sid, cwd string
		if err := rows.Scan(&sid, &cwd); err != nil {
			return nil, err
		}
		out[sid] = cwd
	}
	return out, rows.Err()
}

func nullInt(i int) any {
	if i == 0 {
		return nil
	}
	return i
}
