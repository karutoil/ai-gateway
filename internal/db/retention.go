package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

// DefaultVideoJobRetention is how long a finished video job is kept when the
// provider never told us an expiry (most dialects do not). Client DELETE and
// expiry-driven pruning cover the rest.
const DefaultVideoJobRetention = 7 * 24 * time.Hour

// videoJobTerminalStatuses are the statuses after which a job can never change
// again. A row in any other state is still owned by an in-flight request and is
// never pruned, however old it looks: deleting it would turn a slow generation
// into a 404 for the client polling it.
//
// The first two must stay in sync with videogen.IsTerminal (completed / failed);
// "expired" and "cancelled"/"canceled" are included because providers and
// operators do write them.
var videoJobTerminalStatuses = []string{"completed", "failed", "expired", "cancelled", "canceled"}

// VideoJobPruneResult reports what one sweep did. Rows past their expires_at and
// rows merely past the retention window are counted separately because they mean
// different things operationally (the first is the provider's lifecycle, the
// second is our storage policy).
type VideoJobPruneResult struct {
	Expired   int64
	Retention int64
	// Batches is how many DELETE statements ran. A sweep stops as soon as a
	// batch deletes fewer rows than the limit, so steady state costs one
	// statement per predicate.
	Batches int
}

// Total is the number of rows deleted by the sweep.
func (r VideoJobPruneResult) Total() int64 { return r.Expired + r.Retention }

// PruneVideoJobs deletes terminal video_jobs rows that are past their
// provider-supplied expires_at, plus terminal rows older than retention when no
// expiry was recorded, in bounded batches.
//
// Why this exists: until now the only statement that removed a video_jobs row
// was the client's own DELETE /v1/videos/{id}. Every create — including failed
// and abandoned ones — left a permanent row carrying the prompt text and the
// signed video_url, so the table grew with all video traffic forever, and the
// expires_at the provider supplied was written and never read.
//
// Semantics (so a caller cannot get them wrong):
//   - only terminal rows are deleted (completed / failed / expired / cancelled);
//     queued and in_progress rows are never touched, however old;
//   - every time bound is a bound time.Time, never a SQL date string. A string
//     bound against SQLite's RFC3339Nano text column compares ' ' against 'T'
//     and silently misses the boundary row (verified on a migrated DB);
//   - batches are bounded (batchSize, default 500) and the sweep stops at the
//     first short batch, so a large backlog cannot hold a write transaction open
//     or balloon the WAL;
//   - safe to run concurrently with other sweeps and with live traffic: the
//     DELETE is idempotent, so two concurrent sweeps simply split the work. On
//     SQLite the writer lock serialises them (busy_timeout is set on the DSN) and
//     on Postgres the second deletes nothing. A cancelled context returns the
//     counts accumulated so far plus the context error.
//
// Intended call site (cmd/gateway/main.go is owned by another change, so this is
// exported rather than wired): next to the existing
// `go runLogRetention(database, cfg.LogRetentionDays)` call (~main.go:241), add
//
//	go func() {
//		// Once at boot, then hourly. Sweeping is one indexed statement in
//		// steady state, and a long-lived process must not leak rows between
//		// daily ticks.
//		for {
//			if _, err := db.PruneVideoJobs(context.Background(), database, 500, db.DefaultVideoJobRetention); err != nil {
//				log.Warn().Err(err).Msg("video job retention sweep failed")
//			}
//			time.Sleep(time.Hour)
//		}
//	}()
//
// Optionally also call it opportunistically from proxy.Handler.VideoList
// (mirroring response_store.go's inline expiry) if a faster reaction to provider
// expiry is wanted. Do not call it on the create path.
func PruneVideoJobs(ctx context.Context, database *sql.DB, batchSize int, retention time.Duration) (VideoJobPruneResult, error) {
	var res VideoJobPruneResult
	if database == nil {
		return res, fmt.Errorf("prune video jobs: nil database")
	}
	if batchSize <= 0 {
		batchSize = 500
	}
	if retention <= 0 {
		retention = DefaultVideoJobRetention
	}
	now := time.Now().UTC()
	cutoff := now.Add(-retention)

	// Status placeholders are expanded positionally, so every terminal status is
	// a bound parameter: no string concatenation of values, and Rebind's
	// `?`-renumbering stays correct because the only `?` in the SQL are real
	// placeholders. (No `?` ever appears inside a quoted literal in these
	// queries.)
	statusList := strings.TrimSuffix(strings.Repeat("?,", len(videoJobTerminalStatuses)), ",")

	// Two sweeps rather than one OR-ed predicate: each predicate has its own
	// index from migration 030 (idx_video_jobs_expires and
	// idx_video_jobs_status_created) and an OR across two indexed columns makes
	// both planners fall back to a table scan.
	expiredQ := Rebind(`DELETE FROM video_jobs WHERE id IN (
		SELECT id FROM video_jobs
		 WHERE expires_at IS NOT NULL AND expires_at < ? AND status IN (` + statusList + `)
		 LIMIT ?)`)
	retentionQ := Rebind(`DELETE FROM video_jobs WHERE id IN (
		SELECT id FROM video_jobs
		 WHERE expires_at IS NULL AND created_at < ? AND status IN (` + statusList + `)
		 LIMIT ?)`)

	for _, step := range []struct {
		q     string
		bound time.Time
		count *int64
	}{
		{expiredQ, now, &res.Expired},
		{retentionQ, cutoff, &res.Retention},
	} {
		for {
			if err := ctx.Err(); err != nil {
				return res, err
			}
			args := make([]any, 0, len(videoJobTerminalStatuses)+2)
			args = append(args, step.bound)
			for _, s := range videoJobTerminalStatuses {
				args = append(args, s)
			}
			args = append(args, batchSize)
			r, err := database.ExecContext(ctx, step.q, args...)
			if err != nil {
				return res, fmt.Errorf("prune video jobs: %w", err)
			}
			n, _ := r.RowsAffected()
			res.Batches++
			*step.count += n
			if n < int64(batchSize) {
				break
			}
		}
	}

	if res.Total() > 0 {
		log.Info().
			Int64("expired", res.Expired).
			Int64("retention", res.Retention).
			Int("batches", res.Batches).
			Dur("retention_window", retention).
			Msg("video job retention sweep")
	}
	return res, nil
}
