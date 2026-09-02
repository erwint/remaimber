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

// `remaimber setup` wires up every agent on the machine, each through its own
// tooling: Claude Code's config is written directly, while Codex and pi are
// asked to install the plugin and the package they own. Writing their
// configuration by hand instead would leave a half-installation their own
// commands do not know about.
//
// Nothing here is required to archive: an agent's plugin carries the same hooks
// and fetches the CLI itself, so a plugin install is a complete route on its own
// and setup is the route for people who installed the CLI first.

// AgentStatus is one agent's integration state, as setup can determine it.
type AgentStatus struct {
	Name      string
	Installed bool
	Wired     bool
	// Install is what wires this agent up, as argv. Empty for Claude Code,
	// whose configuration remaimber writes itself.
	Install [][]string
	Note    string   // printed after a successful install
	Next    []string // shown when reporting rather than installing
}

// SetupAgents wires up each detected agent that is not configured yet. only
// restricts it to one agent; dryRun prints the commands without running them.
func SetupAgents(only string, dryRun bool) {
	home, err := homedir.Dir()
	if err != nil {
		return
	}
	for _, s := range []AgentStatus{codexStatus(home), piStatus(home)} {
		if only != "" && only != s.Name {
			continue
		}
		switch {
		case !s.Installed:
			continue
		case s.Wired:
			fmt.Printf("\n%s: already configured\n", s.Name)
			continue
		}
		fmt.Printf("\n%s: installing the %s\n", s.Name, packagingOf(s.Name))
		for _, argv := range s.Install {
			fmt.Printf("  $ %s\n", strings.Join(argv, " "))
			if dryRun {
				continue
			}
			cmd := exec.Command(argv[0], argv[1:]...)
			out, err := cmd.CombinedOutput()
			if trimmed := strings.TrimSpace(string(out)); trimmed != "" {
				fmt.Printf("    %s\n", strings.ReplaceAll(trimmed, "\n", "\n    "))
			}
			if err != nil {
				fmt.Printf("    failed (%v) — run it yourself to see why\n", err)
				break
			}
		}
		if s.Note != "" && !dryRun {
			fmt.Printf("  %s\n", s.Note)
		}
	}
}

func packagingOf(agent string) string {
	if agent == "pi" {
		return "package"
	}
	return "plugin"
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

// PluginProvidesHooks reports whether an enabled rmb plugin already supplies the
// lifecycle hooks. Writing them into settings.json as well makes every event
// fire twice, so setup checks before configuring Claude Code.
func PluginProvidesHooks(home string) bool {
	settings, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil || !strings.Contains(string(settings), `"rmb@remaimber": true`) {
		return false
	}
	return pluginPathWith(home, "hooks/hooks.json") != ""
}

// pluginPathWith returns the installed plugin's path when it contains rel.
func pluginPathWith(home, rel string) string {
	data, err := os.ReadFile(filepath.Join(home, ".claude", "plugins", "installed_plugins.json"))
	if err != nil {
		return ""
	}
	var cfg struct {
		Plugins map[string][]struct {
			InstallPath string `json:"installPath"`
		} `json:"plugins"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		return ""
	}
	for name, installs := range cfg.Plugins {
		if !strings.HasPrefix(name, "rmb@") {
			continue
		}
		for _, in := range installs {
			if in.InstallPath == "" {
				continue
			}
			if _, err := os.Stat(filepath.Join(in.InstallPath, rel)); err == nil {
				return in.InstallPath
			}
		}
	}
	return ""
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
	s.Install = [][]string{
		{"codex", "plugin", "marketplace", "add", "erwint/remaimber"},
		{"codex", "plugin", "add", "rmb@remaimber"},
	}
	s.Note = "now run /hooks inside Codex once and trust them — bundled hooks are skipped until you do"
	s.Next = []string{
		"remaimber setup --agent codex",
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
	s.Install = [][]string{{"pi", "install", "git:github.com/erwint/remaimber"}}
	s.Next = []string{"remaimber setup --agent pi"}
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
