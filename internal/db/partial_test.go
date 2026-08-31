package db

import (
	"database/sql"
	"reflect"
	"testing"

	"github.com/erwint/remaimber/internal/types"
)

func TestParseSegmentSpec(t *testing.T) {
	ok := map[string][]int{
		"3":       {3},
		"3,4":     {3, 4},
		"3-5":     {3, 4, 5},
		"5-3":     {3, 4, 5}, // reversed range is tolerated
		"0,3-5,9": {0, 3, 4, 5, 9},
		" 3 , 4 ": {3, 4},
		"4,4,4":   {4}, // de-duplicated
	}
	for in, want := range ok {
		got, err := ParseSegmentSpec(in)
		if err != nil {
			t.Errorf("ParseSegmentSpec(%q): %v", in, err)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("ParseSegmentSpec(%q) = %v, want %v", in, got, want)
		}
	}
	for _, bad := range []string{"", "abc", "3-x", ","} {
		if _, err := ParseSegmentSpec(bad); err == nil {
			t.Errorf("ParseSegmentSpec(%q) should have failed", bad)
		}
	}
}

func TestWithNeighbours(t *testing.T) {
	if got := WithNeighbours([]int{3, 4}, 1, 20); !reflect.DeepEqual(got, []int{2, 3, 4, 5}) {
		t.Errorf("pad 1 = %v, want [2 3 4 5]", got)
	}
	// Clamped at both ends.
	if got := WithNeighbours([]int{0}, 2, 1); !reflect.DeepEqual(got, []int{0, 1}) {
		t.Errorf("clamp = %v, want [0 1]", got)
	}
	// No padding is a pass-through.
	if got := WithNeighbours([]int{7}, 0, 20); !reflect.DeepEqual(got, []int{7}) {
		t.Errorf("pad 0 = %v, want [7]", got)
	}
}

