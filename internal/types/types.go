package types

import "encoding/json"

// Session represents a conversation session stored in the database.
type Session struct {
	SessionID      string  `json:"session_id"`
	ProjectKey     string  `json:"project_key"`
	ProjectPath    string  `json:"project_path"`
	CustomTitle    string  `json:"custom_title,omitempty"`
	FirstPrompt    string  `json:"first_prompt,omitempty"`
	GitBranch      string  `json:"git_branch,omitempty"`
	CWD            string  `json:"cwd,omitempty"`
	StartedAt      string  `json:"started_at,omitempty"`
	EndedAt        string  `json:"ended_at,omitempty"`
	MessageCount   int     `json:"message_count"`
	FileMtime      float64 `json:"-"`
	FileSize       int64   `json:"-"`
	LastByteOffset int64   `json:"-"`
	ImportedAt     string  `json:"imported_at,omitempty"`
}

// Message represents a single JSONL line stored in the database.
type Message struct {
	ID          int64  `json:"id"`
	SessionID   string `json:"session_id"`
	UUID        string `json:"uuid,omitempty"`
	ParentUUID  string `json:"parent_uuid,omitempty"`
	Type        string `json:"type"`
	Role        string `json:"role,omitempty"`
	ContentText string `json:"content_text,omitempty"`
	ContentJSON string `json:"content_json"`
	ContentHash string `json:"-"`
	Timestamp   string `json:"timestamp,omitempty"`
}

// SearchResult represents a search hit with context.
type SearchResult struct {
	SessionID   string `json:"session_id"`
	ProjectKey  string `json:"project_key"`
	CustomTitle string `json:"custom_title,omitempty"`
	Snippet     string `json:"snippet"`
	Timestamp   string `json:"timestamp,omitempty"`
	Type        string `json:"type"`
	Role        string `json:"role,omitempty"`
}

// JSONLLine represents a raw parsed JSONL line from a conversation file.
type JSONLLine struct {
	Type        string          `json:"type"`
	UUID        string          `json:"uuid,omitempty"`
	ParentUUID  string          `json:"parentUuid,omitempty"`
	SessionID   string          `json:"sessionId,omitempty"`
	Timestamp   string          `json:"timestamp,omitempty"`
	CWD         string          `json:"cwd,omitempty"`
	GitBranch   string          `json:"gitBranch,omitempty"`
	CustomTitle string          `json:"customTitle,omitempty"`
	Message     *MessageContent `json:"message,omitempty"`
	RawJSON     json.RawMessage `json:"-"`
}

// MessageContent represents the message field in user/assistant JSONL lines.
type MessageContent struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}
