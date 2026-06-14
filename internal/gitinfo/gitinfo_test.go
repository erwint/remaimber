package gitinfo

import (
	"os"
	"os/exec"
	"path/filepath"
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
