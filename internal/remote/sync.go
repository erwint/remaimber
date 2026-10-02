package remote

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

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

	st := &PullStats{Listed: len(objs), Claimed: map[string]int{}}
	var changed []Object
	for _, o := range objs {
		if importer.CheckRemoteKey(o.Key) != nil {
			st.NotSessions++
			continue
		}
		if !opts.Force && etags[o.Key] == o.ETag {
			st.Unchanged++
			continue
		}
		changed = append(changed, o)
		st.Bytes += o.Size
	}
	if opts.DryRun || len(changed) == 0 {
		st.Fetched = len(changed)
		return st, nil
	}
	if opts.Fetching != nil {
		opts.Fetching(len(changed), st.Bytes)
	}

	// One importer at a time, as for a local import: a hook firing mid-pull
	// waits rather than contending for the same write lock.
	lock := importer.AcquireLockWait(importer.ImportLockName, importer.InteractiveImportWait)
	if lock == nil {
		return st, errors.New("another import is running; try again when it finishes")
	}
	defer importer.Release(lock)

	for _, o := range changed {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		newMsgs, imported, claimedBy, err := pullOne(ctx, database, src, o, opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sync: %s: %v\n", o.Key, err)
			st.Failed++
			continue
		}
		st.Fetched++
		if claimedBy != "" {
			st.Claimed[claimedBy]++
		} else if imported {
			st.Imported++
			st.Messages += newMsgs
		}
		// Recorded last, so a transcript that failed above is fetched again
		// next time instead of being remembered as synced.
		if err := db.RecordRemoteObject(database, opts.Origin, db.SyncPull, o.Key, o.ETag, o.Size, opts.Source); err != nil {
			return st, err
		}
	}
	return st, nil
}

// pullOne fetches one transcript into a temporary file and imports it. The
// copy is not kept: the archive is the record, and the importer's byte offset
// makes the next pull of the same growing file parse only what was appended.
func pullOne(ctx context.Context, database *sql.DB, src Source, o Object, opts PullOptions) (newMsgs int, imported bool, claimedBy string, err error) {
	tmp, err := os.CreateTemp("", "remaimber-sync-*.jsonl")
	if err != nil {
		return 0, false, "", err
	}
	defer os.Remove(tmp.Name())
	if err := src.Fetch(ctx, o.Key, tmp); err != nil {
		tmp.Close()
		return 0, false, "", err
	}
	if err := tmp.Close(); err != nil {
		return 0, false, "", err
	}

	sf, err := importer.RemoteSessionFile(o.Key, tmp.Name(), opts.Origin)
	if err != nil {
		return 0, false, "", err
	}
	if owner, exists := db.SessionOrigin(database, sf.SessionID); exists && owner != opts.Origin {
		if owner == "" {
			owner = "local"
		}
		return 0, false, owner, nil
	}
	imported, newMsgs, _, err = importer.ImportFile(database, sf, opts.Force)
	return newMsgs, imported, "", err
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
			continue
		}
		st.Local++
		if db.IsPruned(database, sf.SessionID) {
			st.Forgotten++
			continue
		}
		info, err := os.Stat(sf.Path)
		if err != nil {
			continue // removed since the scan
		}
		etag := fmt.Sprintf("%d-%d", info.Size(), info.ModTime().Unix())
		if !opts.Force && etags[key] == etag {
			st.Unchanged++
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
