package importer

import (
	"strings"
	"testing"
)

func TestNormalizePiKey(t *testing.T) {
	cases := map[string]string{
		"--Volumes-Data-src-kyon--": "-Volumes-Data-src-kyon", // pi fencing stripped
		"-Volumes-Data-src-kyon":    "-Volumes-Data-src-kyon", // a Claude key is untouched
		"--Users-erwin--":           "-Users-erwin",
	}
	for in, want := range cases {
		if got := normalizePiKey(in); got != want {
			t.Errorf("normalizePiKey(%q) = %q, want %q", in, got, want)
		}
	}

	// Both encodings must decode to the same path and display name, or a pi
	// session shows up under a different project than the Claude sessions
	// covering the same directory.
	if got := ProjectPathFromKey("--Volumes-Data-src-kyon--"); got != "/Volumes/Data/src/kyon" {
		t.Errorf("pi key path = %q", got)
	}
	if got := PrettyProjectName("--Volumes-Data-src-kyon--"); got != "src/kyon" {
		t.Errorf("pi key pretty name = %q, want src/kyon", got)
	}
}

func TestPiSessionIDFromFilename(t *testing.T) {
	got := piSessionIDFromFilename(
		"/x/--p--/2026-08-17T13-04-08-262Z_01a00fd2-7e46-720c-9bdf-bb4435682628.jsonl")
	if got != "01a00fd2-7e46-720c-9bdf-bb4435682628" {
		t.Errorf("session id = %q, want the uuid after the timestamp", got)
	}
	// A name without the timestamp prefix still yields something usable.
	if got := piSessionIDFromFilename("/x/--p--/plain.jsonl"); got != "plain" {
		t.Errorf("fallback id = %q, want plain", got)
	}
}

// The three message roles must land on the same shape flags Claude Code produces,
// or every downstream filter — summarization, segments, passages, search — treats
// pi conversations differently from Claude ones.
func TestParsePiLineRoles(t *testing.T) {
	user := `{"type":"message","id":"a1","parentId":null,"timestamp":"2026-08-17T13:04:27.291Z",
		"message":{"role":"user","content":[{"type":"text","text":"please summarize the last 5 commits"}]}}`
	asst := `{"type":"message","id":"a2","parentId":"a1","timestamp":"2026-08-17T13:05:32.165Z",
		"message":{"role":"assistant","content":[
			{"type":"thinking","thinking":"internal reasoning that must not be indexed"},
			{"type":"toolCall","id":"c1","name":"bash","arguments":{"command":"git log -5"}}]}}`
	result := `{"type":"message","id":"a3","parentId":"a2","timestamp":"2026-08-17T13:05:32.306Z",
		"message":{"role":"toolResult","toolCallId":"c1","toolName":"bash",
			"content":[{"type":"text","text":"commit 8f98866 Author: Erwin"}],"isError":false}}`

	m, err := ParsePiLine("s", []byte(user))
	if err != nil || m == nil {
		t.Fatalf("user turn: %v", err)
	}
	if m.Role != "user" || m.IsToolResult {
		t.Errorf("user turn = role %q toolResult=%v", m.Role, m.IsToolResult)
	}
	if m.ContentText != "please summarize the last 5 commits" {
		t.Errorf("user text = %q", m.ContentText)
	}
	if m.UUID != "a1" {
		t.Errorf("uuid = %q, want the entry id", m.UUID)
	}

	m, _ = ParsePiLine("s", []byte(asst))
	if m.Role != "assistant" {
		t.Errorf("assistant role = %q", m.Role)
	}
	if strings.Contains(m.ContentText, "internal reasoning") {
		t.Error("thinking content was indexed; it must be skipped as for Claude Code")
	}
	if m.ContentText != "[tool: bash]" {
		t.Errorf("tool call = %q, want the [tool: bash] marker so tool-only turns are recognised", m.ContentText)
	}

	m, _ = ParsePiLine("s", []byte(result))
	if !m.IsToolResult {
		t.Error("toolResult role did not set IsToolResult; it would be summarized as content")
	}
	if m.Role != "user" {
		t.Errorf("toolResult stored as role %q, want user (matching how Claude Code encodes it)", m.Role)
	}
	if m.ParentUUID != "a2" {
		t.Errorf("parent = %q, want the tree parent", m.ParentUUID)
	}
}

