// Package segmenter is the conversation-summary engine: it maintains a session's
// summary as a sequence of segments split at context compactions and a size cap,
// freezing all but the open segment and rolling them up into one session summary.
// The LLM surface is an interface so the reconciliation logic is testable with a
// fake summarizer.
package segmenter

import (
	"context"
	"database/sql"
	"strings"

	"github.com/erwin/remaimber/internal/db"
	"github.com/erwin/remaimber/internal/summarizer"
	"github.com/erwin/remaimber/internal/types"
)

// LLM is the summarization surface the engine needs. summarizer.Config satisfies
// it; tests inject a fake.
type LLM interface {
	WindowSize() int
	Amend(ctx context.Context, prev string, window []types.Message) (string, error)
	ReduceSummaries(ctx context.Context, goal, prior string, partials []string) (string, error)
}

// Reconcile brings a session's segment summaries up to date and returns the
// roll-up session summary plus the message-id high-water mark it reflects.
//
// A session is split at every compaction and at sizeCap content messages; all but
// the last segment are frozen and never recomputed — only the open segment is
// amended. Content is restricted to the active conversation path so a rewind
// invalidates and re-summarizes from the divergence. The per-segment summaries
// are rolled up (a cheap reduce) into the session summary.
func Reconcile(ctx context.Context, llm LLM, database *sql.DB, sessionID, goal string, sizeCap int) (string, int64, error) {
	newID, err := db.MaxUAMessageID(database, sessionID)
	if err != nil {
		return "", 0, err
	}
	content, err := db.SegmentContent(database, sessionID, 0)
	if err != nil {
		return "", 0, err
	}
	boundaries, err := db.CompactionBoundaries(database, sessionID)
	if err != nil {
		return "", 0, err
	}

	// Restrict to the active path (compaction-bridged ancestors of the head) so a
	// rewind drops its abandoned branch; the existing match logic below then
	// re-summarizes from the divergence.
	onPath, err := db.ActivePathSet(database, sessionID)
	if err != nil {
		return "", 0, err
	}
	if onPath != nil {
		kept := content[:0:0]
		for _, m := range content {
			if onPath[m.UUID] {
				kept = append(kept, m)
			}
		}
		content = kept
	}
	var compactions []int64
	for _, b := range boundaries {
		if onPath == nil || onPath[b.UUID] {
			compactions = append(compactions, b.ID)
		}
	}

	plan := db.PlanSegments(content, compactions, sizeCap)
	stored, err := db.GetSegments(database, sessionID)
	if err != nil {
		return "", 0, err
	}

	var segs []db.Segment
	ci := 0 // index into content
	for i, span := range plan {
		var spanMsgs []types.Message
		for ci < len(content) && content[ci].ID <= span.EndID {
			if content[ci].ID >= span.StartID {
				spanMsgs = append(spanMsgs, content[ci])
			}
			ci++
		}

		// Unchanged segment (same boundaries + closed state, already summarized) — keep.
		if i < len(stored) && stored[i].StartUUID == span.StartUUID && stored[i].EndUUID == span.EndUUID &&
			stored[i].Closed == span.Closed && stored[i].Summary != "" {
			segs = append(segs, stored[i])
			continue
		}

		// (Re)summarize. Amend the open segment from its high-water mark only if it
		// merely grew — i.e. its previously-folded end is still on the active path.
		// If a rewind abandoned content inside the open segment, the stored summary
		// baked in now-dead content, so rebuild it from scratch instead.
		prev, fromHW := "", int64(0)
		if i < len(stored) && stored[i].StartUUID == span.StartUUID && !stored[i].Closed &&
			(onPath == nil || onPath[stored[i].EndUUID]) {
			prev, fromHW = stored[i].Summary, stored[i].HighWater
		}
		summary, err := foldSegment(ctx, llm, prev, spanMsgs, fromHW)
		if err != nil {
			return "", 0, err
		}
		seg := db.Segment{
			SessionID: sessionID, Seq: i,
			StartID: span.StartID, EndID: span.EndID,
			StartUUID: span.StartUUID, EndUUID: span.EndUUID,
			Summary: summary, MsgCount: span.Count, HighWater: span.EndID,
			Closed: span.Closed, Reason: span.Reason,
		}
		if err := db.UpsertSegment(database, &seg); err != nil {
			return "", 0, err
		}
		segs = append(segs, seg)
	}
	if len(plan) < len(stored) {
		if err := db.DeleteSegmentsFrom(database, sessionID, len(plan)); err != nil {
			return "", 0, err
		}
	}

	// Roll the per-segment summaries up into one session summary.
	var parts []string
	for _, s := range segs {
		if strings.TrimSpace(s.Summary) != "" {
			parts = append(parts, strings.TrimSpace(s.Summary))
		}
	}
	switch len(parts) {
	case 0:
		return "", newID, nil
	case 1:
		return summarizer.StripEphemeral(parts[0]), newID, nil
	default:
		rolled, err := llm.ReduceSummaries(ctx, goal, "", parts)
		if err != nil {
			return "", 0, err
		}
		return summarizer.StripEphemeral(rolled), newID, nil
	}
}

// foldSegment folds a span's content with id > fromHW into prev (windowed),
// returning the updated, ephemeral-stripped segment summary.
func foldSegment(ctx context.Context, llm LLM, prev string, spanMsgs []types.Message, fromHW int64) (string, error) {
	var toFold []types.Message
	for _, m := range spanMsgs {
		if m.ID > fromHW {
			toFold = append(toFold, m)
		}
	}
	window := llm.WindowSize()
	for i := 0; i < len(toFold); i += window {
		end := i + window
		if end > len(toFold) {
			end = len(toFold)
		}
		updated, err := llm.Amend(ctx, prev, toFold[i:end])
		if err != nil {
			return "", err
		}
		if updated != "" {
			prev = updated
		}
	}
	return summarizer.StripEphemeral(prev), nil
}
