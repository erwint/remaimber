package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/erwin/remaimber/internal/db"
	"github.com/erwin/remaimber/internal/gitinfo"
	"github.com/erwin/remaimber/internal/importer"
	"github.com/erwin/remaimber/internal/mover"
	"github.com/erwin/remaimber/internal/setup"
	"github.com/erwin/remaimber/internal/summarizer"
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
	root.AddCommand(importIfStaleCmd())
	root.AddCommand(importFileCmd())
	root.AddCommand(recordIdentityCmd())
	root.AddCommand(markEndedCmd())
	root.AddCommand(backfillIdentityCmd())
	root.AddCommand(listCmd())
	root.AddCommand(searchCmd())
	root.AddCommand(showCmd())
	root.AddCommand(exportCmd())
	root.AddCommand(deleteCmd())
	root.AddCommand(moveCmd())
	root.AddCommand(resumeCmd())
	root.AddCommand(summarizeCmd())
	root.AddCommand(summarizeIfStaleCmd())
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

// resolveRepoSubpath expands the magic "." value for --repo/--subpath into the
// current directory's durable identity. Non-"." values pass through unchanged.
// Returns an error if "." is requested outside a git repository.
func resolveRepoSubpath(repo, subpath string) (string, string, error) {
	if repo != "." && subpath != "." {
		return repo, subpath, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", "", err
	}
	gi := gitinfo.Resolve(cwd)
	if gi == nil {
		return "", "", fmt.Errorf("current directory is not a git repository; cannot resolve %q", ".")
	}
	if repo == "." {
		repo = gi.RepoID
	}
	if subpath == "." {
		subpath = gi.Subpath
	}
	return repo, subpath, nil
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

func importIfStaleCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "import-if-stale",
		Short:  "Import only if last import was >5 minutes ago (for hooks)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !importer.ShouldImport() {
				return nil
			}
			lock := importer.AcquireImportLock()
			if lock == nil {
				return nil
			}

			// Re-check after acquiring lock
			if !importer.ShouldImport() {
				importer.TouchAndRelease(lock)
				return nil
			}

			database, err := openDB()
			if err != nil {
				importer.TouchAndRelease(lock)
				return err
			}
			defer database.Close()

			importer.ImportAll(database, false)
			importer.TouchAndRelease(lock)
			return nil
		},
	}
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

// hookInput is the JSON Claude Code passes to hooks on stdin.
type hookInput struct {
	SessionID string `json:"session_id"`
	CWD       string `json:"cwd"`
}

func readHookInput() hookInput {
	var h hookInput
	data, _ := io.ReadAll(os.Stdin)
	if len(data) > 0 {
		json.Unmarshal(data, &h)
	}
	return h
}

// recordIdentityCmd captures a session's durable identity at SessionStart,
// while the worktree still exists. It must be fast and must always exit 0 so it
// never blocks or slows session start. Designed to be wired as a SessionStart
// hook reading the hook JSON from stdin; flags override stdin for testing.
func recordIdentityCmd() *cobra.Command {
	var session, cwd string
	cmd := &cobra.Command{
		Use:    "record-identity",
		Short:  "Record durable repo identity for a session (SessionStart hook)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			h := readHookInput()
			if session == "" {
				session = h.SessionID
			}
			if cwd == "" {
				cwd = h.CWD
			}
			if session == "" {
				return nil // nothing to key on; no-op
			}

			id := &types.SessionIdentity{
				SessionID:  session,
				CWD:        cwd,
				CapturedAt: time.Now().UTC().Format(time.RFC3339),
				PID:        os.Getppid(),
			}
			if gi := gitinfo.Resolve(cwd); gi != nil {
				id.RepoID = gi.RepoID
				id.Subpath = gi.Subpath
				id.WorktreeRoot = gi.WorktreeRoot
			}

			database, err := openDB()
			if err != nil {
				return nil // fail soft
			}
			defer database.Close()
			db.UpsertIdentity(database, id) // ignore error — never block session start
			return nil
		},
	}
	cmd.Flags().StringVar(&session, "session", "", "Session ID (overrides stdin)")
	cmd.Flags().StringVar(&cwd, "cwd", "", "Working directory (overrides stdin)")
	return cmd
}

