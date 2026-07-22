package worker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
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
	if total > 0 {
		slog.Info("purged expired idempotency keys", "count", total)
	}
	return nil
}
