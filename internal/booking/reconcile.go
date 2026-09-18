package booking

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// StuckAfter is how old a 'held' booking must be before anything treats it as
// stuck and asks the provider about it. It is comfortably past the 3s payment
// timeout, so the synchronous purchase path has definitely finished with the
// booking. Distinct from hold_expires_at (10 minutes), which is the give-up
// deadline. Every caller reads it from here — the reconciliation worker's scan
// and the sync-reconcile-on-retry path — so the threshold lives in one place.
const StuckAfter = 90 * time.Second

// Provider results, as reported to callers and to the admin API.
const (
	ProviderSucceeded   = "succeeded"
	ProviderFailed      = "failed"
	ProviderNotFound    = "not_found"
	ProviderUnreachable = "unreachable"
)

// Stuck is a booking left 'held' that needs resolving against the provider.
type Stuck struct {
	Held
	IdemKey string
	TierID  string
}

// Resolution is the outcome of one reconcile attempt.
type Resolution struct {
	// Provider is what the provider said: succeeded | failed | not_found |
	// unreachable. Unreachable means we learned nothing and changed nothing.
	Provider string
	// Status is the booking's status afterwards.
	Status string
	// Applied is false when the guarded UPDATE lost — something else (the
	// expiry worker, a sibling reconcile, the sync path) resolved the booking
	// first. Not an error; the caller decides whether it cares.
	Applied bool
	// ReleasedQty is how many tickets went back to available. Redis has
	// already been compensated for these.
	ReleasedQty int
}

// ReconcileBooking is THE resolver: it asks the provider what happened under a
// booking's idempotency key and finalizes accordingly. The reconciliation
// worker, the admin manual-reconcile endpoint, and (later) sync-reconcile-on-
// retry all call this — the money path exists once.
//
// It never holds a transaction across the provider call: lookup happens with
// nothing open, then a fresh short transaction finalizes, then Redis is
// compensated after commit. When the provider cannot be reached it does
// nothing at all and says so — never guess (§3, the money rule).
func ReconcileBooking(ctx context.Context, pool *pgxpool.Pool, rdb *redis.Client, pc *PaymentClient, s Stuck, a Actor) (Resolution, error) {
	// NO transaction is open here. This is the network call.
	res := pc.Lookup(ctx, s.IdemKey)

	out := Resolution{Status: "held"}
	var err error

	switch res.Outcome {
	case Ambiguous:
		// We could not ask. Leave everything exactly as it is.
		return Resolution{Provider: ProviderUnreachable, Status: "held"}, nil

	case Succeeded:
		out.Provider = ProviderSucceeded
		err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			// The tickets are still held by THIS booking, so confirming is
			// safe; Redis is not touched — TX1's decrement still stands.
			_, applied, err := ConfirmBooking(ctx, tx, s.Held, res, a)
			out.Applied = applied
			if applied {
				out.Status = "confirmed"
			}
			return err
		})

	case Declined:
		out.Provider = ProviderFailed
		err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			qty, applied, err := FailBooking(ctx, tx, s.Held, "card_declined", &res, a)
			out.Applied, out.ReleasedQty = applied, qty
			if applied {
				out.Status = "failed"
			}
			return err
		})

	case NotFound:
		// The provider has no record under this key: no money moved. Failing
		// is safe, and no payments row is written — there was no attempt.
		out.Provider = ProviderNotFound
		err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			qty, applied, err := FailBooking(ctx, tx, s.Held, "no_charge_found", nil, a)
			out.Applied, out.ReleasedQty = applied, qty
			if applied {
				out.Status = "failed"
			}
			return err
		})

	default:
		return Resolution{}, fmt.Errorf("reconcile: unknown lookup outcome %d", res.Outcome)
	}
	if err != nil {
		return Resolution{}, err
	}

	// Redis compensation AFTER commit, never inside the transaction. Lost to a
	// crash here → the drift checker heals it; accepted window.
	if out.ReleasedQty > 0 {
		if cerr := CompensateInventory(ctx, rdb, s.TierID, out.ReleasedQty); cerr != nil {
			return out, fmt.Errorf("inventory compensation (drift checker will heal): %w", cerr)
		}
	}

	if !out.Applied {
		// Someone else resolved it first; report what they left behind rather
		// than guessing.
		if err := pool.QueryRow(ctx,
			`SELECT status FROM booking WHERE id = $1`, s.BookingID).Scan(&out.Status); err != nil {
			return out, fmt.Errorf("re-read booking status: %w", err)
		}
	}
	return out, nil
}

// LoadStuck fetches everything ReconcileBooking needs for one booking, plus its
// current status so the caller can reject a booking that is no longer held.
// pgx.ErrNoRows passes through for the handler to map to 404.
func LoadStuck(ctx context.Context, pool *pgxpool.Pool, bookingID string) (Stuck, string, error) {
	var s Stuck
	var status string
	err := pool.QueryRow(ctx,
		`SELECT id, idempotency_key, ticket_tier_id, total_cents, currency, status
		 FROM booking WHERE id = $1`, bookingID,
	).Scan(&s.BookingID, &s.IdemKey, &s.TierID, &s.TotalCents, &s.Currency, &status)
	return s, status, err
}