// markEndedCmd clears a session's liveness marker (SessionEnd hook).
func markEndedCmd() *cobra.Command {
	var session string
	cmd := &cobra.Command{
		Use:    "mark-ended",
		Short:  "Mark a session as ended for liveness tracking (SessionEnd hook)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if session == "" {
				session = readHookInput().SessionID
			}
			if session == "" {
				return nil
			}
			database, err := openDB()
			if err != nil {
				return nil
			}
			defer database.Close()
			db.MarkEnded(database, session, time.Now().UTC().Format(time.RFC3339))
			return nil
		},
	}
	cmd.Flags().StringVar(&session, "session", "", "Session ID (overrides stdin)")
	return cmd
}

// backfillIdentityCmd populates identity for already-imported sessions whose
// cwd still resolves on disk. Sessions from deleted worktrees stay null.
func backfillIdentityCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "backfill-identity",
		Short: "Backfill repo identity for existing sessions whose cwd still exists",
		RunE: func(cmd *cobra.Command, args []string) error {
			database, err := openDB()
			if err != nil {
				return err
			}
			defer database.Close()

			work, err := db.SessionsNeedingIdentity(database)
			if err != nil {
				return err
			}

			var filled, unreachable int
			for sid, cwd := range work {
				if _, err := os.Stat(cwd); err != nil {
					unreachable++
					continue
				}
				gi := gitinfo.Resolve(cwd)
				if gi == nil {
					unreachable++
					continue
				}
				id := &types.SessionIdentity{
					SessionID:    sid,
					RepoID:       gi.RepoID,
					Subpath:      gi.Subpath,
					WorktreeRoot: gi.WorktreeRoot,
					CWD:          cwd,
					CapturedAt:   time.Now().UTC().Format(time.RFC3339),
				}
				if err := db.UpsertIdentity(database, id); err == nil {
					filled++
				}
			}
			fmt.Printf("Backfilled identity: %d sessions\n", filled)
			fmt.Printf("Unreachable (deleted worktree / not a git repo): %d\n", unreachable)
			return nil
		},
	}
}

