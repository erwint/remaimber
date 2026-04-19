package db

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/erwin/remaimber/internal/types"
)

// UpsertSession inserts or updates a session record.
func UpsertSession(tx *sql.Tx, s *types.Session) error {
	_, err := tx.Exec(`
		INSERT INTO sessions (session_id, project_key, project_path, custom_title, first_prompt,
			git_branch, cwd, started_at, ended_at, message_count, file_mtime, file_size, last_byte_offset)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET
			project_key = excluded.project_key,
			project_path = excluded.project_path,
			custom_title = COALESCE(excluded.custom_title, sessions.custom_title),
			first_prompt = COALESCE(excluded.first_prompt, sessions.first_prompt),
			git_branch = COALESCE(excluded.git_branch, sessions.git_branch),
			cwd = COALESCE(excluded.cwd, sessions.cwd),
			started_at = COALESCE(excluded.started_at, sessions.started_at),
			ended_at = COALESCE(excluded.ended_at, sessions.ended_at),
			message_count = excluded.message_count,
			file_mtime = excluded.file_mtime,
			file_size = excluded.file_size,
			last_byte_offset = excluded.last_byte_offset`,
		s.SessionID, s.ProjectKey, s.ProjectPath, nullStr(s.CustomTitle), nullStr(s.FirstPrompt),
		nullStr(s.GitBranch), nullStr(s.CWD), nullStr(s.StartedAt), nullStr(s.EndedAt),
		s.MessageCount, s.FileMtime, s.FileSize, s.LastByteOffset,
	)
	return err
}

