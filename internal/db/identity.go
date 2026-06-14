package db

import (
	"database/sql"

	"github.com/erwin/remaimber/internal/types"
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

// SessionsNeedingIdentity returns session_ids that have a known cwd but no
// identity row yet — the backfill work list.
func SessionsNeedingIdentity(db *sql.DB) (map[string]string, error) {
	rows, err := db.Query(`
		SELECT s.session_id, COALESCE(s.cwd,'')
		FROM sessions s
		LEFT JOIN session_identity si USING(session_id)
		WHERE si.session_id IS NULL AND s.cwd IS NOT NULL AND s.cwd != ''`)
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
