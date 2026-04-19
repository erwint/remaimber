package importer

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/erwin/remaimber/internal/db"
	"github.com/erwin/remaimber/internal/types"
)

// ImportStats tracks import results.
type ImportStats struct {
	FilesScanned  int
	FilesSkipped  int
	FilesImported int
	MessagesNew   int
	MessagesSkip  int
	Errors        int
}

// ImportAll scans and imports all conversation files.
func ImportAll(database *sql.DB, force bool) (*ImportStats, error) {
	files, err := ScanProjects()
	if err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}

	stats := &ImportStats{FilesScanned: len(files)}
	for _, f := range files {
		imported, newMsgs, skipMsgs, err := ImportFile(database, f, force)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error importing %s: %v\n", f.Path, err)
			stats.Errors++
			continue
		}
		if imported {
			stats.FilesImported++
		} else {
			stats.FilesSkipped++
		}
		stats.MessagesNew += newMsgs
		stats.MessagesSkip += skipMsgs
	}
	return stats, nil
}

// ImportFile imports a single JSONL conversation file.
// Returns whether anything was imported and counts of new/skipped messages.
func ImportFile(database *sql.DB, sf SessionFile, force bool) (imported bool, newMsgs, skipMsgs int, err error) {
	// Check file info
	info, err := os.Stat(sf.Path)
	if err != nil {
		return false, 0, 0, err
	}
	mtime := float64(info.ModTime().UnixMilli()) / 1000.0
	size := info.Size()

	// Check if we can skip this file
	if !force {
		prevMtime, prevSize, _, found, err := db.GetSessionMeta(database, sf.SessionID)
		if err != nil {
			return false, 0, 0, err
		}
		if found && prevMtime == mtime && prevSize == size {
			return false, 0, 0, nil // unchanged
		}
	}

	// Get existing offset for incremental parsing
	var offset int64
	if !force {
		_, _, offset, _, _ = db.GetSessionMeta(database, sf.SessionID)
	}

	f, err := os.Open(sf.Path)
	if err != nil {
		return false, 0, 0, err
	}
	defer f.Close()

	// Seek to last known offset
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			// If seek fails, start from beginning
			f.Seek(0, io.SeekStart)
			offset = 0
		}
	}

	// Parse lines
	var messages []*types.Message
	var sessionMeta sessionMetaAccumulator
	sessionMeta.projectKey = sf.ProjectKey
	sessionMeta.projectPath = ProjectPathFromKey(sf.ProjectKey)

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024) // 10MB max line
	bytesRead := offset

	for scanner.Scan() {
		line := scanner.Bytes()
		bytesRead += int64(len(line)) + 1 // +1 for newline

		msg, err := ParseLine(sf.SessionID, line)
		if err != nil {
			continue // skip unparseable lines
		}
		messages = append(messages, msg)

		// Accumulate session metadata
		var jl types.JSONLLine
		if json.Unmarshal(line, &jl) == nil {
			sessionMeta.update(&jl)
		}
	}
	if err := scanner.Err(); err != nil {
		return false, 0, 0, fmt.Errorf("scan: %w", err)
	}

	if len(messages) == 0 && offset > 0 {
		// No new lines, but mtime changed (maybe truncation or touch) — just update tracking
		tx, err := database.Begin()
		if err != nil {
			return false, 0, 0, err
		}
		_, err = tx.Exec(`UPDATE sessions SET file_mtime = ?, file_size = ? WHERE session_id = ?`,
			mtime, size, sf.SessionID)
		if err != nil {
			tx.Rollback()
			return false, 0, 0, err
		}
		return false, 0, 0, tx.Commit()
	}

	// Insert in a transaction
	tx, err := database.Begin()
	if err != nil {
		return false, 0, 0, err
	}
	defer tx.Rollback()

	// Ensure session exists before inserting messages (FK constraint)
	sess := &types.Session{
		SessionID:      sf.SessionID,
		ProjectKey:     sessionMeta.projectKey,
		ProjectPath:    sessionMeta.projectPath,
		CustomTitle:    sessionMeta.customTitle,
		FirstPrompt:    sessionMeta.firstPrompt,
		GitBranch:      sessionMeta.gitBranch,
		CWD:            sessionMeta.cwd,
		StartedAt:      sessionMeta.startedAt,
		EndedAt:        sessionMeta.endedAt,
		MessageCount:   0,
		FileMtime:      mtime,
		FileSize:       size,
		LastByteOffset: bytesRead,
	}
	if err := db.UpsertSession(tx, sess); err != nil {
		return false, 0, 0, fmt.Errorf("upsert session: %w", err)
	}

	for _, msg := range messages {
		if err := db.InsertMessage(tx, msg); err != nil {
			return false, 0, 0, fmt.Errorf("insert message: %w", err)
		}
		newMsgs++ // approximate — INSERT OR IGNORE doesn't tell us if it was a dup
	}

	// Get accurate message count
	var msgCount int
	err = tx.QueryRow(`SELECT COUNT(*) FROM messages WHERE session_id = ?`, sf.SessionID).Scan(&msgCount)
	if err != nil {
		return false, 0, 0, err
	}

	// Update session with accurate message count
	sess.MessageCount = msgCount
	if err := db.UpsertSession(tx, sess); err != nil {
		return false, 0, 0, fmt.Errorf("update session: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return false, 0, 0, fmt.Errorf("commit: %w", err)
	}
	return true, newMsgs, skipMsgs, nil
}

type sessionMetaAccumulator struct {
	projectKey  string
	projectPath string
	customTitle string
	firstPrompt string
	gitBranch   string
	cwd         string
	startedAt   string
	endedAt     string
	seenUser    bool
}

func (a *sessionMetaAccumulator) update(jl *types.JSONLLine) {
	if jl.CWD != "" {
		a.cwd = jl.CWD
	}
	if jl.GitBranch != "" {
		a.gitBranch = jl.GitBranch
	}
	if jl.CustomTitle != "" {
		a.customTitle = jl.CustomTitle
	}
	if jl.Timestamp != "" {
		if a.startedAt == "" || jl.Timestamp < a.startedAt {
			a.startedAt = jl.Timestamp
		}
		if a.endedAt == "" || jl.Timestamp > a.endedAt {
			a.endedAt = jl.Timestamp
		}
	}
	if jl.Type == "user" && !a.seenUser && jl.Message != nil {
		text := extractMessageText(jl.Message)
		if len(text) > 200 {
			text = text[:200]
		}
		a.firstPrompt = text
		a.seenUser = true
	}
}
