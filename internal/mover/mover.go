package mover

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ProjectKeyFromCWD derives Claude Code's project-key encoding for a working
// directory: path separators and dots become dashes. This is the forward
// (lossless-enough) encoding — the inverse is lossy and must not be relied on.
//
//	"/Volumes/Data/src/foo" -> "-Volumes-Data-src-foo"
func ProjectKeyFromCWD(cwd string) string {
	var b strings.Builder
	for _, r := range cwd {
		if r == '/' || r == '.' {
			b.WriteRune('-')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// CarrierKeyForCWD returns the project key under which Claude stores sessions
// launched from cwd (the "carrier"). It prefers an existing project directory
// (authoritative, created by Claude for the live session), then falls back to
// scanning for a dir whose sessions ran in cwd, then to the forward encoding.
func CarrierKeyForCWD(cwd string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	projectsDir := filepath.Join(home, ".claude", "projects")
	encoded := ProjectKeyFromCWD(cwd)

	if _, err := os.Stat(filepath.Join(projectsDir, encoded)); err == nil {
		return encoded, nil
	}
	if key := findKeyByCWD(projectsDir, cwd); key != "" {
		return key, nil
	}
	return encoded, nil
}

// findKeyByCWD scans project dirs for one whose first session file records the
// given cwd. Best-effort: reads only the first line of one JSONL per dir.
func findKeyByCWD(projectsDir, cwd string) string {
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		matches, _ := filepath.Glob(filepath.Join(projectsDir, entry.Name(), "*.jsonl"))
		for _, f := range matches {
			if firstLineCWD(f) == cwd {
				return entry.Name()
			}
			break // only check the first file per dir
		}
	}
	return ""
}

func firstLineCWD(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	if sc.Scan() {
		var line struct {
			CWD string `json:"cwd"`
		}
		if json.Unmarshal(sc.Bytes(), &line) == nil {
			return line.CWD
		}
	}
	return ""
}

// SetIndexSummary best-effort updates the `summary` field of a session's entry
// in Claude Code's native sessions-index.json, preserving every other field and
// the file's structure. No-op if the file, the entries array, or the entry is
// absent. This is what surfaces remaimber summaries in Claude's resume dialog.
func SetIndexSummary(projectKey, sessionID, summary string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	path := filepath.Join(home, ".claude", "projects", projectKey, "sessions-index.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var idx map[string]any
	if json.Unmarshal(data, &idx) != nil {
		return nil // not Claude's format; leave untouched
	}
	entries, ok := idx["entries"].([]any)
	if !ok {
		return nil
	}
	changed := false
	for _, e := range entries {
		em, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if em["sessionId"] == sessionID {
			em["summary"] = summary
			changed = true
			break
		}
	}
	if !changed {
		return nil
	}
	out, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0644)
}

// LinkIntoProject copies a session's JSONL into targetProject so Claude Code can
// resume it from that project's cwd (the carrier). Idempotent: if the session is
// already present in the target, it is treated as success.
func LinkIntoProject(sessionID, targetProject string) error {
	err := Move(sessionID, targetProject, true)
	if err != nil && strings.Contains(err.Error(), "already exists") {
		return nil
	}
	return err
}

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
