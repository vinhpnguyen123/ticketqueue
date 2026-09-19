// Package booking implements TX1 of the purchase flow: the Redis inventory
// fast path, the SKIP LOCKED ticket claim, and the Layer-3 guarded
// assignment that makes double-selling structurally impossible.
package booking

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/vinhpnguyen123/ticketqueue/internal/metrics"
)

// ErrSoldOut means the tier has fewer available tickets than requested.
var ErrSoldOut = errors.New("sold out")

// InventoryKey is the Redis fast-path counter for a tier's available tickets.
func InventoryKey(tierID string) string { return "inventory:" + tierID }

// Layer 1: atomic check-and-decrement in one round trip. Returns 1 on
// success, 0 when short, -1 when the key doesn't exist yet.
var reserveScript = redis.NewScript(`
local v = redis.call('GET', KEYS[1])
if not v then return -1 end
if tonumber(v) >= tonumber(ARGV[1]) then
    redis.call('DECRBY', KEYS[1], ARGV[1])
    return 1
end
return 0
`)

// ReserveInventory runs the Layer-1 fast reject. Returns ErrSoldOut when the
// counter says no; nil when qty was reserved (DECRBYed).
func ReserveInventory(ctx context.Context, rdb *redis.Client, pool *pgxpool.Pool, tierID string, qty int) error {
	for attempt := 0; attempt < 2; attempt++ {
		res, err := reserveScript.Run(ctx, rdb, []string{InventoryKey(tierID)}, qty).Int()
		if err != nil {
			return fmt.Errorf("inventory script: %w", err)
		}
		switch res {
		case 1:
			return nil
		case 0:
			metrics.RedisLuaReject.Inc()
			return ErrSoldOut
		}
		// Key missing: lazy init from Postgres (the truth), then retry once.
		// SETNX so a concurrent initializer can't clobber a counter that
		// other requests have already decremented.
		var available int
		if err := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM tickets WHERE ticket_tier_id = $1 AND status = 'available'`,
			tierID,
		).Scan(&available); err != nil {
			return fmt.Errorf("count available: %w", err)
		}
		if err := rdb.SetNX(ctx, InventoryKey(tierID), available, 0).Err(); err != nil {
			return fmt.Errorf("init inventory key: %w", err)
		}
	}
	return fmt.Errorf("inventory key vanished twice for tier %s", tierID)
}

// CompensateInventory adds qty back to the fast-path counter. Always call
// AFTER the DB transaction has rolled back or committed, never inside it.
// If this increment is lost to a crash, the drift checker heals it —
// that window is accepted.
func CompensateInventory(ctx context.Context, rdb *redis.Client, tierID string, qty int) error {
	return rdb.IncrBy(ctx, InventoryKey(tierID), int64(qty)).Err()
}

// Tier is the pricing snapshot TX1 needs.
type Tier struct {
	ID         string
	EventID    string
	PriceCents int64
	Currency   string
}

// LoadTier fetches a tier; pgx.ErrNoRows passes through for the handler.
func LoadTier(ctx context.Context, pool *pgxpool.Pool, tierID string) (Tier, error) {
	var t Tier
	err := pool.QueryRow(ctx,
		`SELECT id, event_id, price_cents, currency FROM ticket_tier WHERE id = $1`,
		tierID,
	).Scan(&t.ID, &t.EventID, &t.PriceCents, &t.Currency)
	return t, err
}

// Held is the outcome of a successful TX1.
type Held struct {
	BookingID  string
	TotalCents int64
	Currency   string
}

// HoldTickets is TX1: claim qty tickets and create a held booking, in one
// transaction. On ErrSoldOut or any other error the transaction is rolled
// back and the caller must compensate Redis.
func HoldTickets(ctx context.Context, pool *pgxpool.Pool, userID string, tier Tier, qty int, idemKey, requestID string) (Held, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return Held{}, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	// Layer 2: lock qty available ticket rows, skipping rows other
	// purchases hold locks on — concurrent buyers never queue on each
	// other's rows, they just grab different ones.
	rows, err := tx.Query(ctx,
		`SELECT id FROM tickets
		 WHERE ticket_tier_id = $1 AND status = 'available'
		 ORDER BY id LIMIT $2
		 FOR UPDATE SKIP LOCKED`,
		tier.ID, qty)
	if err != nil {
		return Held{}, fmt.Errorf("claim select: %w", err)
	}
	var ticketIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return Held{}, fmt.Errorf("scan ticket: %w", err)
		}
		ticketIDs = append(ticketIDs, id)
	}
	rows.Close()
	if rows.Err() != nil {
		return Held{}, fmt.Errorf("iterate tickets: %w", rows.Err())
	}
	// Recorded even on the short path below: a claim that came up short is
	// exactly what this histogram exists to show.
	metrics.TicketClaimRows.Observe(float64(len(ticketIDs)))
	if len(ticketIDs) < qty {
		// The Lua fast path raced ahead of truth (its counter said yes, the
		// rows say no). Postgres is truth: reject, and let the caller INCRBY
		// the counter back.
		return Held{}, ErrSoldOut
	}

	totalCents := int64(qty) * tier.PriceCents
	var bookingID string
	// hold_expires_at is 10 minutes: deliberately much longer than the 30s
	// reconciliation cycle, so reconciliation always resolves a payment
	// before the hold releases the tickets (Problem 12 Defense 4).
	if err := tx.QueryRow(ctx,
		`INSERT INTO booking (user_id, event_id, ticket_tier_id, quantity, idempotency_key,
		                      status, total_cents, currency, hold_expires_at)
		 VALUES ($1, $2, $3, $4, $5, 'held', $6, $7, NOW() + INTERVAL '10 minutes')
		 RETURNING id`,
		userID, tier.EventID, tier.ID, qty, idemKey, totalCents, tier.Currency,
	).Scan(&bookingID); err != nil {
		return Held{}, fmt.Errorf("insert booking: %w", err)
	}

	// Layer 3: the guarded assignment. Even though the rows are locked, the
	// WHERE re-asserts available+unassigned, and the RowsAffected check makes
	// a partial assignment impossible to commit.
	tag, err := tx.Exec(ctx,
		`UPDATE tickets SET status = 'held', booking_id = $1
		 WHERE id = ANY($2) AND status = 'available' AND booking_id IS NULL`,
		bookingID, ticketIDs)
	if err != nil {
		return Held{}, fmt.Errorf("assign tickets: %w", err)
	}
	if int(tag.RowsAffected()) != qty {
		return Held{}, fmt.Errorf("layer-3 guard: assigned %d of %d tickets", tag.RowsAffected(), qty)
	}

	for _, action := range []string{"booking_created", "tickets_held"} {
		md, err := jsonbParam(map[string]any{"tier_id": tier.ID, "quantity": qty, "total_cents": totalCents})
		if err != nil {
			return Held{}, fmt.Errorf("audit %s: %w", action, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO audit_log (entity_type, entity_id, action, actor_type, actor_user_id, metadata, request_id)
			 VALUES ('booking', $1, $2, 'user', $3, $4::jsonb, $5)`,
			bookingID, action, userID,
			md,
			requestID); err != nil {
			return Held{}, fmt.Errorf("audit %s: %w", action, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return Held{}, fmt.Errorf("commit: %w", err)
	}
	return Held{BookingID: bookingID, TotalCents: totalCents, Currency: tier.Currency}, nil
}

// ActiveBookingID returns the user's existing held booking, if any
// (duplicate-charge guard; uses idx_booking_user_active).
func ActiveBookingID(ctx context.Context, pool *pgxpool.Pool, userID string) (string, bool, error) {
	var id string
	err := pool.QueryRow(ctx,
		`SELECT id FROM booking WHERE user_id = $1 AND status = 'held' LIMIT 1`,
		userID,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return id, true, nil
}
