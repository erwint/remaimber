package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/erwint/remaimber/internal/db"
	"github.com/erwint/remaimber/internal/gitinfo"
	"github.com/erwint/remaimber/internal/types"
)

// A lookup that finds the right passage but not where it happened leaves the
// agent to guess, and an archived branch name read as the present is how an
// agent ends up telling the user their work is "checked out in another
// worktree" when it was merged a week ago. So every lookup carries the
// session's recorded location and a status: where that stands now, judged
// from where the question is asked.

// locator resolves locations for one lookup, caching the git calls that many
// results from one worktree or branch would otherwise repeat.
type locator struct {
	cwd      string
	here     *gitinfo.Identity
	repos    map[string]*gitinfo.Identity
	branches map[string]*gitinfo.BranchState
}

func newLocator() *locator {
	cwd, _ := os.Getwd()
	return &locator{
		cwd:      cwd,
		here:     gitinfo.Resolve(cwd),
		repos:    map[string]*gitinfo.Identity{},
		branches: map[string]*gitinfo.BranchState{},
	}
}

// locate returns the given sessions' locations with their status filled in. A
// failed lookup returns what it has: a location is context for a result, never
// a reason to fail it.
func (lc *locator) locate(database *sql.DB, ids []string) map[string]*types.Location {
	locs, err := db.SessionLocations(database, ids)
	if err != nil {
		return map[string]*types.Location{}
	}
	for _, l := range locs {
		l.Status = lc.status(l)
	}
	return locs
}

// locateOne is locate for a single session.
func locateOne(database *sql.DB, id string) *types.Location {
	return newLocator().locate(database, []string{id})[id]
}

func (lc *locator) status(l *types.Location) string {
	if l.Origin != "" {
		return "on " + l.Origin + ", synced from there: readable here, resumable only there"
	}
	dir := l.WorktreeRoot
	if dir == "" {
		dir = l.CWD
	}
	if dir == "" {
		return ""
	}
	if _, err := os.Stat(dir); err != nil {
		s := "directory no longer exists"
		// Its branch may outlive it, but that is only knowable from the same
		// repository, which only a recorded identity can tell.
		if lc.sameRepo(l.RepoID) && l.Branch != "" && l.Branch != "HEAD" {
			s += "; branch " + l.Branch + " " + lc.branch(l.Branch).Short()
		}
		return s
	}

	// No identity captured at the start: the directory still exists, so ask
	// it now, which is what a backfill would record.
	if l.RepoID == "" {
		if gi := lc.repo(dir); gi != nil {
			l.RepoID, l.WorktreeRoot = gi.RepoID, gi.WorktreeRoot
		}
	}
	switch {
	case l.RepoID == "":
		return "not a git repository"
	case !lc.sameRepo(l.RepoID):
		return "another repository than this one"
	}
	where := "another worktree of this repository"
	if samePath(l.WorktreeRoot, lc.here.WorktreeRoot) {
		where = "this worktree"
	}
	if l.Branch == "" || l.Branch == "HEAD" {
		return where
	}
	b := lc.branch(l.Branch)
	if where == "this worktree" && b != nil && b.Current {
		return "this worktree, same branch"
	}
	return where + "; branch " + l.Branch + " " + b.Short()
}

func (lc *locator) sameRepo(repoID string) bool {
	return repoID != "" && lc.here != nil && repoID == lc.here.RepoID
}

func (lc *locator) repo(dir string) *gitinfo.Identity {
	if gi, ok := lc.repos[dir]; ok {
		return gi
	}
	gi := gitinfo.Resolve(dir)
	lc.repos[dir] = gi
	return gi
}

func (lc *locator) branch(name string) *gitinfo.BranchState {
	if b, ok := lc.branches[name]; ok {
		return b
	}
	b := gitinfo.ResolveBranch(lc.cwd, name)
	lc.branches[name] = b
	return b
}

func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if ra, err := filepath.EvalSymlinks(a); err == nil {
		a = ra
	}
	if rb, err := filepath.EvalSymlinks(b); err == nil {
		b = rb
	}
	return a == b
}

// locationLine renders a location for text output:
//
//	/src/app (feature/x) - another worktree of this repository; branch feature/x merged here
func locationLine(l *types.Location) string {
	if l == nil {
		return ""
	}
	var b strings.Builder
	if l.Origin != "" {
		b.WriteString("@" + l.Origin + " ")
	}
	dir := l.CWD
	if dir == "" {
		dir = l.WorktreeRoot
	}
	b.WriteString(dir)
	switch l.Branch {
	case "":
	case "HEAD":
		b.WriteString(" (detached HEAD)")
	default:
		fmt.Fprintf(&b, " (%s)", l.Branch)
	}
	if l.Status != "" {
		b.WriteString(" - " + l.Status)
	}
	return strings.TrimSpace(b.String())
}
