package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/vinhpnguyen123/ticketqueue/internal/db"
	"github.com/vinhpnguyen123/ticketqueue/internal/metrics"
	"github.com/vinhpnguyen123/ticketqueue/internal/queue"
)

// PromotionInterval is how often the promotion worker runs a cycle.
const PromotionInterval = 5 * time.Second

// PromotionBatch caps admissions per event per cycle, so one backlog cannot hold
// the cycle away from other events and the expired-window sweep. Jitter already
// limits admissions to about 4 a second, so this bounds time, not throughput.
const PromotionBatch = 50

// jitter staggers admissions within a batch so freshly admitted users don't
// hit the purchase endpoint as one thundering herd. Var so tests can zero it.
var jitter = func() {
	time.Sleep(time.Duration(100+rand.Intn(300)) * time.Millisecond)
}

// PromoteCycle runs one promotion pass over every on_sale event: admit
// FIFO by joined_at up to the event's free window count, then sweep
// expired windows so their slots free up for the next cycle.
func PromoteCycle(ctx context.Context, rdb *redis.Client, pool *pgxpool.Pool) error {
	rows, err := pool.Query(ctx,
		`SELECT id, max_concurrent_windows, purchase_window_seconds
		 FROM events WHERE status = 'on_sale'`)
	if err != nil {
		return fmt.Errorf("list on_sale events: %w", err)
	}
	type ev struct {
		id            string
		maxWindows    int
		windowSeconds int
	}
	var events []ev
	for rows.Next() {
		var e ev
		if err := rows.Scan(&e.id, &e.maxWindows, &e.windowSeconds); err != nil {
			rows.Close()
			return fmt.Errorf("scan event: %w", err)
		}
		events = append(events, e)
	}
	rows.Close()
	if rows.Err() != nil {
		return fmt.Errorf("iterate events: %w", rows.Err())
	}

	for _, e := range events {
		if err := promoteEvent(ctx, rdb, pool, e.id, e.maxWindows, e.windowSeconds); err != nil {
			// One broken event must not starve the others.
			slog.Error("promotion failed for event", "event_id", e.id, "error", err)
		}
		if err := sweepExpired(ctx, pool, e.id, e.windowSeconds); err != nil {
			slog.Error("expired-window sweep failed", "event_id", e.id, "error", err)
		}
	}
	return nil
}

// promoteEvent admits waiting users one at a time until the event is full, the
// queue is empty, the pass has used its budget, or the worker is shutting down.
// Postgres decides who is next, so a crash at any point leaves everyone not yet
// admitted still 'waiting' and still reachable next cycle. Redis only mirrors
// the outcome for position reads.
//
// The budget matters because admitNext recounts capacity: windows that expire
// or leave mid-pass look free, and without a limit a short window could keep
// this loop on one event until its whole queue is gone.
func promoteEvent(ctx context.Context, rdb *redis.Client, pool *pgxpool.Pool, eventID string, maxWindows, windowSeconds int) error {
	for range min(maxWindows, PromotionBatch) {
		if ctx.Err() != nil {
			return nil
		}
		userID, admitted, err := admitNext(ctx, pool, eventID, maxWindows, windowSeconds)
		if err != nil {
			return err
		}
		if !admitted {
			return nil
		}
		metrics.WorkerItems.WithLabelValues(metrics.WorkerPromotion).Inc()

		// After commit, Postgres first (§3). WithoutCancel so a shutdown between
		// the commit and here does not leave the member behind. If this fails
		// anyway, the drift checker's queue sync removes it.
		if err := rdb.ZRem(context.WithoutCancel(ctx), queue.Key(eventID), userID).Err(); err != nil {
			slog.Error("zrem admitted user failed", "user_id", userID, "event_id", eventID, "error", err)
		}
		jitter()
	}
	return nil
}

// admitNext admits the earliest waiting user if the event has a free window, in
// one transaction so the audit row never exists without the admission.
//
// The advisory lock makes count-then-admit atomic per event, so two workers
// cannot both spend the same free slot. It is not `events ... FOR UPDATE`
// because that would conflict with the FOR KEY SHARE every booking and queue
// insert takes on its event. Transaction-scoped, so it is safe under PgBouncer
// transaction mode and is released at commit, never held across jitter.
func admitNext(ctx context.Context, pool *pgxpool.Pool, eventID string, maxWindows, windowSeconds int) (userID string, admitted bool, err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return "", false, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`, eventID); err != nil {
		return "", false, fmt.Errorf("event lock: %w", err)
	}

	// Option A semantics: an occupied purchase window IS an admitted,
	// unexpired queue_entries row — no separate window state anywhere.
	var active int
	if err := tx.QueryRow(ctx,
		`SELECT COUNT(*) FROM queue_entries
		 WHERE event_id = $1 AND status = 'admitted'
		   AND admitted_at > NOW() - make_interval(secs => $2)`,
		eventID, windowSeconds,
	).Scan(&active); err != nil {
		return "", false, fmt.Errorf("count active windows: %w", err)
	}
	if active >= maxWindows {
		return "", false, nil
	}

	// SKIP LOCKED: a row locked by a concurrent leave is passed over rather than
	// waited on; if the leave rolls back, that user is first in line next time.
	err = tx.QueryRow(ctx,
		`UPDATE queue_entries SET status = 'admitted', admitted_at = NOW()
		 WHERE id = (SELECT id FROM queue_entries
		             WHERE event_id = $1 AND status = 'waiting'
		             ORDER BY joined_at, id LIMIT 1
		             FOR UPDATE SKIP LOCKED)
		 RETURNING user_id`,
		eventID,
	).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("admit update: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO audit_log (entity_type, entity_id, action, actor_type, metadata)
		 VALUES ('event', $1, 'queue_admitted', 'worker', $2::jsonb)`,
		eventID, userIDMeta(userID)); err != nil {
		return "", false, fmt.Errorf("audit insert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", false, fmt.Errorf("commit: %w", err)
	}
	return userID, true, nil
}

// sweepExpired flips admitted-but-expired windows to 'expired', freeing
// the slots the capacity count in promoteEvent relies on next cycle.
func sweepExpired(ctx context.Context, pool *pgxpool.Pool, eventID string, windowSeconds int) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx,
		`UPDATE queue_entries SET status = 'expired', left_at = NOW()
		 WHERE event_id = $1 AND status = 'admitted'
		   AND admitted_at <= NOW() - make_interval(secs => $2)
		 RETURNING user_id`,
		eventID, windowSeconds)
	if err != nil {
		return fmt.Errorf("expire update: %w", err)
	}
	var userIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("scan expired: %w", err)
		}
		userIDs = append(userIDs, id)
	}
	rows.Close()
	if rows.Err() != nil {
		return fmt.Errorf("iterate expired: %w", rows.Err())
	}

	for _, userID := range userIDs {
		if _, err := tx.Exec(ctx,
			`INSERT INTO audit_log (entity_type, entity_id, action, actor_type, metadata)
			 VALUES ('event', $1, 'queue_window_expired', 'worker', $2::jsonb)`,
			eventID, userIDMeta(userID)); err != nil {
			return fmt.Errorf("audit expired: %w", err)
		}
	}
	return tx.Commit(ctx)
}

// userIDMeta builds the one metadata shape this worker writes. It swallows the
// marshal error because a map with one string in it cannot fail to marshal, and
// an audit row is not worth aborting a promotion cycle over.
func userIDMeta(userID string) any {
	md, _ := db.JSONB(map[string]any{"user_id": userID})
	return md
}
