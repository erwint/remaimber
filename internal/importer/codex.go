package importer

import (
	"bufio"
	"encoding/json"

	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/erwint/remaimber/internal/homedir"
	"github.com/erwint/remaimber/internal/mover"
	"github.com/erwint/remaimber/internal/types"
)

// Codex (OpenAI's CLI) writes one JSONL "rollout" per session, but files it by
// date rather than by project:
//
//	~/.codex/sessions/2026/07/25/rollout-2026-07-25T21-13-51-<uuid>.jsonl
//
// There is no project directory to read a key off, so the key is derived from
// the cwd recorded in the file's own session_meta header — which is what pi and
// Claude Code keys decode back to anyway, so a Codex conversation lands in the
// same project bucket as the others for that directory.
//
// Inside, a rollout is a flat log of typed envelopes. Only "response_item"
// entries carry conversation (the "event_msg" family duplicates them for the
// TUI), so those are what get imported.

// CodexSessionsDir is where Codex stores its rollouts. CODEX_HOME wins, as it
// does for Codex itself.
func CodexSessionsDir() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return filepath.Join(h, "sessions")
	}
	home, err := homedir.Dir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex", "sessions")
}

// ScanCodex finds Codex's rollout files. Returns nothing (not an error) when
// Codex isn't installed, so an archive of one agent doesn't fail because
// another is absent.
func ScanCodex() ([]SessionFile, error) {
	root := CodexSessionsDir()
	if root == "" {
		return nil, nil
	}
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var files []SessionFile
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable date directory must not sink the scan
		}
		if d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		id := codexSessionIDFromFilename(path)
		if id == "" {
			return nil
		}
		files = append(files, SessionFile{
			Path:       path,
			SessionID:  id,
			ProjectKey: codexProjectKey(path),
			Agent:      AgentCodex,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// codexUUID matches the session id Codex appends to a rollout filename. The
// timestamp in front of it carries dashes too, so the id is recognised by shape
// rather than by splitting on the separator.
var codexUUID = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// codexSessionIDFromFilename pulls the session id out of
// "rollout-<timestamp>-<uuid>.jsonl". Codex's own `codex resume <id>` matches on
// exactly this id, and the header line repeats it.
func codexSessionIDFromFilename(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	return codexUUID.FindString(base)
}

// codexProjectKey derives the project key from the cwd in the file's header, so
// Codex sessions group with the Claude Code and pi sessions for the same
// directory. Files whose header is unreadable land in "-unknown" rather than
// being dropped: an unattributed conversation is still worth searching.
func codexProjectKey(path string) string {
	_, cwd, _ := codexSessionHeader(path)
	if cwd == "" {
		return "-unknown"
	}
	return mover.ProjectKeyFromCWD(cwd)
}

// codexSessionHeader reads the session_meta line at the top of a rollout. Codex
// records the cwd and git branch once, at the start, rather than on every entry.
func codexSessionHeader(path string) (id, cwd, branch string) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", ""
	}
	defer f.Close()

	// The header carries the model's full base instructions, so it is large but
	// bounded — a bigger buffer than the default, not an unbounded read.
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	if !sc.Scan() {
		return "", "", ""
	}

	var e codexEntry
	if json.Unmarshal(sc.Bytes(), &e) != nil || e.Type != "session_meta" {
		return "", "", ""
	}
	var p codexPayload
	if json.Unmarshal(e.Payload, &p) != nil {
		return "", "", ""
	}
	id = p.SessionID
	if id == "" {
		id = p.ID
	}
	branch = ""
	if p.Git != nil {
		branch = p.Git.Branch
	}
	return id, p.CWD, branch
}

// codexEntry is one line of a rollout: a typed envelope around a payload whose
// own shape depends on the envelope type.
type codexEntry struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

// codexPayload is the union of the payload fields remaimber reads. Codex writes
// far more than this; everything unread is agent mechanics.
type codexPayload struct {
	Type    string              `json:"type"`
	Role    string              `json:"role"`
	Content []codexContentBlock `json:"content"`
	ID      string              `json:"id"`

	// function_call / custom_tool_call
	Name   string `json:"name"`
	CallID string `json:"call_id"`
	Output string `json:"output"` // ...and their outputs

	// session_meta
	SessionID string `json:"session_id"`
	CWD       string `json:"cwd"`
	Git       *struct {
		Branch string `json:"branch"`
	} `json:"git"`

	// compacted
	Message string `json:"message"`
}

