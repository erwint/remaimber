package setup

import (
	"os"
	"path/filepath"
	"testing"
)

// An agent that is installed but not wired up looks configured, because nothing
// says otherwise — which is how a Codex or pi install can sit there archiving
// nothing. Setup has to be able to tell the two apart.
func TestAgentStatusDistinguishesInstalledFromWired(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))

	codexDir := filepath.Join(home, ".codex")
	piDir := filepath.Join(home, ".pi", "agent")
	for _, d := range []string{codexDir, piDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}

	// Installed, nothing configured.
	if err := os.WriteFile(filepath.Join(codexDir, "config.toml"),
		[]byte("sandbox_mode = \"danger-full-access\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(piDir, "settings.json"),
		[]byte(`{"packages":["npm:@ollama/pi-web-search"]}`), 0644); err != nil {
		t.Fatal(err)
	}
	if s := codexStatus(home); !s.Installed || s.Wired {
		t.Errorf("codex before install = %+v, want installed and unwired", s)
	}
	if s := piStatus(home); !s.Installed || s.Wired {
		t.Errorf("pi before install = %+v, want installed and unwired", s)
	}
	if len(codexStatus(home).Next) == 0 || len(piStatus(home).Next) == 0 {
		t.Error("an unwired agent must come with the commands that wire it up")
	}

	// Configured: Codex records the plugin in config.toml, pi the package in
	// its settings.
	if err := os.WriteFile(filepath.Join(codexDir, "config.toml"),
		[]byte("[plugins.\"rmb@remaimber\"]\nenabled = true\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(piDir, "settings.json"),
		[]byte(`{"packages":["git:github.com/erwint/remaimber"]}`), 0644); err != nil {
		t.Fatal(err)
	}
	if s := codexStatus(home); !s.Wired {
		t.Errorf("codex after install = %+v, want wired", s)
	}
	if s := piStatus(home); !s.Wired {
		t.Errorf("pi after install = %+v, want wired", s)
	}
}

// Registering the MCP server the Codex way counts too: a Codex user who skipped
// the plugin and added the server by hand is wired up.
func TestCodexStatusAcceptsAPlainMCPRegistration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	codexDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexDir, "config.toml"),
		[]byte("[mcp_servers.remaimber]\ncommand = \"remaimber\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if s := codexStatus(home); !s.Wired {
		t.Errorf("codex with an MCP registration = %+v, want wired", s)
	}
}