// InsertMessage inserts a message, ignoring duplicates (by uuid or content_hash).
func InsertMessage(tx *sql.Tx, m *types.Message) error {
	_, err := tx.Exec(`
		INSERT OR IGNORE INTO messages (session_id, uuid, parent_uuid, type, role, content_text, content_json, content_hash, timestamp)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.SessionID, nullStr(m.UUID), nullStr(m.ParentUUID), m.Type, nullStr(m.Role),
		nullStr(m.ContentText), m.ContentJSON, nullStr(m.ContentHash), nullStr(m.Timestamp),
	)
	return err
}

// GetSessionMeta retrieves file tracking metadata for a session.
func GetSessionMeta(db *sql.DB, sessionID string) (mtime float64, size int64, offset int64, found bool, err error) {
	err = db.QueryRow(`SELECT file_mtime, file_size, last_byte_offset FROM sessions WHERE session_id = ?`, sessionID).
		Scan(&mtime, &size, &offset)
	if err == sql.ErrNoRows {
		return 0, 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, 0, false, err
	}
	return mtime, size, offset, true, nil
}

// ListFilter holds filtering options for list and search queries.
type ListFilter struct {
	Project string
	Since   string // ISO timestamp
	Until   string // ISO timestamp
	Limit   int
}

// ListSessions returns sessions matching the filter.
func ListSessions(db *sql.DB, f ListFilter) ([]types.Session, error) {
	query := `SELECT session_id, project_key, project_path, COALESCE(custom_title,''), COALESCE(first_prompt,''),
		COALESCE(git_branch,''), COALESCE(cwd,''), COALESCE(started_at,''), COALESCE(ended_at,''), message_count
		FROM sessions WHERE 1=1`
	var args []any
	if f.Project != "" {
		query += ` AND project_key LIKE ?`
		args = append(args, "%"+f.Project+"%")
	}
	if f.Since != "" {
		query += ` AND ended_at >= ?`
		args = append(args, f.Since)
	}
	if f.Until != "" {
		query += ` AND started_at <= ?`
		args = append(args, f.Until)
	}
	query += ` ORDER BY ended_at DESC LIMIT ?`
	if f.Limit <= 0 {
		f.Limit = 20
	}
	args = append(args, f.Limit)

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sessions []types.Session
	for rows.Next() {
		var s types.Session
		if err := rows.Scan(&s.SessionID, &s.ProjectKey, &s.ProjectPath, &s.CustomTitle, &s.FirstPrompt,
			&s.GitBranch, &s.CWD, &s.StartedAt, &s.EndedAt, &s.MessageCount); err != nil {
			return nil, err
		}
		sessions = append(sessions, s)
	}
	return sessions, rows.Err()
}

// SearchFilter holds filtering options for search queries.
type SearchFilter struct {
	Query   string
	Project string
	Role    string // filter by message role (user, assistant)
	Since   string
	Until   string
	Limit   int
}

// SearchMessages performs FTS5 search and returns results with session context.
func SearchMessages(db *sql.DB, f SearchFilter) ([]types.SearchResult, error) {
	q := `
		SELECT m.session_id, s.project_key, COALESCE(s.custom_title,''),
			snippet(messages_fts, 0, '>>>', '<<<', '...', 40),
			COALESCE(m.timestamp,''), m.type, COALESCE(m.role,'')
		FROM messages_fts
		JOIN messages m ON m.id = messages_fts.rowid
		JOIN sessions s ON s.session_id = m.session_id
		WHERE messages_fts MATCH ?`
	args := []any{f.Query}
	if f.Project != "" {
		q += ` AND s.project_key LIKE ?`
		args = append(args, "%"+f.Project+"%")
	}
	if f.Role != "" {
		q += ` AND m.role = ?`
		args = append(args, f.Role)
	}
	if f.Since != "" {
		q += ` AND m.timestamp >= ?`
		args = append(args, f.Since)
	}
	if f.Until != "" {
		q += ` AND m.timestamp <= ?`
		args = append(args, f.Until)
	}
	if f.Limit <= 0 {
		f.Limit = 20
	}
	q += ` ORDER BY rank LIMIT ?`
	args = append(args, f.Limit)

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []types.SearchResult
	for rows.Next() {
		var r types.SearchResult
		if err := rows.Scan(&r.SessionID, &r.ProjectKey, &r.CustomTitle,
			&r.Snippet, &r.Timestamp, &r.Type, &r.Role); err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// GetSessionMessages returns messages for a session, optionally filtered by type.
func GetSessionMessages(db *sql.DB, sessionID string, msgTypes []string) ([]types.Message, error) {
	query := `SELECT id, session_id, COALESCE(uuid,''), COALESCE(parent_uuid,''), type,
		COALESCE(role,''), COALESCE(content_text,''), content_json, COALESCE(timestamp,'')
		FROM messages WHERE session_id = ?`
	args := []any{sessionID}
	if len(msgTypes) > 0 {
		query += ` AND type IN (`
		for i, t := range msgTypes {
			if i > 0 {
				query += ","
			}
			query += "?"
			args = append(args, t)
		}
		query += `)`
	}
	query += ` ORDER BY id`

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []types.Message
	for rows.Next() {
		var m types.Message
		if err := rows.Scan(&m.ID, &m.SessionID, &m.UUID, &m.ParentUUID, &m.Type,
			&m.Role, &m.ContentText, &m.ContentJSON, &m.Timestamp); err != nil {
			return nil, err
		}
		messages = append(messages, m)
	}
	return messages, rows.Err()
}

// ResolveSessionID resolves a session ID prefix to a full session ID.
// Returns the full ID, or an error if ambiguous or not found.
func ResolveSessionID(db *sql.DB, prefix string) (string, error) {
	rows, err := db.Query(`SELECT session_id FROM sessions WHERE session_id LIKE ?`, prefix+"%")
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var matches []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		matches = append(matches, id)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}

	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no session found matching prefix %q", prefix)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("ambiguous prefix %q matches %d sessions: %s",
			prefix, len(matches), strings.Join(matches[:min(len(matches), 3)], ", "))
	}
}

// DeleteSession deletes a session and all its messages.
func DeleteSession(db *sql.DB, sessionID string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Delete messages first (triggers will clean FTS)
	if _, err := tx.Exec(`DELETE FROM messages WHERE session_id = ?`, sessionID); err != nil {
		return err
	}
	result, err := tx.Exec(`DELETE FROM sessions WHERE session_id = ?`, sessionID)
	if err != nil {
		return err
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		return fmt.Errorf("session %s not found", sessionID)
	}
	return tx.Commit()
}

// VerifyResult holds the results of a database integrity check.
type VerifyResult struct {
	SessionCount    int
	MessageCount    int
	FTSCount        int
	FTSMatch        bool
	DuplicateUUIDs  int
	MessagesByRole  map[string]int
	TopProjects     []ProjectStat
}

// ProjectStat holds a project's message count.
type ProjectStat struct {
	ProjectKey   string
	MessageCount int
}

// Verify performs database integrity checks.
func Verify(database *sql.DB) (*VerifyResult, error) {
	r := &VerifyResult{MessagesByRole: make(map[string]int)}

	database.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&r.SessionCount)
	database.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&r.MessageCount)
	database.QueryRow(`SELECT COUNT(*) FROM messages_fts`).Scan(&r.FTSCount)
	r.FTSMatch = r.MessageCount == r.FTSCount

	database.QueryRow(`SELECT COUNT(*) FROM (
		SELECT uuid, COUNT(*) as cnt FROM messages
		WHERE uuid IS NOT NULL GROUP BY uuid HAVING cnt > 1
	)`).Scan(&r.DuplicateUUIDs)

	// Messages by role
	rows, err := database.Query(`SELECT COALESCE(role,'(none)'), COUNT(*) FROM messages GROUP BY role ORDER BY COUNT(*) DESC`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var role string
			var count int
			rows.Scan(&role, &count)
			r.MessagesByRole[role] = count
		}
	}

	// Top projects
	rows2, err := database.Query(`SELECT s.project_key, COUNT(m.id)
		FROM sessions s JOIN messages m ON m.session_id = s.session_id
		GROUP BY s.project_key ORDER BY COUNT(m.id) DESC LIMIT 5`)
	if err == nil {
		defer rows2.Close()
		for rows2.Next() {
			var ps ProjectStat
			rows2.Scan(&ps.ProjectKey, &ps.MessageCount)
			r.TopProjects = append(r.TopProjects, ps)
		}
	}

	return r, nil
}

// GetStats returns database statistics.
func GetStats(db *sql.DB) (sessionCount, messageCount int, projectKeys []string, err error) {
	err = db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&sessionCount)
	if err != nil {
		return
	}
	err = db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messageCount)
	if err != nil {
		return
	}
	rows, err := db.Query(`SELECT DISTINCT project_key FROM sessions ORDER BY project_key`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		if err = rows.Scan(&k); err != nil {
			return
		}
		projectKeys = append(projectKeys, k)
	}
	err = rows.Err()
	return
}

// GetMessageCount returns the current message count for a session.
func GetMessageCount(db *sql.DB, sessionID string) (int, error) {
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE session_id = ?`, sessionID).Scan(&count)
	return count, err
}

