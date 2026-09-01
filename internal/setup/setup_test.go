package setup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setupTestHome(t *testing.T) (home string, cleanup func()) {
	t.Helper()
	home = t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", home)
	return home, func() { os.Setenv("HOME", origHome) }
}

func TestRun_FreshSettings(t *testing.T) {
	home, cleanup := setupTestHome(t)
	defer cleanup()

	if err := Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}

	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("parse settings: %v", err)
	}

	// Check hooks
	hooks := settings["hooks"].(map[string]any)
	for _, event := range []string{"PreCompact", "SessionEnd"} {
		entries, ok := hooks[event].([]any)
		if !ok || len(entries) == 0 {
			t.Errorf("missing %s hook", event)
			continue
		}
		entry := entries[0].(map[string]any)
		hooksList := entry["hooks"].([]any)
		hook := hooksList[0].(map[string]any)
		cmd := hook["command"].(string)
		if cmd == "" {
			t.Errorf("%s hook command is empty", event)
		}
		if hook["type"] != "command" {
			t.Errorf("%s hook type = %v, want 'command'", event, hook["type"])
		}
	}

	// Claude Code reads user-scope MCP servers from ~/.claude.json, never from
	// settings.json, so setup must not leave a block here that looks registered.
	if _, found := settings["mcpServers"]; found {
		t.Error("settings.json still carries an mcpServers block Claude Code will not read")
	}
}

// An installation configured by an older version carries the inert block. Setup
// has to clear it: left in place it reads as a registered server while
// `claude mcp list` shows none, which is exactly how the search tools went
// missing without anyone noticing.
func TestRun_RemovesInertMCPBlock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	settingsPath := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0755); err != nil {
		t.Fatal(err)
	}
	prior := `{"mcpServers":{"remaimber":{"command":"remaimber","args":["mcp"]},
		"other":{"command":"other"}}}`
	if err := os.WriteFile(settingsPath, []byte(prior), 0644); err != nil {
		t.Fatal(err)
	}

	if err := Run(); err != nil {
		t.Fatal(err)
	}

	var settings map[string]any
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	servers, _ := settings["mcpServers"].(map[string]any)
	if _, found := servers["remaimber"]; found {
		t.Error("the inert remaimber entry survived setup")
	}
	// Somebody else's server is not ours to remove.
	if _, found := servers["other"]; !found {
		t.Error("setup removed an unrelated MCP server")
	}
}

func TestRun_PreservesExistingSettings(t *testing.T) {
	home, cleanup := setupTestHome(t)
	defer cleanup()

	// Write existing settings
	settingsDir := filepath.Join(home, ".claude")
	os.MkdirAll(settingsDir, 0755)
	existing := map[string]any{
		"permissions": map[string]any{
			"allow": []any{"Bash(ls)"},
		},
		"statusLine": map[string]any{
			"type":    "command",
			"command": "my-status",
		},
		"hooks": map[string]any{
			"SessionStart": []any{
				map[string]any{
					"hooks": []any{
						map[string]any{"type": "command", "command": "my-start-hook"},
					},
				},
			},
		},
	}
	data, _ := json.Marshal(existing)
	os.WriteFile(filepath.Join(settingsDir, "settings.json"), data, 0644)

	if err := Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	data, _ = os.ReadFile(filepath.Join(settingsDir, "settings.json"))
	var settings map[string]any
	json.Unmarshal(data, &settings)

	// permissions preserved
	perms := settings["permissions"].(map[string]any)
	allow := perms["allow"].([]any)
	if len(allow) != 1 || allow[0] != "Bash(ls)" {
		t.Error("permissions not preserved")
	}

	// statusLine preserved
	sl := settings["statusLine"].(map[string]any)
	if sl["command"] != "my-status" {
		t.Error("statusLine not preserved")
	}

	// SessionStart: the user's existing hook is preserved, and remaimber adds
	// its own record-identity hook alongside it.
	hooks := settings["hooks"].(map[string]any)
	ss := hooks["SessionStart"].([]any)
	if len(ss) != 2 {
		t.Fatalf("SessionStart should have user hook + remaimber hook, got %d", len(ss))
	}
	var foundUser, foundRemaimber bool
	for _, entry := range ss {
		hookList := entry.(map[string]any)["hooks"].([]any)
		for _, h := range hookList {
			cmd, _ := h.(map[string]any)["command"].(string)
			if cmd == "my-start-hook" {
				foundUser = true
			}
			if strings.Contains(cmd, "record-identity") {
				foundRemaimber = true
			}
		}
	}
	if !foundUser {
		t.Error("user's SessionStart hook not preserved")
	}
	if !foundRemaimber {
		t.Error("remaimber record-identity SessionStart hook not added")
	}
}