// seedSegmented builds a session of three segments: 0 talks about auth, 1 and 2
// about the mail relay, with 2 holding most of the discussion.
func seedSegmented(t *testing.T) *sql.DB {
	t.Helper()
	database := testDB(t)
	insertSession(t, database, &types.Session{SessionID: "s", ProjectKey: "-p"})

	texts := []struct {
		seg  int
		role string
		text string
		json string
		ts   string
	}{
		{0, "user", "add oauth login support", `{"type":"user"}`, "2026-08-06T09:00:00Z"},
		{0, "assistant", "Wired up the oauth flow.", `{"type":"assistant"}`, "2026-08-06T09:05:00Z"},
		{1, "user", "now set up the mail-relay on the nas", `{"type":"user"}`, "2026-08-06T11:20:00Z"},
		{1, "assistant", "[tool: Bash]", `{"type":"assistant"}`, "2026-08-06T11:25:00Z"}, // tool-only, excluded
		{2, "user", "the mail-relay still refuses the handshake", `{"type":"user"}`, "2026-08-06T13:00:00Z"},
		{2, "assistant", "Fixed the mail-relay TLS config.", `{"type":"assistant"}`, "2026-08-06T13:10:00Z"},
		{2, "user", "total 8\n-rw-r--r-- x", `{"type":"user","message":{"content":[{"type":"tool_result"}]}}`, "2026-08-06T13:20:00Z"},
	}
	tx, _ := database.Begin()
	for i, m := range texts {
		typ := m.role
		if err := InsertMessage(tx, &types.Message{
			SessionID: "s", UUID: string(rune('a' + i)), Type: typ, Role: m.role,
			ContentText: m.text, ContentJSON: m.json, Timestamp: m.ts,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// Map ids onto three segments in insertion order.
	var ids []int64
	rows, _ := database.Query(`SELECT id FROM messages WHERE session_id='s' ORDER BY id`)
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()

	for _, s := range []struct{ seq, from, to int }{{0, 0, 1}, {1, 2, 3}, {2, 4, 6}} {
		if err := UpsertSegment(database, &Segment{
			SessionID: "s", Seq: s.seq, StartID: ids[s.from], EndID: ids[s.to],
			Summary: "segment " + string(rune('0'+s.seq)), MsgCount: s.to - s.from + 1, Closed: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return database
}

func TestSegmentsMatchingRanksByHits(t *testing.T) {
	database := seedSegmented(t)

	// Hyphenated term: only works because the FTS fallback quotes it.
	got, err := SegmentsMatching(database, "s", "mail-relay", "", "")
	if err != nil {
		t.Fatalf("SegmentsMatching: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("matched %d segments, want 2 (segments 1 and 2)", len(got))
	}
	if got[0].Seq != 2 {
		t.Errorf("top segment = %d, want 2 (it has the most hits)", got[0].Seq)
	}
	if got[0].Hits < got[1].Hits {
		t.Errorf("segments not ordered by hits: %d then %d", got[0].Hits, got[1].Hits)
	}
	if got[0].Summary == "" {
		t.Error("segment summary not returned; a caller needs it to build context")
	}

	// A topic confined to one segment must not drag in the others.
	got, err = SegmentsMatching(database, "s", "oauth", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Seq != 0 {
		t.Errorf("oauth matched %v, want only segment 0", got)
	}

	if got, _ = SegmentsMatching(database, "s", "kubernetes", "", ""); len(got) != 0 {
		t.Errorf("unrelated term matched %d segments, want 0", len(got))
	}
}

func TestSegmentMessagesFiltersAndScopes(t *testing.T) {
	database := seedSegmented(t)

	msgs, err := SegmentMessages(database, "s", []int{2}, "", "")
	if err != nil {
		t.Fatalf("SegmentMessages: %v", err)
	}
	// Segment 2 holds three rows, but one is a tool_result carried on a user turn.
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2 (tool_result excluded): %+v", len(msgs), msgs)
	}
	for _, m := range msgs {
		if m.SegmentSeq != 2 {
			t.Errorf("message tagged segment %d, want 2", m.SegmentSeq)
		}
	}

	// Tool-only assistant turns are dropped, so segment 1 yields just the prompt.
	msgs, _ = SegmentMessages(database, "s", []int{1}, "", "")
	if len(msgs) != 1 || msgs[0].Role != "user" {
		t.Errorf("segment 1 = %+v, want only the user turn", msgs)
	}

	// Multiple segments come back in conversation order, not selection order.
	msgs, _ = SegmentMessages(database, "s", []int{2, 0}, "", "")
	if len(msgs) < 2 {
		t.Fatalf("multi-segment select returned %d messages", len(msgs))
	}
	for i := 1; i < len(msgs); i++ {
		if msgs[i].ID < msgs[i-1].ID {
			t.Error("messages not in conversation order")
			break
		}
	}

	if msgs, _ = SegmentMessages(database, "s", nil, "", ""); msgs != nil {
		t.Error("empty selection should return nothing")
	}
}

func TestSegmentTimesAndWindow(t *testing.T) {
	database := seedSegmented(t)

	times, err := SegmentTimes(database, "s")
	if err != nil {
		t.Fatal(err)
	}
	if got := times[1]; got[0] != "2026-08-06T11:20:00Z" || got[1] != "2026-08-06T11:25:00Z" {
		t.Errorf("segment 1 span = %v, want the 11:20–11:25 range", got)
	}

	// A window inside a segment must still select it (overlap, not containment):
	// this is the common case, a half hour inside a segment spanning hours.
	sel, err := SegmentsInWindow(database, "s", "2026-08-06T11:21", "2026-08-06T11:24")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sel, []int{1}) {
		t.Errorf("window inside segment 1 selected %v, want [1]", sel)
	}

	// Open-ended windows.
	if sel, _ = SegmentsInWindow(database, "s", "2026-08-06T12:00", ""); !reflect.DeepEqual(sel, []int{2}) {
		t.Errorf("open-ended since selected %v, want [2]", sel)
	}
	if sel, _ = SegmentsInWindow(database, "s", "", "2026-08-06T10:00"); !reflect.DeepEqual(sel, []int{0}) {
		t.Errorf("open-ended until selected %v, want [0]", sel)
	}
	if sel, _ = SegmentsInWindow(database, "s", "2027-01-01", ""); len(sel) != 0 {
		t.Errorf("window past the end selected %v, want nothing", sel)
	}
}

// A window narrows *within* a segment, which is the whole point: selecting the
// segment finds the sitting, the window finds the part of it that matters.
func TestSegmentMessagesHonoursWindow(t *testing.T) {
	database := seedSegmented(t)

	msgs, err := SegmentMessages(database, "s", []int{2}, "", "2026-08-06T13:05")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Role != "user" {
		t.Fatalf("windowed segment 2 = %+v, want just the 13:00 user turn", msgs)
	}

	// The same selection unwindowed returns more, proving the cut was the window.
	all, _ := SegmentMessages(database, "s", []int{2}, "", "")
	if len(all) <= len(msgs) {
		t.Errorf("window did not narrow: %d vs %d", len(msgs), len(all))
	}
}

// A match plus a window must intersect, not pick whichever is broader.
func TestSegmentsMatchingHonoursWindow(t *testing.T) {
	database := seedSegmented(t)

	got, err := SegmentsMatching(database, "s", "mail-relay", "", "2026-08-06T12:00")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Seq != 1 {
		t.Errorf("match+window = %v, want only segment 1 (segment 2 is after the window)", got)
	}
}
