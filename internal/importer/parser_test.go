package importer

import (
	"encoding/json"
	"testing"

	"github.com/erwin/remaimber/internal/types"
)

func TestParseLine_UserMessage_StringContent(t *testing.T) {
	line := `{"type":"user","uuid":"u1","parentUuid":"p1","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"Hello world"},"sessionId":"s1","cwd":"/test","gitBranch":"main"}`
	msg, err := ParseLine("s1", []byte(line))
	if err != nil {
		t.Fatalf("ParseLine: %v", err)
	}
	if msg.Type != "user" {
		t.Errorf("type = %q, want user", msg.Type)
	}
	if msg.UUID != "u1" {
		t.Errorf("uuid = %q, want u1", msg.UUID)
	}
	if msg.ParentUUID != "p1" {
		t.Errorf("parent_uuid = %q, want p1", msg.ParentUUID)
	}
	if msg.Role != "user" {
		t.Errorf("role = %q, want user", msg.Role)
	}
	if msg.ContentText != "Hello world" {
		t.Errorf("content_text = %q, want 'Hello world'", msg.ContentText)
	}
	if msg.SessionID != "s1" {
		t.Errorf("session_id = %q, want s1", msg.SessionID)
	}
	if msg.ContentHash != "" {
		t.Error("expected no content_hash for line with UUID")
	}
}

func TestParseLine_UserMessage_ArrayContent(t *testing.T) {
	line := `{"type":"user","uuid":"u1","message":{"role":"user","content":[{"type":"text","text":"First part"},{"type":"text","text":"Second part"}]}}`
	msg, err := ParseLine("s1", []byte(line))
	if err != nil {
		t.Fatalf("ParseLine: %v", err)
	}
	if msg.ContentText != "First part\nSecond part" {
		t.Errorf("content_text = %q, want 'First part\\nSecond part'", msg.ContentText)
	}
}

func TestParseLine_AssistantMessage_WithToolUse(t *testing.T) {
	line := `{"type":"assistant","uuid":"u2","message":{"role":"assistant","content":[{"type":"text","text":"Let me check that."},{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}`
	msg, err := ParseLine("s1", []byte(line))
	if err != nil {
		t.Fatalf("ParseLine: %v", err)
	}
	if msg.Role != "assistant" {
		t.Errorf("role = %q, want assistant", msg.Role)
	}
	expected := "Let me check that.\n[tool: Bash]"
	if msg.ContentText != expected {
		t.Errorf("content_text = %q, want %q", msg.ContentText, expected)
	}
}

func TestParseLine_AssistantMessage_ThinkingSkipped(t *testing.T) {
	line := `{"type":"assistant","uuid":"u3","message":{"role":"assistant","content":[{"type":"thinking","text":"internal reasoning"},{"type":"text","text":"Visible response"}]}}`
	msg, err := ParseLine("s1", []byte(line))
	if err != nil {
		t.Fatalf("ParseLine: %v", err)
	}
	if msg.ContentText != "Visible response" {
		t.Errorf("content_text = %q, want 'Visible response' (thinking should be skipped)", msg.ContentText)
	}
}

func TestParseLine_CustomTitle(t *testing.T) {
	line := `{"type":"custom-title","customTitle":"My Session","sessionId":"s1"}`
	msg, err := ParseLine("s1", []byte(line))
	if err != nil {
		t.Fatalf("ParseLine: %v", err)
	}
	if msg.Type != "custom-title" {
		t.Errorf("type = %q, want custom-title", msg.Type)
	}
	if msg.ContentText != "My Session" {
		t.Errorf("content_text = %q, want 'My Session'", msg.ContentText)
	}
	if msg.ContentHash == "" {
		t.Error("expected content_hash for uuid-less line")
	}
}

func TestParseLine_Progress(t *testing.T) {
	line := `{"type":"progress","uuid":"u4","data":{"type":"hook_progress"},"timestamp":"2026-01-01T00:00:00Z"}`
	msg, err := ParseLine("s1", []byte(line))
	if err != nil {
		t.Fatalf("ParseLine: %v", err)
	}
	if msg.Type != "progress" {
		t.Errorf("type = %q, want progress", msg.Type)
	}
	if msg.ContentText != "" {
		t.Errorf("content_text = %q, want empty (progress not indexed)", msg.ContentText)
	}
}

func TestParseLine_FileHistorySnapshot(t *testing.T) {
	line := `{"type":"file-history-snapshot","messageId":"m1","snapshot":{}}`
	msg, err := ParseLine("s1", []byte(line))
	if err != nil {
		t.Fatalf("ParseLine: %v", err)
	}
	if msg.Type != "file-history-snapshot" {
		t.Errorf("type = %q, want file-history-snapshot", msg.Type)
	}
	if msg.ContentText != "" {
		t.Errorf("content_text should be empty for file-history-snapshot")
	}
	if msg.ContentHash == "" {
		t.Error("expected content_hash for uuid-less line")
	}
}

