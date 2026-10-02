package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/erwint/remaimber/internal/db"
	"github.com/erwint/remaimber/internal/remote"
)

const originFlagHelp = `Filter by the machine a session was synced from ("local" for this one; default: all)`

func syncCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Bring in conversations from other machines, over ssh or through S3",
		Long: `Bring in conversations from other machines, over ssh or through S3.

Over ssh, pull reads the other machine's agent directories directly - nothing
needs installing there. An S3 bucket is a store between machines: each one
pushes its own transcripts under its name, and the others pull them.

Every pulled session is labelled with the --origin it came from, shown as
@origin in list and search, and filterable with --origin. Only what changed
since the last sync is transferred: each object's etag is recorded, and an
unchanged one is not fetched again.`,
		Example: `  # Read another machine's sessions directly
  remaimber sync pull ssh://build@mac-mini.local --origin mac-mini

  # Publish this machine's sessions to a bucket, and pull another's from it
  remaimber sync push s3://team-bucket/remaimber --aws-profile work
  remaimber sync pull s3://team-bucket/remaimber --origin laptop --aws-profile work

  # What has been synced, from where
  remaimber sync status`,
	}
	cmd.AddCommand(syncPullCmd(), syncPushCmd(), syncStatusCmd())
	return cmd
}

func syncPullCmd() *cobra.Command {
	var origin, agent, profile string
	var force, dryRun, jsonOut bool
	cmd := &cobra.Command{
		Use:   "pull <ssh://[user@]host[/path] | s3://bucket[/prefix]>",
		Short: "Import another machine's conversations",
		Long: `Import another machine's conversations.

ssh://[user@]host[:port][/path]
  Reads the remote over ssh, which must log in without a prompt (a key or an
  agent). The path is a home directory holding .claude/projects, .codex/sessions
  and .pi/agent/sessions (default: the remote home). With --agent it is that
  agent's session directory itself, for one kept somewhere else.

s3://bucket[/prefix]
  Reads <prefix>/<origin>/ in a store filled by 'remaimber sync push'. Uses the
  AWS CLI and its usual credentials: --aws-profile, else AWS_PROFILE, else the
  default chain.

--origin is required: it names the machine, and is how its sessions stay
distinguishable from this one's. A session already in the archive - recorded
here, or pulled under another origin - is left as it is.`,
		Example: `  remaimber sync pull ssh://build@mac-mini.local --origin mac-mini
  remaimber sync pull ssh://nas/~/backups/laptop-home --origin laptop
  remaimber sync pull ssh://box/srv/codex/sessions --agent codex --origin box
  remaimber sync pull s3://team-bucket/remaimber --origin laptop --dry-run`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := remote.ValidOrigin(origin); err != nil {
				return err
			}
			loc, err := remote.Parse(args[0])
			if err != nil {
				return err
			}
			if agent != "" && loc.Scheme != "ssh" {
				return fmt.Errorf("--agent applies to an ssh path; a store's keys already name the agent")
			}
			src, err := remote.OpenSource(loc, remote.Options{Origin: origin, Agent: agent, AWSProfile: profile})
			if err != nil {
				return err
			}

			database, err := openDB()
			if err != nil {
				return err
			}
			defer database.Close()

			st, err := remote.Pull(cmd.Context(), database, src, remote.PullOptions{
				Origin: origin, Source: loc.String(), Force: force, DryRun: dryRun,
				Fetching: func(n int, bytes int64) {
					if !jsonOut {
						fmt.Fprintf(os.Stderr, "Fetching %d changed transcript(s), %s...\n", n, sizeOf(bytes))
					}
				},
			})
			if st != nil && jsonOut {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				if encErr := enc.Encode(st); encErr != nil {
					return encErr
				}
				return err
			}
			if st != nil {
				printPull(loc, origin, st, dryRun)
			}
			return err
		},
	}
	cmd.Flags().StringVar(&origin, "origin", "", "Name of the machine these conversations come from (required)")
	cmd.Flags().StringVar(&agent, "agent", "", "ssh only: the path is this agent's session directory (claude, codex or pi)")
	cmd.Flags().StringVar(&profile, "aws-profile", "", "AWS CLI profile for s3:// (default: AWS_PROFILE)")
	cmd.Flags().BoolVar(&force, "force", false, "Fetch and re-import everything, ignoring recorded etags")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Show what would be fetched, without fetching")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	return cmd
}

