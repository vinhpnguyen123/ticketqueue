//go:build integration

package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vinhpnguyen123/ticketqueue/internal/booking"
	"github.com/vinhpnguyen123/ticketqueue/internal/testutil"
)

// expire forces a booking past its hold deadline.
func expire(t *testing.T, pool *pgxpool.Pool, bookingID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE booking SET hold_expires_at = NOW() - INTERVAL '1 second' WHERE id = $1`,
		bookingID); err != nil {
		t.Fatalf("expire booking: %v", err)
	}
}

func deferrals(t *testing.T, pool *pgxpool.Pool, bookingID string) int {
	t.Helper()
	return count(t, pool,
		`SELECT COUNT(*) FROM audit_log WHERE entity_id = $1 AND action = 'expiry_deferred_provider_unreachable'`,
		bookingID)
}

// §12 #7 — the provider never saw this key, so no money moved: release.
func TestHoldExpiryReleasesWhenProviderHasNoCharge(t *testing.T) {
	pool, rdb := testutil.TestDB(t), testutil.TestRedis(t)
	_, pc := newProvider(t)
	tierID := seedTier(t, pool, rdb, 10)

	held := holdBooking(t, pool, rdb, tierID, "exp-nocharge", 2)
	expire(t, pool, held.BookingID)

	if err := HoldExpiryCycle(context.Background(), rdb, pool, pc); err != nil {
		t.Fatalf("cycle: %v", err)
	}

	if got := bookingStatus(t, pool, held.BookingID); got != "failed" {
		t.Errorf("booking status = %s, want failed", got)
	}
	if n := count(t, pool,
		`SELECT COUNT(*) FROM tickets WHERE ticket_tier_id = $1 AND status = 'available' AND booking_id IS NULL`,
		tierID); n != 10 {
		t.Errorf("available tickets = %d, want 10", n)
	}
	if got := inventory(t, rdb, tierID); got != 10 {
		t.Errorf("inventory = %d, want 10 (given back)", got)
	}
	var reason, source, actor string
	if err := pool.QueryRow(context.Background(),
		`SELECT metadata->>'reason', metadata->>'source', actor_type FROM audit_log
		 WHERE entity_id = $1 AND action = 'booking_failed'`, held.BookingID).
		Scan(&reason, &source, &actor); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if reason != "no_charge_found" || source != "hold_expiry" || actor != "worker" {
		t.Errorf("audit = reason %q source %q actor %q, want no_charge_found/hold_expiry/worker",
			reason, source, actor)
	}
	// No charge ever reached the provider, so there must be no payments row.
	if n := count(t, pool, `SELECT COUNT(*) FROM payments WHERE booking_id = $1`, held.BookingID); n != 0 {
		t.Errorf("payments rows = %d, want 0", n)
	}
}

// §12 #8 — the money rule: unreachable provider never costs a customer their
// seat, and the deferral is marked once, not once per cycle.
func TestHoldExpiryDefersWhenProviderUnreachable(t *testing.T) {
	pool, rdb := testutil.TestDB(t), testutil.TestRedis(t)
	srv, pc := newProvider(t)
	tierID := seedTier(t, pool, rdb, 10)

	held := holdBooking(t, pool, rdb, tierID, "exp-down", 2)
	charge(t, pc, "exp-down", held, "tok_visa") // money DID move
	expire(t, pool, held.BookingID)

	dead := booking.NewPaymentClient("http://127.0.0.1:1", payKey)
	for i := 0; i < 4; i++ {
		if err := HoldExpiryCycle(context.Background(), rdb, pool, dead); err != nil {
			t.Fatalf("cycle %d with dead provider must not error: %v", i, err)
		}
	}

	if got := bookingStatus(t, pool, held.BookingID); got != "held" {
		t.Errorf("booking status = %s, want held (never release on a guess)", got)
	}
	if n := count(t, pool,
		`SELECT COUNT(*) FROM tickets WHERE booking_id = $1 AND status = 'held'`, held.BookingID); n != 2 {
		t.Errorf("held tickets = %d, want 2 (frozen, not released)", n)
	}
	if got := inventory(t, rdb, tierID); got != 8 {
		t.Errorf("inventory = %d, want 8 (NOT given back)", got)
	}
	if n := deferrals(t, pool, held.BookingID); n != 1 {
		t.Errorf("deferral audit rows = %d after 4 cycles, want exactly 1", n)
	}

	// Provider comes back: the booking resolves per its verdict, and the
	// customer who paid keeps the seat they paid for.
	_ = srv
	if err := HoldExpiryCycle(context.Background(), rdb, pool, pc); err != nil {
		t.Fatalf("cycle after recovery: %v", err)
	}
	if got := bookingStatus(t, pool, held.BookingID); got != "confirmed" {
		t.Errorf("booking status after recovery = %s, want confirmed", got)
	}
	if n := count(t, pool,
		`SELECT COUNT(*) FROM tickets WHERE booking_id = $1 AND status = 'sold'`, held.BookingID); n != 2 {
		t.Errorf("sold tickets = %d, want 2", n)
	}
}

// §12 #9 — THE headline case: the hold expired, but the provider says the card
// was charged. Confirm; never release.
func TestHoldExpiryConfirmsWhenProviderCharged(t *testing.T) {
	pool, rdb := testutil.TestDB(t), testutil.TestRedis(t)
	_, pc := newProvider(t)
	tierID := seedTier(t, pool, rdb, 10)

	held := holdBooking(t, pool, rdb, tierID, "exp-charged", 2)
	charge(t, pc, "exp-charged", held, "tok_visa")
	expire(t, pool, held.BookingID)

	if err := HoldExpiryCycle(context.Background(), rdb, pool, pc); err != nil {
		t.Fatalf("cycle: %v", err)
	}

	if got := bookingStatus(t, pool, held.BookingID); got != "confirmed" {
		t.Errorf("booking status = %s, want confirmed", got)
	}
	if n := count(t, pool,
		`SELECT COUNT(*) FROM tickets WHERE booking_id = $1 AND status = 'sold'`, held.BookingID); n != 2 {
		t.Errorf("sold tickets = %d, want 2 (NOT released)", n)
	}
	if n := count(t, pool,
		`SELECT COUNT(*) FROM payments WHERE booking_id = $1 AND status = 'succeeded' AND provider_txn_id IS NOT NULL`,
		held.BookingID); n != 1 {
		t.Errorf("succeeded payments rows = %d, want 1", n)
	}
	if got := inventory(t, rdb, tierID); got != 8 {
		t.Errorf("inventory = %d, want 8 (NOT given back)", got)
	}
}

// §12 #10 — a real decline releases the seat, as it always did.
func TestHoldExpiryReleasesWhenChargeFailed(t *testing.T) {
	pool, rdb := testutil.TestDB(t), testutil.TestRedis(t)
	_, pc := newProvider(t)
	tierID := seedTier(t, pool, rdb, 10)

	held := holdBooking(t, pool, rdb, tierID, "exp-declined", 2)
	charge(t, pc, "exp-declined", held, "tok_declined")
	expire(t, pool, held.BookingID)

	if err := HoldExpiryCycle(context.Background(), rdb, pool, pc); err != nil {
		t.Fatalf("cycle: %v", err)
	}

	if got := bookingStatus(t, pool, held.BookingID); got != "failed" {
		t.Errorf("booking status = %s, want failed", got)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM tickets WHERE booking_id = $1`, held.BookingID); n != 0 {
		t.Errorf("tickets still attached = %d, want 0", n)
	}
	if n := count(t, pool,
		`SELECT COUNT(*) FROM payments WHERE booking_id = $1 AND status = 'failed'`, held.BookingID); n != 1 {
		t.Errorf("failed payments rows = %d, want 1", n)
	}
	if got := inventory(t, rdb, tierID); got != 10 {
		t.Errorf("inventory = %d, want 10 (given back)", got)
	}
}