func TestParseLine_InvalidJSON(t *testing.T) {
	_, err := ParseLine("s1", []byte("not json"))
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestParseLine_ToolResultContent(t *testing.T) {
	line := `{"type":"user","uuid":"u5","message":{"role":"user","content":[{"type":"tool_result","content":[{"type":"text","text":"command output here"}]}]}}`
	msg, err := ParseLine("s1", []byte(line))
	if err != nil {
		t.Fatalf("ParseLine: %v", err)
	}
	if msg.ContentText != "command output here" {
		t.Errorf("content_text = %q, want 'command output here'", msg.ContentText)
	}
}

func TestParseLine_ToolResultStringContent(t *testing.T) {
	line := `{"type":"user","uuid":"u6","message":{"role":"user","content":[{"type":"tool_result","content":"simple string result"}]}}`
	msg, err := ParseLine("s1", []byte(line))
	if err != nil {
		t.Fatalf("ParseLine: %v", err)
	}
	if msg.ContentText != "simple string result" {
		t.Errorf("content_text = %q, want 'simple string result'", msg.ContentText)
	}
}

func TestParseLine_PreservesRawJSON(t *testing.T) {
	line := `{"type":"user","uuid":"u7","message":{"role":"user","content":"test"}}`
	msg, err := ParseLine("s1", []byte(line))
	if err != nil {
		t.Fatalf("ParseLine: %v", err)
	}
	if msg.ContentJSON != line {
		t.Errorf("content_json not preserved")
	}
}

func TestExtractSessionMeta(t *testing.T) {
	jl := &types.JSONLLine{
		Type:      "user",
		CWD:       "/my/project",
		GitBranch: "feature-x",
		Message: &types.MessageContent{
			Role:    "user",
			Content: json.RawMessage(`"What is this project about?"`),
		},
	}
	cwd, branch, title, prompt := ExtractSessionMeta(jl)
	if cwd != "/my/project" {
		t.Errorf("cwd = %q", cwd)
	}
	if branch != "feature-x" {
		t.Errorf("branch = %q", branch)
	}
	if title != "" {
		t.Errorf("title = %q, want empty", title)
	}
	if prompt != "What is this project about?" {
		t.Errorf("prompt = %q", prompt)
	}
}

func TestExtractSessionMeta_CustomTitle(t *testing.T) {
	jl := &types.JSONLLine{
		Type:        "custom-title",
		CustomTitle: "My Great Session",
	}
	_, _, title, _ := ExtractSessionMeta(jl)
	if title != "My Great Session" {
		t.Errorf("title = %q, want 'My Great Session'", title)
	}
}

func TestExtractSessionMeta_LongPromptTruncated(t *testing.T) {
	long := ""
	for i := 0; i < 300; i++ {
		long += "x"
	}
	jl := &types.JSONLLine{
		Type: "user",
		Message: &types.MessageContent{
			Role:    "user",
			Content: json.RawMessage(`"` + long + `"`),
		},
	}
	_, _, _, prompt := ExtractSessionMeta(jl)
	if len(prompt) != 200 {
		t.Errorf("prompt length = %d, want 200 (truncated)", len(prompt))
	}
}

func TestCleanText_SystemReminder(t *testing.T) {
	input := `Some text before <system-reminder>This is a system reminder that should be removed</system-reminder> and after`
	got := CleanText(input)
	expected := "Some text before  and after"
	if got != expected {
		t.Errorf("CleanText = %q, want %q", got, expected)
	}
}

func TestCleanText_MultipleSystemTags(t *testing.T) {
	input := `Hello <system-reminder>noise</system-reminder> world <local-command-caveat>more noise</local-command-caveat> end`
	got := CleanText(input)
	expected := "Hello  world  end"
	if got != expected {
		t.Errorf("CleanText = %q, want %q", got, expected)
	}
}

func TestCleanText_NoTags(t *testing.T) {
	input := "Clean text with no tags"
	got := CleanText(input)
	if got != input {
		t.Errorf("CleanText changed clean text: %q", got)
	}
}

func TestCleanText_MultilineTag(t *testing.T) {
	input := "Before\n<system-reminder>\nLine 1\nLine 2\n</system-reminder>\nAfter"
	got := CleanText(input)
	expected := "Before\n\nAfter"
	if got != expected {
		t.Errorf("CleanText multiline = %q, want %q", got, expected)
	}
}

func TestCleanText_AllSupportedTags(t *testing.T) {
	tags := []string{
		"system-reminder", "local-command-caveat", "local-command-stdout",
		"command-name", "command-message", "command-args", "task-notification",
	}
	for _, tag := range tags {
		input := "a <" + tag + ">noise</" + tag + "> b"
		got := CleanText(input)
		if got != "a  b" {
			t.Errorf("CleanText for <%s> = %q, want %q", tag, got, "a  b")
		}
	}
}

func TestParseLine_ContentCleaning(t *testing.T) {
	line := `{"type":"user","uuid":"u1","message":{"role":"user","content":"Hello <system-reminder>noise</system-reminder> world"}}`
	msg, err := ParseLine("s1", []byte(line))
	if err != nil {
		t.Fatalf("ParseLine: %v", err)
	}
	if msg.ContentText != "Hello  world" {
		t.Errorf("content_text = %q, want 'Hello  world'", msg.ContentText)
	}
}
