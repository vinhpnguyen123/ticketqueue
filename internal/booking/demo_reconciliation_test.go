//go:build integration

// Package booking_test holds the end-to-end demo of the headline reliability
// scenario. It is the Go twin of scripts/demo_reconciliation.sh: the script
// proves it against the live stack over HTTP, this proves the same invariants
// in the normal test suite, without waiting for a real 30s worker tick.
//
// It lives in booking_test (an external test package) rather than booking so
// it can import internal/worker, which imports internal/booking.
package booking_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/vinhpnguyen123/ticketqueue/internal/booking"
	"github.com/vinhpnguyen123/ticketqueue/internal/paymentsim"
	"github.com/vinhpnguyen123/ticketqueue/internal/testutil"
	"github.com/vinhpnguyen123/ticketqueue/internal/worker"
)

const (
	payKey   = "ps_test_localdev"
	demoQty  = 2
	tierSize = 10
)

// TestDemoDroppedResponseSelfHeals is Carl's scenario: the provider charges the
// card and the response is lost on the network. The purchase cannot know
// whether money moved, so it refuses to guess and parks the booking. Nobody
// retries and nobody intervenes — the reconciliation worker asks the provider
// by idempotency key and finishes the job.
func TestDemoDroppedResponseSelfHeals(t *testing.T) {
	ctx := context.Background()
	pool, rdb := testutil.TestDB(t), testutil.TestRedis(t)

	srv := httptest.NewServer(paymentsim.NewServer(payKey))
	t.Cleanup(srv.Close)
	pc := booking.NewPaymentClient(srv.URL, payKey)

	userID, tierID := seedDemo(t, pool, rdb)
	tier, err := booking.LoadTier(ctx, pool, tierID)
	if err != nil {
		t.Fatalf("load tier: %v", err)
	}

	// --- PHASE 1: hold the tickets.
	idemKey := "demo-dropped-response"
	if err := booking.ReserveInventory(ctx, rdb, pool, tierID, demoQty); err != nil {
		t.Fatalf("reserve inventory: %v", err)
	}
	held, err := booking.HoldTickets(ctx, pool, userID, tier, demoQty, idemKey, "req-demo")
	if err != nil {
		t.Fatalf("TX1: %v", err)
	}

	// --- PHASE 2: the charge. tok_drops_response stores the charge, then
	// hijacks the connection — money moved, the answer never arrived.
	charge := pc.Charge(ctx, idemKey, held.TotalCents, held.Currency, "tok_drops_response")
	if charge.Outcome != booking.Ambiguous {
		t.Fatalf("charge outcome = %v, want Ambiguous (the response was dropped)", charge.Outcome)
	}

	// --- PHASE 3: TX2 refuses to guess.
	fin, err := booking.Finalize(ctx, pool, held, tierID, userID, "req-demo", charge)
	if err != nil {
		t.Fatalf("TX2: %v", err)
	}
	if fin.Status != "held_unknown" {
		t.Fatalf("finalize status = %s, want held_unknown", fin.Status)
	}
	assertQuery(t, pool, "held", `SELECT status FROM booking WHERE id = $1`, held.BookingID)
	assertQuery(t, pool, "unknown", `SELECT status FROM payments WHERE booking_id = $1`, held.BookingID)

	// The provider's side of the story: the money DID move. This asymmetry
	// between what we know and what happened is the whole scenario.
	if got := pc.Lookup(ctx, idemKey); got.Outcome != booking.Succeeded {
		t.Fatalf("provider lookup outcome = %v, want Succeeded (the charge is there)", got.Outcome)
	}

	// --- Recovery: no user retry, no manual action. Only the worker.
	// In production the booking becomes a candidate 90s after creation and the
	// worker runs every 30s; the test ages the row instead of sleeping.
	if _, err := pool.Exec(ctx,
		`UPDATE booking SET created_at = NOW() - INTERVAL '5 minutes' WHERE id = $1`, held.BookingID); err != nil {
		t.Fatalf("age booking: %v", err)
	}
	if err := worker.ReconcileCycle(ctx, rdb, pool, pc); err != nil {
		t.Fatalf("reconcile cycle: %v", err)
	}

	// --- The invariants the demo exists to prove.
	assertQuery(t, pool, "confirmed", `SELECT status FROM booking WHERE id = $1`, held.BookingID)
	assertQuery(t, pool, demoQty,
		`SELECT COUNT(*) FROM tickets WHERE booking_id = $1 AND status = 'sold'`, held.BookingID)
	assertQuery(t, pool, 1,
		`SELECT COUNT(*) FROM payments WHERE booking_id = $1 AND status = 'succeeded'`, held.BookingID)
	// Two payments rows, and that is correct: the 'unknown' row is durable
	// evidence that an attempt was made, the 'succeeded' row is what the
	// provider later confirmed. Exactly one of them is 'succeeded' — charged
	// once, never twice.
	assertQuery(t, pool, 2,
		`SELECT COUNT(*) FROM payments WHERE booking_id = $1`, held.BookingID)
	assertQuery(t, pool, 1,
		`SELECT COUNT(*) FROM payments WHERE booking_id = $1 AND status = 'unknown'`, held.BookingID)

	// The trail: every state change, in order, with who caused it.
	// booking_created covers the tickets being held too: TX1 is atomic, so they
	// were always one event written under two names.
	wantTimeline := []string{
		"booking_created:user",
		"payment_outcome_unknown:system",
		"booking_confirmed:worker",
		"payment_succeeded:worker",
	}
	if got := timeline(t, pool, held.BookingID); !equal(got, wantTimeline) {
		t.Errorf("audit timeline =\n  %v\nwant\n  %v", got, wantTimeline)
	}

	assertMoneyCorrectness(t, pool)
}

