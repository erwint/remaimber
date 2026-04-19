package mover

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Move moves or copies a conversation JSONL file to a different project.
func Move(sessionID, targetProject string, copyOnly bool) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	projectsDir := filepath.Join(home, ".claude", "projects")

	// Find source file
	srcPath, srcProject, err := findSession(projectsDir, sessionID)
	if err != nil {
		return err
	}

	targetDir := filepath.Join(projectsDir, targetProject)
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return fmt.Errorf("create target dir: %w", err)
	}

	dstPath := filepath.Join(targetDir, sessionID+".jsonl")
	if _, err := os.Stat(dstPath); err == nil {
		return fmt.Errorf("session already exists in target project %s", targetProject)
	}

	// Copy file
	if err := copyFile(srcPath, dstPath); err != nil {
		return fmt.Errorf("copy: %w", err)
	}

	// Update sessions-index.json in target
	if err := addToSessionsIndex(targetDir, sessionID); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not update target sessions-index.json: %v\n", err)
	}

	// If moving (not copying), remove source and update source index
	if !copyOnly {
		os.Remove(srcPath)
		// Also remove session subdirectory if it exists
		subdir := filepath.Join(filepath.Dir(srcPath), sessionID)
		os.RemoveAll(subdir)
		if err := removeFromSessionsIndex(filepath.Join(projectsDir, srcProject), sessionID); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not update source sessions-index.json: %v\n", err)
		}
	}

	return nil
}

func findSession(projectsDir, sessionID string) (path, projectKey string, err error) {
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return "", "", err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidate := filepath.Join(projectsDir, entry.Name(), sessionID+".jsonl")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, entry.Name(), nil
		}
	}
	return "", "", fmt.Errorf("session %s not found in any project", sessionID)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}

type sessionsIndex struct {
	Sessions []sessionEntry `json:"sessions"`
}

type sessionEntry struct {
	ID string `json:"id"`
}

func readSessionsIndex(dir string) (*sessionsIndex, error) {
	path := filepath.Join(dir, "sessions-index.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &sessionsIndex{}, nil
		}
		return nil, err
	}
	var idx sessionsIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		return &sessionsIndex{}, nil // treat corrupt as empty
	}
	return &idx, nil
}

func writeSessionsIndex(dir string, idx *sessionsIndex) error {
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "sessions-index.json"), data, 0644)
}

func addToSessionsIndex(dir, sessionID string) error {
	idx, err := readSessionsIndex(dir)
	if err != nil {
		return err
	}
	for _, s := range idx.Sessions {
		if s.ID == sessionID {
			return nil // already present
		}
	}
	idx.Sessions = append(idx.Sessions, sessionEntry{ID: sessionID})
	return writeSessionsIndex(dir, idx)
}

func removeFromSessionsIndex(dir, sessionID string) error {
	idx, err := readSessionsIndex(dir)
	if err != nil {
		return err
	}
	filtered := idx.Sessions[:0]
	for _, s := range idx.Sessions {
		if s.ID != sessionID {
			filtered = append(filtered, s)
		}
	}
	idx.Sessions = filtered
	return writeSessionsIndex(dir, idx)
}
