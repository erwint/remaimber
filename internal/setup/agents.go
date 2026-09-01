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

// `remaimber setup` configures Claude Code, because that is the only agent whose
// integration is a pair of config files remaimber can safely write. Codex and pi
// install a plugin and a package respectively — their own tooling owns those,
// and duplicating what it does by hand would leave two half-installations to
// reconcile.
//
// Silence about them is the problem, though: an agent that is installed and
// unarchived looks configured, because nothing ever said otherwise. So setup
// ends by naming every agent it can find and what remains to be done for it.

// AgentStatus is one agent's integration state, as setup can determine it.
type AgentStatus struct {
	Name      string
	Installed bool
	Wired     bool
	Next      []string // commands that would finish the job
}

// ReportAgents prints what is configured and what is not, for every agent
// present on this machine.
func ReportAgents() {
	home, err := homedir.Dir()
	if err != nil {
		return
	}
	statuses := []AgentStatus{claudeStatus(home), codexStatus(home), piStatus(home)}

	fmt.Println("\nAgents")
	for _, s := range statuses {
		switch {
		case !s.Installed:
			fmt.Printf("  %-6s not installed\n", s.Name)
		case s.Wired:
			fmt.Printf("  %-6s configured\n", s.Name)
		default:
			fmt.Printf("  %-6s installed, not wired up yet:\n", s.Name)
			for _, c := range s.Next {
				fmt.Printf("           %s\n", c)
			}
		}
	}
}

func claudeStatus(home string) AgentStatus {
	s := AgentStatus{Name: "claude", Installed: dirExists(filepath.Join(home, ".claude"))}
	inUserConfig, viaPlugin := MCPStatus(home)
	s.Wired = inUserConfig || viaPlugin
	s.Next = []string{
		"remaimber setup",
		"or install the plugin instead:",
		"  claude plugin marketplace add erwint/remaimber",
		"  claude plugin install rmb@remaimber",
	}
	// A plugin installed before the MCP server was bundled provides the commands
	// and nothing else, and updating it is a different command from installing.
	if !s.Wired && pluginInstalled(home) {
		s.Next = []string{
			"claude plugin update rmb@remaimber   (the installed copy predates the bundled MCP server)",
			"or: remaimber setup",
		}
	}
	return s
}

// pluginInstalled reports whether an rmb plugin is installed at all, regardless
// of what it carries.
func pluginInstalled(home string) bool {
	data, err := os.ReadFile(filepath.Join(home, ".claude", "plugins", "installed_plugins.json"))
	if err != nil {
		return false
	}
	var cfg struct {
		Plugins map[string]any `json:"plugins"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		return false
	}
	for name := range cfg.Plugins {
		if strings.HasPrefix(name, "rmb@") {
			return true
		}
	}
	return false
}

// codexStatus reads config.toml rather than asking Codex, so that setup stays a
// read-only observer of an agent it does not configure.
func codexStatus(home string) AgentStatus {
	dir := codexHome(home)
	s := AgentStatus{Name: "codex", Installed: dirExists(dir) || onPath("codex")}
	if data, err := os.ReadFile(filepath.Join(dir, "config.toml")); err == nil {
		cfg := string(data)
		s.Wired = strings.Contains(cfg, `[plugins."rmb@`) ||
			strings.Contains(cfg, "[mcp_servers.remaimber]")
	}
	s.Next = []string{
		"codex plugin marketplace add erwint/remaimber",
		"codex plugin add rmb@remaimber",
		"then run /hooks inside Codex once, to trust the bundled hooks",
	}
	return s
}

func piStatus(home string) AgentStatus {
	dir := filepath.Join(home, ".pi", "agent")
	s := AgentStatus{Name: "pi", Installed: dirExists(dir) || onPath("pi")}
	if data, err := os.ReadFile(filepath.Join(dir, "settings.json")); err == nil {
		s.Wired = strings.Contains(string(data), "remaimber")
	}
	s.Next = []string{"pi install git:github.com/erwint/remaimber"}
	return s
}

func codexHome(home string) string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h
	}
	return filepath.Join(home, ".codex")
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func onPath(bin string) bool {
	_, err := exec.LookPath(bin)
	return err == nil
}