// codexContentBlock is a block of a message's content. Codex tags user text
// "input_text" and assistant text "output_text"; both are plain text.
type codexContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// codexInjectedTags wrap text Codex adds to a user turn rather than text the
// user typed. Stripped for the same reason Claude Code's system reminders are:
// left in, they make every session match a search for the harness's own scaffolding.
var codexInjectedTags = []*regexp.Regexp{
	regexp.MustCompile(`(?s)<environment_context>.*?</environment_context>`),
	regexp.MustCompile(`(?s)<user_instructions>.*?</user_instructions>`),
	regexp.MustCompile(`(?s)<turn_aborted>.*?</turn_aborted>`),
	// AGENTS.md, delivered as a user turn under a heading and an INSTRUCTIONS
	// block. Left in, it becomes the "first prompt" of every session that has an
	// AGENTS.md, and every one of them looks alike.
	regexp.MustCompile(`(?s)<INSTRUCTIONS>.*?</INSTRUCTIONS>`),
}

// codexInjectedLines are the sentences Codex wraps around an AGENTS.md block.
// They survive tag stripping because they sit outside the tags.
var codexInjectedLines = strings.NewReplacer(
	"# AGENTS.md instructions", "",
	"The previously provided AGENTS.md instructions no longer apply.", "",
	"These AGENTS.md instructions replace all previously provided AGENTS.md instructions.", "",
)

// ParseCodexLine parses one line of a Codex rollout into a Message. Returns nil
// for entries that carry no conversation content — reasoning, token counts, and
// the event_msg mirror of every message, which would otherwise double every turn.
func ParseCodexLine(sessionID string, line []byte) (*types.Message, error) {
	var e codexEntry
	if err := json.Unmarshal(line, &e); err != nil {
		return nil, err
	}
	if e.Type != "response_item" && e.Type != "compacted" {
		return nil, nil
	}
	var p codexPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return nil, err
	}

	m := &types.Message{
		SessionID:   sessionID,
		UUID:        p.ID,
		Timestamp:   e.Timestamp,
		ContentJSON: string(line),
	}

	if e.Type == "compacted" {
		// A compaction replaces history with a summary. Marked as such so it
		// anchors segmentation without counting as a turn.
		m.Role, m.Type = "user", "user"
		m.ContentText = CleanText(p.Message)
		m.IsCompactSummary = true
		return m, nil
	}

	switch p.Type {
	case "message":
		if p.Role == "developer" {
			// Harness instructions injected as a turn, not conversation.
			return nil, nil
		}
		m.Role = codexRole(p.Role)
		m.Type = m.Role
		m.ContentText = cleanCodexText(codexContentText(p.Content))

	case "function_call", "custom_tool_call":
		// A tool call is the assistant's, and is recorded with the same
		// "[tool: name]" marker the other parsers emit so a tool-only turn is
		// recognised identically across agents.
		m.Role, m.Type = "assistant", "assistant"
		m.ContentText = "[tool: " + p.Name + "]"

	case "function_call_output", "custom_tool_call_output":
		// Codex models a tool result as its own entry rather than a block inside
		// a user turn. Mapping it onto the same flag keeps every downstream
		// filter — summarization, segments, passages — working unchanged.
		m.Role, m.Type = "user", "user"
		m.ContentText = CleanText(p.Output)
		m.IsToolResult = true

	default:
		// reasoning, web_search_call, tool_search_call: no indexable content.
		return nil, nil
	}

	return m, nil
}

// codexRole maps a rollout role onto the two remaimber stores.
func codexRole(role string) string {
	if role == "assistant" {
		return "assistant"
	}
	return "user"
}

// codexContentText flattens a message's blocks into indexable text.
func codexContentText(blocks []codexContentBlock) string {
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "input_text", "output_text", "text":
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		// input_image and friends carry nothing worth indexing.
	}
	return strings.Join(parts, "\n")
}

// cleanCodexText removes Codex's own injections before the shared cleaner runs.
func cleanCodexText(s string) string {
	for _, re := range codexInjectedTags {
		s = re.ReplaceAllString(s, "")
	}
	return CleanText(codexInjectedLines.Replace(s))
}

// codexFirstPrompt returns the text of a user turn, for the session's
// opening-goal field. Empty for anything else, including the environment context
// Codex injects as the first user turn.
func codexFirstPrompt(line []byte) string {
	var e codexEntry
	if json.Unmarshal(line, &e) != nil || e.Type != "response_item" {
		return ""
	}
	var p codexPayload
	if json.Unmarshal(e.Payload, &p) != nil || p.Type != "message" || p.Role != "user" {
		return ""
	}
	text := cleanCodexText(codexContentText(p.Content))
	if len(text) > 200 {
		text = text[:200]
	}
	return text
}

// CodexSessionPath locates a rollout on disk. Codex's `resume` takes the session
// id from any directory, so this is only needed to tell a still-resumable
// session from one whose file has been cleaned up.
func CodexSessionPath(sessionID string) string {
	root := CodexSessionsDir()
	if root == "" || sessionID == "" {
		return ""
	}
	// Rollouts are filed under year/month/day.
	matches, err := filepath.Glob(filepath.Join(root, "*", "*", "*", "rollout-*-"+sessionID+".jsonl"))
	if err != nil || len(matches) == 0 {
		return ""
	}
	return matches[0]
}
