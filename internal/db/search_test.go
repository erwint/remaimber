package db

import (
	"strings"
	"testing"

	"github.com/erwin/remaimber/internal/types"
)

func TestQuoteFTSQuery(t *testing.T) {
	cases := map[string]string{
		"mail-relay":       `"mail-relay"`,
		"C++":              `"C++"`,
		"a.b":              `"a.b"`,
		"foo:bar":          `"foo:bar"`,
		"two terms":        `"two" "terms"`,
		"  padded  term  ": `"padded" "term"`,
		`say "hi" there`:   `"say" "hi" "there"`,
		"":                 "",
		`"`:                "",
	}
	for in, want := range cases {
		if got := QuoteFTSQuery(in); got != want {
			t.Errorf("QuoteFTSQuery(%q) = %q, want %q", in, got, want)
		}
	}
}

// Terms that are ordinary to type but are FTS5 operators or illegal barewords.
// Each one used to fail the whole search with a syntax or column error.
func TestSearchMessagesQuotesUnparseableTerms(t *testing.T) {
	database := testDB(t)
	insertSession(t, database, &types.Session{SessionID: "s", ProjectKey: "-p"})

	tx, _ := database.Begin()
	InsertMessage(tx, &types.Message{SessionID: "s", UUID: "u1", Type: "user", Role: "user",
		ContentText: "setting up the mail-relay host on the nas", ContentJSON: `{"type":"user"}`})
	InsertMessage(tx, &types.Message{SessionID: "s", UUID: "u2", Type: "user", Role: "user",
		ContentText: "the C++ build broke and I don't know why", ContentJSON: `{"type":"user"}`})
	InsertMessage(tx, &types.Message{SessionID: "s", UUID: "u3", Type: "user", Role: "user",
		ContentText: "unrelated chatter about lunch", ContentJSON: `{"type":"user"}`})
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		query string
		want  string // substring expected in the matched message
	}{
		{"mail-relay", "mail-relay"},
		{"C++", "C++"},
		{"don't", "don't"},
		{"a.b", ""},           // parses only once quoted; simply no match
		{"nas", "mail-relay"}, // plain term, no fallback needed
	} {
		res, err := SearchMessages(database, SearchFilter{Query: tc.query, Limit: 10})
		if err != nil {
			t.Errorf("search %q: unexpected error: %v", tc.query, err)
			continue
		}
		if tc.want == "" {
			continue
		}
		if len(res) == 0 {
			t.Errorf("search %q: no results", tc.query)
			continue
		}
		// Drop the snippet highlight markers before comparing, since they are
		// injected around each matched token ("C++" comes back as ">>>C<<<++").
		plain := strings.NewReplacer(">>>", "", "<<<", "").Replace(res[0].Snippet)
		if !strings.Contains(plain, tc.want) {
			t.Errorf("search %q: snippet %q does not mention %q", tc.query, plain, tc.want)
		}
	}
}

// Real FTS5 syntax must survive untouched — the fallback only fires on a parse
// failure, so an operator query is never silently downgraded to a literal.
func TestSearchMessagesPreservesFTSOperators(t *testing.T) {
	database := testDB(t)
	insertSession(t, database, &types.Session{SessionID: "s", ProjectKey: "-p"})

	tx, _ := database.Begin()
	InsertMessage(tx, &types.Message{SessionID: "s", UUID: "u1", Type: "user", Role: "user",
		ContentText: "compaction boundary handling", ContentJSON: `{"type":"user"}`})
	InsertMessage(tx, &types.Message{SessionID: "s", UUID: "u2", Type: "user", Role: "user",
		ContentText: "segment folding logic", ContentJSON: `{"type":"user"}`})
	InsertMessage(tx, &types.Message{SessionID: "s", UUID: "u3", Type: "user", Role: "user",
		ContentText: "nothing to see", ContentJSON: `{"type":"user"}`})
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	res, err := SearchMessages(database, SearchFilter{Query: "compaction OR segment", Limit: 10})
	if err != nil {
		t.Fatalf("OR query: %v", err)
	}
	if len(res) != 2 {
		t.Errorf("OR query matched %d messages, want 2 (operator was not honoured)", len(res))
	}

	// A prefix search is valid syntax and must not be quoted into a literal.
	res, err = SearchMessages(database, SearchFilter{Query: "segm*", Limit: 10})
	if err != nil {
		t.Fatalf("prefix query: %v", err)
	}
	if len(res) != 1 {
		t.Errorf("prefix query matched %d messages, want 1", len(res))
	}
}
