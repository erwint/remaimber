package importer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/erwin/remaimber/internal/types"
)

func TestCodexSessionIDFromFilename(t *testing.T) {
	got := codexSessionIDFromFilename(
		"/x/2026/07/25/rollout-2026-07-25T21-13-51-019f9ab2-b764-7231-a165-206e19485b91.jsonl")
	if got != "019f9ab2-b764-7231-a165-206e19485b91" {
		t.Errorf("session id = %q, want the uuid after the timestamp", got)
	}
	// The timestamp carries dashes too, so a name without a uuid must yield
	// nothing rather than a slice of the date.
	if got := codexSessionIDFromFilename("/x/rollout-2026-07-25T21-13-51.jsonl"); got != "" {
		t.Errorf("id from a uuid-less name = %q, want empty", got)
	}
}

// A rollout's entries must land on the same roles and shape flags the other
// parsers produce, or every downstream filter — summarization, segments,
// passages, search — treats Codex conversations differently from the rest.
func TestParseCodexLineShapes(t *testing.T) {
	lines := map[string]string{
		"user":      `{"timestamp":"2026-07-25T19:20:00.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"please summarize the last 5 commits"}]}}`,
		"assistant": `{"timestamp":"2026-07-25T19:20:05.000Z","type":"response_item","payload":{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"Here are the last five."}]}}`,
		"call":      `{"timestamp":"2026-07-25T19:20:06.000Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{}","call_id":"call_1"}}`,
		"output":    `{"timestamp":"2026-07-25T19:20:07.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_1","output":"commit 8f98866 Author: Erwin"}}`,
		"patch":     `{"timestamp":"2026-07-25T19:20:08.000Z","type":"response_item","payload":{"type":"custom_tool_call","name":"apply_patch","input":"*** Begin Patch"}}`,
		"compacted": `{"timestamp":"2026-07-25T19:30:00.000Z","type":"compacted","payload":{"message":"earlier work on the mail relay","replacement_history":[]}}`,
	}
	parse := func(key string) *types.Message {
		m, err := ParseCodexLine("s", []byte(lines[key]))
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if m == nil {
			t.Fatalf("%s: no message", key)
		}
		return m
	}

	if m := parse("user"); m.Role != "user" || m.Type != "user" ||
		m.ContentText != "please summarize the last 5 commits" || m.IsToolResult {
		t.Errorf("user turn = %+v", m)
	}
	if m := parse("assistant"); m.Role != "assistant" || m.UUID != "msg_1" ||
		m.ContentText != "Here are the last five." {
		t.Errorf("assistant turn = %+v", m)
	}
	// A tool call is the assistant's, and carries the shared marker so tool-only
	// turns are recognised the same way across agents.
	if m := parse("call"); m.Role != "assistant" || m.ContentText != "[tool: exec_command]" {
		t.Errorf("function call = %+v", m)
	}
	if m := parse("patch"); m.Role != "assistant" || m.ContentText != "[tool: apply_patch]" {
		t.Errorf("custom tool call = %+v", m)
	}
	// Codex records a tool result as its own entry; it has to end up flagged the
	// way Claude Code's tool_result blocks are.
	if m := parse("output"); m.Role != "user" || !m.IsToolResult ||
		!strings.Contains(m.ContentText, "8f98866") {
		t.Errorf("tool output = %+v", m)
	}
	if m := parse("compacted"); !m.IsCompactSummary || m.Role != "user" ||
		m.ContentText != "earlier work on the mail relay" {
		t.Errorf("compaction = %+v", m)
	}

	// Entries that are agent mechanics, not conversation. The event_msg family
	// mirrors every message, so importing it would double each turn.
	for name, line := range map[string]string{
		"reasoning":   `{"timestamp":"t","type":"response_item","payload":{"type":"reasoning","summary":[],"encrypted_content":"gAAAA"}}`,
		"developer":   `{"timestamp":"t","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"<permissions instructions>"}]}}`,
		"event_msg":   `{"timestamp":"t","type":"event_msg","payload":{"type":"agent_message","message":"Here are the last five."}}`,
		"token_count": `{"timestamp":"t","type":"event_msg","payload":{"type":"token_count"}}`,
		"turn":        `{"timestamp":"t","type":"turn_context","payload":{"cwd":"/x","model":"gpt-5.4"}}`,
	} {
		m, err := ParseCodexLine("s", []byte(line))
		if err != nil || m != nil {
			t.Errorf("%s: got %+v (err %v), want no message", name, m, err)
		}
	}
}

