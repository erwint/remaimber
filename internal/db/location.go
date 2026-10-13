package db

import (
	"database/sql"
	"strings"

	"github.com/erwint/remaimber/internal/types"
)

// SessionLocations returns the recorded location of each given session, in
// one query. Sessions not in the archive are absent from the map.
func SessionLocations(db *sql.DB, ids []string) (map[string]*types.Location, error) {
	out := map[string]*types.Location{}
	seen := map[string]bool{}
	var uniq []any
	for _, id := range ids {
		if id != "" && !seen[id] {
			seen[id] = true
			uniq = append(uniq, id)
		}
	}
	if len(uniq) == 0 {
		return out, nil
	}
	rows, err := db.Query(`
		SELECT s.session_id, COALESCE(NULLIF(s.agent,''),'claude'), COALESCE(s.origin,''),
			COALESCE(NULLIF(si.cwd,''), s.cwd, ''), COALESCE(si.worktree_root,''),
			COALESCE(si.repo_id,''), COALESCE(s.git_branch,'')
		FROM sessions s
		LEFT JOIN session_identity si USING(session_id)
		WHERE s.session_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(uniq)), ",")+`)`, uniq...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		l := &types.Location{}
		if err := rows.Scan(&id, &l.Agent, &l.Origin, &l.CWD, &l.WorktreeRoot, &l.RepoID, &l.Branch); err != nil {
			return nil, err
		}
		out[id] = l
	}
	return out, rows.Err()
}
