package remote

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/erwint/remaimber/internal/db"
	"github.com/erwint/remaimber/internal/importer"
)

// PullOptions configure a pull.
type PullOptions struct {
	Origin string // required: the machine these transcripts belong to
	Source string // the location, recorded beside each etag for display
	Force  bool   // fetch and re-import regardless of etags
	DryRun bool   // list and compare only
	// Fetching is called once, before the first transfer, with how many
	// objects changed; the first pull of a busy machine can take minutes.
	Fetching func(changed int, bytes int64)
}

// Change is what a sync does, or would do, with one transcript.
type Change struct {
	Key  string `json:"key"`
	Size int64  `json:"size"`
	// Action is "fetch" or "upload" for one that moves, "skip" for one that
	// does not.
	Action string `json:"action"`
	// Reason says why: "new" or "changed" for one that moves; "unchanged",
	// "already <origin>" (in the archive under another origin, "local" for this
	// machine's own), "not a session" or "forgotten" for one that does not.
	Reason string `json:"reason"`
}

// PullStats is what a pull did.
type PullStats struct {
	Listed      int   `json:"listed"`
	Unchanged   int   `json:"unchanged"`
	Fetched     int   `json:"fetched"`
	Bytes       int64 `json:"bytes"`
	Imported    int   `json:"imported"`
	Messages    int   `json:"messages"`
	NotSessions int   `json:"not_sessions"`
	Failed      int   `json:"failed"`
	// Claimed counts sessions skipped because the archive already holds them
	// under another origin ("local" for this machine's own).
	Claimed map[string]int `json:"claimed,omitempty"`
	// Changes accounts for every listed transcript on a dry run: what would be
	// fetched and what would be skipped, and why.
	Changes []Change `json:"changes,omitempty"`
}

// Pull imports another machine's transcripts, fetching only those whose etag
// changed since the last pull from that origin.
//
// A session belongs to whichever origin recorded it first. Two machines that
// pull from each other through a bucket would otherwise trade their own
// conversations back and forth, each time relabelling the other's copy; and a
// session recorded here must never be relabelled as foreign.
func Pull(ctx context.Context, database *sql.DB, src Source, opts PullOptions) (*PullStats, error) {
	if err := ValidOrigin(opts.Origin); err != nil {
		return nil, err
	}
	objs, err := src.List(ctx)
	if err != nil {
		return nil, err
	}
	etags, err := db.RemoteETags(database, opts.Origin, db.SyncPull)
	if err != nil {
		return nil, err
	}

	// One plan for both runs, so a dry run cannot say something different from
	// what the real one then does.
	st := &PullStats{Listed: len(objs), Claimed: map[string]int{}}
	plan := make([]Change, len(objs))
	var fetching int
	for i, o := range objs {
		plan[i] = planPull(database, o, etags, opts)
		switch {
		case plan[i].Action == "fetch":
			fetching++
			st.Bytes += o.Size
		case plan[i].Reason == "unchanged":
			st.Unchanged++
		case plan[i].Reason == "not a session":
			st.NotSessions++
		}
	}
	if opts.DryRun {
		st.Changes = plan
		st.Fetched = fetching
		for _, c := range plan {
			if owner, ok := strings.CutPrefix(c.Reason, "already "); ok {
				st.Claimed[owner]++
			}
		}
		return st, nil
	}
	if fetching > 0 && opts.Fetching != nil {
		opts.Fetching(fetching, st.Bytes)
	}

	// One importer at a time, as for a local import: a hook firing mid-pull
	// waits rather than contending for the same write lock.
	lock := importer.AcquireLockWait(importer.ImportLockName, importer.InteractiveImportWait)
	if lock == nil {
		return st, errors.New("another import is running; try again when it finishes")
	}
	defer importer.Release(lock)

	for i, o := range objs {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		c := plan[i]
		switch {
		case c.Action == "fetch":
			newMsgs, imported, err := pullOne(ctx, database, src, o, opts)
			if err != nil {
				fmt.Fprintf(os.Stderr, "sync: %s: %v\n", o.Key, err)
				st.Failed++
				continue
			}
			st.Fetched++
			if imported {
				st.Imported++
				st.Messages += newMsgs
			}
		case strings.HasPrefix(c.Reason, "already "):
			// Not fetched: whose it is is known from the key. Its etag is still
			// recorded, so the next pull counts it as unchanged.
			st.Claimed[strings.TrimPrefix(c.Reason, "already ")]++
		default:
			continue
		}
		// Recorded last, so a transcript that failed above is fetched again
		// next time instead of being remembered as synced.
		if err := db.RecordRemoteObject(database, opts.Origin, db.SyncPull, o.Key, o.ETag, o.Size, opts.Source); err != nil {
			return st, err
		}
	}
	return st, nil
}