// GetNthLastSession returns the Nth most recent session (1-indexed).
func GetNthLastSession(db *sql.DB, n int) (*types.Session, error) {
	var s types.Session
	err := db.QueryRow(`SELECT session_id, project_key, project_path, COALESCE(custom_title,''), COALESCE(first_prompt,''),
		COALESCE(git_branch,''), COALESCE(cwd,''), COALESCE(started_at,''), COALESCE(ended_at,''), message_count
		FROM sessions ORDER BY ended_at DESC LIMIT 1 OFFSET ?`, n-1).
		Scan(&s.SessionID, &s.ProjectKey, &s.ProjectPath, &s.CustomTitle, &s.FirstPrompt,
			&s.GitBranch, &s.CWD, &s.StartedAt, &s.EndedAt, &s.MessageCount)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("no session at position %d", n)
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// GetSession returns a single session by ID.
func GetSession(db *sql.DB, sessionID string) (*types.Session, error) {
	var s types.Session
	err := db.QueryRow(`SELECT session_id, project_key, project_path, COALESCE(custom_title,''), COALESCE(first_prompt,''),
		COALESCE(git_branch,''), COALESCE(cwd,''), COALESCE(started_at,''), COALESCE(ended_at,''), message_count
		FROM sessions WHERE session_id = ?`, sessionID).
		Scan(&s.SessionID, &s.ProjectKey, &s.ProjectPath, &s.CustomTitle, &s.FirstPrompt,
			&s.GitBranch, &s.CWD, &s.StartedAt, &s.EndedAt, &s.MessageCount)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("session %s not found", sessionID)
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
