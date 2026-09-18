package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/vinhpnguyen123/ticketqueue/internal/booking"
)

// HoldExpiryInterval is how often expired holds are resolved.
const HoldExpiryInterval = 5 * time.Second

// HoldExpiryBatch caps one cycle so a backlog cannot produce an unbounded run
// of provider calls.
const HoldExpiryBatch = 50

// deferredAuditAction marks, once per booking, that expiry was deferred
// because the provider could not be reached.
const deferredAuditAction = "expiry_deferred_provider_unreachable"

// HoldExpiryCycle resolves bookings whose hold has expired. It does NOT decide
// anything about money on its own: it asks the provider, through the same
// booking.ReconcileBooking every other path uses, and acts on the answer.
//
// THE TRADE-OFF, stated plainly: during a long provider outage tickets stay
// frozen in 'held' past the 10-minute deadline, because we refuse to release a
// hold we cannot prove is unpaid. Frozen inventory costs a few unsold seats for
// a while and heals itself the moment the provider answers. Charging a customer
// and giving their seat to someone else is real customer harm and a dispute.
// When you must pick a failure mode, pick the recoverable one (§3, money rule).
//
// Before rev. 3 this worker failed and released every expired hold without
// asking. That was the §17 gap: a charge that landed during a provider outage,
// or a crash between TX1 and TX2, meant tickets were resold out from under a
// paying customer — and reconciliation could not repair it, because the booking
// was no longer 'held'. Note that a payments row is NOT a usable signal here:
// in the crash-between-TX1-and-TX2 case no payments row exists at all. Only the
// provider knows.
func HoldExpiryCycle(ctx context.Context, rdb *redis.Client, pool *pgxpool.Pool, pc *booking.PaymentClient) error {
	// PLAIN READ, no FOR UPDATE: this worker now calls the provider, and §9's
	// locking rule forbids holding a lock across that call. Uses
	// idx_booking_reconciliation ON booking(hold_expires_at) WHERE status='held'
	// — this worker is what that index was built for.
	rows, err := pool.Query(ctx,
		`SELECT id, idempotency_key, ticket_tier_id, total_cents, currency
		 FROM booking
		 WHERE status = 'held' AND hold_expires_at < NOW()
		 ORDER BY hold_expires_at
		 LIMIT $1`, HoldExpiryBatch)
	if err != nil {
		return fmt.Errorf("select expired holds: %w", err)
	}
	var expired []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.idemKey, &c.tierID, &c.totalCents, &c.currency); err != nil {
			rows.Close()
			return fmt.Errorf("scan expired hold: %w", err)
		}
		expired = append(expired, c)
	}
	rows.Close()
	if rows.Err() != nil {
		return fmt.Errorf("iterate expired holds: %w", rows.Err())
	}

	// One bad booking must never abort the batch.
	failures := 0
	for _, c := range expired {
		if err := expireOne(ctx, rdb, pool, pc, c); err != nil {
			failures++
			slog.Error("hold expiry failed", "booking_id", c.id, "error", err)
		}
	}
	if failures > 0 {
		return fmt.Errorf("hold expiry: %d of %d bookings failed", failures, len(expired))
	}
	return nil
}

func expireOne(ctx context.Context, rdb *redis.Client, pool *pgxpool.Pool, pc *booking.PaymentClient, c candidate) error {
	res, err := booking.ReconcileBooking(ctx, pool, rdb, pc,
		booking.Stuck{
			Held:    booking.Held{BookingID: c.id, TotalCents: c.totalCents, Currency: c.currency},
			IdemKey: c.idemKey,
			TierID:  c.tierID,
		},
		booking.Actor{Type: "worker", Source: "hold_expiry"})
	if err != nil {
		return err
	}

	switch {
	case res.Provider == booking.ProviderUnreachable:
		// Never release on a guess. The hold stays, the tickets stay.
		slog.Warn("hold expiry deferred: provider did not answer, tickets stay held",
			"booking_id", c.id)
		return recordDeferral(ctx, pool, c.id)
	case !res.Applied:
		// Reconciliation, the sync path, or a sibling instance got there first.
		slog.Debug("hold expiry skipped: booking no longer held",
			"booking_id", c.id, "status", res.Status)
	default:
		slog.Info("expired hold resolved",
			"booking_id", c.id, "provider", res.Provider, "status", res.Status,
			"tickets_released", res.ReleasedQty)
	}
	return nil
}

// recordDeferral writes the deferral marker at most once per booking. A 5s
// cycle against a 10-minute outage would otherwise write 120 identical rows per
// stuck booking. The slog.Warn above fires every cycle — that is what tells you
// the outage is ongoing; this row only marks that the booking was affected.
//
// ponytail: read-then-insert, so two worker instances racing can write two rows.
// Harmless duplication in an audit table; a unique partial index would be the
// upgrade if it ever matters.
func recordDeferral(ctx context.Context, pool *pgxpool.Pool, bookingID string) error {
	var exists int
	err := pool.QueryRow(ctx,
		`SELECT 1 FROM audit_log
		 WHERE entity_type = 'booking' AND entity_id = $1 AND action = $2
		 LIMIT 1`, bookingID, deferredAuditAction).Scan(&exists)
	if err == nil {
		return nil // already marked
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("check deferral audit: %w", err)
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO audit_log (entity_type, entity_id, action, actor_type, metadata)
		 VALUES ('booking', $1, $2, 'worker', $3)`,
		bookingID, deferredAuditAction,
		map[string]any{"source": "hold_expiry", "reason": "provider_unreachable"}); err != nil {
		return fmt.Errorf("audit %s: %w", deferredAuditAction, err)
	}
	return nil
}
