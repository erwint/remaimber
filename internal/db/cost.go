package db

import (
	"database/sql"
	"fmt"
	"time"
)

// Cost is recorded per segment, which is the only place it can be attributed
// honestly: a segment is the unit that was summarized, and its price is fixed
// when it freezes. Every rollup below is a grouping of those rows.

// CostTotals is the whole-archive picture, plus the span it was accrued over so
// a rate can be derived rather than guessed.
type CostTotals struct {
	USD       float64 `json:"usd"`
	Calls     int     `json:"llm_calls"`
	Segments  int     `json:"segments"`
	FirstDay  string  `json:"first_day,omitempty"`
	LastDay   string  `json:"last_day,omitempty"`
	DaysSpan  int     `json:"days_span"`
	PerDay    float64 `json:"usd_per_day"`
	PerCall   float64 `json:"usd_per_call"`
	Projected float64 `json:"usd_per_30_days"`
}

// CostRow is one line of a breakdown, whatever it is grouped by.
type CostRow struct {
	Key      string  `json:"key"`
	Label    string  `json:"label,omitempty"`
	Segments int     `json:"segments"`
	Calls    int     `json:"llm_calls"`
	USD      float64 `json:"usd"`
}

// GetCostTotals sums recorded spend. Segments summarized before cost tracking
// carry no cost and are excluded, so the rate reflects what is actually measured
// rather than being diluted by rows that could never have a price.
func GetCostTotals(db *sql.DB, since, until string) (CostTotals, error) {
	var t CostTotals
	q := `SELECT COALESCE(SUM(cost_usd),0), COALESCE(SUM(llm_calls),0), COUNT(*),
			COALESCE(MIN(substr(updated_at,1,10)),''), COALESCE(MAX(substr(updated_at,1,10)),'')
		FROM session_segments WHERE COALESCE(cost_usd,0) > 0`
	args := []any{}
	if since != "" {
		q += ` AND substr(updated_at,1,10) >= ?`
		args = append(args, since)
	}
	if until != "" {
		q += ` AND substr(updated_at,1,10) <= ?`
		args = append(args, until)
	}
	if err := db.QueryRow(q, args...).Scan(&t.USD, &t.Calls, &t.Segments, &t.FirstDay, &t.LastDay); err != nil {
		return t, err
	}

	t.DaysSpan = 1
	if a, errA := time.Parse("2006-01-02", t.FirstDay); errA == nil {
		if b, errB := time.Parse("2006-01-02", t.LastDay); errB == nil {
			if d := int(b.Sub(a).Hours()/24) + 1; d > 1 {
				t.DaysSpan = d
			}
		}
	}
	if t.Calls > 0 {
		t.PerCall = t.USD / float64(t.Calls)
	}
	t.PerDay = t.USD / float64(t.DaysSpan)
	t.Projected = t.PerDay * 30
	return t, nil
}

// CostDimension names a way of slicing spend.
type CostDimension string

const (
	CostByDay     CostDimension = "day"
	CostBySession CostDimension = "session"
	CostByProject CostDimension = "project"
)

// GetCostBreakdown groups recorded spend along one dimension, largest first —
// except by day, which reads as a series and stays chronological.
func GetCostBreakdown(db *sql.DB, by CostDimension, since, until string, limit int) ([]CostRow, error) {
	var keyExpr, labelExpr, order string
	switch by {
	case CostByDay:
		keyExpr, labelExpr, order = `substr(g.updated_at,1,10)`, `''`, `key ASC`
	case CostBySession:
		keyExpr, labelExpr, order = `g.session_id`, `COALESCE(s.project_key,'')`, `usd DESC`
	case CostByProject:
		keyExpr, labelExpr, order = `COALESCE(s.project_key,'(unknown)')`, `''`, `usd DESC`
	default:
		return nil, fmt.Errorf("unknown breakdown %q: want day, session or project", by)
	}

	q := `SELECT ` + keyExpr + ` AS key, ` + labelExpr + ` AS label,
			COUNT(*) AS segs, COALESCE(SUM(g.llm_calls),0) AS calls,
			COALESCE(SUM(g.cost_usd),0) AS usd
		FROM session_segments g
		LEFT JOIN sessions s ON s.session_id = g.session_id
		WHERE COALESCE(g.cost_usd,0) > 0`
	args := []any{}
	if since != "" {
		q += ` AND substr(g.updated_at,1,10) >= ?`
		args = append(args, since)
	}
	if until != "" {
		q += ` AND substr(g.updated_at,1,10) <= ?`
		args = append(args, until)
	}
	q += ` GROUP BY key, label ORDER BY ` + order
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CostRow
	for rows.Next() {
		var r CostRow
		if err := rows.Scan(&r.Key, &r.Label, &r.Segments, &r.Calls, &r.USD); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UnpricedSegments counts summaries written before cost tracking existed. They
// are why a total can look smaller than the work behind it, and saying so is
// better than letting the number quietly understate.
func UnpricedSegments(db *sql.DB) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM session_segments
		WHERE COALESCE(summary,'') != '' AND COALESCE(cost_usd,0) = 0`).Scan(&n)
	return n, err
}