func printPull(loc *remote.Location, origin string, st *remote.PullStats, dryRun bool) {
	fmt.Printf("%s as %q: %d transcript(s) listed, %d unchanged\n", loc, origin, st.Listed, st.Unchanged)
	if dryRun {
		fmt.Printf("Would fetch %d (%s)\n", st.Fetched, sizeOf(st.Bytes))
		return
	}
	if st.Fetched > 0 {
		fmt.Printf("Fetched %d (%s); %d brought new messages (%d)\n",
			st.Fetched, sizeOf(st.Bytes), st.Imported, st.Messages)
	}
	for owner, n := range st.Claimed {
		fmt.Printf("Left %d already in the archive as %s\n", n, owner)
	}
	if st.NotSessions > 0 {
		fmt.Printf("Ignored %d file(s) that are not sessions\n", st.NotSessions)
	}
	if st.Failed > 0 {
		fmt.Printf("%d failed (above); they are retried on the next pull\n", st.Failed)
	}
}

func syncPushCmd() *cobra.Command {
	var origin, profile string
	var force, dryRun, jsonOut bool
	cmd := &cobra.Command{
		Use:   "push <s3://bucket[/prefix]>",
		Short: "Publish this machine's conversations to an S3 store",
		Long: `Publish this machine's conversations to an S3 store, under
<prefix>/<origin>/, for other machines to pull.

Only this machine's own transcripts are sent - nothing pulled from elsewhere -
and a session pruned or forgotten here is not. Unchanged transcripts are skipped
by etag. Over ssh there is nothing to push: run pull on the other machine.`,
		Example: `  remaimber sync push s3://team-bucket/remaimber
  remaimber sync push s3://team-bucket/remaimber --origin laptop --aws-profile work`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if origin == "" {
				origin = defaultOrigin()
			}
			if err := remote.ValidOrigin(origin); err != nil {
				return fmt.Errorf("%w (this machine's name did not make a usable default; pass --origin)", err)
			}
			loc, err := remote.Parse(args[0])
			if err != nil {
				return err
			}
			sink, err := remote.OpenSink(loc, remote.Options{Origin: origin, AWSProfile: profile})
			if err != nil {
				return err
			}

			database, err := openDB()
			if err != nil {
				return err
			}
			defer database.Close()

			st, err := remote.Push(cmd.Context(), database, sink, remote.PushOptions{
				Origin: origin, Dest: loc.String(), Force: force, DryRun: dryRun,
			})
			if st != nil && jsonOut {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				if encErr := enc.Encode(st); encErr != nil {
					return encErr
				}
				return err
			}
			if st != nil {
				verb := "Uploaded"
				if dryRun {
					verb = "Would upload"
				}
				fmt.Printf("%s as %q: %d local transcript(s), %d unchanged\n", loc, origin, st.Local, st.Unchanged)
				fmt.Printf("%s %d (%s)\n", verb, st.Uploaded, sizeOf(st.Bytes))
				if st.Forgotten > 0 {
					fmt.Printf("Held back %d pruned or forgotten here\n", st.Forgotten)
				}
				if st.Failed > 0 {
					fmt.Printf("%d failed (above); they are retried on the next push\n", st.Failed)
				}
			}
			return err
		},
	}
	cmd.Flags().StringVar(&origin, "origin", "", "This machine's name in the store (default: its hostname)")
	cmd.Flags().StringVar(&profile, "aws-profile", "", "AWS CLI profile (default: AWS_PROFILE)")
	cmd.Flags().BoolVar(&force, "force", false, "Upload everything, ignoring recorded etags")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Show what would be uploaded, without uploading")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	return cmd
}

func syncStatusCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show what has been synced, per origin",
		Example: `  # Every origin pulled from or pushed as, with its last sync
  remaimber sync status`,
		RunE: func(cmd *cobra.Command, args []string) error {
			database, err := openDB()
			if err != nil {
				return err
			}
			defer database.Close()
			stats, err := db.SyncStatus(database)
			if err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(stats)
			}
			if len(stats) == 0 {
				fmt.Println("Nothing synced yet. See: remaimber sync --help")
				return nil
			}
			for _, s := range stats {
				if s.Direction == db.SyncPull {
					fmt.Printf("pull  %-16s %5d transcript(s), %5d session(s)  last %s  from %s\n",
						s.Origin, s.Objects, s.Sessions, s.LastSync, s.Source)
				} else {
					fmt.Printf("push  %-16s %5d transcript(s)                    last %s  to %s\n",
						s.Origin, s.Objects, s.LastSync, s.Source)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	return cmd
}

// defaultOrigin is this machine's short hostname, lowercased: what someone
// would call it, and stable across networks that append different domains.
func defaultOrigin() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	h, _, _ = strings.Cut(h, ".")
	return strings.ToLower(h)
}

func sizeOf(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