// planPull decides what a pull does with one listed object. Everything it
// needs is in the key and the archive, so nothing is fetched to decide — not
// even whose session it is, which the key's file name carries.
func planPull(database *sql.DB, o Object, etags map[string]string, opts PullOptions) Change {
	c := Change{Key: o.Key, Size: o.Size, Action: "skip"}
	sf, err := importer.RemoteSessionFile(o.Key, "", opts.Origin)
	if err != nil {
		c.Reason = "not a session"
		return c
	}
	prev, seen := etags[o.Key]
	if !opts.Force && seen && prev == o.ETag {
		c.Reason = "unchanged"
		return c
	}
	if owner, exists := db.SessionOrigin(database, sf.SessionID); exists && owner != opts.Origin {
		if owner == "" {
			owner = "local"
		}
		c.Reason = "already " + owner
		return c
	}
	c.Action = "fetch"
	c.Reason = "new"
	if seen {
		c.Reason = "changed"
	}
	return c
}

// pullOne fetches one transcript into a temporary file and imports it. The
// copy is not kept: the archive is the record, and the importer's byte offset
// makes the next pull of the same growing file parse only what was appended.
func pullOne(ctx context.Context, database *sql.DB, src Source, o Object, opts PullOptions) (newMsgs int, imported bool, err error) {
	tmp, err := os.CreateTemp("", "remaimber-sync-*.jsonl")
	if err != nil {
		return 0, false, err
	}
	defer os.Remove(tmp.Name())
	if err := src.Fetch(ctx, o.Key, tmp); err != nil {
		tmp.Close()
		return 0, false, err
	}
	if err := tmp.Close(); err != nil {
		return 0, false, err
	}
	sf, err := importer.RemoteSessionFile(o.Key, tmp.Name(), opts.Origin)
	if err != nil {
		return 0, false, err
	}
	imported, newMsgs, _, err = importer.ImportFile(database, sf, opts.Force)
	return newMsgs, imported, err
}

// PushOptions configure a push.
type PushOptions struct {
	// Dest is the location pushed to. Etags are kept per destination, so
	// pushing to a second bucket sends everything there rather than trusting
	// what the first one received.
	Dest   string
	Force  bool
	DryRun bool
}

// PushStats is what a push did.
type PushStats struct {
	Local     int   `json:"local"`
	Unchanged int   `json:"unchanged"`
	Uploaded  int   `json:"uploaded"`
	Bytes     int64 `json:"bytes"`
	Forgotten int   `json:"forgotten"`
	Failed    int   `json:"failed"`
	// Changes accounts for every local transcript on a dry run: what would be
	// uploaded and what would be skipped, and why.
	Changes []Change `json:"changes,omitempty"`
}

// Push publishes this machine's transcripts, sending only those that changed
// since the last push under the same origin.
//
// Only transcripts in this machine's agent directories are sent, so what was
// pulled from elsewhere is never republished as this machine's; and a session
// pruned or forgotten here stays that way rather than reappearing in a bucket.
func Push(ctx context.Context, database *sql.DB, sink Sink, opts PushOptions) (*PushStats, error) {
	if opts.Dest == "" {
		return nil, errors.New("push needs a destination")
	}
	files, err := importer.ScanAll()
	if err != nil {
		return nil, err
	}
	etags, err := db.RemoteETags(database, opts.Dest, db.SyncPush)
	if err != nil {
		return nil, err
	}

	st := &PushStats{}
	for _, sf := range files {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		key := importer.RemoteKey(sf)
		if key == "" || !sink.Holds(sf.AgentOf()) {
			// Not something this location takes: outside an agent root, or
			// another agent than the one --agent named.
			continue
		}
		st.Local++
		info, err := os.Stat(sf.Path)
		if err != nil {
			continue // removed since the scan
		}
		c := Change{Key: key, Size: info.Size(), Action: "skip"}
		etag := fmt.Sprintf("%d-%d", info.Size(), info.ModTime().Unix())
		prev, seen := etags[key]
		switch {
		case db.IsPruned(database, sf.SessionID):
			c.Reason = "forgotten"
			st.Forgotten++
		case !opts.Force && seen && prev == etag:
			c.Reason = "unchanged"
			st.Unchanged++
		default:
			c.Action, c.Reason = "upload", "new"
			if seen {
				c.Reason = "changed"
			}
		}
		if opts.DryRun {
			st.Changes = append(st.Changes, c)
		}
		if c.Action != "upload" {
			continue
		}
		st.Bytes += info.Size()
		if opts.DryRun {
			st.Uploaded++
			continue
		}
		if err := sink.Put(ctx, key, sf.Path); err != nil {
			fmt.Fprintf(os.Stderr, "sync: %s: %v\n", key, err)
			st.Failed++
			continue
		}
		st.Uploaded++
		if err := db.RecordRemoteObject(database, opts.Dest, db.SyncPush, key, etag, info.Size(), opts.Dest); err != nil {
			return st, err
		}
	}
	return st, nil
}
