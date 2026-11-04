//go:build integration

package booking_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/vinhpnguyen123/ticketqueue/internal/booking"
	"github.com/vinhpnguyen123/ticketqueue/internal/testutil"
)

// Called at HoldTickets level on purpose: through the HTTP handler, a fast
// first purchase can finish and legitimately let a second one through.
func TestHoldTicketsOneHeldPerUser(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()

	var userID, eventID, tierID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email, password_hash) VALUES ('one-held@test.dev', 'x') RETURNING id`,
	).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO events (name, venue, event_at, status)
		 VALUES ('One Held', 'Venue', NOW() + INTERVAL '30 days', 'on_sale') RETURNING id`,
	).Scan(&eventID); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO ticket_tier (event_id, name, price_cents, quantity_total)
		 VALUES ($1, 'GA', 1000, 50) RETURNING id`, eventID,
	).Scan(&tierID); err != nil {
		t.Fatalf("insert tier: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO tickets (ticket_tier_id, status) SELECT $1, 'available' FROM generate_series(1, 50)`,
		tierID); err != nil {
		t.Fatalf("insert tickets: %v", err)
	}
	tier := booking.Tier{ID: tierID, EventID: eventID, PriceCents: 1000, Currency: "USD"}

	const n, qty = 10, 2
	start := make(chan struct{})
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = booking.HoldTickets(ctx, pool, userID, tier, qty, fmt.Sprintf("one-held-%d", i), "req")
		}()
	}
	close(start)
	wg.Wait()

	var won, blocked int
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case errors.Is(err, booking.ErrActiveBooking):
			blocked++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if won != 1 || blocked != n-1 {
		t.Errorf("won = %d, blocked = %d, want 1 and %d", won, blocked, n-1)
	}

	var bookings, held int
	if err := pool.QueryRow(ctx,
		`SELECT (SELECT COUNT(*) FROM booking WHERE user_id = $1),
		        (SELECT COUNT(*) FROM tickets WHERE ticket_tier_id = $2 AND status = 'held')`,
		userID, tierID,
	).Scan(&bookings, &held); err != nil {
		t.Fatalf("count: %v", err)
	}
	if bookings != 1 || held != qty {
		t.Errorf("bookings = %d, held tickets = %d, want 1 and %d", bookings, held, qty)
	}
}
