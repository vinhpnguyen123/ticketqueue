//go:build integration

package worker

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/vinhpnguyen123/ticketqueue/internal/booking"
	"github.com/vinhpnguyen123/ticketqueue/internal/paymentsim"
	"github.com/vinhpnguyen123/ticketqueue/internal/testutil"
)

const payKey = "ps_test_localdev"

// newProvider starts a paymentsim server plus a client pointed at it.
func newProvider(t *testing.T) (*httptest.Server, *booking.PaymentClient) {
	t.Helper()
	srv := httptest.NewServer(paymentsim.NewServer(payKey))
	t.Cleanup(srv.Close)
	return srv, booking.NewPaymentClient(srv.URL, payKey)
}

// seedTier creates an on_sale event with one tier of n available tickets and
// primes the Redis inventory counter to n.
func seedTier(t *testing.T, pool *pgxpool.Pool, rdb *redis.Client, n int) string {
	t.Helper()
	ctx := context.Background()
	var eventID, tierID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO events (name, venue, event_at, status)
		 VALUES ('Recon Test', 'Venue', NOW() + INTERVAL '30 days', 'on_sale') RETURNING id`,
	).Scan(&eventID); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO ticket_tier (event_id, name, price_cents, quantity_total)
		 VALUES ($1, 'VIP', 50000, $2) RETURNING id`, eventID, n,
	).Scan(&tierID); err != nil {
		t.Fatalf("insert tier: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO tickets (ticket_tier_id, status) SELECT $1, 'available' FROM generate_series(1, $2)`,
		tierID, n); err != nil {
		t.Fatalf("insert tickets: %v", err)
	}
	if err := rdb.Set(ctx, booking.InventoryKey(tierID), n, 0).Err(); err != nil {
		t.Fatalf("prime inventory: %v", err)
	}
	return tierID
}

var userSeq int

// holdBooking runs a real TX1 (so tickets are genuinely held and the Redis
// counter decremented) and returns the booking.
func holdBooking(t *testing.T, pool *pgxpool.Pool, rdb *redis.Client, tierID, idemKey string, qty int) booking.Held {
	t.Helper()
	ctx := context.Background()
	userSeq++
	var userID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email, password_hash) VALUES ($1, 'x') RETURNING id`,
		fmt.Sprintf("recon%d@test.dev", userSeq),
	).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	tier, err := booking.LoadTier(ctx, pool, tierID)
	if err != nil {
		t.Fatalf("load tier: %v", err)
	}
	if err := booking.ReserveInventory(ctx, rdb, pool, tierID, qty); err != nil {
		t.Fatalf("reserve inventory: %v", err)
	}
	held, err := booking.HoldTickets(ctx, pool, userID, tier, qty, idemKey, "req-recon")
	if err != nil {
		t.Fatalf("TX1: %v", err)
	}
	return held
}

// age pushes created_at past the 90s reconciliation threshold.
func age(t *testing.T, pool *pgxpool.Pool, bookingID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE booking SET created_at = NOW() - INTERVAL '5 minutes' WHERE id = $1`, bookingID); err != nil {
		t.Fatalf("age booking: %v", err)
	}
}

// charge makes the provider actually process a charge under idemKey, so a
// later by-key lookup finds it — this is what a dropped response leaves behind.
func charge(t *testing.T, pc *booking.PaymentClient, idemKey string, held booking.Held, token string) {
	t.Helper()
	res := pc.Charge(context.Background(), idemKey, held.TotalCents, held.Currency, token)
	if res.Outcome == booking.Ambiguous {
		t.Fatalf("setup charge was ambiguous, want a definite outcome")
	}
}

func bookingStatus(t *testing.T, pool *pgxpool.Pool, id string) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM booking WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatalf("booking status: %v", err)
	}
	return s
}

