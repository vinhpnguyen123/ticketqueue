package worker

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/vinhpnguyen123/ticketqueue/internal/queue"
)

// PromotionInterval is how often the promotion worker runs a cycle.
const PromotionInterval = 5 * time.Second

// jitter staggers admissions within a batch so freshly admitted users don't
// hit the purchase endpoint as one thundering herd. Var so tests can zero it.
var jitter = func() {
	time.Sleep(time.Duration(100+rand.Intn(300)) * time.Millisecond)
}

// PromoteCycle runs one promotion pass over every on_sale event: admit
// FIFO from the Redis queue up to the event's free window count, then
// sweep expired windows so their slots free up for the next cycle.
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

func promoteEvent(ctx context.Context, rdb *redis.Client, pool *pgxpool.Pool, eventID string, maxWindows, windowSeconds int) error {
	// Option A semantics: an occupied purchase window IS an admitted,
	// unexpired queue_entries row — no separate window state anywhere.
	var active int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM queue_entries
		 WHERE event_id = $1 AND status = 'admitted'
		   AND admitted_at > NOW() - make_interval(secs => $2)`,
		eventID, windowSeconds,
	).Scan(&active); err != nil {
		return fmt.Errorf("count active windows: %w", err)
	}

	slots := maxWindows - active
	if slots <= 0 {
		return nil
	}

	// ZPOPMIN atomically pops the earliest joiners — FIFO admission, and no
	// other worker instance can pop the same members.
	popped, err := rdb.ZPopMin(ctx, queue.Key(eventID), int64(slots)).Result()
	if err != nil {
		return fmt.Errorf("zpopmin queue: %w", err)
	}

	for i, z := range popped {
		userID := z.Member.(string)
		if err := admitUser(ctx, pool, userID, eventID); err != nil {
			slog.Error("admit user failed", "user_id", userID, "event_id", eventID, "error", err)
		}
		if i < len(popped)-1 {
			jitter()
		}
	}
	return nil
}

// admitUser flips one waiting entry to admitted and audits it, in one
// transaction so the audit row never exists without the admission.
func admitUser(ctx context.Context, pool *pgxpool.Pool, userID, eventID string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx,
		`UPDATE queue_entries SET status = 'admitted', admitted_at = NOW()
		 WHERE user_id = $1 AND event_id = $2 AND status = 'waiting'`,
		userID, eventID)
	if err != nil {
		return fmt.Errorf("admit update: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// User left between the ZPOPMIN and this update — skip, don't crash
		// the batch; their durable row is already 'left'.
		slog.Info("skipping admission, entry no longer waiting", "user_id", userID, "event_id", eventID)
		return nil
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO audit_log (entity_type, entity_id, action, actor_type, metadata)
		 VALUES ('event', $1, 'queue_admitted', 'worker', $2)`,
		eventID, map[string]any{"user_id": userID}); err != nil {
		return fmt.Errorf("audit insert: %w", err)
	}
	return tx.Commit(ctx)
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
			 VALUES ('event', $1, 'queue_window_expired', 'worker', $2)`,
			eventID, map[string]any{"user_id": userID}); err != nil {
			return fmt.Errorf("audit expired: %w", err)
		}
	}
	return tx.Commit(ctx)
}
