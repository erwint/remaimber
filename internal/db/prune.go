package db

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// An archive that only grows is a different problem from the one remaimber
// solves: it exists because agents delete transcripts too soon, not because
// every byte deserves to be kept forever. Pruning is therefore graded — the
// cheapest thing to lose goes first, and losing the whole conversation is the
// last resort, not the only option.
//
// Tool output is over half of this archive by volume and is already excluded
// from search by default (it is machine noise, and it includes remaimber's own
// archived results). Dropping it leaves the conversation, the summaries and the
// segments intact, so recall and partial resume still work on a pruned session.

// PruneMode selects how much of an old session to let go of.
type PruneMode string

const (
	// PruneToolOutput removes tool results, keeping the conversation.
	PruneToolOutput PruneMode = "tool-output"
	// PruneMessages removes every message but keeps the session and its
	// summaries, so the archive still remembers what the work was about.
	PruneMessages PruneMode = "messages"
	// PruneSessions removes the sessions outright.
	PruneSessions PruneMode = "sessions"
)

// ParsePruneMode validates a mode name.
func ParsePruneMode(s string) (PruneMode, error) {
	switch PruneMode(s) {
	case PruneToolOutput, PruneMessages, PruneSessions:
		return PruneMode(s), nil
	}
	return "", fmt.Errorf("unknown prune mode %q: want tool-output, messages or sessions", s)
}

// PruneCandidate is one session old enough to prune.
type PruneCandidate struct {
	SessionID  string
	ProjectKey string
	Agent      string
	EndedAt    string
}

// PruneCandidates returns the sessions that ended before the given ISO
// timestamp, oldest first. A session with no end time is judged by when it
// started; one with neither is left alone, since its age is unknown.
func PruneCandidates(db *sql.DB, before string) ([]PruneCandidate, error) {
	rows, err := db.Query(`
		SELECT session_id, project_key, COALESCE(NULLIF(agent,''),'claude'),
			COALESCE(NULLIF(ended_at,''), started_at)
		FROM sessions
		WHERE COALESCE(NULLIF(ended_at,''), started_at) < ?
		  AND COALESCE(NULLIF(ended_at,''), started_at) != ''
		ORDER BY 4`, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PruneCandidate
	for rows.Next() {
		var c PruneCandidate
		if err := rows.Scan(&c.SessionID, &c.ProjectKey, &c.Agent, &c.EndedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// PruneStats is what a prune did, or would do.
type PruneStats struct {
	Sessions int   `json:"sessions"`
	Messages int   `json:"messages"`
	Bytes    int64 `json:"bytes"`
}

// Prune applies mode to the given sessions. With dryRun it only measures.
//
// Every pruned session is tombstoned, so nothing comes back. Deleting messages
// alone would already survive an ordinary import — a transcript whose mtime and
// size are unchanged is skipped — but deleting the session row removes the very
// record of that import, and without a tombstone the next sweep would read the
// file back in full.
func Prune(db *sql.DB, mode PruneMode, ids []string, dryRun bool) (PruneStats, error) {
	var stats PruneStats
	if len(ids) == 0 {
		return stats, nil
	}

	const batch = 400 // keep the parameter list well inside SQLite's limit
	for start := 0; start < len(ids); start += batch {
		end := start + batch
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]

		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}

		where := `session_id IN (` + placeholders + `)`
		if mode == PruneToolOutput {
			where += ` AND is_tool_result = 1`
		}

		var msgs int
		var bytes sql.NullInt64
		if err := db.QueryRow(`SELECT COUNT(*), SUM(LENGTH(content_json) + LENGTH(COALESCE(content_text,'')))
			FROM messages WHERE `+where, args...).Scan(&msgs, &bytes); err != nil {
			return stats, err
		}
		stats.Messages += msgs
		stats.Bytes += bytes.Int64
		stats.Sessions += len(chunk)

		if dryRun {
			continue
		}

		tx, err := db.Begin()
		if err != nil {
			return stats, err
		}
		// The FTS index follows through its delete trigger, so the search index
		// cannot drift from the table.
		if _, err := tx.Exec(`DELETE FROM messages WHERE `+where, args...); err != nil {
			tx.Rollback()
			return stats, err
		}
		if mode == PruneSessions {
			for _, stmt := range []string{
				`DELETE FROM session_segments WHERE session_id IN (` + placeholders + `)`,
				`DELETE FROM session_identity WHERE session_id IN (` + placeholders + `)`,
				`DELETE FROM sessions WHERE session_id IN (` + placeholders + `)`,
			} {
				if _, err := tx.Exec(stmt, args...); err != nil {
					tx.Rollback()
					return stats, err
				}
			}
		} else if mode == PruneMessages {
			// The summary is what the session still is after this, so the
			// offset has to stop claiming those messages were folded in.
			if _, err := tx.Exec(`UPDATE sessions SET message_count = 0 WHERE session_id IN (`+placeholders+`)`,
				args...); err != nil {
				tx.Rollback()
				return stats, err
			}
		}
		if err := markPruned(tx, chunk, string(mode)); err != nil {
			tx.Rollback()
			return stats, err
		}
		if err := tx.Commit(); err != nil {
			return stats, err
		}
	}
	return stats, nil
}

// markPruned records that these sessions were removed deliberately.
func markPruned(tx *sql.Tx, ids []string, reason string) error {
	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO pruned_sessions (session_id, pruned_at, reason)
		VALUES (?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	now := time.Now().UTC().Format(time.RFC3339)
	for _, id := range ids {
		if _, err := stmt.Exec(id, now, reason); err != nil {
			return err
		}
	}
	return nil
}

// MarkPruned tombstones sessions outside a prune — a manual delete leaves the
// same hole, and would refill it the same way.
func MarkPruned(db *sql.DB, ids []string, reason string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if err := markPruned(tx, ids, reason); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// IsPruned reports whether a session was removed on purpose, so an importer can
// leave it alone.
func IsPruned(db *sql.DB, sessionID string) bool {
	var one int
	err := db.QueryRow(`SELECT 1 FROM pruned_sessions WHERE session_id = ?`, sessionID).Scan(&one)
	return err == nil
}

// Forget drops a tombstone, so the session may be imported again.
func Forget(db *sql.DB, sessionID string) error {
	_, err := db.Exec(`DELETE FROM pruned_sessions WHERE session_id = ?`, sessionID)
	return err
}

// CountPruned reports how many sessions are tombstoned.
func CountPruned(db *sql.DB) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM pruned_sessions`).Scan(&n)
	return n, err
}

// Vacuum rebuilds the database file. SQLite keeps deleted pages for reuse, so
// nothing is returned to the filesystem until this runs — on a large archive it
// needs time and room for a second copy, which is why it is never automatic.
func Vacuum(db *sql.DB) error {
	_, err := db.Exec(`VACUUM`)
	return err
}
