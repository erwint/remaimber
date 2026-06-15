package db

import "database/sql"

// ActivePathSet returns the set of message uuids on the active conversation path
// within the current live context: the ancestors of the latest main-line message,
// walked via parent_uuid. Because Claude Code resets the parent chain at every
// context compaction, this naturally covers only the span since the last
// compaction — which is exactly the region a rewind/restore can touch (you cannot
// rewind into already-compacted, frozen history). After a rewind, the abandoned
// branch's messages are absent from this set. Sidechain (sub-agent) messages are
// excluded. Returns nil ("treat as linear") when there is nothing to walk.
func ActivePathSet(db *sql.DB, sessionID string) (map[string]bool, error) {
	rows, err := db.Query(`SELECT COALESCE(uuid,''), COALESCE(parent_uuid,'')
		FROM messages
		WHERE session_id = ? AND content_json NOT LIKE '%"isSidechain":true%'
		ORDER BY id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	parent := make(map[string]string)
	var headUUID string // last row = max id = current active head
	for rows.Next() {
		var uuid, p string
		if err := rows.Scan(&uuid, &p); err != nil {
			return nil, err
		}
		if uuid == "" {
			continue
		}
		parent[uuid] = p
		headUUID = uuid
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if headUUID == "" {
		return nil, nil
	}

	onPath := make(map[string]bool)
	for cur := headUUID; cur != ""; {
		if onPath[cur] {
			break // cycle guard
		}
		onPath[cur] = true
		cur = parent[cur] // "" at the compaction boundary / root / if unknown
	}
	return onPath, nil
}
