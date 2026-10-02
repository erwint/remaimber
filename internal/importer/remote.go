package importer

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"github.com/erwint/remaimber/internal/homedir"
)

// A synced transcript is addressed by a key that is the same on every machine
// and in every store: "<agent>/<path under that agent's session root>", with
// forward slashes. The agent prefix is what lets a pulled file be parsed
// without guessing, and the path under the root is what each agent's own scan
// already derives the session id and project key from — so a pulled file is
// imported by the same rules as a local one.

// AgentRoots maps each agent to its session root relative to a home
// directory, which is where a remote machine keeps them too.
var AgentRoots = map[string]string{
	AgentClaude: ".claude/projects",
	AgentCodex:  ".codex/sessions",
	AgentPi:     ".pi/agent/sessions",
}

// localRoot returns the session root this machine uses for an agent.
func localRoot(agent string) string {
	switch agent {
	case AgentCodex:
		return CodexSessionsDir()
	case AgentPi:
		return PiSessionsDir()
	}
	home, err := homedir.Dir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

// RemoteKey returns the sync key for a local session file, or "" when the file
// does not sit under its agent's root.
func RemoteKey(sf SessionFile) string {
	root := localRoot(sf.AgentOf())
	if root == "" {
		return ""
	}
	rel, err := filepath.Rel(root, sf.Path)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return ""
	}
	return sf.AgentOf() + "/" + filepath.ToSlash(rel)
}

// RemoteSessionFile describes a pulled file for import: key is its sync key,
// localPath where the copy was written, origin the machine it came from. It
// fails for a key whose agent is unknown or whose name carries no session id,
// rather than importing a file under an id it does not have.
func RemoteSessionFile(key, localPath, origin string) (SessionFile, error) {
	agent, rel, ok := strings.Cut(key, "/")
	if !ok || rel == "" {
		return SessionFile{}, fmt.Errorf("sync key %q has no agent prefix", key)
	}
	if !strings.HasSuffix(rel, ".jsonl") {
		return SessionFile{}, fmt.Errorf("sync key %q is not a transcript", key)
	}
	sf := SessionFile{Path: localPath, Agent: agent, Origin: origin}
	dir := path.Dir(rel)

	switch agent {
	case AgentClaude:
		// <project key>/<session id>.jsonl. Subagent transcripts sit one level
		// deeper and are not sessions of their own, as in the local scan.
		if dir == "." || strings.Contains(dir, "/") {
			return SessionFile{}, fmt.Errorf("sync key %q is not a Claude Code session", key)
		}
		sf.ProjectKey = dir
		sf.SessionID = strings.TrimSuffix(path.Base(rel), ".jsonl")
	case AgentPi:
		if dir == "." || strings.Contains(dir, "/") {
			return SessionFile{}, fmt.Errorf("sync key %q is not a pi session", key)
		}
		sf.ProjectKey = dir
		sf.SessionID = piSessionIDFromFilename(rel)
	case AgentCodex:
		// Codex files by date, so the project comes from the header, as it does
		// for a local rollout.
		sf.SessionID = codexSessionIDFromFilename(rel)
		sf.ProjectKey = codexProjectKey(localPath)
	default:
		return SessionFile{}, fmt.Errorf("sync key %q names unknown agent %q", key, agent)
	}
	if sf.SessionID == "" {
		return SessionFile{}, fmt.Errorf("sync key %q carries no session id", key)
	}
	return sf, nil
}

// CheckRemoteKey reports whether a key names an importable session, without
// the file, so a listing can be filtered before anything is fetched.
func CheckRemoteKey(key string) error {
	_, err := RemoteSessionFile(key, "", "")
	return err
}
