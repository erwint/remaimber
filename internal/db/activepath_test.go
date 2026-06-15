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

// A compaction resets the parent chain, so the walk must BRIDGE across it to
// include the pre-compaction span (which is still live after a plain compaction).
func TestActivePathSetBridgesCompaction(t *testing.T) {
	database := testDB(t)
	insertSession(t, database, &types.Session{SessionID: "s", ProjectKey: "-p"})
	tx, _ := database.Begin()
	add := func(uuid, parent, json string) {
		InsertMessage(tx, &types.Message{SessionID: "s", UUID: uuid, ParentUUID: parent,
			Type: "assistant", Role: "assistant", ContentText: uuid, ContentJSON: json})
	}
	add("old1", "", `{"type":"assistant"}`)
	add("old2", "old1", `{"type":"assistant"}`)
	add("comp", "", `{"type":"user","isCompactSummary":true}`) // reset parent chain
	add("new1", "comp", `{"type":"assistant"}`)
	tx.Commit()

	onPath, _ := ActivePathSet(database, "s")
	for _, u := range []string{"old1", "old2", "comp", "new1"} {
		if !onPath[u] {
			t.Errorf("%s should be on path (compaction keeps everything; chain bridged): %+v", u, onPath)
		}
	}
}

// A rewind to a point BEFORE a compaction abandons everything after the rewind
// target — including the compaction and its post-compaction content.
func TestActivePathSetRewindPastCompaction(t *testing.T) {
	database := testDB(t)
	insertSession(t, database, &types.Session{SessionID: "s", ProjectKey: "-p"})
	tx, _ := database.Begin()
	add := func(uuid, parent, json string) {
		InsertMessage(tx, &types.Message{SessionID: "s", UUID: uuid, ParentUUID: parent,
			Type: "assistant", Role: "assistant", ContentText: uuid, ContentJSON: json})
	}
	add("old0", "", `{"type":"assistant"}`)
	add("old1", "old0", `{"type":"assistant"}`)
	add("old2", "old1", `{"type":"assistant"}`)
	add("comp", "", `{"type":"user","isCompactSummary":true}`)
	add("new1", "comp", `{"type":"assistant"}`)
	add("rw", "old1", `{"type":"assistant"}`) // rewind to old1 (before the compaction); newest = head
	tx.Commit()

	onPath, _ := ActivePathSet(database, "s")
	for _, u := range []string{"rw", "old1", "old0"} {
		if !onPath[u] {
			t.Errorf("%s should be on the active (rewound) path: %+v", u, onPath)
		}
	}
	for _, u := range []string{"old2", "comp", "new1"} {
		if onPath[u] {
			t.Errorf("%s was abandoned by the rewind and must be off path", u)
		}
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
