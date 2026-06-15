package db

import (
	"testing"

	"github.com/erwin/remaimber/internal/types"
)

func TestActivePathSetRewind(t *testing.T) {
	database := testDB(t)
	insertSession(t, database, &types.Session{SessionID: "s", ProjectKey: "-p"})

	tx, _ := database.Begin()
	add := func(uuid, parent, json string) {
		InsertMessage(tx, &types.Message{SessionID: "s", UUID: uuid, ParentUUID: parent,
			Type: "assistant", Role: "assistant", ContentText: uuid, ContentJSON: json})
	}
	add("m1", "", `{"type":"assistant"}`)
	add("m2", "m1", `{"type":"assistant"}`)
	add("m3", "m2", `{"type":"assistant"}`)                    // abandoned by the rewind
	add("m4", "m2", `{"type":"assistant"}`)                    // rewind: branched from m2; newest = head
	add("sc", "m4", `{"type":"assistant","isSidechain":true}`) // sidechain — excluded
	tx.Commit()

	onPath, err := ActivePathSet(database, "s")
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"m1", "m2", "m4"} {
		if !onPath[u] {
			t.Errorf("%s should be on the active path", u)
		}
	}
	if onPath["m3"] {
		t.Error("m3 (abandoned branch) must NOT be on the active path")
	}
	if onPath["sc"] {
		t.Error("sidechain message must not be on the active path")
	}
}

// A compaction resets the parent chain, so ActivePathSet covers only the span
// since the last compaction — the caller must keep pre-compaction content
// unconditionally (it is frozen and unreachable by a rewind).
func TestActivePathSetStopsAtCompactionReset(t *testing.T) {
	database := testDB(t)
	insertSession(t, database, &types.Session{SessionID: "s", ProjectKey: "-p"})
	tx, _ := database.Begin()
	add := func(uuid, parent, json string) {
		InsertMessage(tx, &types.Message{SessionID: "s", UUID: uuid, ParentUUID: parent,
			Type: "assistant", Role: "assistant", ContentText: uuid, ContentJSON: json})
	}
	add("old1", "", `{"type":"assistant"}`)
	add("old2", "old1", `{"type":"assistant"}`)
	// compaction summary with a reset parent chain (parent empty), then live content
	add("comp", "", `{"type":"user","isCompactSummary":true}`)
	add("new1", "comp", `{"type":"assistant"}`)
	tx.Commit()

	onPath, _ := ActivePathSet(database, "s")
	if !onPath["new1"] || !onPath["comp"] {
		t.Errorf("live span should be on path: %+v", onPath)
	}
	if onPath["old1"] || onPath["old2"] {
		t.Error("pre-compaction messages are not reachable via the reset chain (caller keeps them unconditionally)")
	}
}

func TestActivePathSetEmpty(t *testing.T) {
	database := testDB(t)
	insertSession(t, database, &types.Session{SessionID: "s", ProjectKey: "-p"})
	onPath, err := ActivePathSet(database, "s")
	if err != nil {
		t.Fatal(err)
	}
	if onPath != nil {
		t.Errorf("no messages should yield nil (linear) path, got %+v", onPath)
	}
}
