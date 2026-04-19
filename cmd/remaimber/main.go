package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/erwin/remaimber/internal/db"
	"github.com/erwin/remaimber/internal/importer"
	"github.com/erwin/remaimber/internal/mover"
	"github.com/erwin/remaimber/internal/setup"
	"github.com/erwin/remaimber/internal/types"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
	dbPath  string
)

func main() {
	root := &cobra.Command{
		Use:     "remaimber",
		Short:   "Archive and search Claude Code conversations",
		Version: fmt.Sprintf("%s (commit: %s, built: %s)", version, commit, date),
	}

	root.PersistentFlags().StringVar(&dbPath, "db", "", "Database path (default: ~/.claude/remaimber/remaimber.db, or REMAIMBER_DB env)")

	root.AddCommand(importCmd())
	root.AddCommand(importFileCmd())
	root.AddCommand(listCmd())
	root.AddCommand(searchCmd())
	root.AddCommand(showCmd())
	root.AddCommand(exportCmd())
	root.AddCommand(deleteCmd())
	root.AddCommand(moveCmd())
	root.AddCommand(statsCmd())
	root.AddCommand(verifyCmd())
	root.AddCommand(setupCmd())
	root.AddCommand(mcpCmd())
	root.AddCommand(completionCmd())

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func openDB() (*sql.DB, error) {
	return db.OpenPath(dbPath)
}

func importCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Import conversations from ~/.claude/projects/",
		RunE: func(cmd *cobra.Command, args []string) error {
			database, err := openDB()
			if err != nil {
				return err
			}
			defer database.Close()

			stats, err := importer.ImportAll(database, force)
			if err != nil {
				return err
			}

			fmt.Printf("Scanned: %d files\n", stats.FilesScanned)
			fmt.Printf("Imported: %d files (%d messages)\n", stats.FilesImported, stats.MessagesNew)
			fmt.Printf("Skipped: %d files (unchanged)\n", stats.FilesSkipped)
			if stats.Errors > 0 {
				fmt.Printf("Errors: %d\n", stats.Errors)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "Force re-import all files from beginning")
	return cmd
}

func importFileCmd() *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "import-file <path>",
		Short: "Import a single JSONL file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			database, err := openDB()
			if err != nil {
				return err
			}
			defer database.Close()

			path := args[0]
			sessionID := strings.TrimSuffix(filepath.Base(path), ".jsonl")
			if project == "" {
				project = "-unknown"
			}

			sf := importer.SessionFile{
				Path:       path,
				SessionID:  sessionID,
				ProjectKey: project,
			}

			imported, newMsgs, _, err := importer.ImportFile(database, sf, true)
			if err != nil {
				return err
			}
			if imported {
				fmt.Printf("Imported %d messages from %s\n", newMsgs, path)
			} else {
				fmt.Println("No new messages found.")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Project key to associate with (default: -unknown)")
	return cmd
}

func listCmd() *cobra.Command {
	var project, since, until string
	var limit int
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List archived sessions",
		RunE: func(cmd *cobra.Command, args []string) error {
			database, err := openDB()
			if err != nil {
				return err
			}
			defer database.Close()

			sessions, err := db.ListSessions(database, db.ListFilter{
				Project: project,
				Since:   since,
				Until:   until,
				Limit:   limit,
			})
			if err != nil {
				return err
			}

			if jsonOut {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(sessions)
			}

			for _, s := range sessions {
				title := s.CustomTitle
				if title == "" {
					title = truncate(s.FirstPrompt, 50)
				}
				fmt.Printf("%-36s  %-20s  %s  [%d msgs]\n",
					s.SessionID, importer.PrettyProjectName(s.ProjectKey), title, s.MessageCount)
			}
			if len(sessions) == 0 {
				fmt.Println("No sessions found. Run 'remaimber import' first.")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Filter by project key (substring match)")
	cmd.Flags().StringVar(&since, "since", "", "Filter sessions ending after this date (ISO 8601)")
	cmd.Flags().StringVar(&until, "until", "", "Filter sessions starting before this date (ISO 8601)")
	cmd.Flags().IntVar(&limit, "limit", 20, "Max results")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	return cmd
}

func searchCmd() *cobra.Command {
	var project, role, since, until string
	var limit int
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Search conversations (FTS5)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			query := strings.Join(args, " ")
			database, err := openDB()
			if err != nil {
				return err
			}
			defer database.Close()

			results, err := db.SearchMessages(database, db.SearchFilter{
				Query:   query,
				Project: project,
				Role:    role,
				Since:   since,
				Until:   until,
				Limit:   limit,
			})
			if err != nil {
				return err
			}

			if jsonOut {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(results)
			}

			for _, r := range results {
				title := r.CustomTitle
				if title == "" {
					title = importer.PrettyProjectName(r.ProjectKey)
				}
				fmt.Printf("[%s] %s (%s/%s)\n  %s\n\n",
					r.Timestamp, title, r.Type, r.Role, r.Snippet)
			}
			if len(results) == 0 {
				fmt.Println("No results found.")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Filter by project key")
	cmd.Flags().StringVar(&role, "role", "", "Filter by role (user, assistant)")
	cmd.Flags().StringVar(&since, "since", "", "Filter messages after this date (ISO 8601)")
	cmd.Flags().StringVar(&until, "until", "", "Filter messages before this date (ISO 8601)")
	cmd.Flags().IntVar(&limit, "limit", 20, "Max results")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	return cmd
}

func showCmd() *cobra.Command {
	var msgTypes string
	cmd := &cobra.Command{
		Use:   "show <session-id>",
		Short: "Show messages from a session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			database, err := openDB()
			if err != nil {
				return err
			}
			defer database.Close()

			sessionID, err := db.ResolveSessionID(database, args[0])
			if err != nil {
				return err
			}

			var types []string
			if msgTypes != "" {
				types = strings.Split(msgTypes, ",")
			}

			messages, err := db.GetSessionMessages(database, sessionID, types)
			if err != nil {
				return err
			}

			for _, m := range messages {
				if m.ContentText == "" {
					continue
				}
				role := m.Role
				if role == "" {
					role = m.Type
				}
				fmt.Printf("--- %s [%s] ---\n%s\n\n", role, m.Timestamp, m.ContentText)
			}
			if len(messages) == 0 {
				fmt.Println("No messages found for this session.")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&msgTypes, "type", "", "Filter by message type (comma-separated, e.g. user,assistant)")
	return cmd
}

func exportCmd() *cobra.Command {
	var format string
	var last int
	var msgTypes string
	cmd := &cobra.Command{
		Use:   "export [session-id]",
		Short: "Export a session in markdown, json, or text format",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			database, err := openDB()
			if err != nil {
				return err
			}
			defer database.Close()

			var sessionID string
			if len(args) > 0 {
				sessionID, err = db.ResolveSessionID(database, args[0])
				if err != nil {
					return err
				}
			} else {
				// Use --last N (default 1)
				sess, err := db.GetNthLastSession(database, last)
				if err != nil {
					return err
				}
				sessionID = sess.SessionID
			}

			var types []string
			if msgTypes != "" {
				types = strings.Split(msgTypes, ",")
			} else {
				types = []string{"user", "assistant"}
			}

			messages, err := db.GetSessionMessages(database, sessionID, types)
			if err != nil {
				return err
			}

			// Get session metadata
			sess, _ := db.GetSession(database, sessionID)

			switch format {
			case "json":
				return exportJSON(sess, messages)
			case "markdown", "md":
				return exportMarkdown(sess, messages)
			default:
				return exportText(sess, messages)
			}
		},
	}
	cmd.Flags().StringVar(&format, "format", "text", "Output format: text, markdown (md), json")
	cmd.Flags().IntVar(&last, "last", 1, "Export the Nth most recent session")
	cmd.Flags().StringVar(&msgTypes, "type", "", "Filter by message type (comma-separated)")
	return cmd
}

func deleteCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "delete <session-id>",
		Short: "Delete a session and its messages from the database",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			database, err := openDB()
			if err != nil {
				return err
			}
			defer database.Close()

			sessionID, err := db.ResolveSessionID(database, args[0])
			if err != nil {
				return err
			}

			if !yes {
				fmt.Printf("Delete session %s? [y/N] ", sessionID)
				reader := bufio.NewReader(os.Stdin)
				input, _ := reader.ReadString('\n')
				input = strings.TrimSpace(strings.ToLower(input))
				if input != "y" && input != "yes" {
					fmt.Println("Cancelled.")
					return nil
				}
			}

			if err := db.DeleteSession(database, sessionID); err != nil {
				return err
			}
			fmt.Printf("Deleted session %s\n", sessionID)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip confirmation prompt")
	return cmd
}

func moveCmd() *cobra.Command {
	var copyOnly bool
	cmd := &cobra.Command{
		Use:   "move <session-id> <target-project>",
		Short: "Move or copy a conversation to a different project",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Try to resolve prefix for move too
			database, err := openDB()
			if err != nil {
				return err
			}
			sessionID, resolveErr := db.ResolveSessionID(database, args[0])
			database.Close()
			if resolveErr != nil {
				sessionID = args[0] // fall back to raw arg
			}

			err = mover.Move(sessionID, args[1], copyOnly)
			if err != nil {
				return err
			}
			action := "Moved"
			if copyOnly {
				action = "Copied"
			}
			fmt.Printf("%s session %s to project %s\n", action, sessionID, args[1])
			return nil
		},
	}
	cmd.Flags().BoolVar(&copyOnly, "copy", false, "Copy instead of move")
	return cmd
}

func statsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stats",
		Short: "Show database statistics",
		RunE: func(cmd *cobra.Command, args []string) error {
			database, err := openDB()
			if err != nil {
				return err
			}
			defer database.Close()

			sessionCount, messageCount, projects, err := db.GetStats(database)
			if err != nil {
				return err
			}

			fmt.Printf("Sessions:  %d\n", sessionCount)
			fmt.Printf("Messages:  %d\n", messageCount)
			fmt.Printf("Projects:  %d\n", len(projects))
			for _, p := range projects {
				fmt.Printf("  - %s (%s)\n", p, importer.PrettyProjectName(p))
			}
			return nil
		},
	}
}

func verifyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "verify",
		Short: "Verify database integrity",
		RunE: func(cmd *cobra.Command, args []string) error {
			database, err := openDB()
			if err != nil {
				return err
			}
			defer database.Close()

			r, err := db.Verify(database)
			if err != nil {
				return err
			}

			fmt.Printf("Sessions:       %d\n", r.SessionCount)
			fmt.Printf("Messages:       %d\n", r.MessageCount)
			fmt.Printf("FTS entries:    %d\n", r.FTSCount)

			if r.FTSMatch {
				fmt.Println("FTS integrity:  OK")
			} else {
				fmt.Printf("FTS integrity:  MISMATCH (messages=%d, fts=%d)\n", r.MessageCount, r.FTSCount)
			}

			if r.DuplicateUUIDs == 0 {
				fmt.Println("UUID dedup:     OK")
			} else {
				fmt.Printf("UUID dedup:     %d duplicate UUIDs found!\n", r.DuplicateUUIDs)
			}

			fmt.Println("\nMessages by role:")
			for role, count := range r.MessagesByRole {
				fmt.Printf("  %-12s %d\n", role, count)
			}

			fmt.Println("\nTop projects by message count:")
			for _, ps := range r.TopProjects {
				fmt.Printf("  %-30s %d\n", importer.PrettyProjectName(ps.ProjectKey), ps.MessageCount)
			}
			return nil
		},
	}
}

func setupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "setup",
		Short: "Configure Claude Code settings (hooks + MCP server)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return setup.Run()
		},
	}
}

func mcpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Start MCP server (stdio transport)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMCP()
		},
	}
}

func completionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "completion [bash|zsh|fish|powershell]",
		Short: "Generate shell completion script",
		Long: `Generate shell completion script for the specified shell.

To load completions:

  bash:  source <(remaimber completion bash)
  zsh:   remaimber completion zsh > "${fpath[1]}/_remaimber"
  fish:  remaimber completion fish | source`,
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"bash", "zsh", "fish", "powershell"},
		RunE: func(cmd *cobra.Command, args []string) error {
			switch args[0] {
			case "bash":
				return cmd.Root().GenBashCompletion(os.Stdout)
			case "zsh":
				return cmd.Root().GenZshCompletion(os.Stdout)
			case "fish":
				return cmd.Root().GenFishCompletion(os.Stdout, true)
			case "powershell":
				return cmd.Root().GenPowerShellCompletion(os.Stdout)
			default:
				return fmt.Errorf("unsupported shell: %s", args[0])
			}
		},
	}
}

func runMCP() error {
	database, err := openDB()
	if err != nil {
		return err
	}
	defer database.Close()

	s := server.NewMCPServer("remaimber", "1.0.0",
		server.WithToolCapabilities(false),
	)

	// search_conversations
	searchTool := mcp.NewTool("search_conversations",
		mcp.WithDescription("Search through archived Claude Code conversations using full-text search"),
		mcp.WithString("query", mcp.Required(), mcp.Description("FTS5 search query")),
		mcp.WithString("project", mcp.Description("Filter by project key (substring match)")),
		mcp.WithString("role", mcp.Description("Filter by role: user or assistant")),
		mcp.WithString("since", mcp.Description("Filter messages after this date (ISO 8601)")),
		mcp.WithString("until", mcp.Description("Filter messages before this date (ISO 8601)")),
		mcp.WithNumber("limit", mcp.Description("Max results (default 10)")),
	)
	s.AddTool(searchTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		query, _ := req.RequireString("query")
		f := db.SearchFilter{
			Query:   query,
			Project: req.GetString("project", ""),
			Role:    req.GetString("role", ""),
			Since:   req.GetString("since", ""),
			Until:   req.GetString("until", ""),
			Limit:   req.GetInt("limit", 10),
		}

		importer.ImportAll(database, false)

		results, err := db.SearchMessages(database, f)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		data, _ := json.MarshalIndent(results, "", "  ")
		return mcp.NewToolResultText(string(data)), nil
	})

	// get_session
	getSessionTool := mcp.NewTool("get_session",
		mcp.WithDescription("Get all messages from a specific conversation session"),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("Session UUID or prefix")),
		mcp.WithString("types", mcp.Description("Comma-separated message types to include (default: user,assistant)")),
	)
	s.AddTool(getSessionTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		prefix, _ := req.RequireString("session_id")
		sessionID, err := db.ResolveSessionID(database, prefix)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		typesStr := req.GetString("types", "")
		var msgTypes []string
		if typesStr != "" {
			msgTypes = strings.Split(typesStr, ",")
		} else {
			msgTypes = []string{"user", "assistant"}
		}

		messages, err := db.GetSessionMessages(database, sessionID, msgTypes)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		data, _ := json.MarshalIndent(messages, "", "  ")
		return mcp.NewToolResultText(string(data)), nil
	})

	// list_sessions
	listSessionsTool := mcp.NewTool("list_sessions",
		mcp.WithDescription("List archived conversation sessions"),
		mcp.WithString("project", mcp.Description("Filter by project key (substring match)")),
		mcp.WithString("since", mcp.Description("Filter sessions after this date (ISO 8601)")),
		mcp.WithString("until", mcp.Description("Filter sessions before this date (ISO 8601)")),
		mcp.WithNumber("limit", mcp.Description("Max results (default 20)")),
	)
	s.AddTool(listSessionsTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		f := db.ListFilter{
			Project: req.GetString("project", ""),
			Since:   req.GetString("since", ""),
			Until:   req.GetString("until", ""),
			Limit:   req.GetInt("limit", 20),
		}

		importer.ImportAll(database, false)

		sessions, err := db.ListSessions(database, f)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		data, _ := json.MarshalIndent(sessions, "", "  ")
		return mcp.NewToolResultText(string(data)), nil
	})

	// move_conversation
	moveConvTool := mcp.NewTool("move_conversation",
		mcp.WithDescription("Move or copy a conversation to a different project"),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("Session UUID to move")),
		mcp.WithString("target_project", mcp.Required(), mcp.Description("Target project key")),
		mcp.WithBoolean("copy", mcp.Description("Copy instead of move (default false)")),
	)
	s.AddTool(moveConvTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sessionID, _ := req.RequireString("session_id")
		targetProject, _ := req.RequireString("target_project")
		copyOnly := req.GetBool("copy", false)

		if err := mover.Move(sessionID, targetProject, copyOnly); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		action := "Moved"
		if copyOnly {
			action = "Copied"
		}
		return mcp.NewToolResultText(fmt.Sprintf("%s session %s to project %s", action, sessionID, targetProject)), nil
	})

	return server.ServeStdio(s)
}

