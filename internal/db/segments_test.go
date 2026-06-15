package db

import (
	"fmt"
	"testing"

	"github.com/erwin/remaimber/internal/types"
)

func content(ids ...int64) []types.Message {
	var ms []types.Message
	for _, id := range ids {
		ms = append(ms, types.Message{ID: id, UUID: fmt.Sprintf("u%d", id)})
	}
	return ms
}

func TestPlanSegmentsSizeCap(t *testing.T) {
	plan := PlanSegments(content(1, 2, 3, 4, 5, 6, 7, 8, 9, 10), nil, 4)
	if len(plan) != 3 {
		t.Fatalf("want 3 segments (4+4+2), got %d: %+v", len(plan), plan)
	}
	if !plan[0].Closed || plan[0].Reason != "sizecap" || plan[0].Count != 4 {
		t.Errorf("seg0 = %+v", plan[0])
	}
	if !plan[1].Closed || plan[1].Count != 4 {
		t.Errorf("seg1 = %+v", plan[1])
	}
	if plan[2].Closed || plan[2].Count != 2 {
		t.Errorf("seg2 (open) = %+v", plan[2])
	}
	if plan[2].StartID != 9 || plan[2].EndID != 10 || plan[2].StartUUID != "u9" {
		t.Errorf("seg2 bounds wrong: %+v", plan[2])
	}
}

func TestPlanSegmentsCompactionSplits(t *testing.T) {
	// content ids 1,2,3 then 5,6 — a compaction message sits at id 4 (not content).
	plan := PlanSegments(content(1, 2, 3, 5, 6), []int64{4}, 100)
	if len(plan) != 2 {
		t.Fatalf("want 2 segments split at compaction, got %d: %+v", len(plan), plan)
	}
	if !plan[0].Closed || plan[0].Reason != "compaction" || plan[0].EndID != 3 {
		t.Errorf("seg0 = %+v", plan[0])
	}
	if plan[1].Closed || plan[1].StartID != 5 || plan[1].Count != 2 {
		t.Errorf("seg1 (open) = %+v", plan[1])
	}
}

func TestPlanSegmentsTrailingCompactionCloses(t *testing.T) {
	// A compaction after all content closes the final segment (nothing open yet).
	plan := PlanSegments(content(1, 2, 3), []int64{9}, 100)
	if len(plan) != 1 || !plan[0].Closed || plan[0].Reason != "compaction" {
		t.Fatalf("trailing compaction should close the last segment: %+v", plan)
	}
}

func TestPlanSegmentsEmpty(t *testing.T) {
	if plan := PlanSegments(nil, nil, 60); len(plan) != 0 {
		t.Errorf("empty content should yield no segments, got %+v", plan)
	}
}

func TestSegmentContentFilters(t *testing.T) {
	database := testDB(t)
	insertSession(t, database, &types.Session{SessionID: "s", ProjectKey: "-p"})
	tx, _ := database.Begin()
	add := func(uuid, typ, role, text, json string) {
		InsertMessage(tx, &types.Message{SessionID: "s", UUID: uuid, Type: typ, Role: role, ContentText: text, ContentJSON: json})
	}
	add("u1", "user", "user", "real prompt", `{"type":"user"}`)                                                // keep
	add("u2", "user", "user", "compaction digest", `{"type":"user","isCompactSummary":true}`)                  // drop (boundary)
	add("u3", "user", "user", "tool output", `{"type":"user","message":{"content":[{"type":"tool_result"}]}}`) // drop
	add("u4", "assistant", "assistant", "[tool: Bash]", `{"type":"assistant"}`)                                // drop (tool-only)
	add("u5", "assistant", "assistant", "real prose answer", `{"type":"assistant"}`)                           // keep
	add("u6", "assistant", "assistant", "sidechain noise", `{"type":"assistant","isSidechain":true}`)          // drop
	tx.Commit()

	msgs, err := SegmentContent(database, "s", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("want 2 salient content messages, got %d: %+v", len(msgs), msgs)
	}
	if msgs[0].ContentText != "real prompt" || msgs[1].ContentText != "real prose answer" {
		t.Errorf("unexpected content: %+v", msgs)
	}
	// ids/uuids populated for boundary keying
	if msgs[0].UUID != "u1" || msgs[0].ID == 0 {
		t.Errorf("id/uuid not populated: %+v", msgs[0])
	}
}

func TestSegmentRoundTrip(t *testing.T) {
	database := testDB(t)
	seg := &Segment{SessionID: "s", Seq: 0, StartID: 1, EndID: 9, StartUUID: "u1", EndUUID: "u9",
		Summary: "did the thing", MsgCount: 5, HighWater: 9, Closed: true, Reason: "compaction"}
	if err := UpsertSegment(database, seg); err != nil {
		t.Fatal(err)
	}
	// open segment
	if err := UpsertSegment(database, &Segment{SessionID: "s", Seq: 1, StartID: 10, EndID: 12,
		StartUUID: "u10", EndUUID: "u12", Summary: "open work", MsgCount: 2, HighWater: 12}); err != nil {
		t.Fatal(err)
	}
	got, err := GetSegments(database, "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got[0].Closed || got[1].Closed {
		t.Fatalf("roundtrip wrong: %+v", got)
	}
	if got[0].Summary != "did the thing" || got[0].EndUUID != "u9" {
		t.Errorf("seg0 = %+v", got[0])
	}

	// DeleteSegmentsFrom drops the open one.
	if err := DeleteSegmentsFrom(database, "s", 1); err != nil {
		t.Fatal(err)
	}
	got, _ = GetSegments(database, "s")
	if len(got) != 1 {
		t.Errorf("want 1 segment after delete, got %d", len(got))
	}
}
