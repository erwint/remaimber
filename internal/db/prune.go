package db

import (
	"database/sql"
	"fmt"
	"strings"
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
// Deleting messages does not bring them back on the next import: a file whose
// mtime and size are unchanged is skipped entirely, so the pruned rows stay
// pruned unless someone forces a re-import. Removing whole sessions is the
// exception — the session row is what records that a file was imported, so a
// transcript still on disk would be read again from scratch. Callers pass only
// sessions whose transcripts are gone when using PruneSessions.
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
		if err := tx.Commit(); err != nil {
			return stats, err
		}
	}
	return stats, nil
}

// Vacuum rebuilds the database file. SQLite keeps deleted pages for reuse, so
// nothing is returned to the filesystem until this runs — on a large archive it
// needs time and room for a second copy, which is why it is never automatic.
func Vacuum(db *sql.DB) error {
	_, err := db.Exec(`VACUUM`)
	return err
}
