package db

import (
	"database/sql"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/erwin/remaimber/internal/types"
)

// A Passage is a contiguous stretch of one conversation that is about a topic —
// the unit a partial resume actually wants. Segments are the wrong granularity
// on their own: a segment is capped by message count, not by subject, so a single
// segment can hold five hours across several unrelated topics. A passage is
// derived from where the matching messages actually fall.
type Passage struct {
	// SessionID names the conversation. Always set, so a passage stays
	// self-describing when results from several conversations are ranked together.
	SessionID string  `json:"session_id"`
	StartID   int64   `json:"start_id"`
	EndID     int64   `json:"end_id"`
	StartedAt string  `json:"started_at,omitempty"`
	EndedAt   string  `json:"ended_at,omitempty"`
	Hits      int     `json:"hits"`
	Score     float64 `json:"score"`
	Coverage  float64 `json:"coverage"` // fraction of query terms present
	Segments  []int   `json:"segments"` // segments this passage spans
	Snippet   string  `json:"snippet,omitempty"`
}

// PassageOpts tunes passage discovery. The zero value is sensible.
type PassageOpts struct {
	Candidates int           // top-ranked messages considered (default 60)
	Gap        time.Duration // silence that separates two passages (default 30m)
	Lead       int           // messages of run-up to include (default 4)
	Trail      int           // messages of follow-through to include (default 4)
}

func (o PassageOpts) withDefaults() PassageOpts {
	if o.Candidates <= 0 {
		o.Candidates = 60
	}
	if o.Gap <= 0 {
		o.Gap = 30 * time.Minute
	}
	if o.Lead <= 0 {
		o.Lead = 4
	}
	if o.Trail <= 0 {
		o.Trail = 4
	}
	return o
}

var wordRe = regexp.MustCompile(`[\w.+#/-]+`)

// stopWords are dropped from a topic before searching. A request arrives as
// prose ("the part where we set up a mail relay on the nas"), and matching on
// its filler both buries the real terms and lets any long stretch of chatter
// outscore the passage that is actually on topic.
var stopWords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`a an and are as at be but by can could did do does
		for from had has have how if in into is it its me my no not of on or our over so
		than that the their them then there these they this to too us was we were what
		when where which who why will with would you your part just get got make made set
		up use using want need like also only some thing things stuff done again still now
		here about back really much many very`) {
		stopWords[w] = true
	}
}

// QueryTerms reduces a topic to the words worth searching for, lowercased and
// de-duplicated. Falls back to the raw words when a query is nothing but filler,
// so a search never silently becomes empty.
func QueryTerms(q string) []string {
	var all, kept []string
	seen := map[string]bool{}
	for _, w := range wordRe.FindAllString(strings.ToLower(q), -1) {
		if len(w) < 2 || seen[w] {
			continue
		}
		seen[w] = true
		all = append(all, w)
		if !stopWords[w] {
			kept = append(kept, w)
		}
	}
	if len(kept) == 0 {
		return all
	}
	return kept
}

// FindPassages locates the stretches of a session that are about a topic, best
// first. Terms are OR-ed and ranked by bm25 rather than AND-ed, so a topic phrased
// as prose still matches; the ranking, not the filter, does the discriminating.
//
// Scoring deliberately does not sum every hit. A cluster's strength is its few
// best messages, multiplied by the square of how many distinct query terms it
// covers. Summing rewards mere length — on a real archive that let a long
// discussion of an unrelated "relay" outrank the passage that actually covered
// "mail relay nas", because it simply contained more weak mentions. Coverage is
// what separates a passage about the topic from one that name-drops a word of it.
func FindPassages(db *sql.DB, sessionID, query string, opts PassageOpts) ([]Passage, error) {
	return findPassages(db, query, PassageFilter{SessionID: sessionID}, opts)
}

// PassageFilter narrows which conversations are searched. The zero value searches
// the whole archive.
type PassageFilter struct {
	SessionID string // one session; empty searches all
	// ExcludeSession drops one conversation from the results — normally the
	// live one, since a session that is currently discussing a topic would
	// otherwise outrank the older conversation where the work was done.
	ExcludeSession string
	Project        string // substring match on project key
	Repo           string // exact repo identity, across worktrees
	Subpath        string // exact monorepo subpath
	Since          string // ISO 8601 bounds on message timestamps
	Until          string
}

// FindPassagesAcross locates the stretches of *any* archived conversation that
// are about a topic, best first. This is the entry point for "find the part where
// we did X" when which conversation it was in is exactly what has been forgotten.
//
// Ranking is global: passages from different sessions compete directly, so a
// short, dense discussion in one conversation outranks a passing mention in
// another. Candidates default higher than the single-session case, because the
// budget is now shared across every conversation in the archive.
func FindPassagesAcross(db *sql.DB, query string, f PassageFilter, opts PassageOpts) ([]Passage, error) {
	if opts.Candidates <= 0 {
		opts.Candidates = 400
	}
	return findPassages(db, query, f, opts)
}

