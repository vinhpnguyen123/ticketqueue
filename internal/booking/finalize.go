package booking

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vinhpnguyen123/ticketqueue/internal/db"
)

// Actor is who caused a state change, for audit_log. Type is one of
// 'user' | 'worker' | 'system' | 'admin'; UserID and RequestID are written as
// NULL when empty (a worker has neither).
//
// Source names the mechanism, which actor_type alone cannot: two workers both
// write actor_type 'worker', so without it the trail cannot say whether a
// booking was failed by hold expiry or by reconciliation. It lands in the
// audit row's metadata as "source".
type Actor struct {
	Type      string
	UserID    string
	RequestID string
	Source    string
}

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

// ConfirmBooking is the shared confirm path: TX2, the reconciliation worker,
// and (later) admin manual reconcile all go through it, so "money moved" is
// written to the database in exactly one place.
//
// The guarded UPDATE is the whole safety story. applied=false means the
// booking was no longer 'held' — the expiry worker or a sibling reconcile
// already resolved it — and NOTHING further is written, because the caller
// decides what that means: TX2 records a reconciliation_needed payment, the
// worker just skips.
//
// Caller supplies the transaction; this never commits and never touches Redis.
func ConfirmBooking(ctx context.Context, tx pgx.Tx, held Held, r ChargeResult, a Actor) (ticketIDs []string, applied bool, err error) {
	tag, err := tx.Exec(ctx,
		`UPDATE booking SET status = 'confirmed', confirmed_at = NOW(), version = version + 1
		 WHERE id = $1 AND status = 'held'`, held.BookingID)
	if err != nil {
		return nil, false, fmt.Errorf("confirm booking: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, false, nil
	}

	// Only tickets this booking still holds; a ticket already flipped elsewhere
	// is not ours to sell.
	rows, err := tx.Query(ctx,
		`UPDATE tickets SET status = 'sold'
		 WHERE booking_id = $1 AND status = 'held' RETURNING id`, held.BookingID)
	if err != nil {
		return nil, false, fmt.Errorf("sell tickets: %w", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, false, fmt.Errorf("scan sold ticket: %w", err)
		}
		ticketIDs = append(ticketIDs, id)
	}
	rows.Close()
	if rows.Err() != nil {
		return nil, false, fmt.Errorf("iterate sold tickets: %w", rows.Err())
	}

	if err := insertSucceededPayment(ctx, tx, held, r); err != nil {
		return nil, false, err
	}
	for _, action := range []string{"booking_confirmed", "payment_succeeded"} {
		if err := audit(ctx, tx, held.BookingID, action, a, nil); err != nil {
			return nil, false, err
		}
	}
	return ticketIDs, true, nil
}

// FailBooking is the shared fail path: release the hold, record the attempt.
// Like ConfirmBooking it only REPORTS a lost guard (applied=false) and writes
// nothing in that case.
//
// r is the provider's answer, or nil when no charge ever reached the provider
// (a 404 by-key lookup). nil → no payments row: a payments row must represent a
// real attempt that reached the provider.
//
// releasedQty is what the caller must INCRBY back into Redis AFTER commit.
func FailBooking(ctx context.Context, tx pgx.Tx, held Held, reason string, r *ChargeResult, a Actor) (releasedQty int, applied bool, err error) {
	tag, err := tx.Exec(ctx,
		`UPDATE booking SET status = 'failed', failed_at = NOW(), version = version + 1
		 WHERE id = $1 AND status = 'held'`, held.BookingID)
	if err != nil {
		return 0, false, fmt.Errorf("fail booking: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return 0, false, nil
	}

	// Only 'held' tickets go back; a ticket already flipped to 'sold' belongs
	// to a paid customer and must never be released.
	rt, err := tx.Exec(ctx,
		`UPDATE tickets SET status = 'available', booking_id = NULL
		 WHERE booking_id = $1 AND status = 'held'`, held.BookingID)
	if err != nil {
		return 0, false, fmt.Errorf("release tickets: %w", err)
	}
	releasedQty = int(rt.RowsAffected())

	if r != nil {
		if err := insertFailedPayment(ctx, tx, held, *r); err != nil {
			return 0, false, err
		}
	}
	if err := audit(ctx, tx, held.BookingID, "booking_failed", a,
		map[string]any{"reason": reason, "tickets_released": releasedQty}); err != nil {
		return 0, false, err
	}
	return releasedQty, true, nil
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
// jsonbParam is the package's single door to a JSONB column. See db.JSONB for
// why every one of them must be marshalled here and cast in the SQL.
func jsonbParam(m map[string]any) (any, error) {
	return db.JSONB(m)
}

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
	if err := audit(ctx, tx, held.BookingID, "payment_outcome_unknown",
		Actor{Type: "system", RequestID: requestID, Source: "purchase"}, nil); err != nil {
		return Finalized{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Finalized{}, fmt.Errorf("commit: %w", err)
	}
	return Finalized{Status: "held_unknown"}, nil
}

// finalizeDeclined handles a hard provider decline: fail the booking, release
// the held tickets, record the failed payment.
func finalizeDeclined(ctx context.Context, pool *pgxpool.Pool, held Held, r ChargeResult, userID, requestID string) (Finalized, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return Finalized{}, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	released, applied, err := FailBooking(ctx, tx, held, "card_declined", &r,
		Actor{Type: "user", UserID: userID, RequestID: requestID, Source: "purchase"})
	if err != nil {
		return Finalized{}, err
	}
	if !applied {
		// The expiry worker already failed + released + compensated Redis.
		// Record the decline for the trail only — no ticket or Redis action.
		if err := insertFailedPayment(ctx, tx, held, r); err != nil {
			return Finalized{}, err
		}
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

	tickets, applied, err := ConfirmBooking(ctx, tx, held, r,
		Actor{Type: "user", UserID: userID, RequestID: requestID, Source: "purchase"})
	if err != nil {
		return Finalized{}, err
	}

	if !applied {
		// Someone else finalized this booking while the charge was in flight.
		// WHICH someone matters, and since hold-expiration started asking the
		// provider first (§17 fix) there are two possibilities, not one.
		var current string
		if err := tx.QueryRow(ctx,
			`SELECT status FROM booking WHERE id = $1`, held.BookingID).Scan(&current); err != nil {
			return Finalized{}, fmt.Errorf("re-read booking status: %w", err)
		}

		if current == "confirmed" {
			// A sibling asked the provider, got the same 'succeeded' answer we
			// did, and already confirmed the booking and recorded the payment.
			// Nothing to record and nothing to flag: the customer has their
			// tickets. Recording here would duplicate the payments row and
			// raise a false refund alarm.
			ids, err := soldTicketIDs(ctx, tx, held.BookingID)
			if err != nil {
				return Finalized{}, err
			}
			if err := tx.Commit(ctx); err != nil {
				return Finalized{}, fmt.Errorf("commit: %w", err)
			}
			return Finalized{Status: "confirmed", Tickets: ids}, nil
		}

		// True Outcome D: the booking is failed/cancelled, the money moved, and
		// the tickets may already be resold. Record the payment and flag it for
		// a human — this is the one case that needs a refund.
		if err := insertSucceededPayment(ctx, tx, held, r); err != nil {
			return Finalized{}, err
		}
		if err := audit(ctx, tx, held.BookingID, "reconciliation_needed",
			Actor{Type: "system", RequestID: requestID, Source: "purchase"},
			map[string]any{"reason": "charge_succeeded_after_hold_expired", "provider_txn_id": r.TxnID}); err != nil {
			return Finalized{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return Finalized{}, fmt.Errorf("commit: %w", err)
		}
		return Finalized{Status: "expired_after_charge"}, nil
	}

	if err := tx.Commit(ctx); err != nil {
		return Finalized{}, fmt.Errorf("commit: %w", err)
	}
	return Finalized{Status: "confirmed", Tickets: tickets}, nil
}

// soldTicketIDs lists the tickets a confirmed booking owns.
func soldTicketIDs(ctx context.Context, tx pgx.Tx, bookingID string) ([]string, error) {
	rows, err := tx.Query(ctx,
		`SELECT id FROM tickets WHERE booking_id = $1 AND status = 'sold' ORDER BY id`, bookingID)
	if err != nil {
		return nil, fmt.Errorf("read sold tickets: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan sold ticket: %w", err)
		}
		ids = append(ids, id)
	}
	if rows.Err() != nil {
		return nil, fmt.Errorf("iterate sold tickets: %w", rows.Err())
	}
	return ids, nil
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

func insertFailedPayment(ctx context.Context, tx pgx.Tx, held Held, r ChargeResult) error {
	if _, err := tx.Exec(ctx,
		`INSERT INTO payments (booking_id, provider, provider_txn_id, status, amount_cents, currency, provider_response)
		 VALUES ($1, 'simulated', $2, 'failed', $3, $4, $5::jsonb)`,
		held.BookingID, nullStr(r.TxnID), held.TotalCents, held.Currency, providerResp(r.Raw)); err != nil {
		return fmt.Errorf("insert failed payment: %w", err)
	}
	return nil
}

func audit(ctx context.Context, tx pgx.Tx, bookingID, action string, a Actor, metadata map[string]any) error {
	if a.Source != "" {
		if metadata == nil {
			metadata = map[string]any{}
		}
		metadata["source"] = a.Source
	}
	md, err := jsonbParam(metadata)
	if err != nil {
		return fmt.Errorf("audit %s: %w", action, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO audit_log (entity_type, entity_id, action, actor_type, actor_user_id, metadata, request_id)
		 VALUES ('booking', $1, $2, $3, $4, $5::jsonb, $6)`,
		bookingID, action, a.Type, nullStr(a.UserID), md, nullStr(a.RequestID)); err != nil {
		return fmt.Errorf("audit %s: %w", action, err)
	}
	return nil
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
