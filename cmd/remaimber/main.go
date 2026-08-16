package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/erwin/remaimber/internal/db"
	"github.com/erwin/remaimber/internal/gitinfo"
	"github.com/erwin/remaimber/internal/importer"
	"github.com/erwin/remaimber/internal/mover"
	"github.com/erwin/remaimber/internal/segmenter"
	"github.com/erwin/remaimber/internal/setup"
	"github.com/erwin/remaimber/internal/summarizer"
	"github.com/erwin/remaimber/internal/types"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"
)

var (
	version = "dev"
	date    = "unknown"
	dbPath  string
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

// newRootCmd assembles the command tree. Split out from main so tests can walk
// it — see TestEveryCommandHasAnExample.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "remaimber",
		Short: "Archive and search Claude Code conversations",
		Long: "Archive and search Claude Code conversations.\n\n" +
			"Transcripts are imported into SQLite and indexed, then summarized in segments so a\n" +
			"long conversation can be recalled — or resumed in part — without reading all of it.",
		Example: `  # One-time setup: install the import hooks and register the MCP server
  remaimber setup

  # Find something you discussed before
  remaimber search mail-relay
  remaimber list --repo .

  # Pull back just the part of a past conversation you need
  remaimber resume --match 'the part where we set up a mail relay on the nas'

  # See what a long session was about, without reading it
  remaimber summary b2bd8168`,
		Version: fmt.Sprintf("%s (built: %s)", version, date),
		// A failing command should print why, not reprint its own usage. The
		// error already says what went wrong, and burying it under a flag list
		// makes diagnostics harder to read, not easier.
		SilenceUsage: true,
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
	root.AddCommand(summaryCmd())
	root.AddCommand(summarizeIfStaleCmd())
	root.AddCommand(statsCmd())
	root.AddCommand(costCmd())
	root.AddCommand(doctorCmd())
	root.AddCommand(recallCmd())
	root.AddCommand(verifyCmd())
	root.AddCommand(setupCmd())
	root.AddCommand(mcpCmd())
	root.AddCommand(completionCmd())

	return root
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
		Example: `  # Import anything new since the last run
  remaimber import

  # Re-read every transcript from the beginning (after a schema change)
  remaimber import --force`,
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
		Example: `  # Import one transcript, guessing the project from the filename
  remaimber import-file ~/.claude/projects/-src-myapp/abc123.jsonl

  # Attribute it to a specific project key
  remaimber import-file ./rescued.jsonl --project -Volumes-Data-src-myapp`,
		Args: cobra.ExactArgs(1),
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
		Use: "backfill-identity",
		Example: `  # Fill in repo identity for older sessions, so --repo/--subpath find them
  remaimber backfill-identity`,
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
		Example: `  # The 20 most recent sessions
  remaimber list

  # Everything for this repo, including other worktrees
  remaimber list --repo .

  # This subpath of a monorepo, last month, as JSON
  remaimber list --repo . --subpath . --since 2026-07-01 --json`,
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
				resumable := " "
				if importer.SessionFileExists(s.ProjectKey, s.SessionID) {
					resumable = "*"
				}
				label := s.CustomTitle
				if label == "" {
					label = truncate(s.FirstPrompt, 50)
				}
				fmt.Printf("%s %-36s  %-20s  %s  [%d msgs]\n",
					resumable, s.SessionID, importer.PrettyProjectName(s.ProjectKey), label, s.MessageCount)
				if loc := sessionLocation(s); loc != "" {
					fmt.Printf("    %s\n", loc)
				}
				if s.Summary != "" {
					fmt.Printf("    %s\n", truncate(strings.ReplaceAll(s.Summary, "\n", " "), 110))
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
	var jsonOut, includeToolOutput bool
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Search conversations (FTS5)",
		Example: `  # Plain words; punctuation and hyphens are handled for you
  remaimber search mail-relay

  # FTS5 operators still work when you want them
  remaimber search 'compaction OR segment'
  remaimber search 'segm*'

  # Narrow to this repo, to what you said, and to a date range
  remaimber search postfix --repo . --role user --since 2026-08-01

  # Search command output too (off by default as machine noise)
  remaimber search 'exit status 1' --include-tool-output`,
		Args: cobra.MinimumNArgs(1),
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
				Query:             query,
				Project:           project,
				Repo:              repo,
				Subpath:           subpath,
				Role:              role,
				Since:             since,
				Until:             until,
				Limit:             limit,
				ExcludeSession:    excludeSession,
				IncludeToolOutput: includeToolOutput,
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
				// The segment locates the hit inside the session, so a long
				// conversation can be resumed at just this part.
				seg := ""
				if r.SegmentSeq >= 0 {
					seg = fmt.Sprintf(" seg %d", r.SegmentSeq)
				}
				fmt.Printf("%s %s%s [%s] %s (%s)\n  %s\n\n",
					resumable, shortID(r.SessionID), seg, r.Timestamp, title, r.Role, r.Snippet)
			}
			if len(results) == 0 {
				fmt.Println("No results found.")
			} else {
				fmt.Printf("Resume just the matching part:  remaimber resume <session-id> --match %q\n", query)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&includeToolOutput, "include-tool-output", false, "Also search tool results (command/file output), off by default as machine noise")
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
		Example: `  # A session id, or any unambiguous prefix of one
  remaimber show b2bd8168

  # Only what was actually said, skipping tool traffic
  remaimber show b2bd8168 --type user,assistant`,
		Args: cobra.ExactArgs(1),
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
		Use: "export [session-id]",
		Example: `  # Export one session as markdown
  remaimber export b2bd8168 --format markdown > session.md

  # The most recent session, and the one before it
  remaimber export --last 1
  remaimber export --last 2 --format json`,
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
		Use: "delete <session-id>",
		Example: `  # Prompts before deleting
  remaimber delete b2bd8168

  # No prompt (scripts)
  remaimber delete b2bd8168 --yes`,
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
		Use: "move <session-id> <target-project>",
		Example: `  # Re-file a session that was archived under the wrong project
  remaimber move b2bd8168 -Volumes-Data-src-myapp

  # Leave the original in place
  remaimber move b2bd8168 -Volumes-Data-src-myapp --copy`,
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
	var match, segSpec, since, until string
	var printMsgs bool
	var contextPad, maxSegs int
	cmd := &cobra.Command{
		Use:   "resume [session-id]",
		Short: "Find and prepare a session to resume in the current worktree",
		Long: "Find and prepare a session to resume in the current worktree.\n\n" +
			"With no arguments, lists sessions for this repo. With a session id, prepares that\n" +
			"session so `claude --resume` can open it here, whichever worktree it ran in.\n\n" +
			"With --match, resumes partially: describe the part you want in plain words and it\n" +
			"locates the stretch of conversation actually about it, rather than loading a session\n" +
			"that may run to thousands of messages. Given no session id, --match searches every\n" +
			"archived conversation — for when which one it was is the part you've forgotten.",
		Example: `  # What can I resume here?
  remaimber resume
  remaimber resume --here          # only this subpath of a monorepo

  # Prepare a whole session to resume in this worktree
  remaimber resume b2bd8168

  # Find the part about a topic, anywhere in the archive
  remaimber resume --match 'the part where we set up a mail relay on the nas'

  # ...or within one session you already know
  remaimber resume b2bd8168 --match 'smtp relay'
  remaimber resume b2bd8168 --match 'smtp relay' --print   # dump the messages

  # Pick a part by hand instead: segment numbers or a time window
  remaimber resume b2bd8168 --segments 4
  remaimber resume b2bd8168 --segments 3-5 --print
  remaimber resume b2bd8168 --since 2026-08-06T11:18 --until 2026-08-06T11:45

  # Widen a match with neighbouring segments for run-up
  remaimber resume b2bd8168 --match 'smtp relay' --context 1`,
		Args: cobra.MaximumNArgs(1),
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
				if match != "" || segSpec != "" || since != "" || until != "" {
					return partialResume(database, args[0], cwd, gi,
						match, segSpec, since, until, printMsgs, contextPad, maxSegs)
				}
				return prepareResume(database, args[0], cwd, gi)
			}
			// A topic with no session id searches the whole archive: which
			// conversation it happened in is usually the forgotten part.
			if match != "" {
				return findAcrossSessions(database, match, cwd, gi, subpathOnly, printMsgs)
			}
			if segSpec != "" || since != "" || until != "" {
				return fmt.Errorf("--segments/--since/--until need a session id: remaimber resume <session-id> --segments 3-5\n" +
					"(or search the whole archive by topic: remaimber resume --match <topic>)")
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
	cmd.Flags().StringVar(&match, "match", "", "Resume only the segments matching this topic")
	cmd.Flags().StringVar(&segSpec, "segments", "", "Resume an explicit selection: \"3\", \"3,4\" or \"3-5\"")
	cmd.Flags().BoolVar(&printMsgs, "print", false, "Print the selected segments' messages instead of just their summaries")
	cmd.Flags().IntVar(&contextPad, "context", 0, "Include this many neighbouring segments on each side")
	cmd.Flags().IntVar(&maxSegs, "max-segments", 3, "Cap on how many matched segments to select")
	cmd.Flags().StringVar(&since, "since", "", "Only this time window (ISO 8601, e.g. 2026-08-06T11:18)")
	cmd.Flags().StringVar(&until, "until", "", "End of the time window (ISO 8601)")
	return cmd
}

// partialResume prepares a session and reports only the part of it that covers a
// topic. The session is still linked into this worktree, so a native full resume
// stays available — narrowing is about what gets read back, not what is reachable.
func partialResume(database *sql.DB, prefix, cwd string, gi *gitinfo.Identity,
	match, segSpec, since, until string, printMsgs bool, contextPad, maxSegs int) error {
	sessionID, err := db.ResolveSessionID(database, prefix)
	if err != nil {
		return err
	}
	all, err := db.GetSegments(database, sessionID)
	if err != nil {
		return err
	}
	if len(all) == 0 {
		return fmt.Errorf("session %s has no segments yet (not summarized)\nrun: remaimber summarize %s",
			shortID(sessionID), shortID(sessionID))
	}
	// A bare topic resolves to the passage it is discussed in, which is finer
	// than the segment holding it.
	if match != "" && segSpec == "" && since == "" && until == "" {
		return passageResume(database, sessionID, cwd, all, match, printMsgs)
	}

	sel, hits, err := selectSegments(database, sessionID, all, match, segSpec, since, until, maxSegs)
	if err != nil {
		return err
	}
	sel = db.WithNeighbours(sel, contextPad, all[len(all)-1].Seq)
	times, err := db.SegmentTimes(database, sessionID)
	if err != nil {
		return err
	}

	var total, picked int
	for _, s := range all {
		total += s.MsgCount
	}
	bySeq := map[int]db.Segment{}
	for _, s := range all {
		bySeq[s.Seq] = s
	}
	for _, q := range sel {
		picked += bySeq[q].MsgCount
	}

	carrierKey, err := mover.CarrierKeyForCWD(cwd)
	if err != nil {
		return err
	}
	if err := mover.LinkIntoProject(sessionID, carrierKey); err != nil {
		return err
	}

	// A time window cuts inside the selected segments, so the segment message
	// counts would overstate what actually comes back. Count the real rows.
	window := ""
	if since != "" || until != "" {
		msgs, err := db.SegmentMessages(database, sessionID, sel, since, until)
		if err != nil {
			return err
		}
		picked = len(msgs)
		window = fmt.Sprintf(", windowed to %s", strings.TrimSpace(since+" – "+until))
	}

	fmt.Printf("Session %s — %d segments, %d messages\n", shortID(sessionID), len(all), total)
	fmt.Printf("Selected %s%s: %d messages (%.0f%% of the session)\n\n",
		formatSeqs(sel), window, picked, 100*float64(picked)/float64(max(total, 1)))
	for _, q := range sel {
		s := bySeq[q]
		hint := ""
		if h := hits[q]; h > 0 {
			hint = fmt.Sprintf(" [%d hits]", h)
		}
		fmt.Printf("  [%d]%s %s\n      %s\n", q, hint, formatSpan(times[q]),
			truncate(strings.ReplaceAll(s.Summary, "\n", " "), 96))
	}

	if printMsgs {
		msgs, err := db.SegmentMessages(database, sessionID, sel, since, until)
		if err != nil {
			return err
		}
		if len(msgs) == 0 {
			return fmt.Errorf("no messages in that selection")
		}
		fmt.Printf("\n%s\n\n", strings.Repeat("─", 72))
		for _, m := range msgs {
			fmt.Printf("[%s] %s\n\n", m.Role, m.ContentText)
		}
		return nil
	}

	win := ""
	if since != "" {
		win += " --since " + since
	}
	if until != "" {
		win += " --until " + until
	}
	fmt.Printf("\nLoad this slice here:  ask Claude to \"continue session %s segments %s\"\n",
		shortID(sessionID), formatSeqs(sel))
	fmt.Printf("Print the messages:    remaimber resume %s --segments %s%s --print\n",
		shortID(sessionID), formatSeqs(sel), win)
	fmt.Printf("Full session instead:  claude --resume %s\n", sessionID)
	return nil
}

// liveSessionID is the conversation this process was invoked from, when there is
// one. Searching from inside a session for a topic that session is discussing
// would otherwise rank its own chatter above the older conversation being looked
// for — the request outranking the work.
func liveSessionID() string {
	return os.Getenv("CLAUDE_CODE_SESSION_ID")
}

// findAcrossSessions answers "find the part where we did X" without being told
// which conversation. Searches the whole archive by default rather than the
// current repo: the reason for asking is usually that the conversation itself
// has been forgotten, and it may well have happened in another worktree.
func findAcrossSessions(database *sql.DB, match, cwd string, gi *gitinfo.Identity, hereOnly, printMsgs bool) error {
	f := db.PassageFilter{ExcludeSession: liveSessionID()}
	if hereOnly {
		f.Repo, f.Subpath = gi.RepoID, gi.Subpath
	}
	passages, err := db.FindPassagesAcross(database, match, f, db.PassageOpts{})
	if err != nil {
		return err
	}
	if len(passages) == 0 {
		scope := "the archive"
		if hereOnly {
			scope = "this repo"
		}
		return fmt.Errorf("nothing in %s is about %q (searched terms: %s)",
			scope, match, strings.Join(db.QueryTerms(match), ", "))
	}

	fmt.Printf("Searched for: %s\n\n", strings.Join(db.QueryTerms(match), ", "))
	shown := len(passages)
	if shown > 5 {
		shown = 5
	}
	for i := 0; i < shown; i++ {
		p := passages[i]
		sess, _ := db.GetSession(database, p.SessionID)
		where := ""
		if sess != nil {
			where = importer.PrettyProjectName(sess.ProjectKey)
		}
		mark := " "
		if i == 0 {
			mark = "*"
		}
		fmt.Printf("%s %s  %s  %-22s  %d hits\n", mark, shortID(p.SessionID),
			formatSpan([2]string{p.StartedAt, p.EndedAt}), where, p.Hits)
		fmt.Printf("    %s\n", truncate(p.Snippet, 92))
	}

	best := passages[0]
	msgs, err := db.PassageMessages(database, best.SessionID, best)
	if err != nil {
		return err
	}
	fmt.Printf("\nBest match: %s, %s — %d messages\n",
		shortID(best.SessionID), formatSpan([2]string{best.StartedAt, best.EndedAt}), len(msgs))

	if printMsgs {
		fmt.Printf("\n%s\n\n", strings.Repeat("─", 72))
		for _, m := range msgs {
			fmt.Printf("[%s] %s\n\n", m.Role, m.ContentText)
		}
		return nil
	}
	fmt.Printf("Print it:   remaimber resume %s --match %q --print\n", shortID(best.SessionID), match)
	fmt.Printf("Resume it:  remaimber resume %s --match %q\n", shortID(best.SessionID), match)
	return nil
}

// passageResume reports the stretch of a session that covers a topic, plus the
// runners-up so an ambiguous term can be re-aimed without guessing at times.
func passageResume(database *sql.DB, sessionID, cwd string, all []db.Segment, match string, printMsgs bool) error {
	passages, err := db.FindPassages(database, sessionID, match, db.PassageOpts{})
	if err != nil {
		return err
	}
	if len(passages) == 0 {
		return fmt.Errorf("nothing in %s is about %q (searched terms: %s)",
			shortID(sessionID), match, strings.Join(db.QueryTerms(match), ", "))
	}
	focus := passages[0]

	var total int
	for _, s := range all {
		total += s.MsgCount
	}
	msgs, err := db.PassageMessages(database, sessionID, focus)
	if err != nil {
		return err
	}

	carrierKey, err := mover.CarrierKeyForCWD(cwd)
	if err != nil {
		return err
	}
	if err := mover.LinkIntoProject(sessionID, carrierKey); err != nil {
		return err
	}

	fmt.Printf("Session %s — %d segments, %d messages\n", shortID(sessionID), len(all), total)
	fmt.Printf("Searched for: %s\n\n", strings.Join(db.QueryTerms(match), ", "))
	fmt.Printf("Found the part about it: %s\n", formatSpan([2]string{focus.StartedAt, focus.EndedAt}))
	fmt.Printf("  %d messages (%.0f%% of the session), segments %s, %d matching turns\n",
		len(msgs), 100*float64(len(msgs))/float64(max(total, 1)), formatSeqs(focus.Segments), focus.Hits)
	if focus.Snippet != "" {
		fmt.Printf("  %s\n", truncate(focus.Snippet, 96))
	}

	if len(passages) > 1 {
		fmt.Printf("\nOther candidates (use --since/--until to pick one):\n")
		for i := 1; i < len(passages) && i <= 3; i++ {
			p := passages[i]
			fmt.Printf("  %s  %d hits  %s\n", formatSpan([2]string{p.StartedAt, p.EndedAt}),
				p.Hits, truncate(p.Snippet, 68))
		}
	}

	if printMsgs {
		fmt.Printf("\n%s\n\n", strings.Repeat("─", 72))
		for _, m := range msgs {
			fmt.Printf("[%s] %s\n\n", m.Role, m.ContentText)
		}
		return nil
	}
	fmt.Printf("\nPrint it:  remaimber resume %s --match %q --print\n", shortID(sessionID), match)
	fmt.Printf("Full one:  claude --resume %s\n", sessionID)
	return nil
}

// formatSpan renders a segment's time range compactly: the date once, then both
// clock times, since a segment almost always sits inside one day.
func formatSpan(span [2]string) string {
	lo, hi := span[0], span[1]
	if lo == "" || hi == "" {
		return ""
	}
	day, from, okA := strings.Cut(lo, "T")
	_, to, okB := strings.Cut(hi, "T")
	if !okA || !okB {
		return lo + "…" + hi
	}
	clip := func(s string) string {
		if len(s) >= 5 {
			return s[:5]
		}
		return s
	}
	return fmt.Sprintf("%s %s–%s", day, clip(from), clip(to))
}

// formatSeqs renders segment numbers compactly, collapsing runs into ranges
// ("3,4,5,9" becomes "3-5,9") so it round-trips through --segments.
func formatSeqs(seqs []int) string {
	if len(seqs) == 0 {
		return "(none)"
	}
	var parts []string
	for i := 0; i < len(seqs); {
		j := i
		for j+1 < len(seqs) && seqs[j+1] == seqs[j]+1 {
			j++
		}
		switch {
		case j == i:
			parts = append(parts, strconv.Itoa(seqs[i]))
		case j == i+1:
			parts = append(parts, strconv.Itoa(seqs[i]), strconv.Itoa(seqs[j]))
		default:
			parts = append(parts, fmt.Sprintf("%d-%d", seqs[i], seqs[j]))
		}
		i = j + 1
	}
	return strings.Join(parts, ",")
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

// passageResult answers a topic query with the passage that covers it, the
// summaries of the segments it sits in, and the runners-up. Alternatives are
// returned rather than hidden because a one-word topic is often ambiguous — an
// archive here has two unrelated "relay" discussions — and the caller is better
// placed than a ranking function to tell which one was meant.
func passageResult(database *sql.DB, sessionID string, all []db.Segment, match string, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	passages, err := db.FindPassages(database, sessionID, match, db.PassageOpts{})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if len(passages) == 0 {
		return mcp.NewToolResultError(fmt.Sprintf("nothing in %s is about %q (searched terms: %v)",
			shortID(sessionID), match, db.QueryTerms(match))), nil
	}
	focus := passages[0]

	times, err := db.SegmentTimes(database, sessionID)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	bySeq := map[int]db.Segment{}
	for _, s := range all {
		bySeq[s.Seq] = s
	}

	out := struct {
		SessionID     string          `json:"session_id"`
		Terms         []string        `json:"searched_terms"`
		TotalSegments int             `json:"total_segments"`
		TotalMessages int             `json:"total_messages"`
		Focus         db.Passage      `json:"focus"`
		Segments      []db.SegmentHit `json:"segments"`
		Alternatives  []db.Passage    `json:"alternatives,omitempty"`
		Messages      []types.Message `json:"messages,omitempty"`
		Note          string          `json:"note,omitempty"`
	}{SessionID: sessionID, Terms: db.QueryTerms(match), TotalSegments: len(all), Focus: focus}
	for _, s := range all {
		out.TotalMessages += s.MsgCount
	}
	for _, seq := range focus.Segments {
		if s, ok := bySeq[seq]; ok {
			span := times[seq]
			out.Segments = append(out.Segments, db.SegmentHit{Segment: s, StartedAt: span[0], EndedAt: span[1]})
		}
	}
	for i := 1; i < len(passages) && i <= 3; i++ {
		out.Alternatives = append(out.Alternatives, passages[i])
	}

	if req.GetBool("include_messages", false) {
		if out.Messages, err = db.PassageMessages(database, sessionID, focus); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
	} else {
		out.Note = "segment summaries and the located passage; call again with include_messages=true for its conversation text"
	}
	data, _ := json.MarshalIndent(out, "", "  ")
	return mcp.NewToolResultText(string(data)), nil
}

// selectSegments resolves which segments a partial resume should cover, from an
// explicit spec, a topic match, or neither (the whole session). It returns the
// chosen sequence numbers and, when a match drove the choice, the per-segment hit
// counts. A match that finds nothing is an error rather than a silent fall back
// to everything, since quietly loading a 1200-message session when the caller
// asked for one topic is the opposite of what they wanted.
func selectSegments(database *sql.DB, sessionID string, all []db.Segment, match, spec, since, until string, maxSegments int) ([]int, map[int]int, error) {
	hits := map[int]int{}

	if spec != "" {
		sel, err := db.ParseSegmentSpec(spec)
		if err != nil {
			return nil, nil, err
		}
		valid := map[int]bool{}
		for _, s := range all {
			valid[s.Seq] = true
		}
		for _, s := range sel {
			if !valid[s] {
				return nil, nil, fmt.Errorf("segment %d does not exist (session has %d: 0-%d)",
					s, len(all), all[len(all)-1].Seq)
			}
		}
		return sel, hits, nil
	}

	if match == "" {
		// A bare time window selects by when rather than by what.
		if since != "" || until != "" {
			sel, err := db.SegmentsInWindow(database, sessionID, since, until)
			if err != nil {
				return nil, nil, err
			}
			if len(sel) == 0 {
				return nil, nil, fmt.Errorf("no segment of %s falls in that time window", shortID(sessionID))
			}
			return sel, hits, nil
		}
		sel := make([]int, len(all))
		for i, s := range all {
			sel[i] = s.Seq
		}
		return sel, hits, nil
	}

	matched, err := db.SegmentsMatching(database, sessionID, match, since, until)
	if err != nil {
		return nil, nil, err
	}
	if len(matched) == 0 {
		where := ""
		if since != "" || until != "" {
			where = " in that time window"
		}
		return nil, nil, fmt.Errorf("no segment of %s matches %q%s", shortID(sessionID), match, where)
	}
	// Drop incidental mentions. A topic discussed across a session concentrates
	// its hits in a couple of segments; a segment holding a small fraction of the
	// leader's hits merely name-dropped it, and pulling it in dilutes the slice.
	// The leader always survives, so this can never empty a non-empty match.
	floor := matched[0].Hits / 5
	kept := matched[:1]
	for _, m := range matched[1:] {
		if m.Hits >= floor {
			kept = append(kept, m)
		}
	}
	matched = kept

	if maxSegments > 0 && len(matched) > maxSegments {
		matched = matched[:maxSegments]
	}
	sel := make([]int, len(matched))
	for i, m := range matched {
		sel[i] = m.Seq
		hits[m.Seq] = m.Hits
	}
	sort.Ints(sel)
	return sel, hits, nil
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
	var all, reindex bool
	cmd := &cobra.Command{
		Use: "summarize [session-id]",
		Example: `  # Summarize every session with enough new material
  remaimber summarize

  # Re-summarize one session
  remaimber summarize b2bd8168

  # Only sessions with at least 20 new messages
  remaimber summarize --min 20

  # Catch up everything, however little new material it has
  remaimber summarize --all

  # Rebuild the summary search index without calling a model
  remaimber summarize --reindex

  # Use a local model instead of the claude CLI
  REMAIMBER_LLM=http://localhost:11434/v1 REMAIMBER_LLM_MODEL=qwen3 remaimber summarize`,
		Short: "Generate or update rolling summaries of sessions",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := summarizer.LoadConfig()
			cfg.Cost = &summarizer.CostMeter{}

			database, err := openDB()
			if err != nil {
				return err
			}
			defer database.Close()

			// Reindexing is pure bookkeeping over summaries that already exist,
			// so it costs nothing and is safe to run at any time.
			if reindex {
				n, err := db.ReindexSummaries(database)
				if err != nil {
					return err
				}
				fmt.Printf("Reindexed %d segment summaries.\n", n)
				return nil
			}
			// --all lowers the bar to a single new message, so sessions that
			// never crossed the threshold stop being permanently invisible.
			if all {
				minNew = 1
			}

			if len(args) == 1 {
				sessionID, err := db.ResolveSessionID(database, args[0])
				if err != nil {
					return err
				}
				sess, err := db.GetSession(database, sessionID)
				if err != nil {
					return err
				}
				summary, newID, err := segmenter.Reconcile(cmd.Context(), cfg, database, sessionID, sess.FirstPrompt, segmentCap())
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
	cmd.Flags().BoolVar(&all, "all", false, "Summarize every session with any new material (implies --min 1)")
	cmd.Flags().BoolVar(&reindex, "reindex", false, "Rebuild the summary search index from stored summaries; makes no model calls")
	return cmd
}

// summaryCmd shows a session's roll-up summary and its per-segment summaries.
func summaryCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use: "summary <session-id>",
		Example: `  # Roll-up plus per-segment summaries
  remaimber summary b2bd8168

  # As JSON, for scripting
  remaimber summary b2bd8168 --json`,
		Short: "Show a session's summary and its segments",
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
			sess, err := db.GetSession(database, sessionID)
			if err != nil {
				return err
			}
			segs, err := db.GetSegments(database, sessionID)
			if err != nil {
				return err
			}

			if jsonOut {
				out := struct {
					SessionID string       `json:"session_id"`
					Summary   string       `json:"summary"`
					Segments  []db.Segment `json:"segments"`
				}{sessionID, sess.Summary, segs}
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(out)
			}

			if sess.Summary == "" {
				fmt.Printf("No summary for %s yet. Run 'remaimber summarize %s'.\n", shortID(sessionID), shortID(sessionID))
				return nil
			}
			fmt.Printf("%s\n", sess.Summary)
			if len(segs) > 0 {
				fmt.Printf("\nSegments (%d):\n", len(segs))
				for _, s := range segs {
					state := "open"
					if s.Closed {
						state = s.Reason
					}
					line := strings.ReplaceAll(s.Summary, "\n", " ")
					fmt.Printf("  [%d] %-10s %3d msgs  %s\n", s.Seq, state, s.MsgCount, truncate(line, 96))
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	return cmd
}

// segmentCap returns the soft-split size, overridable via REMAIMBER_SEGMENT_CAP.
func segmentCap() int {
	if s := os.Getenv("REMAIMBER_SEGMENT_CAP"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return n
		}
	}
	return db.DefaultSegmentCap
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
		summary, newID, err := segmenter.Reconcile(ctx, cfg, database, w.SessionID, w.FirstPrompt, segmentCap())
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
// so summaries still get produced. It throttles via a stamp file. Both backends
// run from hooks now: the claude backend uses --no-session-persistence, which runs
// fine inside a session and creates no nested session to recurse through.
func summarizeIfStaleCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "summarize-if-stale",
		Short:  "Summarize stale sessions if the throttle interval has elapsed (for hooks)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := summarizer.LoadConfig()
			cfg.Cost = &summarizer.CostMeter{}
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
		Use: "stats",
		Example: `  # Session, message and project counts
  remaimber stats`,
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

			// Coverage decides whether recall can be trusted: an unsummarized
			// session is invisible to every summary- and segment-based lookup.
			if c, err := db.GetSummaryCoverage(database); err == nil {
				fmt.Printf("\nSummaries:\n")
				fmt.Printf("  Sessions summarized:  %d/%d (%s)\n",
					c.SessionsWithSum, c.Sessions, pct(c.SessionsWithSum, c.Sessions))
				fmt.Printf("  Segments summarized:  %d/%d (%s)\n",
					c.SegmentsWithSum, c.Segments, pct(c.SegmentsWithSum, c.Segments))
				fmt.Printf("  Indexed for search:   %d\n", c.IndexedSummaries)
				if c.TotalLLMCalls > 0 {
					fmt.Printf("  Cost so far:          $%.2f over %d model calls\n", c.TotalCostUSD, c.TotalLLMCalls)
				} else {
					fmt.Printf("  Cost so far:          not recorded (summarized before cost tracking)\n")
				}
				// Most unsummarized sessions are slash-command invocations a few
				// messages long. Only a real backlog is worth acting on.
				if c.Backlog > 0 {
					fmt.Printf("  Backlog:              %d session(s) with material — run: remaimber summarize --all\n", c.Backlog)
				}
				if c.TooSmall > 0 {
					fmt.Printf("  Skipped as trivial:   %d session(s) under %d messages (slash commands, prompts)\n",
						c.TooSmall, db.SummarizeThreshold)
				}
			}

			fmt.Printf("\nProjects:\n")
			for _, p := range projects {
				fmt.Printf("  - %s (%s)\n", p, importer.PrettyProjectName(p))
			}
			return nil
		},
	}
}

func verifyCmd() *cobra.Command {
	return &cobra.Command{
		Use: "verify",
		Example: `  # Check the FTS index and segment ranges against the messages table
  remaimber verify`,
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
		Use: "setup",
		Example: `  # Wire up the import hooks and register the MCP server
  remaimber setup`,
		Short: "Configure Claude Code settings (hooks + MCP server)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return setup.Run()
		},
	}
}

func mcpCmd() *cobra.Command {
	return &cobra.Command{
		Use: "mcp",
		Example: `  # Started by Claude Code, not usually by hand
  remaimber mcp

  # Exercise a tool directly (stdio JSON-RPC)
  echo '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' | remaimber mcp`,
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
		mcp.WithBoolean("include_tool_output", mcp.Description("Also search tool results (command/file output). Off by default: they are machine noise and include this tool's own archived output.")),
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
			Query:             query,
			Project:           req.GetString("project", ""),
			Repo:              repo,
			Subpath:           subpath,
			Role:              req.GetString("role", ""),
			Since:             req.GetString("since", ""),
			Until:             req.GetString("until", ""),
			Limit:             req.GetInt("limit", 10),
			ExcludeSession:    req.GetString("exclude_session", ""),
			IncludeToolOutput: req.GetBool("include_tool_output", false),
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

	// get_summary
	getSummaryTool := mcp.NewTool("get_summary",
		mcp.WithDescription("Get a session's recall summary and its per-segment summaries (cheaper than reading the full session)"),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("Session UUID or prefix")),
	)
	s.AddTool(getSummaryTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		prefix, _ := req.RequireString("session_id")
		sessionID, err := db.ResolveSessionID(database, prefix)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		sess, err := db.GetSession(database, sessionID)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		segs, err := db.GetSegments(database, sessionID)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		out := struct {
			SessionID string       `json:"session_id"`
			Summary   string       `json:"summary"`
			Segments  []db.Segment `json:"segments"`
		}{sessionID, sess.Summary, segs}
		data, _ := json.MarshalIndent(out, "", "  ")
		return mcp.NewToolResultText(string(data)), nil
	})

	// get_segments — partial resume. Locates the part(s) of a conversation a
	// topic actually lives in, so a caller can rebuild just that context instead
	// of loading a session that may run to thousands of messages.
	getSegmentsTool := mcp.NewTool("get_segments",
		mcp.WithDescription("Partial resume: given a topic in plain words, find the stretch of a conversation that is actually about it "+
			"and return that context. Use this instead of get_session when only part of a long conversation is relevant. "+
			"With 'match' it locates the passage itself — no need to know segment numbers or times — and reports alternative "+
			"passages when a term is ambiguous, so a wrong guess can be corrected by picking another. "+
			"Pass include_messages=true to get the conversation text of the chosen passage."),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("Session UUID or prefix")),
		mcp.WithString("match", mcp.Description("Topic to locate; segments are ranked by hit count. Omit to list every segment.")),
		mcp.WithString("segments", mcp.Description("Explicit selection instead of a match: \"3\", \"3,4\" or \"3-5\"")),
		mcp.WithBoolean("include_messages", mcp.Description("Include the full messages of the selected segments, not just summaries (default false)")),
		mcp.WithNumber("context", mcp.Description("Also include this many neighbouring segments on each side (default 0)")),
		mcp.WithNumber("max_segments", mcp.Description("Cap on how many matched segments to select (default 3)")),
		mcp.WithString("since", mcp.Description("Restrict to a time window (ISO 8601). A segment can span hours, so this narrows within one; usable alone to select purely by when.")),
		mcp.WithString("until", mcp.Description("End of the time window (ISO 8601)")),
	)
	s.AddTool(getSegmentsTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		prefix, _ := req.RequireString("session_id")
		sessionID, err := db.ResolveSessionID(database, prefix)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		all, err := db.GetSegments(database, sessionID)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if len(all) == 0 {
			return mcp.NewToolResultError(fmt.Sprintf(
				"session %s has no segments yet (not summarized); use get_session instead or run 'remaimber summarize %s'",
				shortID(sessionID), shortID(sessionID))), nil
		}
		maxSeq := all[len(all)-1].Seq

		since, until := req.GetString("since", ""), req.GetString("until", "")
		match := req.GetString("match", "")

		// A topic with no explicit narrowing resolves to a passage: the stretch
		// the topic is actually discussed in, which is finer than a segment and
		// is what the caller means by "the part about X".
		if match != "" && since == "" && until == "" && req.GetString("segments", "") == "" {
			return passageResult(database, sessionID, all, match, req)
		}

		sel, hits, err := selectSegments(database, sessionID, all,
			match, req.GetString("segments", ""), since, until,
			req.GetInt("max_segments", 3))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		sel = db.WithNeighbours(sel, req.GetInt("context", 0), maxSeq)
		times, err := db.SegmentTimes(database, sessionID)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		out := struct {
			SessionID     string          `json:"session_id"`
			TotalSegments int             `json:"total_segments"`
			TotalMessages int             `json:"total_messages"`
			Selected      []int           `json:"selected_segments"`
			Segments      []db.SegmentHit `json:"segments"`
			Messages      []types.Message `json:"messages,omitempty"`
			Note          string          `json:"note,omitempty"`
		}{SessionID: sessionID, TotalSegments: len(all), Selected: sel}
		for _, s := range all {
			out.TotalMessages += s.MsgCount
		}
		for _, s := range all {
			for _, want := range sel {
				if s.Seq == want {
					span := times[s.Seq]
					out.Segments = append(out.Segments, db.SegmentHit{
						Segment: s, Hits: hits[s.Seq], StartedAt: span[0], EndedAt: span[1]})
				}
			}
		}
		if req.GetBool("include_messages", false) {
			if out.Messages, err = db.SegmentMessages(database, sessionID, sel, since, until); err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
		} else {
			out.Note = "summaries only; call again with include_messages=true for the full text of these segments"
		}
		data, _ := json.MarshalIndent(out, "", "  ")
		return mcp.NewToolResultText(string(data)), nil
	})

	// find_context — cross-session passage search. The entry point when the
	// conversation a topic was discussed in is itself what has been forgotten.
	findContextTool := mcp.NewTool("find_context",
		mcp.WithDescription("Find the part of ANY archived conversation that is about a topic, described in plain words "+
			"(\"the part where we set up a mail relay on the nas\"). Returns ranked passages across all conversations — each with "+
			"its session, time span, segment summaries and a snippet — and the messages of the best one on request. "+
			"Use this when you don't know which conversation holds something; use get_segments when you already have a session id."),
		mcp.WithString("topic", mcp.Required(), mcp.Description("What to find, in plain words")),
		mcp.WithBoolean("include_messages", mcp.Description("Include the conversation text of the best passage (default false)")),
		mcp.WithNumber("limit", mcp.Description("How many passages to return (default 5)")),
		mcp.WithString("project", mcp.Description("Restrict to a project key (substring match)")),
		mcp.WithString("repo", mcp.Description("Restrict to a repo identity across worktrees ('.' = current repo)")),
		mcp.WithString("subpath", mcp.Description("Restrict to a monorepo subpath ('.' = current subpath)")),
		mcp.WithString("since", mcp.Description("Only messages after this time (ISO 8601)")),
		mcp.WithString("until", mcp.Description("Only messages before this time (ISO 8601)")),
		mcp.WithString("exclude_session", mcp.Description("Drop a conversation from the results; defaults to the live session so it cannot rank its own discussion of the topic")),
	)
	s.AddTool(findContextTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		topic, _ := req.RequireString("topic")
		repo, subpath, err := resolveRepoSubpath(req.GetString("repo", ""), req.GetString("subpath", ""))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		exclude := req.GetString("exclude_session", "")
		if exclude == "" {
			exclude = liveSessionID()
		}

		importer.ImportAll(database, false)

		passages, err := db.FindPassagesAcross(database, topic, db.PassageFilter{
			Project: req.GetString("project", ""), Repo: repo, Subpath: subpath,
			Since: req.GetString("since", ""), Until: req.GetString("until", ""),
			ExcludeSession: exclude,
		}, db.PassageOpts{})
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if len(passages) == 0 {
			return mcp.NewToolResultError(fmt.Sprintf("nothing in the archive is about %q (searched terms: %v)",
				topic, db.QueryTerms(topic))), nil
		}
		limit := req.GetInt("limit", 5)
		if limit > 0 && len(passages) > limit {
			passages = passages[:limit]
		}

		type found struct {
			db.Passage
			Project     string   `json:"project"`
			SessionInfo string   `json:"session_summary,omitempty"`
			SegSummary  []string `json:"segment_summaries,omitempty"`
		}
		out := struct {
			Terms    []string        `json:"searched_terms"`
			Passages []found         `json:"passages"`
			Messages []types.Message `json:"messages,omitempty"`
			Note     string          `json:"note,omitempty"`
		}{Terms: db.QueryTerms(topic)}

		for _, p := range passages {
			f := found{Passage: p}
			if sess, _ := db.GetSession(database, p.SessionID); sess != nil {
				f.Project = importer.PrettyProjectName(sess.ProjectKey)
				f.SessionInfo = truncate(sess.Summary, 220)
			}
			// The segment summaries are what a caller reads to decide whether
			// this passage is the one, before paying for its messages.
			if segs, err := db.GetSegments(database, p.SessionID); err == nil {
				for _, sg := range segs {
					for _, want := range p.Segments {
						if sg.Seq == want && sg.Summary != "" {
							f.SegSummary = append(f.SegSummary, truncate(sg.Summary, 200))
						}
					}
				}
			}
			out.Passages = append(out.Passages, f)
		}

		if req.GetBool("include_messages", false) {
			if out.Messages, err = db.PassageMessages(database, passages[0].SessionID, passages[0]); err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			out.Note = "messages are those of the first passage; call get_segments with another session_id to fetch a different one"
		} else {
			out.Note = "call again with include_messages=true for the conversation text of the best passage"
		}
		data, _ := json.MarshalIndent(out, "", "  ")
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

// pct renders a share as a percentage, reading "n/a" rather than "0%" when there
// is nothing to divide by.
func pct(n, total int) string {
	if total == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.0f%%", 100*float64(n)/float64(total))
}

// doctorCmd checks the things that fail quietly. Import and summarization run
// from hooks, so when they stop running nothing reports an error — the archive
// simply stops keeping up, and that is only noticed when a search comes back
// wrong months later.
func doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check that archiving, summarization and the index are healthy",
		Example: `  # What is silently not working?
  remaimber doctor`,
		RunE: func(cmd *cobra.Command, args []string) error {
			database, err := openDB()
			if err != nil {
				return err
			}
			defer database.Close()

			var problems int
			ok := func(format string, a ...any) { fmt.Printf("  ok    %s\n", fmt.Sprintf(format, a...)) }
			warn := func(format string, a ...any) {
				problems++
				fmt.Printf("  WARN  %s\n", fmt.Sprintf(format, a...))
			}

			fmt.Println("Archiving")
			if home, err := os.UserHomeDir(); err == nil {
				hooksSeen := false
				for _, p := range []string{
					filepath.Join(home, ".claude", "settings.json"),
					filepath.Join(home, ".claude", "settings.local.json"),
				} {
					if b, err := os.ReadFile(p); err == nil && strings.Contains(string(b), "remaimber") {
						hooksSeen = true
					}
				}
				// The plugin ships its own hooks, so absence from settings.json is
				// only a problem if the plugin is not providing them either.
				pluginHooks := false
				if b, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json")); err == nil {
					pluginHooks = strings.Contains(string(b), `"rmb@remaimber": true`)
				}
				switch {
				case hooksSeen || pluginHooks:
					ok("import hooks are configured")
				default:
					warn("no remaimber hooks found in settings.json and the plugin is not enabled — run: remaimber setup")
				}
			}

			var lastImport string
			database.QueryRow(`SELECT COALESCE(MAX(imported_at),'') FROM sessions`).Scan(&lastImport)
			if lastImport == "" {
				warn("nothing has ever been imported — run: remaimber import")
			} else if t, err := time.Parse(time.RFC3339, lastImport); err == nil && time.Since(t) > 7*24*time.Hour {
				warn("last import was %s (%.0f days ago) — hooks may not be running",
					lastImport[:10], time.Since(t).Hours()/24)
			} else {
				ok("last import %s", lastImport[:min(len(lastImport), 16)])
			}

			fmt.Println("\nSummarization")
			c, err := db.GetSummaryCoverage(database)
			if err != nil {
				return err
			}
			switch {
			case c.Sessions == 0:
				warn("no sessions archived yet")
			case c.Backlog > 0:
				warn("%d session(s) with real content are unsummarized — run: remaimber summarize --all", c.Backlog)
			default:
				ok("everything worth summarizing is summarized (%d/%d sessions)", c.SessionsWithSum, c.Sessions)
			}
			if c.TooSmall > 0 {
				// Stated, not warned: these are slash-command invocations and
				// permission prompts, and summarizing them would spend model
				// calls to describe nothing.
				fmt.Printf("  note  %d session(s) skipped as trivial (under %d messages)\n",
					c.TooSmall, db.SummarizeThreshold)
			}
			if c.SegmentsWithSum > c.IndexedSummaries {
				warn("%d summaries are not in the search index — run: remaimber summarize --reindex",
					c.SegmentsWithSum-c.IndexedSummaries)
			} else if c.IndexedSummaries > 0 {
				ok("%d summaries indexed for intent-level search", c.IndexedSummaries)
			}
			if _, err := exec.LookPath("claude"); err != nil {
				warn("the `claude` CLI is not on PATH; summarization will fail unless REMAIMBER_LLM points elsewhere")
			} else {
				ok("summarization backend available")
			}

			fmt.Println("\nIndex")
			var unclassified int
			database.QueryRow(`SELECT COUNT(*) FROM messages WHERE is_sidechain IS NULL`).Scan(&unclassified)
			if unclassified > 0 {
				warn("%d messages have no shape flags; reopen the database to backfill", unclassified)
			} else {
				ok("message flags fully populated")
			}
			res, err := db.Verify(database)
			if err != nil {
				return err
			}
			if !res.FTSMatch {
				warn("full-text index out of step: %d messages but %d indexed — run: remaimber import --force",
					res.MessageCount, res.FTSCount)
			} else {
				ok("full-text index consistent (%d rows)", res.FTSCount)
			}
			if res.DuplicateUUIDs > 0 {
				warn("%d duplicate message uuids", res.DuplicateUUIDs)
			}

			fmt.Println()
			if problems == 0 {
				fmt.Println("No problems found.")
				return nil
			}
			return fmt.Errorf("%d problem(s) found", problems)
		},
	}
}

