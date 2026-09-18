package booking

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound means no booking matched — including the case where one exists
// but belongs to someone else. Callers map it to 404 and never to 403: telling
// a stranger that a booking id is real is itself a leak (§11).
var ErrNotFound = errors.New("booking not found")

// Summary is one row of a user's booking list.
type Summary struct {
	ID          string
	Status      string
	Quantity    int
	TotalCents  int64
	Currency    string
	CreatedAt   time.Time
	ConfirmedAt *time.Time
	FailedAt    *time.Time
	EventID     string
	EventName   string
	EventAt     time.Time
	TierID      string
	TierName    string
}

// Ticket is one ticket on a booking, as the owner sees it.
type Ticket struct {
	ID     string
	Status string
}

// Payment is one payment attempt, as the owner sees it. Deliberately omits
// provider_response: it is a raw third-party blob (§11).
type Payment struct {
	Status    string
	TxnID     *string
	CardBrand *string
	CardLast4 *string
	CreatedAt time.Time
}

// Detail is one booking with its tickets and payment attempts. It carries no
// idempotency_key: that is a credential for replaying a purchase, never
// something an API response hands back (§11).
type Detail struct {
	Summary
	Tickets  []Ticket
	Payments []Payment
}

// PaymentStatus applies the §6 reading rule: a booking that went through an
// ambiguous outcome and was later resolved keeps BOTH an 'unknown' row (TX2's
// evidence of the attempt) and a resolved row (the resolver's verdict). Current
// state is the most recent row that is not 'unknown'; 'unknown' is the answer
// only when nothing has resolved yet. Reading "the latest row" instead would
// report every healed booking as unknown.
func (d Detail) PaymentStatus() string {
	for _, p := range d.Payments { // ordered newest first
		if p.Status != "unknown" {
			return p.Status
		}
	}
	if len(d.Payments) == 0 {
		return "none"
	}
	return "unknown"
}

const summaryColumns = `b.id, b.status, b.quantity, b.total_cents, b.currency,
	b.created_at, b.confirmed_at, b.failed_at,
	e.id, e.name, e.event_at, t.id, t.name`

const summaryFrom = `FROM booking b
	JOIN events e ON e.id = b.event_id
	JOIN ticket_tier t ON t.id = b.ticket_tier_id`

func scanSummary(row pgx.Row) (Summary, error) {
	var s Summary
	err := row.Scan(&s.ID, &s.Status, &s.Quantity, &s.TotalCents, &s.Currency,
		&s.CreatedAt, &s.ConfirmedAt, &s.FailedAt,
		&s.EventID, &s.EventName, &s.EventAt, &s.TierID, &s.TierName)
	return s, err
}

// ListByUser returns one page of a user's bookings, newest id first, plus the
// cursor for the next page (nil when this is the last one). It fetches limit+1
// rows to detect whether more exist.
//
// ponytail: paginating by id uses idx_booking_user(user_id, id DESC) directly,
// but booking ids are random UUIDs, so the order is stable rather than
// chronological. Switch the cursor to (created_at, id) with a matching index
// when a UI needs newest-first.
func ListByUser(ctx context.Context, pool *pgxpool.Pool, userID string, limit int, cursor *string) ([]Summary, *string, error) {
	rows, err := pool.Query(ctx,
		`SELECT `+summaryColumns+` `+summaryFrom+`
		 WHERE b.user_id = $1 AND ($2::uuid IS NULL OR b.id < $2)
		 ORDER BY b.id DESC
		 LIMIT $3`, userID, cursor, limit+1)
	if err != nil {
		return nil, nil, fmt.Errorf("list bookings: %w", err)
	}
	defer rows.Close()

	var out []Summary
	for rows.Next() {
		s, err := scanSummary(rows)
		if err != nil {
			return nil, nil, fmt.Errorf("scan booking: %w", err)
		}
		out = append(out, s)
	}
	if rows.Err() != nil {
		return nil, nil, fmt.Errorf("iterate bookings: %w", rows.Err())
	}

	var next *string
	if len(out) > limit {
		out = out[:limit]
		next = &out[limit-1].ID
	}
	return out, next, nil
}

// GetByIDForUser loads one booking owned by userID. The ownership check is in
// the WHERE clause, not a post-hoc comparison, so a miss and a stranger's
// booking are indistinguishable from here.
func GetByIDForUser(ctx context.Context, pool *pgxpool.Pool, bookingID, userID string) (Detail, error) {
	s, err := scanSummary(pool.QueryRow(ctx,
		`SELECT `+summaryColumns+` `+summaryFrom+`
		 WHERE b.id = $1 AND b.user_id = $2`, bookingID, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Detail{}, ErrNotFound
	}
	if err != nil {
		return Detail{}, fmt.Errorf("get booking: %w", err)
	}
	d := Detail{Summary: s}

	tickets, err := pool.Query(ctx,
		`SELECT id, status FROM tickets WHERE booking_id = $1 ORDER BY id`, bookingID)
	if err != nil {
		return Detail{}, fmt.Errorf("list tickets: %w", err)
	}
	defer tickets.Close()
	for tickets.Next() {
		var t Ticket
		if err := tickets.Scan(&t.ID, &t.Status); err != nil {
			return Detail{}, fmt.Errorf("scan ticket: %w", err)
		}
		d.Tickets = append(d.Tickets, t)
	}
	if tickets.Err() != nil {
		return Detail{}, fmt.Errorf("iterate tickets: %w", tickets.Err())
	}

	// Newest first, so PaymentStatus can take the first resolved row.
	payments, err := pool.Query(ctx,
		`SELECT status, provider_txn_id, card_brand, card_last4, created_at
		 FROM payments WHERE booking_id = $1 ORDER BY created_at DESC, id DESC`, bookingID)
	if err != nil {
		return Detail{}, fmt.Errorf("list payments: %w", err)
	}
	defer payments.Close()
	for payments.Next() {
		var p Payment
		if err := payments.Scan(&p.Status, &p.TxnID, &p.CardBrand, &p.CardLast4, &p.CreatedAt); err != nil {
			return Detail{}, fmt.Errorf("scan payment: %w", err)
		}
		d.Payments = append(d.Payments, p)
	}
	if payments.Err() != nil {
		return Detail{}, fmt.Errorf("iterate payments: %w", payments.Err())
	}
	return d, nil
}
