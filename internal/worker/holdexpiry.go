package worker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/vinhpnguyen123/ticketqueue/internal/booking"
)

// HoldExpiryInterval is how often expired holds are released.
const HoldExpiryInterval = 5 * time.Second

// HoldExpiryCycle fails bookings whose hold expired and releases their held
// tickets back to available, then compensates the Redis inventory counters.
func HoldExpiryCycle(ctx context.Context, rdb *redis.Client, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	// SKIP LOCKED: another worker instance (or TX2 finalizing a payment,
	// which also locks the booking row) can hold these rows; skip, don't wait.
	rows, err := tx.Query(ctx,
		`SELECT id, ticket_tier_id FROM booking
		 WHERE status = 'held' AND hold_expires_at < NOW()
		 ORDER BY hold_expires_at LIMIT 50
		 FOR UPDATE SKIP LOCKED`)
	if err != nil {
		return fmt.Errorf("select expired holds: %w", err)
	}
	type hold struct{ id, tierID string }
	var holds []hold
	for rows.Next() {
		var h hold
		if err := rows.Scan(&h.id, &h.tierID); err != nil {
			rows.Close()
			return fmt.Errorf("scan hold: %w", err)
		}
		holds = append(holds, h)
	}
	rows.Close()
	if rows.Err() != nil {
		return fmt.Errorf("iterate holds: %w", rows.Err())
	}
	if len(holds) == 0 {
		return nil
	}

	released := map[string]int{} // tier_id → count, for post-commit Redis compensation
	for _, h := range holds {
		tag, err := tx.Exec(ctx,
			`UPDATE booking SET status = 'failed', failed_at = NOW(), version = version + 1
			 WHERE id = $1 AND status = 'held'`, h.id)
		if err != nil {
			return fmt.Errorf("fail booking %s: %w", h.id, err)
		}
		if tag.RowsAffected() == 0 {
			continue // guarded update lost: someone else resolved it — fine
		}

		// Only 'held' tickets go back; a ticket TX2 already flipped to
		// 'sold' belongs to a paid customer and must never be released.
		tag, err = tx.Exec(ctx,
			`UPDATE tickets SET status = 'available', booking_id = NULL
			 WHERE booking_id = $1 AND status = 'held'`, h.id)
		if err != nil {
			return fmt.Errorf("release tickets for %s: %w", h.id, err)
		}
		released[h.tierID] += int(tag.RowsAffected())

		for _, action := range []string{"booking_expired", "tickets_released"} {
			if _, err := tx.Exec(ctx,
				`INSERT INTO audit_log (entity_type, entity_id, action, actor_type, metadata)
				 VALUES ('booking', $1, $2, 'worker', $3)`,
				h.id, action, map[string]any{"tickets_released": tag.RowsAffected()}); err != nil {
				return fmt.Errorf("audit %s: %w", action, err)
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	// Redis compensation AFTER commit, never inside the transaction. Lost to
	// a crash here → the drift checker heals it; accepted window.
	for tierID, qty := range released {
		if qty == 0 {
			continue
		}
		if err := booking.CompensateInventory(ctx, rdb, tierID, qty); err != nil {
			slog.Error("hold-expiry inventory compensation failed (drift checker will heal)",
				"tier_id", tierID, "qty", qty, "error", err)
		}
	}
	slog.Info("released expired holds", "bookings", len(holds))
	return nil
}
