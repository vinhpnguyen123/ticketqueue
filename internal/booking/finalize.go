package booking

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Finalized is TX2's outcome, mapped by the handler to an HTTP response.
type Finalized struct {
	// Status is one of:
	//   "confirmed"            — money moved, tickets sold      → 201
	//   "failed"               — card declined, tickets released → 402
	//   "held_unknown"         — ambiguous, booking left held    → 504
	//   "expired_after_charge" — money moved but expiry won      → 409
	Status  string
	Tickets []string
	// ReleaseQty is how many tickets TX2 released back; the handler runs the
	// Redis INCRBY compensation for exactly this many AFTER commit. Zero when
	// nothing was released (confirmed, ambiguous, or the expiry-won race where
	// the expiry worker already compensated).
	ReleaseQty int
}

// Finalize is PHASE 3 / TX2: it turns a classified ChargeResult into a durable
// final state. Each branch runs in a single transaction; the caller performs
// Redis compensation (never inside the tx). The guarded booking UPDATE
// (held→X, assert RowsAffected) is what makes the payment-vs-expiry race safe:
// zero rows means the hold-expiration worker already won.
func Finalize(ctx context.Context, pool *pgxpool.Pool, held Held, tierID, userID, requestID string, r ChargeResult) (Finalized, error) {
	switch r.Outcome {
	case Ambiguous:
		return recordUnknown(ctx, pool, held, requestID)
	case Declined:
		return finalizeDeclined(ctx, pool, held, r, userID, requestID)
	case Succeeded:
		return finalizeConfirmed(ctx, pool, held, r, userID, requestID)
	default:
		return Finalized{}, fmt.Errorf("finalize: unknown outcome %d", r.Outcome)
	}
}

// providerResp returns the raw provider body as a jsonb-castable value, or nil
// for NULL when the provider never answered.
func providerResp(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}

