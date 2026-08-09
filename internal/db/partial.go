package db

import (
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/erwin/remaimber/internal/types"
)

// SegmentHit is a segment of a session together with how strongly it matched a
// search — the unit of a partial resume. Ranking by hit count is what lets a
// caller pull the two segments a topic actually lives in rather than the whole
// conversation.
type SegmentHit struct {
	Segment
	Hits      int    `json:"hits"`
	FirstSeen string `json:"first_seen,omitempty"` // timestamp of the earliest hit
}

// segmentRange matches a message id to its segment. An open segment has no
// end_id yet, so it extends to its high-water mark, and to the message itself
// when even that is unset (a segment opened but not yet folded).
const segmentRange = `g.session_id = m.session_id
	AND m.id >= g.start_id AND m.id <= COALESCE(g.end_id, g.high_water, m.id)`

// SegmentsMatching returns the segments of one session whose messages match the
// query, most hits first. The query goes through the same quote-on-parse-failure
// path as SearchMessages, so hyphenated and punctuated terms behave as literals.
//
// A session that has never been summarized has no segments and yields nothing;
// callers should fall back to the whole session rather than treating that as "no
// match".
func SegmentsMatching(db *sql.DB, sessionID, query string) ([]SegmentHit, error) {
	hits, err := segmentsMatching(db, sessionID, query)
	if err == nil || !isFTSParseError(err) {
		return hits, err
	}
	if quoted := QuoteFTSQuery(query); quoted != "" && quoted != query {
		return segmentsMatching(db, sessionID, quoted)
	}
	return hits, err
}

func segmentsMatching(db *sql.DB, sessionID, match string) ([]SegmentHit, error) {
	rows, err := db.Query(`
		SELECT g.seq, g.start_id, COALESCE(g.end_id,0), COALESCE(g.summary,''),
			g.msg_count, g.closed, COALESCE(g.reason,''),
			COUNT(*) AS hits, COALESCE(MIN(m.timestamp),'')
		FROM messages_fts
		JOIN messages m ON m.id = messages_fts.rowid
		JOIN session_segments g ON `+segmentRange+`
		WHERE messages_fts MATCH ? AND m.session_id = ?
		GROUP BY g.seq
		ORDER BY hits DESC, g.seq`, match, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SegmentHit
	for rows.Next() {
		h := SegmentHit{Segment: Segment{SessionID: sessionID}}
		var closed int
		if err := rows.Scan(&h.Seq, &h.StartID, &h.EndID, &h.Summary,
			&h.MsgCount, &closed, &h.Reason, &h.Hits, &h.FirstSeen); err != nil {
			return nil, err
		}
		h.Closed = closed == 1
		out = append(out, h)
	}
	return out, rows.Err()
}

// SegmentMessages returns the salient messages inside the given segments of a
// session, in conversation order. Applies the same filtering the summarizer uses
// — no tool results, no compaction markers, no sidechains — because the point is
// to reconstruct what was said, not to replay the agent's mechanics.
//
// Unlike the summarizer's loader this does not truncate: a partial resume is
// meant to be read as context, and the whole reason for selecting segments is
// that the caller can now afford the full text of the part that matters.
func SegmentMessages(db *sql.DB, sessionID string, seqs []int) ([]types.Message, error) {
	if len(seqs) == 0 {
		return nil, nil
	}
	ph := make([]string, len(seqs))
	args := []any{sessionID}
	for i, s := range seqs {
		ph[i] = "?"
		args = append(args, s)
	}
	args = append(args, sessionID)

	rows, err := db.Query(`
		SELECT m.id, COALESCE(m.uuid,''), COALESCE(m.role,''), m.type,
			COALESCE(m.content_text,''), COALESCE(m.timestamp,''), g.seq
		FROM messages m
		JOIN session_segments g ON `+segmentRange+`
		WHERE g.session_id = ? AND g.seq IN (`+strings.Join(ph, ",")+`)
		  AND m.session_id = ?
		  AND m.role IN ('user','assistant')
		  AND COALESCE(m.content_text,'') != ''
		  AND m.content_json NOT LIKE '%"isCompactSummary":true%'
		  AND m.content_json NOT LIKE '%"isSidechain":true%'
		  AND NOT (m.type = 'user' AND m.content_json LIKE '%"tool_result"%')
		ORDER BY m.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []types.Message
	for rows.Next() {
		var m types.Message
		var seq int
		if err := rows.Scan(&m.ID, &m.UUID, &m.Role, &m.Type, &m.ContentText, &m.Timestamp, &seq); err != nil {
			return nil, err
		}
		if m.Type == "assistant" && isToolOnly(m.ContentText) {
			continue
		}
		m.SegmentSeq = seq
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

// ParseSegmentSpec turns a "3", "3,4" or "3-5" selection into segment numbers.
// Ranges are inclusive; the result is sorted and de-duplicated.
func ParseSegmentSpec(spec string) ([]int, error) {
	seen := map[int]bool{}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(strings.TrimSpace(lo))
		if err != nil {
			return nil, fmt.Errorf("bad segment %q: want a number, range (3-5) or list (3,4)", part)
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(strings.TrimSpace(hi)); err != nil {
				return nil, fmt.Errorf("bad segment range %q", part)
			}
		}
		if b < a {
			a, b = b, a
		}
		for i := a; i <= b; i++ {
			seen[i] = true
		}
	}
	if len(seen) == 0 {
		return nil, fmt.Errorf("no segments selected")
	}
	out := make([]int, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Ints(out)
	return out, nil
}

// WithNeighbours widens a selection by pad segments on each side, clamped to
// [0, maxSeq]. A topic usually starts just before the first line that names it,
// so one segment of lead-in often makes a partial resume make sense on its own.
func WithNeighbours(seqs []int, pad, maxSeq int) []int {
	if pad <= 0 || len(seqs) == 0 {
		return seqs
	}
	seen := map[int]bool{}
	for _, s := range seqs {
		for i := s - pad; i <= s+pad; i++ {
			if i >= 0 && i <= maxSeq {
				seen[i] = true
			}
		}
	}
	out := make([]int, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Ints(out)
	return out
}
