package mover

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProjectKeyFromCWD(t *testing.T) {
	cases := map[string]string{
		"/Volumes/Data/src/foo":   "-Volumes-Data-src-foo",
		"/tmp/claude-501/wt/pkg":  "-tmp-claude-501-wt-pkg",
		"/Users/x/.config/app":    "-Users-x--config-app",
		"/Volumes/Data/src/a.b.c": "-Volumes-Data-src-a-b-c",
	}
	for in, want := range cases {
		if got := ProjectKeyFromCWD(in); got != want {
			t.Errorf("ProjectKeyFromCWD(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCarrierKeyPrefersExistingDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cwd := "/Volumes/Data/src/foo"
	key := ProjectKeyFromCWD(cwd)
	if err := os.MkdirAll(filepath.Join(home, ".claude", "projects", key), 0755); err != nil {
		t.Fatal(err)
	}

	got, err := CarrierKeyForCWD(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if got != key {
		t.Errorf("CarrierKeyForCWD = %q, want existing dir %q", got, key)
	}
}

func TestCarrierKeyFallsBackToEncoding(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".claude", "projects"), 0755)

	cwd := "/brand/new/path"
	got, err := CarrierKeyForCWD(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if got != ProjectKeyFromCWD(cwd) {
		t.Errorf("CarrierKeyForCWD = %q, want encoded %q", got, ProjectKeyFromCWD(cwd))
	}
}

func TestCarrierKeyFindsByCWD(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	projects := filepath.Join(home, ".claude", "projects")

	// A project dir whose encoded name does NOT match cwd, but whose session
	// recorded that cwd. CarrierKeyForCWD should discover it by content.
	weird := filepath.Join(projects, "-some-renamed-key")
	os.MkdirAll(weird, 0755)
	os.WriteFile(filepath.Join(weird, "s1.jsonl"),
		[]byte(`{"type":"user","cwd":"/real/launch/dir","message":{"role":"user","content":"hi"}}`+"\n"), 0644)

	got, err := CarrierKeyForCWD("/real/launch/dir")
	if err != nil {
		t.Fatal(err)
	}
	if got != "-some-renamed-key" {
		t.Errorf("CarrierKeyForCWD = %q, want discovered -some-renamed-key", got)
	}
}
