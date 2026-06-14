package db

import (
	"testing"

	"github.com/erwin/remaimber/internal/types"
)

func TestUserAssistantMessagesFiltersToolNoise(t *testing.T) {
	database := testDB(t)
	insertSession(t, database, &types.Session{SessionID: "s", ProjectKey: "-p"})

	tx, _ := database.Begin()
	// real user prompt — kept
	InsertMessage(tx, &types.Message{SessionID: "s", UUID: "u1", Type: "user", Role: "user",
		ContentText: "please add a --repo flag", ContentJSON: `{"type":"user","message":{"content":"add flag"}}`})
	// tool_result carried on a user-role turn — noise, excluded
	InsertMessage(tx, &types.Message{SessionID: "s", UUID: "u2", Type: "user", Role: "user",
		ContentText: "total 8\n-rw-r--r-- main.go", ContentJSON: `{"type":"user","message":{"content":[{"type":"tool_result","content":"total 8"}]}}`})
	// assistant prose — kept
	InsertMessage(tx, &types.Message{SessionID: "s", UUID: "a1", Type: "assistant", Role: "assistant",
		ContentText: "Added the --repo flag and wired it through.", ContentJSON: `{"type":"assistant"}`})
	// empty-text turn (e.g. tool-call only) — excluded
	InsertMessage(tx, &types.Message{SessionID: "s", UUID: "a2", Type: "assistant", Role: "assistant",
		ContentText: "", ContentJSON: `{"type":"assistant"}`})
	tx.Commit()

	msgs, err := UserAssistantMessages(database, "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("want 2 salient messages (real user + assistant prose), got %d: %+v", len(msgs), msgs)
	}
	if msgs[0].ContentText != "please add a --repo flag" || msgs[1].ContentText != "Added the --repo flag and wired it through." {
		t.Errorf("unexpected salient set / order: %+v", msgs)
	}
}

func TestSessionsNeedingSummary(t *testing.T) {
	database := testDB(t)
	insertSession(t, database, &types.Session{SessionID: "s1", ProjectKey: "-p", EndedAt: "2026-01-01"})

	// Add 6 user/assistant messages and a couple of non-conversational ones.
	tx, _ := database.Begin()
	for i := 0; i < 3; i++ {
		InsertMessage(tx, &types.Message{SessionID: "s1", UUID: u("u", i), Type: "user", Role: "user", ContentText: "q", ContentJSON: "{}"})
		InsertMessage(tx, &types.Message{SessionID: "s1", UUID: u("a", i), Type: "assistant", Role: "assistant", ContentText: "a", ContentJSON: "{}"})
	}
	InsertMessage(tx, &types.Message{SessionID: "s1", UUID: "prog", Type: "progress", ContentJSON: "{}"})
	tx.Commit()

	work, err := SessionsNeedingSummary(database, 6)
	if err != nil {
		t.Fatal(err)
	}
	if len(work) != 1 {
		t.Fatalf("expected 1 session needing summary, got %d", len(work))
	}
	if work[0].UACount != 6 {
		t.Errorf("UACount = %d, want 6 (non-conversational excluded)", work[0].UACount)
	}

	// After summarizing up to offset 6, it no longer needs work.
	if err := UpdateSummary(database, "s1", "did stuff", 6); err != nil {
		t.Fatal(err)
	}
	work, _ = SessionsNeedingSummary(database, 6)
	if len(work) != 0 {
		t.Errorf("expected no work after summarizing, got %d", len(work))
	}

	// Summary is persisted and searchable.
	got, _ := GetSession(database, "s1")
	if got.Summary != "did stuff" || got.SummaryOffset != 6 {
		t.Errorf("summary not persisted: %+v", got)
	}
}

func u(prefix string, i int) string {
	return prefix + string(rune('0'+i))
}