func listCmd() *cobra.Command {
	var project, repo, subpath, since, until string
	var limit int
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List archived sessions",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, subpath, err := resolveRepoSubpath(repo, subpath)
			if err != nil {
				return err
			}

			database, err := openDB()
			if err != nil {
				return err
			}
			defer database.Close()

			sessions, err := db.ListSessions(database, db.ListFilter{
				Project: project,
				Repo:    repo,
				Subpath: subpath,
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
				resumable := " "
				if importer.SessionFileExists(s.ProjectKey, s.SessionID) {
					resumable = "*"
				}
				fmt.Printf("%s %-36s  %-20s  %s  [%d msgs]\n",
					resumable, s.SessionID, importer.PrettyProjectName(s.ProjectKey), title, s.MessageCount)
				if loc := sessionLocation(s); loc != "" {
					fmt.Printf("    %s\n", loc)
				}
			}
			if len(sessions) == 0 {
				fmt.Println("No sessions found. Run 'remaimber import' first.")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Filter by project key (substring match)")
	cmd.Flags().StringVar(&repo, "repo", "", "Filter by repo identity across worktrees ('.' = current repo)")
	cmd.Flags().StringVar(&subpath, "subpath", "", "Filter by monorepo subpath ('.' = current subpath)")
	cmd.Flags().StringVar(&since, "since", "", "Filter sessions ending after this date (ISO 8601)")
	cmd.Flags().StringVar(&until, "until", "", "Filter sessions starting before this date (ISO 8601)")
	cmd.Flags().IntVar(&limit, "limit", 20, "Max results")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	return cmd
}

// sessionLocation renders the worktree/branch context of a session for text
// output, preferring the durable identity's cwd (authoritative) over the lossy
// project_key reverse. Returns "" when nothing useful is known.
func sessionLocation(s types.Session) string {
	loc := s.IdentityCWD
	if loc == "" {
		loc = s.CWD
	}
	if loc == "" && s.WorktreeRoot == "" {
		return ""
	}
	parts := []string{}
	if loc != "" {
		parts = append(parts, loc)
	}
	if s.GitBranch != "" {
		parts = append(parts, "("+s.GitBranch+")")
	}
	return strings.Join(parts, " ")
}

func searchCmd() *cobra.Command {
	var project, repo, subpath, role, since, until, excludeSession string
	var limit int
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Search conversations (FTS5)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			query := strings.Join(args, " ")
			repo, subpath, err := resolveRepoSubpath(repo, subpath)
			if err != nil {
				return err
			}

			database, err := openDB()
			if err != nil {
				return err
			}
			defer database.Close()

			results, err := db.SearchMessages(database, db.SearchFilter{
				Query:          query,
				Project:        project,
				Repo:           repo,
				Subpath:        subpath,
				Role:           role,
				Since:          since,
				Until:          until,
				Limit:          limit,
				ExcludeSession: excludeSession,
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
				resumable := " "
				if importer.SessionFileExists(r.ProjectKey, r.SessionID) {
					resumable = "*"
				}
				fmt.Printf("%s %s [%s] %s (%s)\n  %s\n\n",
					resumable, shortID(r.SessionID), r.Timestamp, title, r.Role, r.Snippet)
			}
			if len(results) == 0 {
				fmt.Println("No results found.")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Filter by project key")
	cmd.Flags().StringVar(&repo, "repo", "", "Filter by repo identity across worktrees ('.' = current repo)")
	cmd.Flags().StringVar(&subpath, "subpath", "", "Filter by monorepo subpath ('.' = current subpath)")
	cmd.Flags().StringVar(&role, "role", "", "Filter by role (user, assistant)")
	cmd.Flags().StringVar(&since, "since", "", "Filter messages after this date (ISO 8601)")
	cmd.Flags().StringVar(&until, "until", "", "Filter messages before this date (ISO 8601)")
	cmd.Flags().IntVar(&limit, "limit", 20, "Max results")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	cmd.Flags().StringVar(&excludeSession, "exclude-session", "", "Exclude this session ID from results")
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

// resumeCmd finds sessions for the current repo (across all worktrees) and, for
// a chosen session, places its JSONL under the current cwd's carrier key so it
// can be resumed here — no worktree switching. With no argument it lists
// candidates; with a session id it prepares that session for resume.
func resumeCmd() *cobra.Command {
	var subpathOnly bool
	cmd := &cobra.Command{
		Use:   "resume [session-id]",
		Short: "Find and prepare a session to resume in the current worktree",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			gi := gitinfo.Resolve(cwd)
			if gi == nil {
				return fmt.Errorf("current directory is not a git repository; run from inside the repo you want to resume into")
			}

			database, err := openDB()
			if err != nil {
				return err
			}
			defer database.Close()

			if len(args) == 1 {
				return prepareResume(database, args[0], cwd, gi)
			}

			// List candidates for this repo (optionally narrowed to this subpath).
			filter := db.ListFilter{Repo: gi.RepoID, Limit: 20}
			if subpathOnly {
				filter.Subpath = gi.Subpath
			}
			sessions, err := db.ListSessions(database, filter)
			if err != nil {
				return err
			}
			if len(sessions) == 0 {
				fmt.Printf("No archived sessions found for this repo (%s).\n", gi.RepoID)
				return nil
			}
			fmt.Printf("Sessions for this repo (%s):\n\n", gi.RepoID)
			for _, s := range sessions {
				title := s.Summary
				if title == "" {
					title = s.CustomTitle
				}
				if title == "" {
					title = truncate(s.FirstPrompt, 60)
				}
				sub := s.Subpath
				if sub == "" {
					sub = "(root)"
				}
				fmt.Printf("  %s  %-18s  %s\n", shortID(s.SessionID), sub, title)
			}
			fmt.Printf("\nResume one here with:  remaimber resume <session-id>\n")
			return nil
		},
	}
	cmd.Flags().BoolVar(&subpathOnly, "here", false, "Only list sessions from the current subpath")
	return cmd
}

// prepareResume places a session under the carrier key and prints resume options.
func prepareResume(database *sql.DB, prefix, cwd string, gi *gitinfo.Identity) error {
	sessionID, err := db.ResolveSessionID(database, prefix)
	if err != nil {
		return err
	}
	sess, err := db.GetSession(database, sessionID)
	if err != nil {
		return err
	}

	// Liveness guard: refuse-by-default warning if the session looks still-running
	// in another worktree (resuming would double-append and corrupt the transcript).
	if id, _ := db.GetIdentity(database, sessionID); id != nil && id.EndedAt == "" {
		if isLikelyLive(sess) {
			fmt.Fprintf(os.Stderr, "WARNING: session %s appears to be live (running in %s, no SessionEnd recorded).\n",
				shortID(sessionID), id.WorktreeRoot)
			fmt.Fprintf(os.Stderr, "Resuming it now risks transcript corruption. Close that session first.\n\n")
		}
	}

	carrierKey, err := mover.CarrierKeyForCWD(cwd)
	if err != nil {
		return err
	}
	if err := mover.LinkIntoProject(sessionID, carrierKey); err != nil {
		return err
	}

	fmt.Printf("Session %s is ready to resume in this worktree.\n\n", shortID(sessionID))
	if sess.GitBranch != "" {
		fmt.Printf("  Branch at capture: %s   (git checkout %s to match)\n\n", sess.GitBranch, sess.GitBranch)
	}
	fmt.Printf("  Native resume (new process):  claude --resume %s\n", sessionID)
	fmt.Printf("  Continue here (no restart):   ask Claude to \"continue session %s\" — it will load the\n", shortID(sessionID))
	fmt.Printf("                                context via remaimber and pick up without a restart.\n")
	return nil
}

// isLikelyLive reports whether a session's source JSONL was modified very
// recently, suggesting it is still being written by an active Claude process.
func isLikelyLive(s *types.Session) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	path := filepath.Join(home, ".claude", "projects", s.ProjectKey, s.SessionID+".jsonl")
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return time.Since(info.ModTime()) < 5*time.Minute
}

// summarizeCmd builds/updates rolling session summaries. With a session id it
// (re)summarizes that session; otherwise it processes every session whose new
// message count has grown past the threshold.
func summarizeCmd() *cobra.Command {
	var minNew int
	cmd := &cobra.Command{
		Use:   "summarize [session-id]",
		Short: "Generate or update rolling summaries of sessions",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := summarizer.LoadConfig()

			// The `claude` backend shells out to `claude -p`, which must not run
			// nested inside a live Claude Code session or hook (recursion, cost,
			// and Claude refuses to launch inside itself). Every hook runs with
			// CLAUDECODE set, so skip cleanly there. Summaries via the `claude`
			// backend are produced out-of-session: a manual run or a cron/launchd
			// job. A local OpenAI-compatible backend has no such constraint and
			// runs fine from hooks.
			if summarizeBlockedInSession(cfg) {
				fmt.Println("Skipping: the 'claude' summary backend can't run inside a Claude session.")
				fmt.Println("Run `remaimber summarize` from a plain shell, or set REMAIMBER_LLM to a")
				fmt.Println("local OpenAI-compatible endpoint (e.g. http://localhost:1234/v1) for in-session summaries.")
				return nil
			}

			database, err := openDB()
			if err != nil {
				return err
			}
			defer database.Close()

			if len(args) == 1 {
				sessionID, err := db.ResolveSessionID(database, args[0])
				if err != nil {
					return err
				}
				sess, err := db.GetSession(database, sessionID)
				if err != nil {
					return err
				}
				summary, newID, err := buildSummary(cmd.Context(), cfg, database, sessionID, sess.FirstPrompt)
				if err != nil {
					return err
				}
				if err := db.UpdateSummary(database, sessionID, summary, newID); err != nil {
					return err
				}
				mover.SetIndexSummary(sess.ProjectKey, sessionID, summary) // best-effort
				fmt.Printf("Summarized %s:\n%s\n", shortID(sessionID), summary)
				return nil
			}

			done, err := runBatchSummarize(cmd.Context(), cfg, database, minNew)
			if err != nil {
				return err
			}
			fmt.Printf("Summarized %d sessions\n", done)
			return nil
		},
	}
	cmd.Flags().IntVar(&minNew, "min", 6, "Minimum new user/assistant messages before (re)summarizing")
	return cmd
}

// buildSummary builds a session's summary map-reduce: each window of salient
// messages is summarized independently (map), then the window summaries are
// consolidated into one, anchored on goal (the opening prompt). It rebuilds from
// the whole session each time — avoiding the recency bias of an incremental fold
// — and returns the summary plus the message-id high-water mark it now reflects.
// An empty session yields an empty summary, but the high-water mark still
// advances so the session settles.
func buildSummary(ctx context.Context, cfg summarizer.Config, database *sql.DB, sessionID, goal string) (string, int64, error) {
	newID, err := db.MaxUAMessageID(database, sessionID)
	if err != nil {
		return "", 0, err
	}

	// Compaction handling: if the session was context-compacted, Claude's
	// compaction summary already distills everything before it. Depending on the
	// mode, anchor on it (and map only post-compaction messages), summarize only
	// post-compaction, or ignore it and map the whole session.
	fromID, prior := int64(0), ""
	if cfg.CompactMode != "full" {
		if text, compactID, ok := db.LatestCompactSummary(database, sessionID); ok {
			fromID = compactID
			if cfg.CompactMode == "anchor" {
				prior = text
			}
		}
	}

	msgs, err := db.UserAssistantMessagesAfter(database, sessionID, fromID)
	if err != nil {
		return "", 0, err
	}

	// Map: an independent summary per window.
	window := cfg.WindowSize()
	var partials []string
	for i := 0; i < len(msgs); i += window {
		end := i + window
		if end > len(msgs) {
			end = len(msgs)
		}
		p, err := cfg.MapWindow(ctx, msgs[i:end])
		if err != nil {
			return "", 0, err
		}
		if strings.TrimSpace(p) != "" {
			partials = append(partials, p)
		}
	}

	// Reduce: consolidate. With no prior and a single window, the partial is the
	// summary; otherwise consolidate (prior, if any, anchors the earlier portion).
	var summary string
	switch {
	case prior == "" && len(partials) == 0:
		return "", newID, nil
	case prior == "" && len(partials) == 1:
		summary = partials[0]
	default:
		summary, err = cfg.ReduceSummaries(ctx, goal, prior, partials)
		if err != nil {
			return "", 0, err
		}
	}
	return summarizer.StripEphemeral(summary), newID, nil
}

// summarizeBlockedInSession reports whether summarization must be skipped because
// the `claude` backend cannot run nested inside a live Claude session/hook.
func summarizeBlockedInSession(cfg summarizer.Config) bool {
	return os.Getenv("CLAUDECODE") != "" && !cfg.IsHTTP()
}

// runBatchSummarize summarizes every session whose user/assistant message count
// has grown at least minNew beyond its current summary. Active sessions are
// included on purpose: the rolling summary is offset-based, so checkpointing a
// live session is valid and leaves a usable (at most one throttle-interval old)
// summary if the machine is killed before the session ends cleanly. Returns the
// number of sessions summarized.
func runBatchSummarize(ctx context.Context, cfg summarizer.Config, database *sql.DB, minNew int) (done int, err error) {
	work, err := db.SessionsNeedingSummary(database, minNew)
	if err != nil {
		return 0, err
	}
	for _, w := range work {
		summary, newID, err := buildSummary(ctx, cfg, database, w.SessionID, w.FirstPrompt)
		if err != nil {
			fmt.Fprintf(os.Stderr, "summarize %s: %v\n", shortID(w.SessionID), err)
			continue
		}
		if err := db.UpdateSummary(database, w.SessionID, summary, newID); err != nil {
			return done, err
		}
		mover.SetIndexSummary(w.ProjectKey, w.SessionID, summary) // best-effort
		done++
	}
	return done, nil
}

// summarizeIfStaleCmd is the throttled background summary sweep used by hooks.
// Because SessionEnd is not guaranteed to fire (e.g. a VM killed overnight), this
// runs opportunistically on reliable, recurring events (SessionStart, Notification)
// so summaries still get produced. It throttles via a stamp file and self-skips
// for the `claude` backend inside a session.
func summarizeIfStaleCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "summarize-if-stale",
		Short:  "Summarize stale sessions if the throttle interval has elapsed (for hooks)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := summarizer.LoadConfig()
			if summarizeBlockedInSession(cfg) {
				return nil // claude backend can't run nested; cron/manual handles it
			}
			if !importer.ShouldSummarize() {
				return nil
			}
			lock := importer.AcquireSummarizeLock()
			if lock == nil {
				return nil
			}
			defer importer.TouchAndRelease(lock)
			if !importer.ShouldSummarize() { // re-check after locking
				return nil
			}

			database, err := openDB()
			if err != nil {
				return nil // fail soft in hooks
			}
			defer database.Close()
			runBatchSummarize(cmd.Context(), cfg, database, 6)
			return nil
		},
	}
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
		mcp.WithString("exclude_session", mcp.Description("Exclude this session ID from results")),
		mcp.WithString("repo", mcp.Description("Filter by repo identity across worktrees ('.' = current repo)")),
		mcp.WithString("subpath", mcp.Description("Filter by monorepo subpath ('.' = current subpath)")),
	)
	s.AddTool(searchTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		query, _ := req.RequireString("query")
		repo, subpath, err := resolveRepoSubpath(req.GetString("repo", ""), req.GetString("subpath", ""))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		f := db.SearchFilter{
			Query:          query,
			Project:        req.GetString("project", ""),
			Repo:           repo,
			Subpath:        subpath,
			Role:           req.GetString("role", ""),
			Since:          req.GetString("since", ""),
			Until:          req.GetString("until", ""),
			Limit:          req.GetInt("limit", 10),
			ExcludeSession: req.GetString("exclude_session", ""),
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
		mcp.WithString("repo", mcp.Description("Filter by repo identity across worktrees ('.' = current repo)")),
		mcp.WithString("subpath", mcp.Description("Filter by monorepo subpath ('.' = current subpath)")),
		mcp.WithString("since", mcp.Description("Filter sessions after this date (ISO 8601)")),
		mcp.WithString("until", mcp.Description("Filter sessions before this date (ISO 8601)")),
		mcp.WithNumber("limit", mcp.Description("Max results (default 20)")),
	)
	s.AddTool(listSessionsTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		repo, subpath, err := resolveRepoSubpath(req.GetString("repo", ""), req.GetString("subpath", ""))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		f := db.ListFilter{
			Project: req.GetString("project", ""),
			Repo:    repo,
			Subpath: subpath,
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

	// link_session — place a session under the carrier (current cwd) project key
	// so Claude can `--resume` it here, even if it ran in another worktree.
	linkTool := mcp.NewTool("link_session",
		mcp.WithDescription("Link a session into the current project so it can be resumed here (cross-worktree)"),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("Session UUID or prefix")),
		mcp.WithString("target_project", mcp.Description("Target project key (default: derived from the server's cwd)")),
	)
	s.AddTool(linkTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		prefix, _ := req.RequireString("session_id")
		sessionID, err := db.ResolveSessionID(database, prefix)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		targetProject := req.GetString("target_project", "")
		if targetProject == "" {
			cwd, err := os.Getwd()
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			targetProject, err = mover.CarrierKeyForCWD(cwd)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
		}

		// Liveness guard.
		var warning string
		if id, _ := db.GetIdentity(database, sessionID); id != nil && id.EndedAt == "" {
			if sess, _ := db.GetSession(database, sessionID); sess != nil && isLikelyLive(sess) {
				warning = fmt.Sprintf("\nWARNING: session appears live in %s — resuming risks transcript corruption.", id.WorktreeRoot)
			}
		}

		if err := mover.LinkIntoProject(sessionID, targetProject); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		sess, _ := db.GetSession(database, sessionID)
		branch := ""
		if sess != nil && sess.GitBranch != "" {
			branch = fmt.Sprintf(" Branch at capture: %s (git checkout %s to match).", sess.GitBranch, sess.GitBranch)
		}
		return mcp.NewToolResultText(fmt.Sprintf(
			"Linked %s into %s. Resume natively with `claude --resume %s`, or continue here without restart.%s%s",
			sessionID, targetProject, sessionID, branch, warning)), nil
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

// shortID returns the first 8 characters of an id (or the whole id if shorter),
// for compact display. Safe against ids shorter than 8 runes.
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
