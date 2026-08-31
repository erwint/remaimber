package db

import (
	"database/sql"
	"strings"
)

// Segment summaries are the distilled account of what happened. Searching raw
// messages finds the words someone typed; searching summaries finds what the work
// turned out to be, which is usually how a conversation is remembered later.
// The index is maintained explicitly rather than by trigger, because summaries are
// rewritten repeatedly as an open segment folds and an external-content FTS table
// would need the old value on every update.

// IndexSegmentSummary adds or replaces one segment's summary in the index.
func IndexSegmentSummary(db *sql.DB, sessionID string, seq int, summary string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var rowid int64
	err = tx.QueryRow(`SELECT rowid FROM segments_fts_map WHERE session_id=? AND seq=?`,
		sessionID, seq).Scan(&rowid)
	switch {
	case err == sql.ErrNoRows:
		res, err := tx.Exec(`INSERT INTO segments_fts_map (session_id, seq) VALUES (?,?)`, sessionID, seq)
		if err != nil {
			return err
		}
		if rowid, err = res.LastInsertId(); err != nil {
			return err
		}
	case err != nil:
		return err
	default:
		if _, err := tx.Exec(`DELETE FROM segments_fts WHERE rowid=?`, rowid); err != nil {
			return err
		}
	}
	if strings.TrimSpace(summary) != "" {
		if _, err := tx.Exec(`INSERT INTO segments_fts (rowid, summary) VALUES (?,?)`, rowid, summary); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SummaryHit is a segment whose summary matched, with its conversation.
type SummaryHit struct {
	SessionID  string  `json:"session_id"`
	Seq        int     `json:"seq"`
	Summary    string  `json:"summary"`
	Score      float64 `json:"score"`
	ProjectKey string  `json:"project_key,omitempty"`
	StartedAt  string  `json:"started_at,omitempty"`
	EndedAt    string  `json:"ended_at,omitempty"`
}

// SearchSummaries finds segments whose summary matches, best first. Complements
// SearchMessages: a summary says "deployed a postfix relay on the NAS" even when
// nobody typed those words together, so intent-level recall works where literal
// matching does not.
// Terms are OR-ed and ranked, not AND-ed. A summary is a few sentences, so
// requiring every word of a remembered phrase to appear in one of them matches
// almost nothing — asking for "smtp relay on the nas" found zero summaries while
// the work was plainly there. Filler is dropped first and bm25 does the ranking,
// the same treatment passages get.
func SearchSummaries(db *sql.DB, query, agent string, limit int) ([]SummaryHit, error) {
	terms := QueryTerms(query)
	if len(terms) == 0 {
		return nil, nil
	}
	quoted := make([]string, len(terms))
	for i, t := range terms {
		quoted[i] = `"` + strings.ReplaceAll(t, `"`, "") + `"`
	}
	return searchSummaries(db, strings.Join(quoted, " OR "), agent, limit)
}

func searchSummaries(db *sql.DB, match, agent string, limit int) ([]SummaryHit, error) {
	if limit <= 0 {
		limit = 20
	}
	q := `
		SELECT m.session_id, m.seq, f.summary, bm25(segments_fts),
			COALESCE(s.project_key,'')
		FROM segments_fts f
		JOIN segments_fts_map m ON m.rowid = f.rowid
		LEFT JOIN sessions s ON s.session_id = m.session_id
		WHERE segments_fts MATCH ?`
	args := []any{match}
	if agent != "" {
		// An unsummarized session has no segment rows at all, so this filters
		// what recall can see rather than narrowing a complete set.
		q += ` AND ` + agentIs
		args = append(args, agent)
	}
	q += `
		ORDER BY bm25(segments_fts)
		LIMIT ?`
	args = append(args, limit)

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SummaryHit
	for rows.Next() {
		var h SummaryHit
		var bm float64
		if err := rows.Scan(&h.SessionID, &h.Seq, &h.Summary, &bm, &h.ProjectKey); err != nil {
			return nil, err
		}
		h.Score = -bm
		out = append(out, h)
	}
	return out, rows.Err()
}

// ReindexSummaries rebuilds the summary index from the segments table. Used by
// the backfill, and after any change that wrote summaries without indexing them.
func ReindexSummaries(db *sql.DB) (int, error) {
	rows, err := db.Query(`SELECT session_id, seq, COALESCE(summary,'') FROM session_segments
		WHERE COALESCE(summary,'') != ''`)
	if err != nil {
		return 0, err
	}
	type row struct {
		sid string
		seq int
		sum string
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.sid, &r.seq, &r.sum); err != nil {
			rows.Close()
			return 0, err
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for _, r := range all {
		if err := IndexSegmentSummary(db, r.sid, r.seq, r.sum); err != nil {
			return 0, err
		}
	}
	return len(all), nil
}

// SummaryCoverage reports how much of the archive has been summarized — the
// number that says whether recall can be trusted, and the one thing `stats` could
// not previously answer.
type SummaryCoverage struct {
	Sessions         int
	SessionsWithSum  int
	Segments         int
	SegmentsWithSum  int
	IndexedSummaries int
	TotalCostUSD     float64
	TotalLLMCalls    int
	// Unsummarized sessions split by whether summarizing them would be worth
	// anything. Most sessions that lack a summary are slash-command invocations
	// and permission prompts a few messages long; reporting those as a gap is a
	// false alarm that sends someone off to spend model calls on nothing.
	// Backlog counts only sessions with enough real content to be worth a summary.
	Backlog            int
	TooSmall           int
	OldestUnsummarized string
}

// SummarizeThreshold is the default minimum of salient messages before a session
// is worth summarizing. Shared so coverage reporting and the summarize sweep
// cannot disagree about what counts as a backlog.
const SummarizeThreshold = 6

func GetSummaryCoverage(db *sql.DB) (SummaryCoverage, error) {
	var c SummaryCoverage
	err := db.QueryRow(`
		SELECT
			(SELECT COUNT(*) FROM sessions),
			(SELECT COUNT(*) FROM sessions WHERE COALESCE(summary,'') != ''),
			(SELECT COUNT(*) FROM session_segments),
			(SELECT COUNT(*) FROM session_segments WHERE COALESCE(summary,'') != ''),
			(SELECT COUNT(*) FROM segments_fts_map),
			(SELECT COALESCE(SUM(cost_usd),0) FROM session_segments),
			(SELECT COALESCE(SUM(llm_calls),0) FROM session_segments),
			(SELECT COALESCE(MIN(COALESCE(started_at,'')),'') FROM sessions WHERE COALESCE(summary,'') = '')
	`).Scan(&c.Sessions, &c.SessionsWithSum, &c.Segments, &c.SegmentsWithSum,
		&c.IndexedSummaries, &c.TotalCostUSD, &c.TotalLLMCalls, &c.OldestUnsummarized)
	if err != nil {
		return c, err
	}

	err = db.QueryRow(`
		SELECT
			COALESCE(SUM(CASE WHEN n >= ? THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN n <  ? THEN 1 ELSE 0 END),0)
		FROM (
			SELECT COUNT(m.id) AS n
			FROM sessions s
			LEFT JOIN messages m ON m.session_id = s.session_id
				AND m.role IN ('user','assistant')
				AND m.is_tool_result = 0 AND m.is_sidechain = 0 AND m.is_compact_summary = 0
			WHERE COALESCE(s.summary,'') = ''
			GROUP BY s.session_id
		)`, SummarizeThreshold, SummarizeThreshold).Scan(&c.Backlog, &c.TooSmall)
	return c, err
}