func TestRun_ReplacesClaudeVaultHooks(t *testing.T) {
	home, cleanup := setupTestHome(t)
	defer cleanup()

	settingsDir := filepath.Join(home, ".claude")
	os.MkdirAll(settingsDir, 0755)
	existing := map[string]any{
		"hooks": map[string]any{
			"PreCompact": []any{
				map[string]any{
					"hooks": []any{
						map[string]any{"type": "command", "command": "claude-vault import >/dev/null 2>&1"},
					},
				},
			},
			"SessionEnd": []any{
				map[string]any{
					"hooks": []any{
						map[string]any{"type": "command", "command": "claude-vault import >/dev/null 2>&1 &"},
					},
				},
			},
		},
	}
	data, _ := json.Marshal(existing)
	os.WriteFile(filepath.Join(settingsDir, "settings.json"), data, 0644)

	if err := Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	data, _ = os.ReadFile(filepath.Join(settingsDir, "settings.json"))
	var settings map[string]any
	json.Unmarshal(data, &settings)

	hooks := settings["hooks"].(map[string]any)

	// Verify claude-vault hooks are replaced, not duplicated
	for _, event := range []string{"PreCompact", "SessionEnd"} {
		entries := hooks[event].([]any)
		for _, entry := range entries {
			hooksList := entry.(map[string]any)["hooks"].([]any)
			for _, h := range hooksList {
				cmd := h.(map[string]any)["command"].(string)
				if cmd == "claude-vault import >/dev/null 2>&1" || cmd == "claude-vault import >/dev/null 2>&1 &" {
					t.Errorf("claude-vault hook still present in %s", event)
				}
			}
		}
	}
}

func TestRun_Idempotent(t *testing.T) {
	_, cleanup := setupTestHome(t)
	defer cleanup()

	// Run twice
	Run()
	Run()

	home, _ := os.UserHomeDir()
	data, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	var settings map[string]any
	json.Unmarshal(data, &settings)

	// Should not have duplicate hook entries
	hooks := settings["hooks"].(map[string]any)
	for _, event := range []string{"PreCompact", "SessionEnd"} {
		entries := hooks[event].([]any)
		remaimberCount := 0
		for _, entry := range entries {
			hooksList := entry.(map[string]any)["hooks"].([]any)
			for _, h := range hooksList {
				cmd := h.(map[string]any)["command"].(string)
				if cmd != "" {
					remaimberCount++
				}
			}
		}
		if remaimberCount != 1 {
			t.Errorf("%s has %d remaimber hooks, want 1", event, remaimberCount)
		}
	}
}

func TestRun_PreservesOtherHooksOnSameEvent(t *testing.T) {
	home, cleanup := setupTestHome(t)
	defer cleanup()

	settingsDir := filepath.Join(home, ".claude")
	os.MkdirAll(settingsDir, 0755)
	existing := map[string]any{
		"hooks": map[string]any{
			"PreCompact": []any{
				map[string]any{
					"hooks": []any{
						map[string]any{"type": "command", "command": "my-custom-precompact-hook"},
					},
				},
			},
		},
	}
	data, _ := json.Marshal(existing)
	os.WriteFile(filepath.Join(settingsDir, "settings.json"), data, 0644)

	Run()

	data, _ = os.ReadFile(filepath.Join(settingsDir, "settings.json"))
	var settings map[string]any
	json.Unmarshal(data, &settings)

	hooks := settings["hooks"].(map[string]any)
	entries := hooks["PreCompact"].([]any)

	// Should have both: the custom hook and the remaimber hook
	if len(entries) != 2 {
		t.Errorf("PreCompact entries = %d, want 2 (custom + remaimber)", len(entries))
	}
}