// recallCmd searches segment summaries rather than raw messages — what the work
// turned out to be, rather than the words someone happened to type.
func recallCmd() *cobra.Command {
	var limit int
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "recall <topic>",
		Short: "Search what conversations were about (segment summaries)",
		Long: "Search segment summaries rather than raw message text.\n\n" +
			"Summaries say what the work turned out to be, so this matches when you remember\n" +
			"the outcome but not the wording. Use `search` for literal text.",
		Example: `  # Match on outcome, not phrasing
  remaimber recall 'smtp relay on the nas'

  # More results, as JSON
  remaimber recall 'flaky tests' --limit 10 --json`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			database, err := openDB()
			if err != nil {
				return err
			}
			defer database.Close()

			hits, err := db.SearchSummaries(database, strings.Join(args, " "), limit)
			if err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(hits)
			}
			if len(hits) == 0 {
				fmt.Println("Nothing recalled. Try `remaimber search` for literal text,")
				fmt.Println("or check coverage with `remaimber stats`.")
				return nil
			}
			for _, h := range hits {
				fmt.Printf("%s seg %d  %s\n    %s\n\n", shortID(h.SessionID), h.Seq,
					importer.PrettyProjectName(h.ProjectKey),
					truncate(strings.ReplaceAll(h.Summary, "\n", " "), 150))
			}
			fmt.Printf("Pull one back:  remaimber resume <session-id> --segments <seq>\n")
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 20, "Max results")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	return cmd
}

