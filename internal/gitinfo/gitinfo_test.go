package gitinfo

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveInRepo(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	id := Resolve(wd)
	if id == nil {
		t.Fatal("expected identity inside the remaimber repo, got nil")
	}
	if id.RepoID == "" {
		t.Error("RepoID should be non-empty inside a git repo")
	}
	if !filepath.IsAbs(id.RepoID) {
		t.Errorf("RepoID should be an absolute realpath, got %q", id.RepoID)
	}
	// We are in internal/gitinfo, so the subpath should reflect that.
	if id.Subpath == "" {
		t.Error("Subpath should be non-empty when not at repo root")
	}
	if id.WorktreeRoot == "" {
		t.Error("WorktreeRoot should be non-empty inside a git repo")
	}
}

func TestResolveRepoIDStableAcrossSubdirs(t *testing.T) {
	wd, _ := os.Getwd()
	here := Resolve(wd)
	parent := Resolve(filepath.Dir(wd))
	if here == nil || parent == nil {
		t.Fatal("expected identity in both dirs")
	}
	// RepoID is the anchor: identical regardless of subdir. Subpath differs.
	if here.RepoID != parent.RepoID {
		t.Errorf("RepoID should match across subdirs: %q vs %q", here.RepoID, parent.RepoID)
	}
	if here.Subpath == parent.Subpath {
		t.Errorf("Subpath should differ across subdirs, both %q", here.Subpath)
	}
}

func TestResolveNonGit(t *testing.T) {
	dir := t.TempDir()
	// Guard: skip if the temp dir happens to live inside a repo.
	if err := exec.Command("git", "-C", dir, "rev-parse", "--git-dir").Run(); err == nil {
		t.Skip("temp dir is inside a git repo; cannot test non-git case")
	}
	if id := Resolve(dir); id != nil {
		t.Errorf("expected nil for non-git dir, got %+v", id)
	}
}

func TestResolveEmpty(t *testing.T) {
	if id := Resolve(""); id != nil {
		t.Errorf("expected nil for empty dir, got %+v", id)
	}
}

// mkRepo builds a repo with one commit on main and a branch off it.
func mkRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{
			"-c", "user.email=t@example.com", "-c", "user.name=t",
			"-c", "init.defaultBranch=main"}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q", ".")
	run("commit", "-q", "--allow-empty", "-m", "one")
	// feature carries a commit main does not, or it would already count as
	// merged — which is itself one of the states under test.
	run("checkout", "-q", "-b", "feature")
	run("commit", "-q", "--allow-empty", "-m", "two")
	run("checkout", "-q", "main")
	return dir
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{
		"-c", "user.email=t@example.com", "-c", "user.name=t"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// The case that started this: a branch held by another worktree cannot be
// checked out here, so advising it produces git's "already used by worktree"
// refusal instead of the resume the agent was trying to perform.
func TestResolveBranchSeesAnotherWorktree(t *testing.T) {
	dir := mkRepo(t)
	other := filepath.Join(t.TempDir(), "wt")
	gitIn(t, dir, "worktree", "add", "-q", other, "feature")

	st := ResolveBranch(dir, "feature")
	if st == nil {
		t.Fatal("ResolveBranch returned nil inside a repo")
	}
	if st.CheckedOutAt == "" {
		t.Fatal("the other worktree was not found")
	}
	if advice := st.Advice(); !strings.Contains(advice, "another worktree") ||
		strings.Contains(advice, "git checkout feature to match") {
		t.Errorf("advice = %q; must name the worktree and not advise a checkout", advice)
	}
}

// Work that has since landed here is the other stale-fact case: the archived
// branch is not where the code is any more, and switching to it would step back.
func TestResolveBranchSeesMergedAndCurrent(t *testing.T) {
	dir := mkRepo(t)
	gitIn(t, dir, "merge", "-q", "feature") // already an ancestor: a no-op merge

	st := ResolveBranch(dir, "feature")
	if !st.Merged || st.Current {
		t.Fatalf("state = %+v, want merged and not current", st)
	}
	if advice := st.Advice(); !strings.Contains(advice, "no checkout needed") {
		t.Errorf("advice = %q, want it to say the work is already here", advice)
	}

	gitIn(t, dir, "checkout", "-q", "feature")
	if st := ResolveBranch(dir, "feature"); !st.Current {
		t.Errorf("state = %+v, want current after checking it out", st)
	}
}

// A branch that is gone is history, not a location; and a free branch still
// gets the plain checkout it always got.
func TestResolveBranchGoneAndFree(t *testing.T) {
	dir := mkRepo(t)
	if st := ResolveBranch(dir, "feature"); !st.Exists || st.Merged || st.CheckedOutAt != "" {
		t.Fatalf("state = %+v, want a free existing branch", st)
	} else if advice := st.Advice(); !strings.Contains(advice, "git checkout feature to match") {
		t.Errorf("advice = %q, want the plain checkout", advice)
	}
	if st := ResolveBranch(dir, "never-existed"); st.Exists {
		t.Error("a missing branch must not report as existing")
	} else if advice := st.Advice(); !strings.Contains(advice, "gone from this repo") {
		t.Errorf("advice = %q, want it to say the branch is gone", advice)
	}
	// Outside a repo there is nothing to say, and saying nothing is the point.
	if ResolveBranch(t.TempDir(), "feature") != nil {
		t.Error("ResolveBranch outside a repo must return nil")
	}
	if (*BranchState)(nil).Advice() != "" {
		t.Error("a nil state must render as nothing")
	}
}