func findPassages(db *sql.DB, query string, f PassageFilter, opts PassageOpts) ([]Passage, error) {
	opts = opts.withDefaults()
	terms := QueryTerms(query)
	if len(terms) == 0 {
		return nil, nil
	}
	quoted := make([]string, len(terms))
	for i, t := range terms {
		quoted[i] = `"` + strings.ReplaceAll(t, `"`, "") + `"`
	}

	q := `
		SELECT m.session_id, m.id, COALESCE(m.timestamp,''), bm25(messages_fts),
			lower(substr(COALESCE(m.content_text,''),1,4000)),
			substr(COALESCE(m.content_text,''),1,160)
		FROM messages_fts
		JOIN messages m ON m.id = messages_fts.rowid`
	if f.Project != "" || f.Repo != "" || f.Subpath != "" {
		q += `
		JOIN sessions s ON s.session_id = m.session_id
		LEFT JOIN session_identity si ON si.session_id = m.session_id`
	}
	// Tool-result turns are excluded, as everywhere else that reads conversation
	// content. Beyond being machine noise, they close a feedback loop specific to
	// this tool: running a search archives its own output, so the results become
	// messages that match the same query next time and can outrank the
	// conversation being looked for. Compaction markers are summaries of earlier
	// turns, not turns, and would double-count whatever they describe.
	q += `
		WHERE messages_fts MATCH ?
		  AND m.content_json NOT LIKE '%"isSidechain":true%'
		  AND m.content_json NOT LIKE '%"isCompactSummary":true%'
		  AND NOT (m.type = 'user' AND m.content_json LIKE '%"tool_result"%')`
	args := []any{strings.Join(quoted, " OR ")}
	if f.SessionID != "" {
		q += ` AND m.session_id = ?`
		args = append(args, f.SessionID)
	}
	if f.ExcludeSession != "" {
		q += ` AND m.session_id != ?`
		args = append(args, f.ExcludeSession)
	}
	if f.Project != "" {
		q += ` AND s.project_key LIKE ?`
		args = append(args, "%"+f.Project+"%")
	}
	if f.Repo != "" {
		q += ` AND si.repo_id = ?`
		args = append(args, f.Repo)
	}
	if f.Subpath != "" {
		q += ` AND si.subpath = ?`
		args = append(args, f.Subpath)
	}
	if f.Since != "" {
		q += ` AND m.timestamp >= ?`
		args = append(args, f.Since)
	}
	if f.Until != "" {
		q += ` AND m.timestamp <= ?`
		args = append(args, f.Until)
	}
	q += ` ORDER BY bm25(messages_fts) LIMIT ?`
	args = append(args, opts.Candidates)

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type hit struct {
		session string
		id      int64
		ts      string
		score   float64 // bm25 is negative; kept negated so bigger is better
		body    string
		snippet string
	}
	bySession := map[string][]hit{}
	var order []string
	for rows.Next() {
		var h hit
		var bm float64
		if err := rows.Scan(&h.session, &h.id, &h.ts, &bm, &h.body, &h.snippet); err != nil {
			return nil, err
		}
		h.score = -bm
		if _, seen := bySession[h.session]; !seen {
			order = append(order, h.session)
		}
		bySession[h.session] = append(bySession[h.session], h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(bySession) == 0 {
		return nil, nil
	}

	// Cluster within each conversation: a silence separates two sittings, and a
	// session boundary always does.
	var groups [][]hit
	for _, sid := range order {
		hits := bySession[sid]
		sort.Slice(hits, func(i, j int) bool { return hits[i].id < hits[j].id })
		cur := []hit{hits[0]}
		for i := 1; i < len(hits); i++ {
			if silenceBetween(hits[i-1].ts, hits[i].ts) > opts.Gap {
				groups = append(groups, cur)
				cur = nil
			}
			cur = append(cur, hits[i])
		}
		groups = append(groups, cur)
	}

	var out []Passage
	for _, g := range groups {
		strengths := make([]float64, 0, len(g))
		var body strings.Builder
		for _, h := range g {
			strengths = append(strengths, h.score)
			body.WriteString(h.body)
			body.WriteByte(' ')
		}
		sort.Sort(sort.Reverse(sort.Float64Slice(strengths)))
		var strength float64
		for i := 0; i < len(strengths) && i < 5; i++ {
			strength += strengths[i]
		}
		blob := body.String()
		covered := 0
		for _, t := range terms {
			if strings.Contains(blob, t) {
				covered++
			}
		}
		cov := float64(covered) / float64(len(terms))

		best := g[0]
		for _, h := range g {
			if h.score > best.score {
				best = h
			}
		}
		out = append(out, Passage{
			SessionID: g[0].session,
			StartID:   g[0].id,
			EndID:     g[len(g)-1].id,
			StartedAt: g[0].ts,
			EndedAt:   g[len(g)-1].ts,
			Hits:      len(g),
			Score:     strength * cov * cov,
			Coverage:  cov,
			Snippet:   strings.TrimSpace(strings.ReplaceAll(best.snippet, "\n", " ")),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })

	// Drop the long tail. Searching a whole archive turns up clusters that share
	// one incidental word with the topic and score orders of magnitude below the
	// real answer; listing them as candidates implies a choice that isn't there.
	// Relative, not absolute, so a weak-but-best match still comes back.
	if len(out) > 1 {
		floor := out[0].Score / 20
		kept := out[:1]
		for _, p := range out[1:] {
			if p.Score >= floor {
				kept = append(kept, p)
			}
		}
		out = kept
	}

	// Widen each passage to take in the run-up and the follow-through, then
	// record which segments it touches.
	for i := range out {
		if err := widen(db, out[i].SessionID, &out[i], opts.Lead, opts.Trail, opts.Gap); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// silenceBetween is the time between two messages, in either order; zero when
// either lacks a timestamp, so missing times keep a run together rather than
// shattering it.
func silenceBetween(a, b string) time.Duration {
	ta, ea := time.Parse(time.RFC3339, a)
	tb, eb := time.Parse(time.RFC3339, b)
	if ea != nil || eb != nil {
		return 0
	}
	if d := tb.Sub(ta); d > 0 {
		return d
	} else if d < 0 {
		return -d
	}
	return 0
}

// widen extends a passage by a few salient messages either side and fills in its
// time bounds and covering segments. The message that first raises a topic is
// often just before the first keyword match, and the conclusion just after the
// last, so the raw hit span usually starts and ends mid-thought.
//
// Widening stops at a silence. Adjacent in message order is not adjacent in
// conversation: the messages before a passage may be hours earlier, from an
// unrelated sitting, and dragging those in both misreports when the passage
// happened and pads the context with something else entirely.
func widen(db *sql.DB, sessionID string, p *Passage, lead, trail int, gap time.Duration) error {
	edge := func(sqlText string, from int64, limit int, better func(int64) bool) error {
		rows, err := db.Query(sqlText, sessionID, from, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		prev := from
		var prevTS string
		db.QueryRow(`SELECT COALESCE(timestamp,'') FROM messages WHERE id=?`, from).Scan(&prevTS)
		for rows.Next() {
			var id int64
			var ts string
			if err := rows.Scan(&id, &ts); err != nil {
				return err
			}
			if silenceBetween(prevTS, ts) > gap {
				break
			}
			prev, prevTS = id, ts
		}
		if better(prev) {
			return nil
		}
		return rows.Err()
	}

	if err := edge(`SELECT id, COALESCE(timestamp,'') FROM messages
		WHERE session_id=? AND id < ? AND role IN ('user','assistant')
		  AND COALESCE(content_text,'') != ''
		ORDER BY id DESC LIMIT ?`, p.StartID, lead, func(id int64) bool {
		if id < p.StartID {
			p.StartID = id
		}
		return false
	}); err != nil {
		return err
	}
	if err := edge(`SELECT id, COALESCE(timestamp,'') FROM messages
		WHERE session_id=? AND id > ? AND role IN ('user','assistant')
		  AND COALESCE(content_text,'') != ''
		ORDER BY id LIMIT ?`, p.EndID, trail, func(id int64) bool {
		if id > p.EndID {
			p.EndID = id
		}
		return false
	}); err != nil {
		return err
	}

	db.QueryRow(`SELECT COALESCE(MIN(timestamp),''), COALESCE(MAX(timestamp),'')
		FROM messages WHERE session_id=? AND id BETWEEN ? AND ? AND COALESCE(timestamp,'') != ''`,
		sessionID, p.StartID, p.EndID).Scan(&p.StartedAt, &p.EndedAt)

	rows, err := db.Query(`SELECT DISTINCT g.seq
		FROM session_segments g
		WHERE g.session_id = ?
		  AND g.start_id <= ? AND COALESCE(g.end_id, g.high_water, g.start_id) >= ?
		ORDER BY g.seq`, sessionID, p.EndID, p.StartID)
	if err != nil {
		return err
	}
	defer rows.Close()
	p.Segments = nil
	for rows.Next() {
		var seq int
		if err := rows.Scan(&seq); err != nil {
			return err
		}
		p.Segments = append(p.Segments, seq)
	}
	return rows.Err()
}

// PassageMessages returns the salient messages of a passage, in order, with the
// same filtering the summarizer uses.
func PassageMessages(db *sql.DB, sessionID string, p Passage) ([]types.Message, error) {
	rows, err := db.Query(`
		SELECT id, COALESCE(uuid,''), COALESCE(role,''), type,
			COALESCE(content_text,''), COALESCE(timestamp,'')
		FROM messages
		WHERE session_id = ? AND id BETWEEN ? AND ?
		  AND role IN ('user','assistant')
		  AND COALESCE(content_text,'') != ''
		  AND content_json NOT LIKE '%"isCompactSummary":true%'
		  AND content_json NOT LIKE '%"isSidechain":true%'
		  AND NOT (type = 'user' AND content_json LIKE '%"tool_result"%')
		ORDER BY id`, sessionID, p.StartID, p.EndID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var msgs []types.Message
	for rows.Next() {
		var m types.Message
		if err := rows.Scan(&m.ID, &m.UUID, &m.Role, &m.Type, &m.ContentText, &m.Timestamp); err != nil {
			return nil, err
		}
		if m.Type == "assistant" && isToolOnly(m.ContentText) {
			continue
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}
