package db

import (
	"database/sql"
	"time"
)

// Sync directions recorded in remote_objects.
const (
	SyncPull = "pull"
	SyncPush = "push"
)

// RemoteETags returns the etag last synced for every key of one origin and
// direction, in one query: a listing of a few thousand objects is compared
// against it in memory rather than with a query per object.
func RemoteETags(db *sql.DB, origin, direction string) (map[string]string, error) {
	rows, err := db.Query(`SELECT key, etag FROM remote_objects WHERE origin = ? AND direction = ?`,
		origin, direction)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, e string
		if err := rows.Scan(&k, &e); err != nil {
			return nil, err
		}
		out[k] = e
	}
	return out, rows.Err()
}

// RecordRemoteObject stores the etag of an object once it has been synced. It
// is written only after the object was imported (or uploaded), so a failure
// leaves the old etag in place and the next sync tries again.
func RecordRemoteObject(db *sql.DB, origin, direction, key, etag string, size int64, source string) error {
	_, err := db.Exec(`INSERT INTO remote_objects (origin, direction, key, etag, size, source, synced_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(origin, direction, key) DO UPDATE SET
			etag = excluded.etag, size = excluded.size, source = excluded.source,
			synced_at = excluded.synced_at`,
		origin, direction, key, etag, size, source, time.Now().UTC().Format(time.RFC3339))
	return err
}

// SessionOrigin reports whether a session is in the archive and, if so, which
// machine it came from ("" for this one).
func SessionOrigin(db *sql.DB, sessionID string) (origin string, exists bool) {
	var o sql.NullString
	if err := db.QueryRow(`SELECT origin FROM sessions WHERE session_id = ?`, sessionID).Scan(&o); err != nil {
		return "", false
	}
	return o.String, true
}

// OriginStat summarizes what has been synced from or to one origin.
type OriginStat struct {
	Origin    string `json:"origin"`
	Direction string `json:"direction"`
	Objects   int    `json:"objects"`
	Sessions  int    `json:"sessions"`
	Source    string `json:"source"`
	LastSync  string `json:"last_sync"`
}

// SyncStatus lists every origin sync has touched, newest first.
func SyncStatus(db *sql.DB) ([]OriginStat, error) {
	rows, err := db.Query(`
		SELECT r.origin, r.direction, COUNT(*), MAX(r.synced_at),
			(SELECT source FROM remote_objects r2 WHERE r2.origin = r.origin AND r2.direction = r.direction
				ORDER BY synced_at DESC LIMIT 1),
			CASE WHEN r.direction = 'pull'
				THEN (SELECT COUNT(*) FROM sessions s WHERE s.origin = r.origin) ELSE 0 END
		FROM remote_objects r
		GROUP BY r.origin, r.direction
		ORDER BY MAX(r.synced_at) DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OriginStat
	for rows.Next() {
		var st OriginStat
		var src sql.NullString
		if err := rows.Scan(&st.Origin, &st.Direction, &st.Objects, &st.LastSync, &src, &st.Sessions); err != nil {
			return nil, err
		}
		st.Source = src.String
		out = append(out, st)
	}
	return out, rows.Err()
}
