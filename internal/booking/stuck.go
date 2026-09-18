package booking

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Stuck-booking categories. These are the operator's two kinds of trouble, and
// they are NOT equivalent: one freezes inventory, the other owes someone money.
const (
	// CategoryHeldTooLong: still 'held' past the threshold. Reconciliation has
	// not resolved it yet; inventory is frozen but no money is at stake.
	CategoryHeldTooLong = "held_too_long"
	// CategoryChargedNotDelivered: Outcome D (§8). The money moved but the
	// booking is not confirmed and its tickets may already be resold.
	CategoryChargedNotDelivered = "charged_not_delivered"
)

// The two definitions of "stuck", written once. The admin endpoint lists these
// rows and the worker gauge counts them; sharing the predicates is what stops
// the dashboard and the API from disagreeing about what is wrong.
const (
	predicateHeldTooLong = `b.status = 'held' AND b.created_at < NOW() - make_interval(secs => $1)`

	predicateChargedNotDelivered = `b.status <> 'confirmed'
		AND EXISTS (SELECT 1 FROM audit_log a
		            WHERE a.entity_type = 'booking' AND a.entity_id = b.id
		              AND a.action = 'reconciliation_needed')`
)

// StuckBooking is one row of the admin stuck list.
type StuckBooking struct {
	ID               string
	Category         string
	UserID           string
	Status           string
	Quantity         int
	TotalCents       int64
	Currency         string
	CreatedAt        time.Time
	HoldExpiresAt    time.Time
	AgeSeconds       int
	PaymentStatus    string
	FlaggedAt        *time.Time
	ResolutionSource *string
}

// ListStuck returns both categories in one list, charged_not_delivered first
// because money outranks frozen inventory, then oldest-first within each.
//
// payment_status follows the §6 reading rule: the most recent payments row that
// is not 'unknown'. A reconciled booking keeps its 'unknown' row as history, so
// reading "the latest row" would report every healed booking as unknown.
//
// resolution_source is which path last reconciled the booking; 'purchase' is
// excluded because that is the synchronous path, not a reconcile.
func ListStuck(ctx context.Context, pool *pgxpool.Pool, olderThan time.Duration, limit int) ([]StuckBooking, error) {
	rows, err := pool.Query(ctx,
		`SELECT x.id, x.category, x.user_id, x.status, x.quantity, x.total_cents, x.currency,
		        x.created_at, x.hold_expires_at, x.flagged_at,
		        EXTRACT(EPOCH FROM (NOW() - x.created_at))::int AS age_seconds,
		        COALESCE((SELECT p.status FROM payments p
		                  WHERE p.booking_id = x.id AND p.status <> 'unknown'
		                  ORDER BY p.created_at DESC LIMIT 1), 'unknown') AS payment_status,
		        (SELECT a.metadata->>'source' FROM audit_log a
		         WHERE a.entity_type = 'booking' AND a.entity_id = x.id
		           AND a.metadata->>'source' IN ('reconciliation', 'hold_expiry', 'sync_retry', 'admin_manual')
		         ORDER BY a.created_at DESC LIMIT 1) AS resolution_source
		 FROM (`+stuckUnion+`) x
		 ORDER BY (x.category = '`+CategoryChargedNotDelivered+`') DESC, x.created_at ASC
		 LIMIT $2`, olderThan.Seconds(), limit)
	if err != nil {
		return nil, fmt.Errorf("list stuck bookings: %w", err)
	}
	defer rows.Close()

	out := []StuckBooking{}
	for rows.Next() {
		var b StuckBooking
		if err := rows.Scan(&b.ID, &b.Category, &b.UserID, &b.Status, &b.Quantity,
			&b.TotalCents, &b.Currency, &b.CreatedAt, &b.HoldExpiresAt, &b.FlaggedAt,
			&b.AgeSeconds, &b.PaymentStatus, &b.ResolutionSource); err != nil {
			return nil, fmt.Errorf("scan stuck booking: %w", err)
		}
		out = append(out, b)
	}
	if rows.Err() != nil {
		return nil, fmt.Errorf("iterate stuck bookings: %w", rows.Err())
	}
	return out, nil
}

// CountStuck returns how many bookings are in each category, for the gauge. It
// reuses the same predicates as ListStuck, so the dashboard and the admin API
// can never tell the operator different things.
func CountStuck(ctx context.Context, pool *pgxpool.Pool, olderThan time.Duration) (map[string]int, error) {
	rows, err := pool.Query(ctx,
		`SELECT category, COUNT(*) FROM (`+stuckUnion+`) x GROUP BY category`,
		olderThan.Seconds())
	if err != nil {
		return nil, fmt.Errorf("count stuck bookings: %w", err)
	}
	defer rows.Close()

	// Both categories always reported, so a gauge that dropped to zero shows as
	// zero rather than as a stale series.
	out := map[string]int{CategoryHeldTooLong: 0, CategoryChargedNotDelivered: 0}
	for rows.Next() {
		var category string
		var n int
		if err := rows.Scan(&category, &n); err != nil {
			return nil, fmt.Errorf("scan stuck count: %w", err)
		}
		out[category] = n
	}
	if rows.Err() != nil {
		return nil, fmt.Errorf("iterate stuck counts: %w", rows.Err())
	}
	return out, nil
}

// stuckUnion is the shared FROM body: $1 is the held-too-long threshold in
// seconds. Both categories select the same columns so callers can union them.
const stuckUnion = `
	SELECT b.id, b.user_id, b.status, b.quantity, b.total_cents, b.currency,
	       b.created_at, b.hold_expires_at,
	       '` + CategoryHeldTooLong + `' AS category, NULL::timestamptz AS flagged_at
	FROM booking b
	WHERE ` + predicateHeldTooLong + `
	UNION ALL
	SELECT b.id, b.user_id, b.status, b.quantity, b.total_cents, b.currency,
	       b.created_at, b.hold_expires_at,
	       '` + CategoryChargedNotDelivered + `',
	       (SELECT MIN(a.created_at) FROM audit_log a
	        WHERE a.entity_type = 'booking' AND a.entity_id = b.id
	          AND a.action = 'reconciliation_needed')
	FROM booking b
	WHERE ` + predicateChargedNotDelivered

// BookingsHeldCount is the raw count of bookings sitting in 'held', for the
// bookings_held_current gauge.
func BookingsHeldCount(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM booking WHERE status = 'held'`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count held bookings: %w", err)
	}
	return n, nil
}