// Export helpers

func exportText(sess *types.Session, messages []types.Message) error {
	if sess != nil {
		fmt.Printf("Session: %s\n", sess.SessionID)
		if sess.CustomTitle != "" {
			fmt.Printf("Title: %s\n", sess.CustomTitle)
		}
		fmt.Printf("Project: %s\n", importer.PrettyProjectName(sess.ProjectKey))
		fmt.Printf("Period: %s to %s\n\n", sess.StartedAt, sess.EndedAt)
	}
	for _, m := range messages {
		if m.ContentText == "" {
			continue
		}
		role := m.Role
		if role == "" {
			role = m.Type
		}
		fmt.Printf("[%s] %s\n%s\n\n", role, m.Timestamp, m.ContentText)
	}
	return nil
}

func exportMarkdown(sess *types.Session, messages []types.Message) error {
	if sess != nil {
		title := sess.CustomTitle
		if title == "" {
			title = truncate(sess.FirstPrompt, 60)
		}
		fmt.Printf("# %s\n\n", title)
		fmt.Printf("**Session:** `%s`\n", sess.SessionID)
		fmt.Printf("**Project:** %s\n", importer.PrettyProjectName(sess.ProjectKey))
		fmt.Printf("**Period:** %s to %s\n\n---\n\n", sess.StartedAt, sess.EndedAt)
	}
	for _, m := range messages {
		if m.ContentText == "" {
			continue
		}
		role := m.Role
		if role == "" {
			role = m.Type
		}
		fmt.Printf("### %s\n", strings.ToUpper(role))
		fmt.Printf("*%s*\n\n", m.Timestamp)
		fmt.Printf("%s\n\n---\n\n", m.ContentText)
	}
	return nil
}

func exportJSON(sess *types.Session, messages []types.Message) error {
	out := struct {
		Session  *types.Session  `json:"session,omitempty"`
		Messages []types.Message `json:"messages"`
	}{
		Session:  sess,
		Messages: messages,
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
