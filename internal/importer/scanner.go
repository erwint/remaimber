package importer

import (
	"os"
	"path/filepath"
	"strings"
)

// Agent names the coding agent a conversation came from. Sessions from different
// agents live in one archive so search and recall span them, but they are parsed,
// keyed, and resumed differently, so each row records its origin.
const (
	AgentClaude = "claude"
	AgentPi     = "pi"
)

// SessionFile represents a discovered JSONL conversation file.
type SessionFile struct {
	Path       string
	SessionID  string
	ProjectKey string
	// Agent is the coding agent that wrote the file. Empty means Claude Code,
	// so rows imported before pi support keep their meaning.
	Agent string
}

// AgentOf returns the file's agent, defaulting to Claude Code.
func (s SessionFile) AgentOf() string {
	if s.Agent == "" {
		return AgentClaude
	}
	return s.Agent
}

// ScanAll finds conversations from every supported agent. A failure to scan one
// agent does not hide the others: an archive is more useful partially filled than
// not at all, and a missing agent directory is the normal case rather than a fault.
func ScanAll() ([]SessionFile, error) {
	files, err := ScanProjects()
	if err != nil {
		return nil, err
	}
	pi, piErr := ScanPi()
	files = append(files, pi...)
	if err == nil && piErr != nil && len(files) == 0 {
		return nil, piErr
	}
	return files, nil
}

// ScanProjects scans ~/.claude/projects/ for JSONL conversation files.
func ScanProjects() ([]SessionFile, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	projectsDir := filepath.Join(home, ".claude", "projects")

	entries, err := os.ReadDir(projectsDir)
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
		projectKey := entry.Name()
		projectDir := filepath.Join(projectsDir, projectKey)

		jsonlFiles, err := filepath.Glob(filepath.Join(projectDir, "*.jsonl"))
		if err != nil {
			continue
		}
		for _, f := range jsonlFiles {
			sessionID := strings.TrimSuffix(filepath.Base(f), ".jsonl")
			files = append(files, SessionFile{
				Path:       f,
				SessionID:  sessionID,
				ProjectKey: projectKey,
				Agent:      AgentClaude,
			})
		}
	}
	return files, nil
}

// ProjectPathFromKey converts a project key back to a path.
// e.g., "-Volumes-Data-src-foo" -> "/Volumes/Data/src/foo"
func ProjectPathFromKey(key string) string {
	if key == "" {
		return ""
	}
	key = normalizePiKey(key)
	// Replace leading dash with /, then remaining dashes with /
	// But dashes within directory names are ambiguous — we do best-effort
	return "/" + strings.ReplaceAll(key[1:], "-", "/")
}

// SessionFileExists checks if a session's JSONL file still exists on disk.
// Agent may be empty for rows predating multi-agent support, which means Claude Code.
func SessionFileExists(projectKey, sessionID, agent string) bool {
	if agent == AgentPi {
		return PiSessionPath(projectKey, sessionID) != ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	path := filepath.Join(home, ".claude", "projects", projectKey, sessionID+".jsonl")
	_, err = os.Stat(path)
	return err == nil
}

// PrettyProjectName formats a project key for display.
// Extracts the last 2 path components: "-Volumes-Data-src-owner-repo" -> "owner/repo"
// Falls back to full key if too short.
func PrettyProjectName(key string) string {
	if key == "" {
		return ""
	}
	key = normalizePiKey(key)
	// Remove leading dash and split
	parts := strings.Split(key[1:], "-")
	if len(parts) <= 2 {
		return strings.Join(parts, "/")
	}
	return strings.Join(parts[len(parts)-2:], "/")
}
