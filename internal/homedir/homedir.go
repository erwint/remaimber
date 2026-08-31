// Package homedir resolves the user's home directory without depending on the
// environment.
//
// Everything remaimber reads lives under the home directory — the archive, and
// every agent's transcripts — so a process that cannot name it can do nothing at
// all. os.UserHomeDir consults $HOME and nothing else, which is fine for a
// terminal but not for how remaimber is actually launched: an MCP server is
// spawned by an agent, and some agents (Codex among them) hand stdio servers a
// whitelisted environment that need not carry HOME. The passwd database has the
// same answer and does not depend on the environment.
package homedir

import (
	"errors"
	"os"
	"os/user"
)

// Dir returns the user's home directory, falling back to the passwd entry when
// the environment carries no HOME.
func Dir() (string, error) {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h, nil
	}
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	if u.HomeDir == "" {
		return "", errors.New("no home directory for the current user")
	}
	return u.HomeDir, nil
}
