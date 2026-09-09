// Package gitinfo resolves durable, worktree-independent git identity for a
// directory. The anchor is realpath(git-common-dir): it is identical across
// every worktree of a repository and resolves symlinks, so symlinked and
// canonical launches collapse to one identity.
package gitinfo

import (
	"context"
	"fmt"
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

// A recalled session carries the branch it ran on, and that fact is as old as
// the conversation. Telling an agent to `git checkout` it is wrong in three
// ordinary cases: the branch is checked out in another worktree, where git
// refuses with "already used by worktree at ..."; the work has since been merged
// here, so switching away is the opposite of what is wanted; or the branch is
// gone. Each looks to the agent like the archive contradicting the repo, so the
// captured branch is resolved against the repo as it is now before it is shown.

// BranchState is a captured branch seen from the current worktree.
type BranchState struct {
	Name         string // the branch as captured
	Exists       bool   // still a branch in this repo
	Current      bool   // it is what HEAD points at here
	Merged       bool   // its tip is an ancestor of HEAD here
	CheckedOutAt string // another worktree holding it, "" if none
}

// ResolveBranch reports where a captured branch stands relative to dir. A nil
// result means the question could not be answered — dir is not a repo, or git
// is unavailable — and the caller should say nothing rather than guess.
func ResolveBranch(dir, branch string) *BranchState {
	if dir == "" || branch == "" || Resolve(dir) == nil {
		return nil
	}
	st := &BranchState{Name: branch}

	st.Exists = git(dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch) != nil
	if head := git(dir, "rev-parse", "--abbrev-ref", "HEAD"); head != nil {
		st.Current = *head == branch
	}
	if st.Exists && !st.Current {
		// merge-base --is-ancestor exits 0 when the branch is already contained
		// in HEAD, which is the "you have this work already" case.
		st.Merged = git(dir, "merge-base", "--is-ancestor", branch, "HEAD") != nil
		st.CheckedOutAt = worktreeHolding(dir, branch)
	}
	return st
}

// worktreeHolding returns the worktree that has branch checked out, other than
// the one at dir. `git worktree list --porcelain` emits "worktree <path>" and
// "branch refs/heads/<name>" records separated by blank lines.
func worktreeHolding(dir, branch string) string {
	out := git(dir, "worktree", "list", "--porcelain")
	if out == nil {
		return ""
	}
	here := ""
	if id := Resolve(dir); id != nil {
		here = id.WorktreeRoot
	}
	path := ""
	for _, line := range strings.Split(*out, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			path = strings.TrimPrefix(line, "worktree ")
		case line == "branch refs/heads/"+branch:
			if realpath(path) != here {
				return path
			}
		}
	}
	return ""
}

// Advice renders a branch state as one line for an agent to act on, ending in
// the command to run when there is one. Returns "" when nothing is known.
func (b *BranchState) Advice() string {
	if b == nil || b.Name == "" {
		return ""
	}
	switch {
	case b.Current:
		return fmt.Sprintf("Branch at capture: %s - checked out here already; nothing to switch.", b.Name)
	case b.CheckedOutAt != "":
		return fmt.Sprintf("Branch at capture: %s - checked out in another worktree (%s), so `git checkout` here will refuse. "+
			"Work there, or continue on this branch if the change has already landed.", b.Name, b.CheckedOutAt)
	case b.Merged:
		return fmt.Sprintf("Branch at capture: %s - already merged into this branch; you have that work here, no checkout needed.", b.Name)
	case !b.Exists:
		return fmt.Sprintf("Branch at capture: %s - gone from this repo (merged and deleted, or it never existed here); "+
			"treat the branch name as history, not as where the code is.", b.Name)
	default:
		return fmt.Sprintf("Branch at capture: %s (git checkout %s to match).", b.Name, b.Name)
	}
}

// git runs one git command in dir and returns its trimmed output, or nil if it
// failed. A non-zero exit is an answer here ("no such branch", "not an
// ancestor"), not an error to report.
func git(dir string, args ...string) *string {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	s := strings.TrimSpace(string(out))
	return &s
}
