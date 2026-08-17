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
	// Agent is the coding agent that produced the conversation ("claude", "pi").
	// Empty on rows imported before multi-agent support, which means Claude Code.
	Agent string `json:"agent,omitempty"`

	// Rolling summary (Phase 5). SummaryOffset is the message-id high-water mark
	// the summary reflects. Always emitted (even empty) so consumers get a stable
	// schema and can tell "no summary yet" from a missing field.
	Summary       string `json:"summary"`
	SummaryOffset int64  `json:"-"`

	// Durable cross-worktree identity, populated via LEFT JOIN session_identity.
	RepoID       string `json:"repo_id,omitempty"`
	Subpath      string `json:"subpath,omitempty"`
	WorktreeRoot string `json:"worktree_root,omitempty"`
	IdentityCWD  string `json:"identity_cwd,omitempty"`
}

// SessionIdentity is the durable, worktree-independent identity of a session,
// captured at session start so it survives deletion of the worktree.
type SessionIdentity struct {
	SessionID    string `json:"session_id"`
	RepoID       string `json:"repo_id,omitempty"`
	Subpath      string `json:"subpath,omitempty"`
	WorktreeRoot string `json:"worktree_root,omitempty"`
	CWD          string `json:"cwd,omitempty"`
	CapturedAt   string `json:"captured_at,omitempty"`
	PID          int    `json:"pid,omitempty"`
	EndedAt      string `json:"ended_at,omitempty"`
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
	// Shape flags, lifted out of content_json at import so queries can filter on
	// an index instead of scanning the largest column in the database.
	IsSidechain      bool `json:"-"`
	IsCompactSummary bool `json:"-"`
	IsToolResult     bool `json:"-"`
	// SegmentSeq is populated only when a message is loaded as part of a segment
	// selection (a partial resume); zero otherwise.
	SegmentSeq int `json:"segment_seq,omitempty"`
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
	Summary     string `json:"summary,omitempty"`
	RepoID      string `json:"repo_id,omitempty"`
	CWD         string `json:"cwd,omitempty"`
	// SegmentSeq locates the hit within the session, so a caller can pull just
	// that part of the conversation instead of the whole thing. -1 when the
	// session has not been segmented yet.
	SegmentSeq int `json:"segment_seq"`
	// Agent is the coding agent the conversation came from; how it is resumed
	// depends on it.
	Agent string `json:"agent,omitempty"`
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
