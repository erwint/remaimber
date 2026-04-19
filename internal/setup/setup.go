package setup

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Run configures ~/.claude/settings.json with remaimber hooks and MCP server.
func Run() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	settingsPath := filepath.Join(home, ".claude", "settings.json")

	// Read existing settings
	settings := make(map[string]any)
	data, err := os.ReadFile(settingsPath)
	if err == nil {
		if err := json.Unmarshal(data, &settings); err != nil {
			return fmt.Errorf("parse settings.json: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read settings.json: %w", err)
	}

	// Configure hooks
	configureHooks(settings)

	// Configure MCP server
	configureMCP(settings)

	// Write back
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(settingsPath), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(settingsPath, out, 0644); err != nil {
		return err
	}
	fmt.Printf("Settings saved to %s\n", settingsPath)
	return nil
}

func configureHooks(settings map[string]any) {
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = make(map[string]any)
		settings["hooks"] = hooks
	}

	remaimberImport := map[string]any{
		"type":    "command",
		"command": "remaimber import >/dev/null 2>&1",
	}
	remaimberImportBg := map[string]any{
		"type":    "command",
		"command": "remaimber import >/dev/null 2>&1 &",
	}
	remaimberThrottled := map[string]any{
		"type":    "command",
		"command": "remaimber import-if-stale >/dev/null 2>&1 &",
	}

	for _, event := range []struct {
		name string
		hook map[string]any
	}{
		{"PreCompact", remaimberImport},
		{"Notification", remaimberThrottled},
		{"SessionEnd", remaimberImportBg},
	} {
		existing, _ := hooks[event.name].([]any)
		replaced := false

		// Filter out claude-vault hooks and existing remaimber hooks
		var filtered []any
		for _, entry := range existing {
			entryMap, ok := entry.(map[string]any)
			if !ok {
				filtered = append(filtered, entry)
				continue
			}
			hooksList, _ := entryMap["hooks"].([]any)
			var keepHooks []any
			for _, h := range hooksList {
				hMap, ok := h.(map[string]any)
				if !ok {
					keepHooks = append(keepHooks, h)
					continue
				}
				cmd, _ := hMap["command"].(string)
				if strings.Contains(cmd, "claude-vault") {
					fmt.Printf("Replaced claude-vault hook in %s\n", event.name)
					replaced = true
					continue
				}
				if strings.Contains(cmd, "remaimber") {
					replaced = true
					continue
				}
				keepHooks = append(keepHooks, h)
			}
			if len(keepHooks) > 0 {
				entryMap["hooks"] = keepHooks
				filtered = append(filtered, entryMap)
			}
		}

		// Add remaimber hook
		newEntry := map[string]any{
			"hooks": []any{event.hook},
		}
		filtered = append(filtered, newEntry)
		hooks[event.name] = filtered

		if replaced {
			fmt.Printf("Updated %s hook\n", event.name)
		} else {
			fmt.Printf("Added %s hook\n", event.name)
		}
	}
}

func configureMCP(settings map[string]any) {
	mcpServers, _ := settings["mcpServers"].(map[string]any)
	if mcpServers == nil {
		mcpServers = make(map[string]any)
		settings["mcpServers"] = mcpServers
	}

	_, existed := mcpServers["remaimber"]
	mcpServers["remaimber"] = map[string]any{
		"command": "remaimber",
		"args":    []any{"mcp"},
	}

	if existed {
		fmt.Println("Updated MCP server \"remaimber\"")
	} else {
		fmt.Println("Added MCP server \"remaimber\"")
	}
}
