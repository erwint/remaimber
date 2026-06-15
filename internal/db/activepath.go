package db

import (
	"database/sql"
	"sort"
)

// ActivePathSet returns the set of message uuids on the active conversation path:
// the ancestors of the latest main-line message via parent_uuid, bridged across
// context compactions.
//
// Two facts make this subtle. (1) Claude resets the parent chain at every
// compaction, so a plain ancestor-walk stops at the last compaction even though
// the pre-compaction content is still live. (2) A rewind/restore can target ANY
// earlier point — including before a compaction — abandoning the branch after it.
//
// Bridging handles both: whenever the walk reaches a chain root (empty parent)
// that has earlier content, it jumps to the highest-id earlier main-line message
// and continues. In the normal case this stitches the pre-compaction span back in.
// A rewind that bypasses a compaction never lands on that compaction's reset root
// (the active branch flows through the rewind target, whose parent is intact), so
// the abandoned content — and the bypassed compaction — are simply never visited.
//
// Sidechain (sub-agent) messages are excluded. Returns nil ("treat as linear")
// when there is nothing to walk.
func ActivePathSet(db *sql.DB, sessionID string) (map[string]bool, error) {
	rows, err := db.Query(`SELECT id, COALESCE(uuid,''), COALESCE(parent_uuid,'')
		FROM messages
		WHERE session_id = ? AND content_json NOT LIKE '%"isSidechain":true%'
		ORDER BY id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type ml struct {
		id   int64
		uuid string
	}
	var ordered []ml // by id ascending
	parent := make(map[string]string)
	idOf := make(map[string]int64)
	var headUUID string
	for rows.Next() {
		var id int64
		var uuid, p string
		if err := rows.Scan(&id, &uuid, &p); err != nil {
			return nil, err
		}
		if uuid == "" {
			continue
		}
		parent[uuid] = p
		idOf[uuid] = id
		ordered = append(ordered, ml{id, uuid})
		headUUID = uuid
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if headUUID == "" {
		return nil, nil
	}

	// maxBefore returns the uuid of the highest-id main-line message with id < x.
	maxBefore := func(x int64) string {
		i := sort.Search(len(ordered), func(i int) bool { return ordered[i].id >= x })
		if i == 0 {
			return ""
		}
		return ordered[i-1].uuid
	}

	onPath := make(map[string]bool)
	for cur := headUUID; cur != ""; {
		if onPath[cur] {
			break // cycle guard
		}
		onPath[cur] = true
		if p := parent[cur]; p != "" {
			cur = p
			continue
		}
		// Chain root: bridge across a compaction reset to the prior span, if any.
		cur = maxBefore(idOf[cur])
	}
	return onPath, nil
}

// CompactionBoundary is a compaction marker's id and uuid.
type CompactionBoundary struct {
	ID   int64
	UUID string
}

// CompactionBoundaries returns a session's compaction markers (id + uuid), sorted
// by id. The uuid lets callers drop boundaries abandoned by a rewind.
func CompactionBoundaries(db *sql.DB, sessionID string) ([]CompactionBoundary, error) {
	rows, err := db.Query(`SELECT id, COALESCE(uuid,'') FROM messages
		WHERE session_id = ? AND content_json LIKE '%"isCompactSummary":true%'
		ORDER BY id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CompactionBoundary
	for rows.Next() {
		var b CompactionBoundary
		if err := rows.Scan(&b.ID, &b.UUID); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
