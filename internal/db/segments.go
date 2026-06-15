package db

import (
	"database/sql"
	"time"

	"github.com/erwin/remaimber/internal/types"
)

// DefaultSegmentCap is the soft-split size (in content messages): when the open
// segment grows past this without a compaction, it is committed and a new one
// begins, so segments stay bounded and summaries stay sharp.
const DefaultSegmentCap = 60

// Segment is one summary chunk of a conversation. All segments but the last
// (open) one are closed/immutable. Boundaries carry uuids so a later phase can
// validate them against the active conversation path after a rewind.
type Segment struct {
	SessionID string `json:"-"`
	Seq       int    `json:"seq"`
	StartID   int64  `json:"start_id"`
	EndID     int64  `json:"end_id"`
	StartUUID string `json:"-"`
	EndUUID   string `json:"-"`
	Summary   string `json:"summary"`
	MsgCount  int    `json:"msg_count"`
	HighWater int64  `json:"-"`
	Closed    bool   `json:"closed"`
	Reason    string `json:"reason,omitempty"`
}

// SegSpan is a planned segment boundary (no summary yet) — the pure output of
// computeSegmentPlan over a session's content and compaction points.
type SegSpan struct {
	StartID   int64
	EndID     int64
	StartUUID string
	EndUUID   string
	Count     int
	Closed    bool
	Reason    string
}

// PlanSegments partitions content (salient messages, id-ordered) into segments,
// splitting at each compaction id and whenever a segment reaches sizeCap. Every
// segment but the last is closed; the last is open unless a compaction occurs
// after all content (then it too is closed). Pure + testable.
func PlanSegments(content []types.Message, compactionIDs []int64, sizeCap int) []SegSpan {
	if sizeCap <= 0 {
		sizeCap = DefaultSegmentCap
	}
	var spans []SegSpan
	var cur *SegSpan
	ci := 0

	closeCur := func(reason string) {
		if cur != nil && cur.Count > 0 {
			cur.Closed = true
			cur.Reason = reason
			spans = append(spans, *cur)
		}
		cur = nil
	}

	for _, m := range content {
		// A compaction strictly before this message closes the current segment.
		for ci < len(compactionIDs) && compactionIDs[ci] < m.ID {
			closeCur("compaction")
			ci++
		}
		if cur != nil && cur.Count >= sizeCap {
			closeCur("sizecap")
		}
		if cur == nil {
			cur = &SegSpan{StartID: m.ID, StartUUID: m.UUID}
		}
		cur.EndID = m.ID
		cur.EndUUID = m.UUID
		cur.Count++
	}

	if cur != nil {
		// Any compaction id left is after the last content message, so it closes
		// the final segment (a new, currently-empty open segment will form once
		// post-compaction content arrives).
		if ci < len(compactionIDs) {
			cur.Closed = true
			cur.Reason = "compaction"
		}
		spans = append(spans, *cur)
	}
	return spans
}

// SegmentContent returns a session's salient content messages with id > afterID,
// id-ordered, for segmentation and folding. It excludes tool-result turns,
// compaction summaries (boundary markers, not content), and sidechain (sub-agent)
// traffic; tool-only assistant turns are dropped in Go. ID and UUID are populated
// so segment boundaries can be keyed by uuid.
func SegmentContent(db *sql.DB, sessionID string, afterID int64) ([]types.Message, error) {
	rows, err := db.Query(`SELECT id, COALESCE(uuid,''), COALESCE(role,''), type,
			substr(COALESCE(content_text,''), 1, ?)
		FROM messages
		WHERE session_id = ? AND id > ? AND role IN ('user','assistant')
		  AND COALESCE(content_text,'') != ''
		  AND content_json NOT LIKE '%"isCompactSummary":true%'
		  AND content_json NOT LIKE '%"isSidechain":true%'
		  AND NOT (type = 'user' AND content_json LIKE '%"tool_result"%')
		ORDER BY id`, summaryTextCap, sessionID, afterID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []types.Message
	for rows.Next() {
		var m types.Message
		if err := rows.Scan(&m.ID, &m.UUID, &m.Role, &m.Type, &m.ContentText); err != nil {
			return nil, err
		}
		if m.Type == "assistant" && isToolOnly(m.ContentText) {
			continue
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

// CompactionIDs returns the ids of a session's compaction-summary messages
// (the boundary markers), sorted ascending.
func CompactionIDs(db *sql.DB, sessionID string) ([]int64, error) {
	rows, err := db.Query(`SELECT id FROM messages
		WHERE session_id = ? AND content_json LIKE '%"isCompactSummary":true%'
		ORDER BY id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// GetSegments returns a session's stored segments ordered by seq.
func GetSegments(db *sql.DB, sessionID string) ([]Segment, error) {
	rows, err := db.Query(`SELECT seq, start_id, COALESCE(end_id,0), COALESCE(start_uuid,''),
			COALESCE(end_uuid,''), COALESCE(summary,''), msg_count, high_water, closed, COALESCE(reason,'')
		FROM session_segments WHERE session_id = ? ORDER BY seq`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var segs []Segment
	for rows.Next() {
		s := Segment{SessionID: sessionID}
		var closed int
		if err := rows.Scan(&s.Seq, &s.StartID, &s.EndID, &s.StartUUID, &s.EndUUID,
			&s.Summary, &s.MsgCount, &s.HighWater, &closed, &s.Reason); err != nil {
			return nil, err
		}
		s.Closed = closed == 1
		segs = append(segs, s)
	}
	return segs, rows.Err()
}

// UpsertSegment stores a segment.
func UpsertSegment(db *sql.DB, s *Segment) error {
	closed := 0
	if s.Closed {
		closed = 1
	}
	_, err := db.Exec(`
		INSERT INTO session_segments
			(session_id, seq, start_id, end_id, start_uuid, end_uuid, summary, msg_count, high_water, closed, reason, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(session_id, seq) DO UPDATE SET
			start_id=excluded.start_id, end_id=excluded.end_id,
			start_uuid=excluded.start_uuid, end_uuid=excluded.end_uuid,
			summary=excluded.summary, msg_count=excluded.msg_count, high_water=excluded.high_water,
			closed=excluded.closed, reason=excluded.reason, updated_at=excluded.updated_at`,
		s.SessionID, s.Seq, s.StartID, s.EndID, nullStr(s.StartUUID), nullStr(s.EndUUID),
		nullStr(s.Summary), s.MsgCount, s.HighWater, closed, nullStr(s.Reason),
		time.Now().UTC().Format(time.RFC3339))
	return err
}

// DeleteSegmentsFrom removes a session's segments with seq >= fromSeq (used to
// drop stale trailing segments during reconciliation).
func DeleteSegmentsFrom(db *sql.DB, sessionID string, fromSeq int) error {
	_, err := db.Exec(`DELETE FROM session_segments WHERE session_id = ? AND seq >= ?`, sessionID, fromSeq)
	return err
}
