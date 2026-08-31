package homedir

import (
	"os"
	"testing"
)

// remaimber is launched as an MCP server by agents, and an agent may hand a
// stdio server a whitelisted environment with no HOME in it. Without a home
// directory nothing can be found — not the archive, not any agent's transcripts
// — and the failure surfaces to the user as an unexplained MCP handshake error.
func TestDirWithoutHOME(t *testing.T) {
	want, err := os.UserHomeDir()
	if err != nil || want == "" {
		t.Skip("no HOME in the test environment to compare against")
	}

	t.Setenv("HOME", "")
	if _, err := os.UserHomeDir(); err == nil {
		t.Skip("this platform does not read HOME from the environment")
	}

	got, err := Dir()
	if err != nil {
		t.Fatalf("Dir() with no HOME: %v", err)
	}
	if got != want {
		t.Errorf("Dir() = %q, want %q", got, want)
	}
}

// The environment still wins when it has an answer: a test harness or a wrapper
// that sets HOME is redirecting deliberately, and the passwd entry would quietly
// send remaimber to the real home instead.
func TestDirPrefersEnvironment(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	want := os.Getenv("HOME")

	got, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("Dir() = %q, want the HOME that was set (%q)", got, want)
	}
}
