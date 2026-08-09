package db

import (
	"database/sql"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/erwin/remaimber/internal/types"
)

func TestQueryTerms(t *testing.T) {
	got := QueryTerms("the part where we set up a mail relay on the nas")
	if !reflect.DeepEqual(got, []string{"mail", "relay", "nas"}) {
		t.Errorf("got %v, want [mail relay nas] — filler must be stripped", got)
	}
	if got := QueryTerms("mail MAIL Mail"); !reflect.DeepEqual(got, []string{"mail"}) {
		t.Errorf("got %v, want [mail] — case-folded and de-duplicated", got)
	}
	// A query of pure filler must not become an empty search.
	if got := QueryTerms("the where we"); len(got) == 0 {
		t.Error("all-stopword query produced no terms; should fall back to the raw words")
	}
	if got := QueryTerms("mail-relay C++"); !reflect.DeepEqual(got, []string{"mail-relay", "c++"}) {
		t.Errorf("got %v, want punctuation preserved in terms", got)
	}
}

// seedTwoTopics builds a session with two sittings an hour apart. The first is a
// long, chatty discussion of a *rustdesk* relay; the second is a shorter one that
// is genuinely about a mail relay on the nas. The first has more raw mentions of
// "relay", so a scorer that just counts hits picks the wrong one.
func seedTwoTopics(t *testing.T) *sql.DB {
	t.Helper()
	database := testDB(t)
	insertSession(t, database, &types.Session{SessionID: "s", ProjectKey: "-p"})

	base := time.Date(2026, 8, 6, 7, 0, 0, 0, time.UTC)
	add := func(tx *sql.Tx, i int, at time.Time, role, text string) {
		if err := InsertMessage(tx, &types.Message{
			SessionID: "s", UUID: fmt.Sprintf("u%d", i), Type: role, Role: role,
			ContentText: text, ContentJSON: `{"type":"` + role + `"}`,
			Timestamp: at.Format(time.RFC3339),
		}); err != nil {
			t.Fatal(err)
		}
	}
	tx, _ := database.Begin()
	i := 0
	// Sitting one: many mentions of relay, nothing about mail or the nas.
	for n := 0; n < 12; n++ {
		add(tx, i, base.Add(time.Duration(n)*time.Minute), "user",
			"the rustdesk relay hbbr keeps dropping the relay connection")
		i++
		add(tx, i, base.Add(time.Duration(n)*time.Minute+30*time.Second), "assistant",
			"Checked the rustdesk relay logs; the relay restarted again.")
		i++
	}
	// Two hours later: the actual topic, fewer messages but full term coverage.
	later := base.Add(2 * time.Hour)
	for n := 0; n < 4; n++ {
		add(tx, i, later.Add(time.Duration(n)*time.Minute), "user",
			"set up an smtp mail relay on the nas so the printer can send mail")
		i++
		add(tx, i, later.Add(time.Duration(n)*time.Minute+30*time.Second), "assistant",
			"Deployed a postfix mail relay on the nas; it relays to the upstream server.")
		i++
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return database
}

func TestFindPassagesPrefersCoverageOverVolume(t *testing.T) {
	database := seedTwoTopics(t)

	got, err := FindPassages(database, "s", "the part where we set up a mail relay on the nas", PassageOpts{})
	if err != nil {
		t.Fatalf("FindPassages: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no passages found")
	}
	// The winner must be the 09:00 sitting: fewer mentions, but it covers every
	// term. The 07:00 one has far more "relay" hits and must lose. Here it loses
	// so heavily it falls under the relevance floor — "relay" is in every message
	// of this corpus, so the word carries no information and its cluster scores
	// near zero. Whatever survives must be ranked below the winner.
	if got[0].StartedAt[11:13] != "09" {
		t.Errorf("focus started at %s, want the 09:00 sitting — volume beat coverage",
			got[0].StartedAt)
	}
	if got[0].Coverage != 1 {
		t.Errorf("focus coverage = %.2f, want 1.0", got[0].Coverage)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Score > got[i-1].Score {
			t.Errorf("passages not ordered by score: %g then %g", got[i-1].Score, got[i].Score)
		}
	}

	// An unambiguous term must still reach its own sitting.
	got, err = FindPassages(database, "s", "rustdesk", PassageOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[0].StartedAt[11:13] != "07" {
		t.Errorf("rustdesk focus = %+v, want the 07:00 sitting", got)
	}
}

func TestFindPassagesSplitsOnSilence(t *testing.T) {
	database := seedTwoTopics(t)

	// "relay" appears in both sittings; the two-hour gap must separate them.
	got, err := FindPassages(database, "s", "relay", PassageOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d passages for a term spanning both sittings, want 2", len(got))
	}
	for _, p := range got {
		if p.StartID > p.EndID {
			t.Errorf("passage has inverted bounds: %+v", p)
		}
	}

	// A gap wider than the separation collapses them into one passage.
	got, err = FindPassages(database, "s", "relay", PassageOpts{Gap: 6 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("got %d passages with a 6h gap tolerance, want 1", len(got))
	}
}

func TestFindPassagesNoMatch(t *testing.T) {
	database := seedTwoTopics(t)
	got, err := FindPassages(database, "s", "kubernetes ingress", PassageOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("unrelated topic returned %d passages, want none", len(got))
	}
}

func TestPassageMessagesCoversTheWindow(t *testing.T) {
	database := seedTwoTopics(t)
	got, err := FindPassages(database, "s", "mail relay nas printer", PassageOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("no passage found")
	}
	msgs, err := PassageMessages(database, "s", got[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) == 0 {
		t.Fatal("passage returned no messages")
	}
	// Every message must lie inside the passage bounds, in order.
	for i, m := range msgs {
		if m.ID < got[0].StartID || m.ID > got[0].EndID {
			t.Errorf("message %d (id %d) outside passage [%d,%d]", i, m.ID, got[0].StartID, got[0].EndID)
		}
		if i > 0 && msgs[i-1].ID >= m.ID {
			t.Error("messages not in conversation order")
		}
	}
	// The passage must actually contain the topic.
	var found bool
	for _, m := range msgs {
		if containsFold(m.ContentText, "mail relay on the nas") {
			found = true
		}
	}
	if !found {
		t.Error("located passage does not contain the topic it was asked for")
	}
}

func containsFold(hay, needle string) bool {
	return len(hay) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(hay); i++ {
			if equalFold(hay[i:i+len(needle)], needle) {
				return true
			}
		}
		return false
	})()
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 32
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 32
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// seedTwoSessions puts the same topic in two conversations: one where it is the
// subject, one where it is mentioned in passing.
func seedTwoSessions(t *testing.T) *sql.DB {
	t.Helper()
	database := testDB(t)
	insertSession(t, database, &types.Session{SessionID: "work", ProjectKey: "-infra"})
	insertSession(t, database, &types.Session{SessionID: "aside", ProjectKey: "-webapp"})

	base := time.Date(2026, 8, 6, 9, 0, 0, 0, time.UTC)
	tx, _ := database.Begin()
	i := 0
	add := func(sid string, at time.Time, role, text string) {
		if err := InsertMessage(tx, &types.Message{
			SessionID: sid, UUID: fmt.Sprintf("u%d", i), Type: role, Role: role,
			ContentText: text, ContentJSON: `{"type":"` + role + `"}`,
			Timestamp: at.Format(time.RFC3339),
		}); err != nil {
			t.Fatal(err)
		}
		i++
	}
	for n := 0; n < 5; n++ {
		add("work", base.Add(time.Duration(n)*time.Minute), "user",
			"configure the smtp mail relay on the nas for the printer")
		add("work", base.Add(time.Duration(n)*time.Minute+30*time.Second), "assistant",
			"Deployed the mail relay on the nas; smtp auth verified for the printer.")
	}
	for n := 0; n < 6; n++ {
		add("aside", base.Add(time.Duration(n)*time.Minute), "user",
			"unrelated frontend work on the login page")
		add("aside", base.Add(time.Duration(n)*time.Minute+30*time.Second), "assistant",
			"Adjusted the login form and the session cookie handling.")
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return database
}

func TestFindPassagesAcrossSessions(t *testing.T) {
	database := seedTwoSessions(t)

	got, err := FindPassagesAcross(database, "mail relay on the nas", PassageFilter{}, PassageOpts{})
	if err != nil {
		t.Fatalf("FindPassagesAcross: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no passages found across sessions")
	}
	if got[0].SessionID != "work" {
		t.Errorf("best passage came from %q, want \"work\"", got[0].SessionID)
	}
	// Every passage must name its conversation, or a cross-session result is
	// unusable.
	for _, p := range got {
		if p.SessionID == "" {
			t.Errorf("passage without a session id: %+v", p)
		}
	}

	// A passage never straddles two conversations.
	for _, p := range got {
		var n int
		database.QueryRow(`SELECT COUNT(DISTINCT session_id) FROM messages WHERE id BETWEEN ? AND ?`,
			p.StartID, p.EndID).Scan(&n)
		if n > 1 {
			t.Errorf("passage [%d,%d] spans %d sessions", p.StartID, p.EndID, n)
		}
	}
}

func TestFindPassagesAcrossFilters(t *testing.T) {
	database := seedTwoSessions(t)

	// Excluding the winning conversation must fall through to the other, not
	// return it anyway.
	got, err := FindPassagesAcross(database, "relay login", PassageFilter{ExcludeSession: "work"}, PassageOpts{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range got {
		if p.SessionID == "work" {
			t.Errorf("excluded session still returned: %+v", p)
		}
	}

	// Project filter scopes the search.
	got, err = FindPassagesAcross(database, "mail relay nas", PassageFilter{Project: "infra"}, PassageOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[0].SessionID != "work" {
		t.Errorf("project filter = %+v, want the infra session", got)
	}
	if got, _ = FindPassagesAcross(database, "mail relay nas", PassageFilter{Project: "webapp"}, PassageOpts{}); len(got) != 0 {
		t.Errorf("project filter matched the wrong project: %+v", got)
	}

	// Time bounds apply.
	if got, _ = FindPassagesAcross(database, "mail relay nas", PassageFilter{Since: "2027-01-01"}, PassageOpts{}); len(got) != 0 {
		t.Errorf("since-filter past the end returned %d passages", len(got))
	}
}