// costCmd answers what summarization has cost. The figure exists only because
// each call's price is captured as it happens — there is no transcript to price
// afterwards — so this is the only place it can be read.
func costCmd() *cobra.Command {
	var by, since, until string
	var limit int
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "cost",
		Short: "Show what summarization has cost",
		Long: "Show what summarization has cost.\n\n" +
			"Cost is recorded per segment as it is summarized. Segments summarized before\n" +
			"cost tracking existed carry no price and are excluded, so rates reflect what was\n" +
			"actually measured.",
		Example: `  # Overall spend, recent days, and the priciest conversations
  remaimber cost

  # One breakdown at a time
  remaimber cost --by day
  remaimber cost --by session --limit 10
  remaimber cost --by project

  # A specific window, or machine-readable
  remaimber cost --since 2026-08-11 --until 2026-08-14
  remaimber cost --by day --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			database, err := openDB()
			if err != nil {
				return err
			}
			defer database.Close()

			totals, err := db.GetCostTotals(database, since, until)
			if err != nil {
				return err
			}

			if by != "" {
				rows, err := db.GetCostBreakdown(database, db.CostDimension(by), since, until, limit)
				if err != nil {
					return err
				}
				if jsonOut {
					return jsonOut2(struct {
						Totals db.CostTotals `json:"totals"`
						By     string        `json:"by"`
						Rows   []db.CostRow  `json:"rows"`
					}{totals, by, rows})
				}
				printCostRows(db.CostDimension(by), rows, totals)
				return nil
			}

			if jsonOut {
				return jsonOut2(totals)
			}
			if totals.Segments == 0 {
				fmt.Println("No summarization recorded yet.")
				fmt.Println("Summaries written before cost tracking carry no figures; new ones will.")
				return nil
			}

			// A self-hosted backend produces real work at no price. Reporting a
			// rate and a projection for it would be dressing up zero as a finding.
			if totals.USD == 0 {
				fmt.Printf("Summarization cost\n")
				fmt.Printf("  Total:      $0.00 — every summary ran on a self-hosted model\n")
				fmt.Printf("  Work done:  %d model calls over %d segments\n", totals.Calls, totals.Segments)
				fmt.Printf("  Period:     %s to %s (%d days, %.0f calls/day)\n",
					totals.FirstDay, totals.LastDay, totals.DaysSpan,
					float64(totals.Calls)/float64(totals.DaysSpan))
				if rows, err := db.GetCostBreakdown(database, db.CostByModel, since, until, 0); err == nil && len(rows) > 0 {
					fmt.Printf("\nBy model\n")
					printCostRows(db.CostByModel, rows, totals)
				}
				return nil
			}

			fmt.Printf("Summarization cost\n")
			fmt.Printf("  Total:      $%.2f over %d model calls (%d segments)\n",
				totals.USD, totals.Calls, totals.Segments)
			fmt.Printf("  Period:     %s to %s (%d days)\n", totals.FirstDay, totals.LastDay, totals.DaysSpan)
			fmt.Printf("  Rate:       $%.2f/day  ·  $%.4f/call\n", totals.PerDay, totals.PerCall)
			fmt.Printf("  At this rate: ~$%.0f per 30 days\n", totals.Projected)
			// A mixed setup would otherwise show a rate that quietly averages paid
			// work with free work, understating what the paid backend costs.
			if totals.FreeSegments > 0 {
				fmt.Printf("  Free:       %d of %d segments ran on a self-hosted model (%d calls, no charge)\n",
					totals.FreeSegments, totals.Segments, totals.FreeCalls)
			}

			if n, err := db.UnpricedSegments(database); err == nil && n > 0 {
				fmt.Printf("\n  %d earlier segment(s) were never metered (summarized before cost\n", n)
				fmt.Printf("  tracking), so the total covers only what has been measured.\n")
			}

			days, err := db.GetCostBreakdown(database, db.CostByDay, since, until, 0)
			if err != nil {
				return err
			}
			if len(days) > 1 {
				fmt.Printf("\nBy day\n")
				printCostRows(db.CostByDay, tailRows(days, 14), totals)
			}
			sessions, err := db.GetCostBreakdown(database, db.CostBySession, since, until, 5)
			if err != nil {
				return err
			}
			if len(sessions) > 0 {
				fmt.Printf("\nTop conversations\n")
				printCostRows(db.CostBySession, sessions, totals)
			}
			fmt.Printf("\nBreak it down:  remaimber cost --by day|session|project\n")
			return nil
		},
	}
	cmd.Flags().StringVar(&by, "by", "", "Break down by: day, session, project, or model")
	cmd.Flags().StringVar(&since, "since", "", "Only spend on or after this date (YYYY-MM-DD)")
	cmd.Flags().StringVar(&until, "until", "", "Only spend on or before this date (YYYY-MM-DD)")
	cmd.Flags().IntVar(&limit, "limit", 20, "Max rows in a breakdown (0 = all)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	return cmd
}

func tailRows(rows []db.CostRow, n int) []db.CostRow {
	if len(rows) <= n {
		return rows
	}
	return rows[len(rows)-n:]
}

// printCostRows renders a breakdown with a proportion bar, so the row that
// dominates spend is visible without reading the numbers.
func printCostRows(by db.CostDimension, rows []db.CostRow, totals db.CostTotals) {
	var max float64
	for _, r := range rows {
		if r.USD > max {
			max = r.USD
		}
	}
	for _, r := range rows {
		key, label := r.Key, r.Label
		if by == db.CostBySession {
			key = shortID(key)
			label = importer.PrettyProjectName(label)
		} else if by == db.CostByProject {
			label = importer.PrettyProjectName(r.Key)
			key = ""
		} else if by == db.CostByModel {
			label = r.Key
			key = ""
		}
		bar := ""
		if max > 0 {
			n := int(12 * r.USD / max)
			bar = strings.Repeat("█", n) + strings.Repeat("·", 12-n)
		}
		switch by {
		case db.CostByDay:
			fmt.Printf("  %-10s %s  $%6.2f  %3d calls\n", r.Key, bar, r.USD, r.Calls)
		case db.CostByProject, db.CostByModel:
			fmt.Printf("  %s  $%6.2f  %3d calls  %s\n", bar, r.USD, r.Calls, label)
		default:
			fmt.Printf("  %-8s %s  $%6.2f  %3d calls  %s\n", key, bar, r.USD, r.Calls, label)
		}
	}
}

// jsonOut2 writes v as indented JSON to stdout.
func jsonOut2(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
