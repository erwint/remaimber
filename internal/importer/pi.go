package importer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/erwin/remaimber/internal/db"
	"github.com/erwin/remaimber/internal/homedir"
	"github.com/erwin/remaimber/internal/types"
)

// pi (the pi coding agent) keeps conversations in the same broad shape as Claude
// Code — one JSONL file per session, under a directory named after the project —
// but the encoding differs in every detail, so it gets its own scanner and parser
// rather than a widened Claude Code one.
//
//	~/.pi/agent/sessions/--Volumes-Data-src-kyon--/2026-08-17T13-04-08-262Z_<uuid>.jsonl
//
// The project key is dash-encoded like Claude Code's but fenced with a doubled
// dash at each end, and the filename carries a timestamp before the session id.
// Inside, entries form an append-only tree keyed by id/parentId; the first line
// is a header carrying the session id and the cwd.

// PiSessionsDir is where pi stores its session tree.
func PiSessionsDir() string {
	home, err := homedir.Dir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".pi", "agent", "sessions")
}

// ScanPi finds pi's session files. Returns nothing (not an error) when pi isn't
// installed, so an archive of one agent doesn't fail because the other is absent.
func ScanPi() ([]SessionFile, error) {
	root := PiSessionsDir()
	if root == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var files []SessionFile
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		key := entry.Name()
		matches, err := filepath.Glob(filepath.Join(root, key, "*.jsonl"))
		if err != nil {
			continue
		}
		for _, f := range matches {
			id := piSessionIDFromFilename(f)
			if id == "" {
				continue
			}
			files = append(files, SessionFile{
				Path:       f,
				SessionID:  id,
				ProjectKey: key,
				Agent:      AgentPi,
			})
		}
	}
	return files, nil
}

// piSessionIDFromFilename pulls the session id out of "<timestamp>_<uuid>.jsonl".
// The header line carries the same id, but the filename is available before the
// file is read and is what pi's own --session lookup matches on.
func piSessionIDFromFilename(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if _, id, ok := strings.Cut(base, "_"); ok {
		return id
	}
	return base
}

// PiProjectPathFromKey decodes pi's "--Volumes-Data-src-kyon--" fencing before
// falling back to the shared dash decoding. Both encodings lose the distinction
// between a path separator and a literal dash, so this is best-effort in the same
// way Claude Code's is — the session's own recorded cwd is authoritative.
func PiProjectPathFromKey(key string) string {
	return ProjectPathFromKey(normalizePiKey(key))
}

// normalizePiKey strips pi's doubled-dash fences, yielding a key in the same
// shape Claude Code uses so the existing decoders apply unchanged.
func normalizePiKey(key string) string {
	key = strings.TrimSuffix(key, "--")
	if strings.HasPrefix(key, "--") {
		key = key[1:] // leave one leading dash, as Claude Code keys carry
	}
	return key
}

// piEntry is one line of a pi session file. The header (type "session") and the
// tree entries share enough shape to decode together.
type piEntry struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	ParentID  string          `json:"parentId"`
	Timestamp string          `json:"timestamp"`
	CWD       string          `json:"cwd"`     // header only
	Summary   string          `json:"summary"` // compaction / branch_summary only
	Message   *piMessage      `json:"message"`
	Raw       json.RawMessage `json:"-"`
}

// piMessage covers pi's three message roles. Content is a union: user messages
// may carry a bare string, everything else an array of typed blocks.
type piMessage struct {
	Role     string          `json:"role"`
	Content  json.RawMessage `json:"content"`
	ToolName string          `json:"toolName"`
	Model    string          `json:"model"`
	Provider string          `json:"provider"`
}

