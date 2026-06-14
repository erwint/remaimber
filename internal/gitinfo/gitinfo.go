// Package gitinfo resolves durable, worktree-independent git identity for a
// directory. The anchor is realpath(git-common-dir): it is identical across
// every worktree of a repository and resolves symlinks, so symlinked and
// canonical launches collapse to one identity.
package gitinfo

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Identity is the durable identity of a session's working directory.
// Any field may be empty if it could not be resolved.
type Identity struct {
	RepoID       string // realpath(git rev-parse --git-common-dir) — the anchor
	Subpath      string // git rev-parse --show-prefix, verbatim ('' = repo root)
	WorktreeRoot string // realpath(git rev-parse --show-toplevel) — carrier path
}

// gitTimeout bounds each git invocation so a slow/broken repo never stalls a hook.
const gitTimeout = 2 * time.Second

// Resolve runs the git primitives in dir and returns its identity, or nil if
// dir is not inside a git repository (or git is unavailable). It never blocks
// for longer than gitTimeout and treats every failure as "not resolvable".
func Resolve(dir string) *Identity {
	if dir == "" {
		return nil
	}

	out := gitRevParse(dir)
	if out == nil {
		return nil
	}
	commonDir, subpath, topLevel := out[0], out[1], out[2]
	if commonDir == "" {
		return nil
	}

	id := &Identity{Subpath: subpath}

	// --git-common-dir may be relative to dir (e.g. ".git" at repo root).
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(dir, commonDir)
	}
	id.RepoID = realpath(commonDir)

	if topLevel != "" {
		id.WorktreeRoot = realpath(topLevel)
	}

	if id.RepoID == "" {
		return nil
	}
	return id
}

// gitRevParse runs a single rev-parse that emits common-dir, show-prefix, and
// show-toplevel on three lines. Returns nil on any failure. show-prefix is
// empty at the repo root, which is a valid (not failed) result.
func gitRevParse(dir string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "rev-parse",
		"--git-common-dir", "--show-prefix", "--show-toplevel")
	cmd.Dir = dir
	stdout, err := cmd.Output()
	if err != nil {
		return nil
	}

	lines := strings.Split(strings.TrimRight(string(stdout), "\n"), "\n")
	// Expect exactly 3 lines; show-prefix may be blank.
	for len(lines) < 3 {
		lines = append(lines, "")
	}
	return []string{
		strings.TrimSpace(lines[0]),
		strings.TrimRight(lines[1], "/"),
		strings.TrimSpace(lines[2]),
	}
}

// realpath resolves symlinks; falls back to a cleaned absolute path on failure.
func realpath(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}
