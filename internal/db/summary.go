package db

import (
	"database/sql"

	"github.com/erwin/remaimber/internal/types"
)

// SummaryWork describes a session that needs (re)summarizing and how far the
// existing rolling summary has already consumed its user/assistant messages.
type SummaryWork struct {
	SessionID    string
	Summary      string
	Offset       int // user/assistant messages already folded into Summary
	UACount      int // current count of user/assistant messages
	ProjectKey   string
	SessionMtime string
}

// SessionsNeedingSummary returns sessions whose count of user/assistant messages
// has grown at least minNew beyond what the current summary covers. A brand-new
// session (offset 0) qualifies once it has minNew such messages.
func SessionsNeedingSummary(db *sql.DB, minNew int) ([]SummaryWork, error) {
	if minNew <= 0 {
		minNew = 6
	}
	rows, err := db.Query(`
		SELECT s.session_id, COALESCE(s.summary,''), COALESCE(s.summary_offset,0),
			COUNT(m.id), s.project_key
		FROM sessions s
		LEFT JOIN messages m
			ON m.session_id = s.session_id AND m.role IN ('user','assistant')
		GROUP BY s.session_id
		HAVING COUNT(m.id) - COALESCE(s.summary_offset,0) >= ?
		ORDER BY s.ended_at DESC`, minNew)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SummaryWork
	for rows.Next() {
		var w SummaryWork
		if err := rows.Scan(&w.SessionID, &w.Summary, &w.Offset, &w.UACount, &w.ProjectKey); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// UpdateSummary stores a session's rolling summary and the offset (count of
// user/assistant messages) it now reflects. Also mirrors the summary into the
// FTS-free sessions row so it is searchable via SearchSessionsBySummary.
func UpdateSummary(db *sql.DB, sessionID, summary string, offset int) error {
	_, err := db.Exec(`UPDATE sessions SET summary = ?, summary_offset = ? WHERE session_id = ?`,
		summary, offset, sessionID)
	return err
}

// UserAssistantMessages returns a session's user and assistant messages in
// order — the input to summarization.
// summaryTextCap bounds how much of each message's text is loaded for
// summarization. Summaries don't need full message bodies, and loading the raw
// content_json of a large session can spike memory into the gigabytes (enough to
// be OOM-killed). renderPrompt truncates further; this just caps memory.
const summaryTextCap = 2000

func UserAssistantMessages(db *sql.DB, sessionID string) ([]types.Message, error) {
	// Select only what summarization needs (role/type/text) — crucially NOT
	// content_json — and cap text length in SQL so a giant session can't blow up
	// memory.
	rows, err := db.Query(`SELECT COALESCE(role,''), type, substr(COALESCE(content_text,''), 1, ?)
		FROM messages
		WHERE session_id = ? AND role IN ('user','assistant')
		ORDER BY id`, summaryTextCap, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []types.Message
	for rows.Next() {
		var m types.Message
		if err := rows.Scan(&m.Role, &m.Type, &m.ContentText); err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}
