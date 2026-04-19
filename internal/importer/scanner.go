package importer

import (
	"os"
	"path/filepath"
	"strings"
)

// SessionFile represents a discovered JSONL conversation file.
type SessionFile struct {
	Path       string
	SessionID  string
	ProjectKey string
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
	// Replace leading dash with /, then remaining dashes with /
	// But dashes within directory names are ambiguous — we do best-effort
	return "/" + strings.ReplaceAll(key[1:], "-", "/")
}

// PrettyProjectName formats a project key for display.
// Extracts the last 2 path components: "-Volumes-Data-src-owner-repo" -> "owner/repo"
// Falls back to full key if too short.
func PrettyProjectName(key string) string {
	if key == "" {
		return ""
	}
	// Remove leading dash and split
	parts := strings.Split(key[1:], "-")
	if len(parts) <= 2 {
		return strings.Join(parts, "/")
	}
	return strings.Join(parts[len(parts)-2:], "/")
}