// ParsePiLine parses one line of a pi session file into a Message. Returns nil
// for entries that carry no conversation content (model changes, labels, session
// metadata) — they are bookkeeping, not turns.
func ParsePiLine(sessionID string, line []byte) (*types.Message, error) {
	var e piEntry
	if err := json.Unmarshal(line, &e); err != nil {
		return nil, err
	}

	m := &types.Message{
		SessionID:   sessionID,
		UUID:        e.ID,
		ParentUUID:  e.ParentID,
		Type:        e.Type,
		Timestamp:   e.Timestamp,
		ContentJSON: string(line),
	}

	switch e.Type {
	case "message":
		if e.Message == nil {
			return nil, nil
		}
		m.Role = piRole(e.Message.Role)
		m.Type = m.Role
		m.ContentText = CleanText(piContentText(e.Message))
		// pi models a tool result as its own role rather than a block inside a
		// user turn. Mapping it onto the same flag keeps every downstream filter
		// — summarization, segments, passages — working without a special case.
		m.IsToolResult = e.Message.Role == "toolResult"

	case "compaction", "branch_summary":
		// A compaction entry is a summary of what came before, not a turn. Marked
		// as such so it anchors segmentation without being counted as content.
		m.Role = "user"
		m.Type = "user"
		m.ContentText = CleanText(e.Summary)
		m.IsCompactSummary = true

	default:
		// session header, model_change, thinking_level_change, label,
		// session_info, custom: no conversation content.
		return nil, nil
	}

	if strings.TrimSpace(m.ContentText) == "" && !m.IsCompactSummary {
		// Keep the row (it holds tree structure) but with nothing to index.
		m.ContentText = ""
	}
	return m, nil
}

// piRole maps pi's roles onto the two remaimber stores. A tool result is recorded
// as a user turn carrying the tool-result flag, matching how Claude Code encodes
// the same thing, so role-based queries mean the same across both agents.
func piRole(role string) string {
	switch role {
	case "assistant":
		return "assistant"
	default: // "user", "toolResult"
		return "user"
	}
}

// piContentText flattens a message's content into indexable text. Text blocks are
// joined; a tool call becomes the same "[tool: name]" marker Claude Code's parser
// emits, so tool-only turns are recognised identically; thinking blocks are
// skipped, as they are for Claude Code.
func piContentText(msg *piMessage) string {
	if len(msg.Content) == 0 {
		return ""
	}
	// A user message may carry a bare string instead of blocks.
	var s string
	if err := json.Unmarshal(msg.Content, &s); err == nil {
		return s
	}

	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		return ""
	}

	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		case "toolCall":
			parts = append(parts, "[tool: "+b.Name+"]")
		}
		// "thinking" and "image" carry nothing worth indexing.
	}
	return strings.Join(parts, "\n")
}

// piSessionHeader reads the header line's session id and cwd. pi records the cwd
// once, at the top of the file, rather than on every line.
func piSessionHeader(line []byte) (id, cwd string, ok bool) {
	var e piEntry
	if err := json.Unmarshal(line, &e); err != nil || e.Type != "session" {
		return "", "", false
	}
	return e.ID, e.CWD, true
}

// piFirstPrompt returns the text of a user turn, for the session's opening-goal
// field. Empty for anything else.
func piFirstPrompt(line []byte) string {
	var e piEntry
	if err := json.Unmarshal(line, &e); err != nil || e.Type != "message" || e.Message == nil {
		return ""
	}
	if e.Message.Role != "user" {
		return ""
	}
	text := CleanText(piContentText(e.Message))
	if len(text) > 200 {
		text = text[:200]
	}
	return text
}

// compile-time guard: the pi parser must keep producing rows the shared shape
// classifier can read, since import writes those columns directly.
var _ = db.ClassifyRaw

// PiSessionPath locates a pi session file on disk. pi's --session accepts an
// absolute path, so resuming a pi conversation needs no equivalent of Claude
// Code's carrier-key linking — the path alone works from any directory.
func PiSessionPath(projectKey, sessionID string) string {
	root := PiSessionsDir()
	if root == "" || projectKey == "" || sessionID == "" {
		return ""
	}
	matches, err := filepath.Glob(filepath.Join(root, projectKey, "*_"+sessionID+".jsonl"))
	if err != nil || len(matches) == 0 {
		return ""
	}
	return matches[0]
}
