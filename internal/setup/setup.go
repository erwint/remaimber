package setup

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/erwint/remaimber/internal/homedir"
)

// Run configures Claude Code: hooks in settings.json, and the MCP server where
// Claude Code actually looks for one.
func Run() error {
	home, err := homedir.Dir()
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

	// Drop the MCP block earlier versions wrote here. Claude Code does not read
	// mcpServers from settings.json — the server has to be registered in
	// ~/.claude.json — so that block never did anything except look like it had.
	dropStaleMCP(settings)

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
	// Throttled background maintenance: import new messages, then summarize stale
	// sessions. Runs on recurring events so work still happens even if SessionEnd
	// never fires (e.g. a VM killed overnight). Both steps are individually
	// throttled and self-skipping, so this is cheap to fire often.
	remaimberMaintain := map[string]any{
		"type":    "command",
		"command": "{ remaimber import-if-stale; remaimber summarize-if-stale; } >/dev/null 2>&1 &",
	}
	// SessionStart: capture durable identity (foreground, fast, reads stdin),
	// then run a maintenance sweep that catches up anything left unsummarized by
	// a previous session that ended uncleanly.
	remaimberSessionStart := map[string]any{
		"type":    "command",
		"command": "remaimber record-identity >/dev/null 2>&1; { remaimber import-if-stale; remaimber summarize-if-stale; } >/dev/null 2>&1 &",
	}
	// On session end (best-effort; not guaranteed to fire): clear the liveness
	// marker, then import and summarize in the background.
	remaimberSessionEnd := map[string]any{
		"type":    "command",
		"command": "remaimber mark-ended >/dev/null 2>&1; { remaimber import; remaimber summarize-if-stale; } >/dev/null 2>&1 &",
	}

	for _, event := range []struct {
		name string
		hook map[string]any
	}{
		{"SessionStart", remaimberSessionStart},
		{"PreCompact", remaimberImport},
		{"Notification", remaimberMaintain},
		{"SessionEnd", remaimberSessionEnd},
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

// dropStaleMCP removes the inert mcpServers entry earlier versions wrote into
// settings.json. Left there it is worse than nothing: it reads as a registered
// server while `claude mcp list` shows none, which is how the search tools went
// missing without anyone noticing.
func dropStaleMCP(settings map[string]any) {
	mcpServers, _ := settings["mcpServers"].(map[string]any)
	if mcpServers == nil {
		return
	}
	if _, found := mcpServers["remaimber"]; !found {
		return
	}
	delete(mcpServers, "remaimber")
	if len(mcpServers) == 0 {
		delete(settings, "mcpServers")
	}
	fmt.Println("Removed the inert \"remaimber\" entry from settings.json (Claude Code never read it)")
}

// mcpRegistered reports whether Claude Code has the server in its user config.
func mcpRegistered(home string) bool {
	data, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		return false
	}
	var cfg struct {
		MCPServers map[string]any `json:"mcpServers"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		return false
	}
	_, found := cfg.MCPServers["remaimber"]
	return found
}

// MCPStatus reports how Claude Code can reach the MCP server: registered in the
// user config, or shipped by an installed plugin. Both false means the search
// tools are simply absent — a state that has no symptom until an agent says the
// tool does not exist, so it is worth naming explicitly.
func MCPStatus(home string) (userConfig, viaPlugin bool) {
	return mcpRegistered(home), pluginShipsMCP(home)
}

// pluginShipsMCP reports whether an installed rmb plugin carries an .mcp.json.
// Enabled is not enough: a plugin installed before the server was bundled is
// enabled and current-looking while providing no tools at all.
func pluginShipsMCP(home string) bool {
	data, err := os.ReadFile(filepath.Join(home, ".claude", "plugins", "installed_plugins.json"))
	if err != nil {
		return false
	}
	var cfg struct {
		Plugins map[string][]struct {
			InstallPath string `json:"installPath"`
		} `json:"plugins"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		return false
	}
	for name, installs := range cfg.Plugins {
		if !strings.HasPrefix(name, "rmb@") {
			continue
		}
		for _, in := range installs {
			if in.InstallPath == "" {
				continue
			}
			if _, err := os.Stat(filepath.Join(in.InstallPath, ".mcp.json")); err == nil {
				return true
			}
		}
	}
	return false
}

// RegisterMCP adds the server to Claude Code's user config, through the CLI that
// owns that file. ~/.claude.json holds Claude Code's own state as well, so it is
// not ours to rewrite by hand. Kept out of Run so that configuring files and
// invoking another program stay separable — the second is not something a test
// should trigger.
func RegisterMCP() {
	home, err := homedir.Dir()
	if err != nil {
		return
	}
	if mcpRegistered(home) {
		fmt.Println("MCP server \"remaimber\" already registered")
		return
	}
	if _, err := exec.LookPath("claude"); err != nil {
		fmt.Println("The `claude` CLI is not on PATH, so the MCP search tools were not registered.")
		fmt.Println("Run this once it is available:  claude mcp add --scope user remaimber -- remaimber mcp")
		return
	}
	out, err := exec.Command("claude", "mcp", "add", "--scope", "user",
		"remaimber", "--", "remaimber", "mcp").CombinedOutput()
	if err != nil {
		fmt.Printf("Could not register the MCP server (%v). Run it by hand:\n", err)
		fmt.Println("  claude mcp add --scope user remaimber -- remaimber mcp")
		if msg := strings.TrimSpace(string(out)); msg != "" {
			fmt.Printf("  %s\n", msg)
		}
		return
	}
	fmt.Println("Registered MCP server \"remaimber\" (restart Claude Code to pick it up)")
}
