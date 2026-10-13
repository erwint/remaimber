package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/erwint/remaimber/internal/db"
	"github.com/erwint/remaimber/internal/types"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.email=t@example.com", "-c", "user.name=t",
		"-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// Every lookup says where its session ran and where that stands now, judged
// from where the lookup is made. Each case here is a way an archived location
// goes stale.
func TestLocationStatus(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	main := filepath.Join(base, "app")
	os.Mkdir(main, 0o755)
	git(t, main, "init", "-q", ".")
	git(t, main, "commit", "-q", "--allow-empty", "-m", "one")
	// feature/held lives on in a second worktree; feature/done was merged and
	// its worktree removed; feature/lost was deleted along with its worktree.
	git(t, main, "worktree", "add", "-q", "-b", "feature/held", filepath.Join(base, "held"))
	for _, b := range []string{"feature/done", "feature/lost"} {
		git(t, main, "branch", b)
	}
	git(t, main, "branch", "-D", "feature/lost")
	notRepo := filepath.Join(base, "scratch")
	os.Mkdir(notRepo, 0o755)
	other := filepath.Join(base, "other")
	os.Mkdir(other, 0o755)
	git(t, other, "init", "-q", ".")

	database, err := db.OpenAt(filepath.Join(base, "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repoID := filepath.Join(main, ".git")
	add := func(id, cwd, branch, origin string, identity bool) {
		t.Helper()
		if _, err := database.Exec(`INSERT INTO sessions (session_id, project_key, cwd, git_branch, origin)
			VALUES (?, '-p', ?, NULLIF(?,''), NULLIF(?,''))`, id, cwd, branch, origin); err != nil {
			t.Fatal(err)
		}
		if identity {
			wt := cwd
			if err := db.UpsertIdentity(database, &types.SessionIdentity{
				SessionID: id, CWD: cwd, RepoID: repoID, WorktreeRoot: wt}); err != nil {
				t.Fatal(err)
			}
		}
	}
	add("here", main, "main", "", true)
	add("held", filepath.Join(base, "held"), "feature/held", "", true)
	add("done", filepath.Join(base, "gone-done"), "feature/done", "", true)
	add("lost", filepath.Join(base, "gone-lost"), "feature/lost", "", true)
	add("uncaptured", main, "main", "", false) // SessionStart capture never ran
	add("scratch", notRepo, "", "", false)
	add("other", other, "main", "", false)
	add("pulled", "/Users/someone/app", "main", "laptop", false)
	add("detached", main, "HEAD", "", true)

	t.Chdir(main)
	locs := newLocator().locate(database,
		[]string{"here", "held", "done", "lost", "uncaptured", "scratch", "other", "pulled", "detached"})

	want := map[string]string{
		"here":       "this worktree, same branch",
		"held":       "another worktree of this repository; branch feature/held checked out in " + filepath.Join(base, "held"),
		"done":       "directory no longer exists; branch feature/done merged here",
		"lost":       "directory no longer exists; branch feature/lost deleted",
		"uncaptured": "this worktree, same branch",
		"scratch":    "not a git repository",
		"other":      "another repository than this one",
		"pulled":     "on laptop, synced from there",
		"detached":   "this worktree",
	}
	for id, w := range want {
		l := locs[id]
		if l == nil {
			t.Errorf("%s: no location", id)
			continue
		}
		if !strings.HasPrefix(l.Status, w) {
			t.Errorf("%s: status %q, want %q", id, l.Status, w)
		}
	}
	// Without a captured identity the directory still answers, so the
	// location is not left blank for an agent to guess at.
	if l := locs["uncaptured"]; l.WorktreeRoot != main || l.RepoID == "" {
		t.Errorf("uncaptured: worktree %q repo %q, want them resolved from its directory", l.WorktreeRoot, l.RepoID)
	}
	if l := locs["detached"]; l.Status != "this worktree" || !strings.Contains(locationLine(l), "(detached HEAD)") {
		t.Errorf("detached: status %q, line %q", l.Status, locationLine(l))
	}
	if line := locationLine(locs["held"]); !strings.Contains(line, "(feature/held)") || !strings.Contains(line, " - another worktree") {
		t.Errorf("text line = %q", line)
	}
}

// An identity row that only SessionEnd wrote, with no directory in it, is the
// trace of a capture that never ran. Backfill used to pass it over for good,
// since it only looked for sessions with no row at all, and filling one must
// not clear its end time, or a finished session looks live again.
func TestBackfillFillsEndOnlyRows(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	repo := filepath.Join(base, "app")
	os.Mkdir(repo, 0o755)
	git(t, repo, "init", "-q", ".")
	dbFile := filepath.Join(base, "r.db")
	database, err := db.OpenAt(dbFile)
	if err != nil {
		t.Fatal(err)
	}
	database.Exec(`INSERT INTO sessions (session_id, project_key, cwd) VALUES ('s1', '-p', ?)`, repo)
	if err := db.MarkEnded(database, "s1", "2026-10-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	database.Close()

	if err := run(t, "--db", dbFile, "backfill-identity"); err != nil {
		t.Fatal(err)
	}
	database, _ = db.OpenAt(dbFile)
	defer database.Close()
	id, err := db.GetIdentity(database, "s1")
	if err != nil || id == nil {
		t.Fatalf("identity: %v, %v", id, err)
	}
	if id.WorktreeRoot != repo || id.RepoID == "" {
		t.Errorf("worktree %q repo %q, want them filled from the session's directory", id.WorktreeRoot, id.RepoID)
	}
	if id.EndedAt == "" {
		t.Error("backfill cleared the end time, making a finished session look live")
	}
}