// assertMoneyCorrectness is the symmetric pair that has to hold after ANY
// sequence of events, not just this one.
func assertMoneyCorrectness(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()

	// (a) No confirmed booking lacks a succeeded payment — nobody got a free ticket.
	var orphanBooking string
	err := pool.QueryRow(ctx,
		`SELECT b.id FROM booking b
		 LEFT JOIN payments p ON p.booking_id = b.id AND p.status = 'succeeded'
		 WHERE b.status = 'confirmed' AND p.id IS NULL`).Scan(&orphanBooking)
	if err == nil {
		t.Errorf("confirmed booking %s has no succeeded payment — someone got a free ticket", orphanBooking)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("free-ticket check: %v", err)
	}

	// (b) No succeeded payment lacks a confirmed booking — nobody paid without
	// getting one.
	var orphanPayment string
	err = pool.QueryRow(ctx,
		`SELECT p.id FROM payments p
		 JOIN booking b ON b.id = p.booking_id
		 WHERE p.status = 'succeeded' AND b.status <> 'confirmed'`).Scan(&orphanPayment)
	if err == nil {
		t.Errorf("succeeded payment %s has no confirmed booking — someone paid for nothing", orphanPayment)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("paid-for-nothing check: %v", err)
	}
}

func seedDemo(t *testing.T, pool *pgxpool.Pool, rdb *redis.Client) (userID, tierID string) {
	t.Helper()
	ctx := context.Background()
	var eventID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO events (name, venue, event_at, status)
		 VALUES ('Demo Concert', 'Saigon Arena', NOW() + INTERVAL '30 days', 'on_sale') RETURNING id`,
	).Scan(&eventID); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO ticket_tier (event_id, name, price_cents, quantity_total)
		 VALUES ($1, 'VIP', 50000, $2) RETURNING id`, eventID, tierSize,
	).Scan(&tierID); err != nil {
		t.Fatalf("insert tier: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO tickets (ticket_tier_id, status) SELECT $1, 'available' FROM generate_series(1, $2)`,
		tierID, tierSize); err != nil {
		t.Fatalf("insert tickets: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email, password_hash) VALUES ('carl@demo.test', 'x') RETURNING id`,
	).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if err := rdb.Set(ctx, booking.InventoryKey(tierID), tierSize, 0).Err(); err != nil {
		t.Fatalf("prime inventory: %v", err)
	}
	return userID, tierID
}

// timeline returns the booking's audit trail as "action:actor_type" in order.
// audit_log.created_at defaults to NOW(), which is transaction time, so the two
// rows written by one transaction tie exactly; action is the tiebreaker that
// keeps this deterministic.
func timeline(t *testing.T, pool *pgxpool.Pool, bookingID string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT action, actor_type FROM audit_log
		 WHERE entity_type = 'booking' AND entity_id = $1 ORDER BY created_at, action`, bookingID)
	if err != nil {
		t.Fatalf("read audit timeline: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var action, actor string
		if err := rows.Scan(&action, &actor); err != nil {
			t.Fatalf("scan audit row: %v", err)
		}
		out = append(out, action+":"+actor)
	}
	if rows.Err() != nil {
		t.Fatalf("iterate audit rows: %v", rows.Err())
	}
	return out
}

// assertQuery runs a single-value query and compares it to want.
func assertQuery(t *testing.T, pool *pgxpool.Pool, want any, query string, args ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	switch w := want.(type) {
	case string:
		var got string
		if err := pool.QueryRow(ctx, query, args...).Scan(&got); err != nil {
			t.Fatalf("query %q: %v", query, err)
		}
		if got != w {
			t.Errorf("%s\n  got %q, want %q", query, got, w)
		}
	case int:
		var got int
		if err := pool.QueryRow(ctx, query, args...).Scan(&got); err != nil {
			t.Fatalf("query %q: %v", query, err)
		}
		if got != w {
			t.Errorf("%s\n  got %d, want %d", query, got, w)
		}
	default:
		t.Fatalf("assertQuery: unsupported want type %T", want)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
