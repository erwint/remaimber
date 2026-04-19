package importer

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/erwin/remaimber/internal/types"
)

// systemTagPatterns matches system-injected XML noise tags and their content.
var systemTagPatterns []*regexp.Regexp

func init() {
	tags := []string{
		"system-reminder", "local-command-caveat", "local-command-stdout",
		"command-name", "command-message", "command-args", "task-notification",
	}
	for _, tag := range tags {
		systemTagPatterns = append(systemTagPatterns,
			regexp.MustCompile(`(?s)<`+tag+`>.*?</`+tag+`>`))
	}
}

// ParseLine parses a single JSONL line and returns a Message ready for insertion.
func ParseLine(sessionID string, line []byte) (*types.Message, error) {
	var jl types.JSONLLine
	if err := json.Unmarshal(line, &jl); err != nil {
		return nil, fmt.Errorf("parse json: %w", err)
	}
	jl.RawJSON = line

	m := &types.Message{
		SessionID:  sessionID,
		UUID:       jl.UUID,
		ParentUUID: jl.ParentUUID,
		Type:       jl.Type,
		Timestamp:  jl.Timestamp,
		ContentJSON: string(line),
	}

	// For lines without UUID, compute content hash for dedup
	if jl.UUID == "" {
		h := sha256.Sum256(line)
		m.ContentHash = fmt.Sprintf("%x", h)
	}

	// Extract searchable text and role
	switch jl.Type {
	case "user", "assistant":
		if jl.Message != nil {
			m.Role = jl.Message.Role
			m.ContentText = CleanText(extractMessageText(jl.Message))
		}
	case "custom-title":
		m.ContentText = jl.CustomTitle
	}
	// progress, file-history-snapshot: content_text stays empty (not indexed)

	return m, nil
}

// ExtractSessionMeta extracts session-level metadata from a parsed JSONL line.
func ExtractSessionMeta(jl *types.JSONLLine) (cwd, gitBranch, customTitle, firstPrompt string) {
	cwd = jl.CWD
	gitBranch = jl.GitBranch
	customTitle = jl.CustomTitle

	if jl.Type == "user" && jl.Message != nil {
		text := extractMessageText(jl.Message)
		if len(text) > 200 {
			text = text[:200]
		}
		firstPrompt = text
	}
	return
}

func extractMessageText(msg *types.MessageContent) string {
	if msg == nil {
		return ""
	}

	// content can be a string or an array of content blocks
	raw := msg.Content

	// Try as string first
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}

	// Try as array of content blocks
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}

	var text string
	for _, block := range blocks {
		var cb struct {
			Type string `json:"type"`
			Text string `json:"text"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(block, &cb); err != nil {
			continue
		}

		switch cb.Type {
		case "text":
			if text != "" {
				text += "\n"
			}
			text += cb.Text
		case "tool_use":
			if text != "" {
				text += "\n"
			}
			text += "[tool: " + cb.Name + "]"
		case "tool_result":
			// Extract text from tool result content
			var tr struct {
				Content json.RawMessage `json:"content"`
			}
			if err := json.Unmarshal(block, &tr); err != nil {
				continue
			}
			trText := extractToolResultText(tr.Content)
			if trText != "" {
				if text != "" {
					text += "\n"
				}
				text += trText
			}
		}
		// skip "thinking" blocks — not indexed
	}
	return text
}

// CleanText strips system-injected XML tags and noise from extracted text.
func CleanText(s string) string {
	for _, re := range systemTagPatterns {
		s = re.ReplaceAllString(s, "")
	}
	s = strings.TrimSpace(s)
	return s
}

func extractToolResultText(content json.RawMessage) string {
	if content == nil {
		return ""
	}
	// Can be a string
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return s
	}
	// Can be an array of blocks
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &blocks); err != nil {
		return ""
	}
	var text string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			if text != "" {
				text += "\n"
			}
			text += b.Text
		}
	}
	return text
}