func count(t *testing.T, pool *pgxpool.Pool, q string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func inventory(t *testing.T, rdb *redis.Client, tierID string) int {
	t.Helper()
	n, err := rdb.Get(context.Background(), booking.InventoryKey(tierID)).Int()
	if err != nil {
		t.Fatalf("get inventory: %v", err)
	}
	return n
}

// Test 1: the provider charged successfully but the response never arrived.
// Reconciliation finds the charge by key and confirms.
func TestReconcileConfirmsSucceededCharge(t *testing.T) {
	pool, rdb := testutil.TestDB(t), testutil.TestRedis(t)
	_, pc := newProvider(t)
	tierID := seedTier(t, pool, rdb, 10)

	held := holdBooking(t, pool, rdb, tierID, "recon-ok", 2)
	charge(t, pc, "recon-ok", held, "tok_visa")
	age(t, pool, held.BookingID)

	if err := ReconcileCycle(context.Background(), rdb, pool, pc); err != nil {
		t.Fatalf("cycle: %v", err)
	}

	if got := bookingStatus(t, pool, held.BookingID); got != "confirmed" {
		t.Errorf("booking status = %s, want confirmed", got)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM tickets WHERE booking_id = $1 AND status = 'sold'`, held.BookingID); n != 2 {
		t.Errorf("sold tickets = %d, want 2", n)
	}
	if n := count(t, pool,
		`SELECT COUNT(*) FROM payments WHERE booking_id = $1 AND status = 'succeeded' AND provider_txn_id IS NOT NULL`,
		held.BookingID); n != 1 {
		t.Errorf("succeeded payments rows = %d, want 1", n)
	}
	if n := count(t, pool,
		`SELECT COUNT(*) FROM audit_log WHERE entity_id = $1 AND actor_type = 'worker'
		 AND action IN ('booking_confirmed','payment_succeeded')`, held.BookingID); n != 2 {
		t.Errorf("worker audit rows = %d, want 2", n)
	}
	// Redis is NOT given back: those tickets are sold.
	if got := inventory(t, rdb, tierID); got != 8 {
		t.Errorf("inventory = %d, want 8", got)
	}
}

// Test 2: the provider has a failed charge → fail the booking, release the
// tickets, give the inventory back.
func TestReconcileFailsDeclinedCharge(t *testing.T) {
	pool, rdb := testutil.TestDB(t), testutil.TestRedis(t)
	_, pc := newProvider(t)
	tierID := seedTier(t, pool, rdb, 10)

	held := holdBooking(t, pool, rdb, tierID, "recon-dec", 2)
	charge(t, pc, "recon-dec", held, "tok_declined")
	age(t, pool, held.BookingID)

	if err := ReconcileCycle(context.Background(), rdb, pool, pc); err != nil {
		t.Fatalf("cycle: %v", err)
	}

	if got := bookingStatus(t, pool, held.BookingID); got != "failed" {
		t.Errorf("booking status = %s, want failed", got)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM tickets WHERE booking_id = $1`, held.BookingID); n != 0 {
		t.Errorf("tickets still attached = %d, want 0", n)
	}
	if n := count(t, pool,
		`SELECT COUNT(*) FROM tickets WHERE ticket_tier_id = $1 AND status = 'available'`, tierID); n != 10 {
		t.Errorf("available tickets = %d, want 10", n)
	}
	if n := count(t, pool,
		`SELECT COUNT(*) FROM payments WHERE booking_id = $1 AND status = 'failed'`, held.BookingID); n != 1 {
		t.Errorf("failed payments rows = %d, want 1", n)
	}
	if got := inventory(t, rdb, tierID); got != 10 {
		t.Errorf("inventory = %d, want 10 (given back)", got)
	}
}

// Test 3: the provider never saw this key → no money moved. Fail cleanly, and
// write NO payments row (a payments row must mean a real attempt).
func TestReconcileFailsWhenProviderHasNoCharge(t *testing.T) {
	pool, rdb := testutil.TestDB(t), testutil.TestRedis(t)
	_, pc := newProvider(t)
	tierID := seedTier(t, pool, rdb, 10)

	held := holdBooking(t, pool, rdb, tierID, "recon-404", 3)
	age(t, pool, held.BookingID)

	if err := ReconcileCycle(context.Background(), rdb, pool, pc); err != nil {
		t.Fatalf("cycle: %v", err)
	}

	if got := bookingStatus(t, pool, held.BookingID); got != "failed" {
		t.Errorf("booking status = %s, want failed", got)
	}
	var reason string
	if err := pool.QueryRow(context.Background(),
		`SELECT metadata->>'reason' FROM audit_log WHERE entity_id = $1 AND action = 'booking_failed'`,
		held.BookingID).Scan(&reason); err != nil {
		t.Fatalf("read audit reason: %v", err)
	}
	if reason != "no_charge_found" {
		t.Errorf("audit reason = %q, want no_charge_found", reason)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM payments WHERE booking_id = $1`, held.BookingID); n != 0 {
		t.Errorf("payments rows = %d, want 0 (no attempt ever reached the provider)", n)
	}
	if got := inventory(t, rdb, tierID); got != 10 {
		t.Errorf("inventory = %d, want 10 (given back)", got)
	}
}

// Test 4: provider unreachable → never guess. Booking stays held, nothing is
// written; once the provider is back the next cycle resolves it.
func TestReconcileDefersWhenProviderUnreachable(t *testing.T) {
	pool, rdb := testutil.TestDB(t), testutil.TestRedis(t)
	srv, pc := newProvider(t)
	tierID := seedTier(t, pool, rdb, 10)

	held := holdBooking(t, pool, rdb, tierID, "recon-down", 2)
	charge(t, pc, "recon-down", held, "tok_visa")
	age(t, pool, held.BookingID)

	dead := booking.NewPaymentClient("http://127.0.0.1:1", payKey)
	if err := ReconcileCycle(context.Background(), rdb, pool, dead); err != nil {
		t.Fatalf("cycle with dead provider must not error: %v", err)
	}
	if got := bookingStatus(t, pool, held.BookingID); got != "held" {
		t.Errorf("booking status = %s, want held (never guess)", got)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM tickets WHERE booking_id = $1 AND status = 'held'`, held.BookingID); n != 2 {
		t.Errorf("held tickets = %d, want 2 (untouched)", n)
	}
	if n := count(t, pool,
		`SELECT COUNT(*) FROM audit_log WHERE entity_id = $1 AND actor_type = 'worker'`, held.BookingID); n != 0 {
		t.Errorf("worker audit rows = %d, want 0 (a deferral is not a state change)", n)
	}

	// Provider back.
	_ = srv
	if err := ReconcileCycle(context.Background(), rdb, pool, pc); err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if got := bookingStatus(t, pool, held.BookingID); got != "confirmed" {
		t.Errorf("booking status after recovery = %s, want confirmed", got)
	}
}

// Test 5: running twice is a no-op the second time — the guarded UPDATE loses,
// so no duplicate payments or audit rows.
func TestReconcileIsIdempotent(t *testing.T) {
	pool, rdb := testutil.TestDB(t), testutil.TestRedis(t)
	_, pc := newProvider(t)
	tierID := seedTier(t, pool, rdb, 10)

	held := holdBooking(t, pool, rdb, tierID, "recon-twice", 2)
	charge(t, pc, "recon-twice", held, "tok_visa")
	age(t, pool, held.BookingID)

	for i := 0; i < 2; i++ {
		if err := ReconcileCycle(context.Background(), rdb, pool, pc); err != nil {
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
}

// Test 6: a fresh booking is still in the synchronous path's hands.
func TestReconcileSkipsYoungBookings(t *testing.T) {
	pool, rdb := testutil.TestDB(t), testutil.TestRedis(t)
	_, pc := newProvider(t)
	tierID := seedTier(t, pool, rdb, 10)

	held := holdBooking(t, pool, rdb, tierID, "recon-young", 2) // not aged

	if err := ReconcileCycle(context.Background(), rdb, pool, pc); err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if got := bookingStatus(t, pool, held.BookingID); got != "held" {
		t.Errorf("booking status = %s, want held (younger than 90s)", got)
	}
}

// Test 7: one booking the provider won't answer for must not stop the rest of
// the batch.
func TestReconcileBatchContinuesPastOneBadBooking(t *testing.T) {
	pool, rdb := testutil.TestDB(t), testutil.TestRedis(t)
	srv, pc := newProvider(t)
	tierID := seedTier(t, pool, rdb, 10)

	bad := holdBooking(t, pool, rdb, tierID, "recon-bad", 1)
	good := holdBooking(t, pool, rdb, tierID, "recon-good", 1)
	charge(t, pc, "recon-good", good, "tok_visa")
	age(t, pool, bad.BookingID)
	age(t, pool, good.BookingID)

	// 500 on the bad key only; everything else goes through untouched.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/charges/by-idempotency-key/recon-bad" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		proxyTo(t, srv.URL, w, r)
	}))
	defer proxy.Close()

	if err := ReconcileCycle(context.Background(), rdb, pool, booking.NewPaymentClient(proxy.URL, payKey)); err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if got := bookingStatus(t, pool, bad.BookingID); got != "held" {
		t.Errorf("bad booking = %s, want held (deferred)", got)
	}
	if got := bookingStatus(t, pool, good.BookingID); got != "confirmed" {
		t.Errorf("good booking = %s, want confirmed (batch continued)", got)
	}
}

