package db

import (
	"database/sql"
	"regexp"
	"strings"

	"github.com/erwin/remaimber/internal/types"
)

// toolMarkerLine matches a lone tool-call marker the parser emits for a
// tool_use block, e.g. "[tool: Bash]".
var toolMarkerLine = regexp.MustCompile(`^\[tool: [^\]]*\]$`)

// isToolOnly reports whether an assistant message's text is nothing but tool-call
// markers (no prose) — pure agent mechanics with no recall value.
func isToolOnly(text string) bool {
	sawMarker := false
	for _, ln := range strings.Split(strings.TrimSpace(text), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		if !toolMarkerLine.MatchString(ln) {
			return false
		}
		sawMarker = true
	}
	return sawMarker
}

// SummaryWork describes a session that needs (re)summarizing. AfterID is the
// message-id high-water mark the current summary already reflects; folding
// resumes from messages newer than it.
type SummaryWork struct {
	SessionID   string
	Summary     string
	AfterID     int64 // summary covers messages up to and including this id
	NewCount    int   // user/assistant messages newer than AfterID
	ProjectKey  string
	FirstPrompt string // opening user prompt, the pinned goal for the reduce step
}

// SessionsNeedingSummary returns sessions that have at least minNew user/assistant
// messages newer than what their summary already covers. Using a message-id
// high-water mark (not a message count) keeps this consistent with the filtered
// fold: once caught up, a session settles and is not re-summarized until genuinely
// new messages arrive.
func SessionsNeedingSummary(db *sql.DB, minNew int) ([]SummaryWork, error) {
	if minNew <= 0 {
		minNew = 6
	}
	rows, err := db.Query(`
		SELECT s.session_id, COALESCE(s.summary,''), COALESCE(s.summary_offset,0),
			COUNT(m.id), s.project_key, COALESCE(s.first_prompt,'')
		FROM sessions s
		LEFT JOIN messages m
			ON m.session_id = s.session_id AND m.role IN ('user','assistant')
			AND m.id > COALESCE(s.summary_offset,0)
		GROUP BY s.session_id
		HAVING COUNT(m.id) >= ?
		ORDER BY s.ended_at DESC`, minNew)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SummaryWork
	for rows.Next() {
		var w SummaryWork
		if err := rows.Scan(&w.SessionID, &w.Summary, &w.AfterID, &w.NewCount, &w.ProjectKey, &w.FirstPrompt); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// UpdateSummary stores a session's rolling summary and the message-id high-water
// mark it now reflects. Also mirrors the summary into the sessions row so it is
// searchable via SearchSessionsBySummary.
func UpdateSummary(db *sql.DB, sessionID, summary string, afterID int64) error {
	_, err := db.Exec(`UPDATE sessions SET summary = ?, summary_offset = ? WHERE session_id = ?`,
		summary, afterID, sessionID)
	return err
}

// MaxUAMessageID returns the highest message id among a session's user/assistant
// messages (0 if none) — the high-water mark a fresh summary has caught up to.
func MaxUAMessageID(db *sql.DB, sessionID string) (int64, error) {
	var id int64
	err := db.QueryRow(`SELECT COALESCE(MAX(id),0) FROM messages
		WHERE session_id = ? AND role IN ('user','assistant')`, sessionID).Scan(&id)
	return id, err
}

// summaryTextCap bounds how much of each message's text is loaded for
// summarization. Summaries don't need full message bodies, and loading the raw
// content_json of a large session can spike memory into the gigabytes (enough to
// be OOM-killed). The map step truncates further; this just caps memory.
const summaryTextCap = 2000

// UserAssistantMessagesAfter returns a session's salient user/assistant messages
// with id greater than afterID, in order — the input to the map step.
//
// Selects only what summarization needs (role/type/text) — crucially NOT
// content_json — and caps text length in SQL so a giant session can't blow up
// memory. Excludes tool-result turns (user-role messages carrying file reads /
// command output, the bulk of an agentic session and pure noise for recall); the
// content_json LIKE runs inside SQLite so the heavy column is never materialized.
// Empty-text turns are skipped; tool-only assistant turns are dropped below.
func UserAssistantMessagesAfter(db *sql.DB, sessionID string, afterID int64) ([]types.Message, error) {
	rows, err := db.Query(`SELECT COALESCE(role,''), type, substr(COALESCE(content_text,''), 1, ?)
		FROM messages
		WHERE session_id = ? AND id > ? AND role IN ('user','assistant')
		  AND COALESCE(content_text,'') != ''
		  AND NOT (type = 'user' AND content_json LIKE '%"tool_result"%')
		ORDER BY id`, summaryTextCap, sessionID, afterID)
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
		// Drop assistant turns that are only tool-call markers (no prose). The
		// text is already loaded and short, so this is cheap; doing it in Go
		// avoids brittle SQL pattern matching.
		if m.Type == "assistant" && isToolOnly(m.ContentText) {
			continue
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}