// A user turn may carry a bare string rather than content blocks.
func TestParsePiLineStringContent(t *testing.T) {
	m, err := ParsePiLine("s", []byte(
		`{"type":"message","id":"a1","timestamp":"t","message":{"role":"user","content":"just a string"}}`))
	if err != nil || m == nil {
		t.Fatalf("string content: %v", err)
	}
	if m.ContentText != "just a string" {
		t.Errorf("text = %q, want the bare string", m.ContentText)
	}
}

// A compaction summarizes earlier turns; counting it as content would double-count
// everything it describes, exactly as for Claude Code's compaction markers.
func TestParsePiLineCompaction(t *testing.T) {
	m, err := ParsePiLine("s", []byte(
		`{"type":"compaction","id":"c1","parentId":"a9","timestamp":"t",
		  "summary":"Earlier: refactored the parser.","firstKeptEntryId":"a5","tokensBefore":40000}`))
	if err != nil || m == nil {
		t.Fatalf("compaction: %v", err)
	}
	if !m.IsCompactSummary {
		t.Error("compaction entry not flagged; it would be summarized as a turn")
	}
	if !strings.Contains(m.ContentText, "refactored the parser") {
		t.Errorf("compaction text = %q, want the summary", m.ContentText)
	}
}

// Bookkeeping entries are not turns and must not become empty rows.
func TestParsePiLineSkipsNonContent(t *testing.T) {
	for _, line := range []string{
		`{"type":"session","version":3,"id":"s1","timestamp":"t","cwd":"/Volumes/Data/src/kyon"}`,
		`{"type":"model_change","id":"m1","parentId":null,"timestamp":"t","provider":"ollama","modelId":"qwen3.8:27b"}`,
		`{"type":"thinking_level_change","id":"t1","parentId":"m1","timestamp":"t","thinkingLevel":"medium"}`,
		`{"type":"label","id":"l1","parentId":"a1","timestamp":"t","targetId":"a1","label":"here"}`,
		`{"type":"session_info","id":"i1","parentId":null,"timestamp":"t","name":"my session"}`,
	} {
		m, err := ParsePiLine("s", []byte(line))
		if err != nil {
			t.Errorf("unexpected error on %.30s: %v", line, err)
		}
		if m != nil {
			t.Errorf("%.30s produced a message row; it carries no conversation content", line)
		}
	}

	if _, err := ParsePiLine("s", []byte("{not json")); err == nil {
		t.Error("malformed line should error, not silently import")
	}
}

func TestPiSessionHeaderAndFirstPrompt(t *testing.T) {
	id, cwd, ok := piSessionHeader([]byte(
		`{"type":"session","version":3,"id":"01a00fd2","timestamp":"t","cwd":"/Volumes/Data/src/kyon"}`))
	if !ok || id != "01a00fd2" || cwd != "/Volumes/Data/src/kyon" {
		t.Errorf("header = %q %q ok=%v", id, cwd, ok)
	}
	if _, _, ok := piSessionHeader([]byte(`{"type":"message","id":"a1"}`)); ok {
		t.Error("a message entry was read as a header")
	}

	got := piFirstPrompt([]byte(
		`{"type":"message","id":"a1","timestamp":"t","message":{"role":"user","content":[{"type":"text","text":"do the thing"}]}}`))
	if got != "do the thing" {
		t.Errorf("first prompt = %q", got)
	}
	// Only a user turn opens a session.
	if got := piFirstPrompt([]byte(
		`{"type":"message","id":"a2","timestamp":"t","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`)); got != "" {
		t.Errorf("assistant turn used as first prompt: %q", got)
	}
}

// Scanning must not fail when pi isn't installed — an archive of one agent should
// not break because the other is absent.
func TestScanPiMissingIsNotAnError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	files, err := ScanPi()
	if err != nil {
		t.Fatalf("scanning a home without pi failed: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("got %d files from an empty home", len(files))
	}

	all, err := ScanAll()
	if err != nil {
		t.Fatalf("ScanAll with neither agent present: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("got %d files with no agents installed", len(all))
	}
}

func TestSessionFileAgentDefault(t *testing.T) {
	if got := (SessionFile{}).AgentOf(); got != AgentClaude {
		t.Errorf("empty agent = %q, want claude so pre-existing rows keep their meaning", got)
	}
	if got := (SessionFile{Agent: AgentPi}).AgentOf(); got != AgentPi {
		t.Errorf("agent = %q, want pi", got)
	}
}