// Codex delivers its own scaffolding as user turns. Indexed, they make every
// session match a search for the harness's boilerplate, and they become the
// "first prompt" of every session that has an AGENTS.md.
func TestParseCodexLineStripsInjections(t *testing.T) {
	env := `{"timestamp":"t","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>\n  <cwd>/Volumes/Data/src/kyon</cwd>\n</environment_context>"}]}}`
	agents := `{"timestamp":"t","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"# AGENTS.md instructions\n\n<INSTRUCTIONS>\n# Commit messages\nNo attribution.\n</INSTRUCTIONS>"}]}}`

	for name, line := range map[string]string{"environment": env, "agents": agents} {
		m, err := ParseCodexLine("s", []byte(line))
		if err != nil || m == nil {
			t.Fatalf("%s: %v", name, err)
		}
		if m.ContentText != "" {
			t.Errorf("%s: content_text = %q, want empty", name, m.ContentText)
		}
		if codexFirstPrompt([]byte(line)) != "" {
			t.Errorf("%s: injection used as the session's first prompt", name)
		}
	}

	// What the user actually typed survives.
	real := `{"timestamp":"t","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"ok, then can we address all of these one by one?"}]}}`
	if got := codexFirstPrompt([]byte(real)); got != "ok, then can we address all of these one by one?" {
		t.Errorf("first prompt = %q", got)
	}
}

// Codex files rollouts by date, so the project a conversation belongs to can
// only come from the cwd in its own header. Getting this wrong scatters Codex
// sessions away from the Claude Code and pi sessions for the same directory.
func TestScanCodexKeysByHeaderCWD(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)

	day := filepath.Join(home, "sessions", "2026", "07", "25")
	if err := os.MkdirAll(day, 0755); err != nil {
		t.Fatal(err)
	}
	id := "019f9ab2-b764-7231-a165-206e19485b91"
	path := filepath.Join(day, "rollout-2026-07-25T21-13-51-"+id+".jsonl")
	header := `{"timestamp":"2026-07-25T19:13:51.495Z","type":"session_meta","payload":{"session_id":"` + id +
		`","cwd":"/Volumes/Data/src/kyon","originator":"codex-tui","git":{"branch":"master"}}}` + "\n"
	if err := os.WriteFile(path, []byte(header), 0644); err != nil {
		t.Fatal(err)
	}

	files, err := ScanCodex()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("scanned %d files, want 1", len(files))
	}
	f := files[0]
	if f.SessionID != id || f.AgentOf() != AgentCodex {
		t.Errorf("scanned %+v", f)
	}
	if f.ProjectKey != "-Volumes-Data-src-kyon" {
		t.Errorf("project key = %q, want the key the cwd encodes to", f.ProjectKey)
	}
	if got := CodexSessionPath(id); got != path {
		t.Errorf("CodexSessionPath = %q, want %q", got, path)
	}
	if !SessionFileExists(f.ProjectKey, id, AgentCodex) {
		t.Error("a rollout still on disk must count as resumable")
	}
	if SessionFileExists(f.ProjectKey, "00000000-0000-0000-0000-000000000000", AgentCodex) {
		t.Error("a missing rollout must not count as resumable")
	}

	// A rollout whose header is unreadable is still worth archiving.
	orphan := filepath.Join(day, "rollout-2026-07-25T22-00-00-019f9ab2-b764-7231-a165-206e19485b92.jsonl")
	if err := os.WriteFile(orphan, []byte("not json\n"), 0644); err != nil {
		t.Fatal(err)
	}
	files, err = ScanCodex()
	if err != nil || len(files) != 2 {
		t.Fatalf("scanned %d files (err %v), want 2", len(files), err)
	}
	for _, f := range files {
		if strings.HasSuffix(f.Path, "b92.jsonl") && f.ProjectKey != "-unknown" {
			t.Errorf("headerless rollout key = %q, want -unknown", f.ProjectKey)
		}
	}
}

// A missing Codex install is the normal case, not a fault.
func TestScanCodexWithoutCodex(t *testing.T) {
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "absent"))
	files, err := ScanCodex()
	if err != nil || len(files) != 0 {
		t.Errorf("ScanCodex without Codex = %d files, %v", len(files), err)
	}
}