// Test 8: no lock is held across the provider call. With the lookup artificially
// slow, a concurrent purchase on the same tier must still complete — if the scan
// locked its rows, this would block for the whole provider delay.
func TestReconcileHoldsNoLockAcrossProviderCall(t *testing.T) {
	pool, rdb := testutil.TestDB(t), testutil.TestRedis(t)
	srv, pc := newProvider(t)
	tierID := seedTier(t, pool, rdb, 10)

	held := holdBooking(t, pool, rdb, tierID, "recon-slow", 2)
	charge(t, pc, "recon-slow", held, "tok_visa")
	age(t, pool, held.BookingID)

	const delay = 2 * time.Second
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		proxyTo(t, srv.URL, w, r)
	}))
	defer slow.Close()

	done := make(chan error, 1)
	go func() {
		done <- ReconcileCycle(context.Background(), rdb, pool, booking.NewPaymentClient(slow.URL, payKey))
	}()

	time.Sleep(200 * time.Millisecond) // the cycle is now inside the provider call
	start := time.Now()
	other := holdBooking(t, pool, rdb, tierID, "recon-concurrent", 1)
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

// proxyTo forwards a request to the real paymentsim server. Only what these
// tests need: method, path and the auth header.
func proxyTo(t *testing.T, base string, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	req, err := http.NewRequest(r.Method, base+r.URL.Path, r.Body)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	req.Header = r.Header.Clone()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		t.Logf("proxy copy: %v", err)
	}
}
