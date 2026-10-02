package main

import (
	"encoding/json"
	"fmt"
	"os"

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

A location is ssh://[user@]host[/path] or s3://bucket[/prefix], and the path
is read as a home directory with each agent's sessions in their native layout:
.claude/projects, .codex/sessions, .pi/agent/sessions. With --agent, the path is
that one agent's session directory instead, for sessions kept or copied
anywhere else.

Over ssh, nothing needs installing on the other machine. In S3, whatever already
syncs session directories into the bucket can keep doing so; push is there for a
machine with nothing else to do it.

Every pulled session is labelled with the --origin it came from, shown as
@origin in list and search, and filterable with --origin. Only what changed
since the last sync is transferred: each object's etag is recorded, and an
unchanged one is not fetched again.`,
		Example: `  # Read another machine's sessions directly
  remaimber sync pull ssh://build@mac-mini.local --origin mac-mini

  # Sessions some other system copies into a bucket, one home per machine
  remaimber sync pull s3://team-bucket/homes/laptop --origin laptop --aws-profile work

  # Only Codex's session directory, copied on its own
  remaimber sync pull s3://team-bucket/laptop/codex-sessions --agent codex --origin laptop

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

The path is a home directory holding each agent's sessions in their native
layout - .claude/projects, .codex/sessions, .pi/agent/sessions - and any agent
missing there is skipped. With --agent it is that agent's session directory
itself, in that agent's native layout.

ssh://[user@]host[:port][/path]
  Reads the remote over ssh, which must log in without a prompt (a key or an
  agent). Default path: the remote home.

s3://bucket[/prefix]
  Reads objects under the prefix, as written by whatever syncs them there (or by
  'remaimber sync push'). Uses the AWS CLI and its usual credentials:
  --aws-profile, else AWS_PROFILE, else the default chain. Default prefix: the
  bucket root.

--origin is required: it names the machine, and is how its sessions stay
distinguishable from this one's. A session already in the archive - recorded
here, or pulled under another origin - is left as it is.`,
		Example: `  remaimber sync pull ssh://build@mac-mini.local --origin mac-mini
  remaimber sync pull ssh://nas/~/backups/laptop-home --origin laptop
  remaimber sync pull ssh://box/srv/codex/sessions --agent codex --origin box
  remaimber sync pull s3://team-bucket/homes/laptop --origin laptop --dry-run
  remaimber sync pull s3://team-bucket/laptop/claude-projects --agent claude --origin laptop`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := remote.ValidOrigin(origin); err != nil {
				return err
			}
			loc, err := remote.Parse(args[0])
			if err != nil {
				return err
			}
			src, err := remote.OpenSource(loc, remote.Options{Agent: agent, AWSProfile: profile})
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
	cmd.Flags().StringVar(&agent, "agent", "", "The path is this agent's session directory (claude, codex or pi)")
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
	var agent, profile string
	var force, dryRun, jsonOut bool
	cmd := &cobra.Command{
		Use:   "push <s3://bucket[/prefix]>",
		Short: "Copy this machine's conversations into S3",
		Long: `Copy this machine's conversations into S3, for when nothing else already does.

They are written in the agents' native layout under the prefix, as a home
directory would hold them (.claude/projects/..., .codex/sessions/...), so
'sync pull' on another machine reads them like any other copy. Give each
machine its own prefix. With --agent, only that agent's sessions are written,
and the prefix is its session directory.

Only this machine's own transcripts are sent - nothing pulled from elsewhere -
and a session pruned or forgotten here is not. Unchanged transcripts are skipped
by etag. Over ssh there is nothing to push: run pull on the other machine.`,
		Example: `  remaimber sync push s3://team-bucket/homes/laptop --aws-profile work
  remaimber sync push s3://team-bucket/laptop/codex-sessions --agent codex`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			loc, err := remote.Parse(args[0])
			if err != nil {
				return err
			}
			sink, err := remote.OpenSink(loc, remote.Options{Agent: agent, AWSProfile: profile})
			if err != nil {
				return err
			}

			database, err := openDB()
			if err != nil {
				return err
			}
			defer database.Close()

			st, err := remote.Push(cmd.Context(), database, sink, remote.PushOptions{
				Dest: loc.String(), Force: force, DryRun: dryRun,
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
				fmt.Printf("%s: %d local transcript(s), %d unchanged\n", loc, st.Local, st.Unchanged)
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
	cmd.Flags().StringVar(&agent, "agent", "", "Push only this agent's sessions; the prefix is its session directory")
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
						"", s.Objects, s.LastSync, s.Source)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	return cmd
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