// recordUnknown handles the ambiguous case: booking stays 'held' as durable
// evidence, a 'unknown' payments row records the attempt, and we NEVER guess.
// The reconciliation worker resolves it by querying the provider by key.
func recordUnknown(ctx context.Context, pool *pgxpool.Pool, held Held, requestID string) (Finalized, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return Finalized{}, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`INSERT INTO payments (booking_id, provider, provider_txn_id, status, amount_cents, currency)
		 VALUES ($1, 'simulated', NULL, 'unknown', $2, $3)`,
		held.BookingID, held.TotalCents, held.Currency); err != nil {
		return Finalized{}, fmt.Errorf("insert unknown payment: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO audit_log (entity_type, entity_id, action, actor_type, request_id)
		 VALUES ('booking', $1, 'payment_outcome_unknown', 'system', $2)`,
		held.BookingID, requestID); err != nil {
		return Finalized{}, fmt.Errorf("audit unknown: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Finalized{}, fmt.Errorf("commit: %w", err)
	}
	return Finalized{Status: "held_unknown"}, nil
}

// finalizeDeclined handles a hard provider decline: fail the booking, release
// the held tickets, record the failed payment. If the guarded UPDATE loses to
// the expiry worker, the booking is already failed and tickets already
// released — we only record the payment for the audit trail.
func finalizeDeclined(ctx context.Context, pool *pgxpool.Pool, held Held, r ChargeResult, userID, requestID string) (Finalized, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return Finalized{}, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx,
		`UPDATE booking SET status = 'failed', failed_at = NOW(), version = version + 1
		 WHERE id = $1 AND status = 'held'`, held.BookingID)
	if err != nil {
		return Finalized{}, fmt.Errorf("fail booking: %w", err)
	}

	released := 0
	if tag.RowsAffected() == 1 {
		// We won: release the still-held tickets back to available.
		rt, err := tx.Exec(ctx,
			`UPDATE tickets SET status = 'available', booking_id = NULL
			 WHERE booking_id = $1 AND status = 'held'`, held.BookingID)
		if err != nil {
			return Finalized{}, fmt.Errorf("release tickets: %w", err)
		}
		released = int(rt.RowsAffected())
		if err := auditPair(ctx, tx, held.BookingID, userID, requestID,
			"booking_failed", "payment_declined"); err != nil {
			return Finalized{}, err
		}
	}
	// else: expiry worker already failed + released + compensated Redis;
	// record the decline for the trail only, no ticket/Redis action.

	if _, err := tx.Exec(ctx,
		`INSERT INTO payments (booking_id, provider, provider_txn_id, status, amount_cents, currency, provider_response)
		 VALUES ($1, 'simulated', $2, 'failed', $3, $4, $5::jsonb)`,
		held.BookingID, nullStr(r.TxnID), held.TotalCents, held.Currency, providerResp(r.Raw)); err != nil {
		return Finalized{}, fmt.Errorf("insert failed payment: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Finalized{}, fmt.Errorf("commit: %w", err)
	}
	return Finalized{Status: "failed", ReleaseQty: released}, nil
}

// finalizeConfirmed handles the happy path: confirm the booking, sell the
// tickets, record the succeeded payment. If the guarded UPDATE loses to the
// expiry worker, money moved but the hold already expired — the
// money-moved-but-hold-expired edge: record the succeeded payment anyway and
// flag it for human/worker refund via a 'reconciliation_needed' audit row.
func finalizeConfirmed(ctx context.Context, pool *pgxpool.Pool, held Held, r ChargeResult, userID, requestID string) (Finalized, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return Finalized{}, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx,
		`UPDATE booking SET status = 'confirmed', confirmed_at = NOW(), version = version + 1
		 WHERE id = $1 AND status = 'held'`, held.BookingID)
	if err != nil {
		return Finalized{}, fmt.Errorf("confirm booking: %w", err)
	}

	if tag.RowsAffected() == 0 {
		// The expiry worker won and released the tickets, but the charge
		// succeeded. hold_expires_at is 10min precisely to make this rare.
		if err := insertSucceededPayment(ctx, tx, held, r); err != nil {
			return Finalized{}, err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO audit_log (entity_type, entity_id, action, actor_type, metadata, request_id)
			 VALUES ('booking', $1, 'reconciliation_needed', 'system', $2, $3)`,
			held.BookingID,
			map[string]any{"reason": "charge_succeeded_after_hold_expired", "provider_txn_id": r.TxnID},
			requestID); err != nil {
			return Finalized{}, fmt.Errorf("audit reconciliation_needed: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return Finalized{}, fmt.Errorf("commit: %w", err)
		}
		return Finalized{Status: "expired_after_charge"}, nil
	}

	rows, err := tx.Query(ctx,
		`UPDATE tickets SET status = 'sold'
		 WHERE booking_id = $1 AND status = 'held' RETURNING id`, held.BookingID)
	if err != nil {
		return Finalized{}, fmt.Errorf("sell tickets: %w", err)
	}
	var ticketIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return Finalized{}, fmt.Errorf("scan sold ticket: %w", err)
		}
		ticketIDs = append(ticketIDs, id)
	}
	rows.Close()
	if rows.Err() != nil {
		return Finalized{}, fmt.Errorf("iterate sold tickets: %w", rows.Err())
	}

	if err := insertSucceededPayment(ctx, tx, held, r); err != nil {
		return Finalized{}, err
	}
	if err := auditPair(ctx, tx, held.BookingID, userID, requestID,
		"booking_confirmed", "payment_succeeded"); err != nil {
		return Finalized{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Finalized{}, fmt.Errorf("commit: %w", err)
	}
	return Finalized{Status: "confirmed", Tickets: ticketIDs}, nil
}

func insertSucceededPayment(ctx context.Context, tx pgx.Tx, held Held, r ChargeResult) error {
	if _, err := tx.Exec(ctx,
		`INSERT INTO payments (booking_id, provider, provider_txn_id, status, amount_cents, currency,
		                       card_brand, card_last4, provider_response)
		 VALUES ($1, 'simulated', $2, 'succeeded', $3, $4, $5, $6, $7::jsonb)`,
		held.BookingID, nullStr(r.TxnID), held.TotalCents, held.Currency,
		nullStr(r.CardBrand), nullStr(r.CardLast4), providerResp(r.Raw)); err != nil {
		return fmt.Errorf("insert succeeded payment: %w", err)
	}
	return nil
}

func auditPair(ctx context.Context, tx pgx.Tx, bookingID, userID, requestID, a1, a2 string) error {
	for _, action := range []string{a1, a2} {
		if _, err := tx.Exec(ctx,
			`INSERT INTO audit_log (entity_type, entity_id, action, actor_type, actor_user_id, request_id)
			 VALUES ('booking', $1, $2, 'user', $3, $4)`,
			bookingID, action, userID, requestID); err != nil {
			return fmt.Errorf("audit %s: %w", action, err)
		}
	}
	return nil
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
