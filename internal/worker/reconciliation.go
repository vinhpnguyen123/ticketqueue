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

// ReconcileInterval is how often stuck holds are resolved against the provider.
const ReconcileInterval = 30 * time.Second

// ReconcileBatch caps one cycle so a backlog can't produce an unbounded run of
// provider calls.
const ReconcileBatch = 50

type candidate struct {
	id         string
	idemKey    string
	tierID     string
	totalCents int64
	currency   string
}

// ReconcileCycle resolves bookings left 'held' by an ambiguous payment outcome:
// it asks the provider what really happened, by the booking's idempotency key,
// and confirms or fails accordingly. When the provider can't be reached it does
// nothing — never guess (§3, the money rule).
//
// The cycle is deliberately three separate steps: a PLAIN READ to find work,
// the provider call with NO transaction open, then a fresh short transaction to
// finalize. Holding a row lock across the network call would violate
// Principle #2. Safety comes instead from the guarded UPDATEs inside
// ConfirmBooking/FailBooking plus the provider's own idempotency: if two
// instances reconcile the same booking, the loser gets applied=false and skips,
// and the duplicated lookup is side-effect-free.
func ReconcileCycle(ctx context.Context, rdb *redis.Client, pool *pgxpool.Pool, pc *booking.PaymentClient) error {
	// The threshold lives in booking.StuckAfter, shared with the
	// sync-reconcile-on-retry path, so the two can never disagree.
	rows, err := pool.Query(ctx,
		`SELECT id, idempotency_key, ticket_tier_id, total_cents, currency
		 FROM booking
		 WHERE status = 'held' AND created_at < NOW() - make_interval(secs => $1)
		 ORDER BY created_at
		 LIMIT $2`, booking.StuckAfter.Seconds(), ReconcileBatch)
	if err != nil {
		return fmt.Errorf("select stuck holds: %w", err)
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.idemKey, &c.tierID, &c.totalCents, &c.currency); err != nil {
			rows.Close()
			return fmt.Errorf("scan candidate: %w", err)
		}
		candidates = append(candidates, c)
	}
	rows.Close()
	if rows.Err() != nil {
		return fmt.Errorf("iterate candidates: %w", rows.Err())
	}

	// One bad booking must never abort the batch: count and carry on.
	failures := 0
	for _, c := range candidates {
		if err := reconcileOne(ctx, rdb, pool, pc, c); err != nil {
			failures++
			slog.Error("reconcile booking failed", "booking_id", c.id, "error", err)
		}
	}
	if failures > 0 {
		return fmt.Errorf("reconcile: %d of %d bookings failed", failures, len(candidates))
	}
	return nil
}

// reconcileOne resolves a single booking. The decision logic lives in
// booking.ReconcileBooking, shared with the admin manual-reconcile endpoint;
// the worker only adds logging and the deferral policy.
func reconcileOne(ctx context.Context, rdb *redis.Client, pool *pgxpool.Pool, pc *booking.PaymentClient, c candidate) error {
	res, err := booking.ReconcileBooking(ctx, pool, rdb, pc,
		booking.Stuck{
			Held:    booking.Held{BookingID: c.id, TotalCents: c.totalCents, Currency: c.currency},
			IdemKey: c.idemKey,
			TierID:  c.tierID,
		},
		booking.Actor{Type: "worker", Source: "reconciliation"})
	if err != nil {
		return err
	}

	switch {
	case res.Provider == booking.ProviderUnreachable:
		// Leave it held and try again next cycle. No audit row: a deferral is
		// not a state change, and a long provider outage would write one row
		// per booking per cycle for zero information.
		slog.Warn("reconcile deferred: provider did not answer", "booking_id", c.id)
	case !res.Applied:
		// The expiry worker, the sync path, or a sibling instance got there
		// first. Never an error.
		slog.Debug("reconcile skipped: booking no longer held",
			"booking_id", c.id, "status", res.Status)
	default:
		slog.Info("reconciled stuck booking",
			"booking_id", c.id, "provider", res.Provider, "status", res.Status)
	}
	return nil
}
