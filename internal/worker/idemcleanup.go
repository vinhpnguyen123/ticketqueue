package worker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vinhpnguyen123/ticketqueue/internal/metrics"
)

// IdemCleanupInterval is how often expired idempotency keys are purged.
const IdemCleanupInterval = 10 * time.Minute

// IdemCleanupCycle deletes expired idempotency_keys rows in batches of 1000
// so one huge backlog never holds locks for a whole-table delete.
func IdemCleanupCycle(ctx context.Context, pool *pgxpool.Pool) error {
	var total int64
	for {
		// Postgres has no DELETE ... LIMIT; ctid subquery batches instead.
		tag, err := pool.Exec(ctx,
			`DELETE FROM idempotency_keys WHERE ctid IN (
			     SELECT ctid FROM idempotency_keys WHERE expires_at < NOW() LIMIT 1000)`)
		if err != nil {
			return fmt.Errorf("delete expired idempotency keys: %w", err)
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < 1000 {
			break
		}
	}
	metrics.WorkerItems.WithLabelValues(metrics.WorkerIdemCleanup).Add(float64(total))
	if total > 0 {
		slog.Info("purged expired idempotency keys", "count", total)
	}

	// The webhook dedup ledger only needs to outlive the provider's retry
	// window; 7 days is far past it and keeps the table from growing forever.
	webhooks, err := purgeBatched(ctx, pool,
		`DELETE FROM webhook_events_seen WHERE ctid IN (
		     SELECT ctid FROM webhook_events_seen WHERE received_at < NOW() - INTERVAL '7 days' LIMIT 1000)`)
	if err != nil {
		return err
	}
	metrics.WorkerItems.WithLabelValues(metrics.WorkerIdemCleanup).Add(float64(webhooks))
	if webhooks > 0 {
		slog.Info("purged old webhook dedup rows", "count", webhooks)
	}
	return nil
}

// purgeBatched runs a ctid-limited DELETE until it stops finding rows, so one
// huge backlog never holds locks for a whole-table delete.
func purgeBatched(ctx context.Context, pool *pgxpool.Pool, query string) (int64, error) {
	var total int64
	for {
		tag, err := pool.Exec(ctx, query)
		if err != nil {
			return total, fmt.Errorf("batched delete: %w", err)
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < 1000 {
			return total, nil
		}
	}
}