// §12 #11 — a second cycle over an already-resolved booking changes nothing.
func TestHoldExpiryIsIdempotent(t *testing.T) {
	pool, rdb := testutil.TestDB(t), testutil.TestRedis(t)
	_, pc := newProvider(t)
	tierID := seedTier(t, pool, rdb, 10)

	held := holdBooking(t, pool, rdb, tierID, "exp-twice", 2)
	charge(t, pc, "exp-twice", held, "tok_visa")
	expire(t, pool, held.BookingID)

	for i := 0; i < 2; i++ {
		if err := HoldExpiryCycle(context.Background(), rdb, pool, pc); err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
	}

	if n := count(t, pool, `SELECT COUNT(*) FROM payments WHERE booking_id = $1`, held.BookingID); n != 1 {
		t.Errorf("payments rows = %d, want exactly 1", n)
	}
	if n := count(t, pool,
		`SELECT COUNT(*) FROM audit_log WHERE entity_id = $1 AND action = 'booking_confirmed'`,
		held.BookingID); n != 1 {
		t.Errorf("booking_confirmed audit rows = %d, want 1", n)
	}
	if got := inventory(t, rdb, tierID); got != 8 {
		t.Errorf("inventory = %d, want 8", got)
	}
}

// §12 #12 — no lock is held across the provider call: a slow provider mid-cycle
// must not block a concurrent purchase on the same tier.
func TestHoldExpiryHoldsNoLockAcrossProviderCall(t *testing.T) {
	pool, rdb := testutil.TestDB(t), testutil.TestRedis(t)
	srv, pc := newProvider(t)
	tierID := seedTier(t, pool, rdb, 10)

	held := holdBooking(t, pool, rdb, tierID, "exp-slow", 2)
	charge(t, pc, "exp-slow", held, "tok_visa")
	expire(t, pool, held.BookingID)

	const delay = 2 * time.Second
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		proxyTo(t, srv.URL, w, r)
	}))
	defer slow.Close()

	done := make(chan error, 1)
	go func() {
		done <- HoldExpiryCycle(context.Background(), rdb, pool, booking.NewPaymentClient(slow.URL, payKey))
	}()

	time.Sleep(200 * time.Millisecond) // the cycle is inside the provider call
	start := time.Now()
	other := holdBooking(t, pool, rdb, tierID, "exp-concurrent", 1)
	if elapsed := time.Since(start); elapsed > delay/2 {
		t.Errorf("concurrent purchase took %v — the scan is holding locks across the network call", elapsed)
	}
	if got := bookingStatus(t, pool, other.BookingID); got != "held" {
		t.Errorf("concurrent booking = %s, want held", got)
	}

	if err := <-done; err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if got := bookingStatus(t, pool, held.BookingID); got != "confirmed" {
		t.Errorf("slow-path booking = %s, want confirmed", got)
	}
}
