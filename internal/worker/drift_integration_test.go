//go:build integration

package worker

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/redis/go-redis/v9"

	"github.com/vinhpnguyen123/ticketqueue/internal/booking"
	"github.com/vinhpnguyen123/ticketqueue/internal/metrics"
	"github.com/vinhpnguyen123/ticketqueue/internal/queue"
	"github.com/vinhpnguyen123/ticketqueue/internal/testutil"
)

func counter(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var pb dto.Metric
	if err := c.Write(&pb); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	return pb.GetCounter().GetValue()
}

func audits(t *testing.T, pool *pgxpool.Pool, entityID, action string) int {
	t.Helper()
	return count(t, pool,
		`SELECT COUNT(*) FROM audit_log WHERE entity_id = $1 AND action = $2`, entityID, action)
}

func setInventory(t *testing.T, rdb *redis.Client, tierID string, v int) {
	t.Helper()
	if err := rdb.Set(context.Background(), booking.InventoryKey(tierID), v, 0).Err(); err != nil {
		t.Fatalf("set inventory: %v", err)
	}
}

// §12 #11a: the two-cycle rule — detect on the first cycle, correct on the second.
func TestDriftCorrectsOnlyAfterTwoCycles(t *testing.T) {
	metrics.Register()
	pool, rdb := testutil.TestDB(t), testutil.TestRedis(t)
	ctx := context.Background()
	tierID := seedTier(t, pool, rdb, 10)
	d := NewDriftChecker()

	setInventory(t, rdb, tierID, 3) // truth is 10
	before := counter(t, metrics.InventoryDrift)

	if err := d.Cycle(ctx, rdb, pool); err != nil {
		t.Fatalf("cycle 1: %v", err)
	}
	if got := inventory(t, rdb, tierID); got != 3 {
		t.Errorf("inventory after cycle 1 = %d, want 3 (observed, NOT corrected)", got)
	}
	if got := counter(t, metrics.InventoryDrift); got != before {
		t.Errorf("drift counter moved on the first observation: %v -> %v", before, got)
	}

	if err := d.Cycle(ctx, rdb, pool); err != nil {
		t.Fatalf("cycle 2: %v", err)
	}
	if got := inventory(t, rdb, tierID); got != 10 {
		t.Errorf("inventory after cycle 2 = %d, want 10 (Postgres wins)", got)
	}
	if got := counter(t, metrics.InventoryDrift); got != before+1 {
		t.Errorf("drift counter = %v, want %v", got, before+1)
	}
	if n := audits(t, pool, tierID, "inventory_drift_corrected"); n != 1 {
		t.Errorf("drift audit rows = %d, want 1", n)
	}
	var before64, after64 int64
	if err := pool.QueryRow(ctx,
		`SELECT (metadata->>'before')::bigint, (metadata->>'after')::bigint FROM audit_log
		 WHERE entity_id = $1 AND action = 'inventory_drift_corrected'`, tierID).
		Scan(&before64, &after64); err != nil {
		t.Fatalf("read audit metadata: %v", err)
	}
	if before64 != 3 || after64 != 10 {
		t.Errorf("audit before/after = %d/%d, want 3/10", before64, after64)
	}
}

// §12 #11b: an in-flight TX1 changes the numbers between cycles, so the
// observation must reset instead of hardening into a correction.
func TestDriftDoesNotCorrectWhenNumbersMove(t *testing.T) {
	metrics.Register()
	pool, rdb := testutil.TestDB(t), testutil.TestRedis(t)
	ctx := context.Background()
	tierID := seedTier(t, pool, rdb, 10)
	d := NewDriftChecker()

	setInventory(t, rdb, tierID, 8)
	before := counter(t, metrics.InventoryDrift)
	if err := d.Cycle(ctx, rdb, pool); err != nil {
		t.Fatalf("cycle 1: %v", err)
	}

	// A claim commits between cycles: a real TX1 takes 2 tickets, so truth
	// moves to 8 and now matches the counter. (The schema requires a held
	// ticket to belong to a booking, so this cannot be faked with an UPDATE.)
	holdBooking(t, pool, rdb, tierID, "drift-inflight", 2)
	setInventory(t, rdb, tierID, 8) // holdBooking decremented it again

	if err := d.Cycle(ctx, rdb, pool); err != nil {
		t.Fatalf("cycle 2: %v", err)
	}
	if got := inventory(t, rdb, tierID); got != 8 {
		t.Errorf("inventory = %d, want 8 (untouched — the gap was in-flight work)", got)
	}
	if got := counter(t, metrics.InventoryDrift); got != before {
		t.Errorf("drift counter moved on settled in-flight work: %v -> %v", before, got)
	}
	if n := audits(t, pool, tierID, "inventory_drift_corrected"); n != 0 {
		t.Errorf("drift audit rows = %d, want 0", n)
	}
}

// §12 #11c: a missing key is seeded, not "corrected".
func TestDriftLazyInitIsNotDrift(t *testing.T) {
	metrics.Register()
	pool, rdb := testutil.TestDB(t), testutil.TestRedis(t)
	ctx := context.Background()
	tierID := seedTier(t, pool, rdb, 10)
	d := NewDriftChecker()

	if err := rdb.Del(ctx, booking.InventoryKey(tierID)).Err(); err != nil {
		t.Fatal(err)
	}
	before := counter(t, metrics.InventoryDrift)

	if err := d.Cycle(ctx, rdb, pool); err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if got := inventory(t, rdb, tierID); got != 10 {
		t.Errorf("inventory = %d, want 10 (lazy init)", got)
	}
	if got := counter(t, metrics.InventoryDrift); got != before {
		t.Errorf("drift counter moved on a lazy init: %v -> %v", before, got)
	}
}

// §12 #12a-c: every invariant is detected, audited, counted — and NOTHING is
// repaired. A checker that "fixes" what it does not understand destroys the
// evidence of the real bug.
func TestDriftInvariantsDetectButNeverFix(t *testing.T) {
	metrics.Register()
	pool, rdb := testutil.TestDB(t), testutil.TestRedis(t)
	ctx := context.Background()
	tierID := seedTier(t, pool, rdb, 10)
	d := NewDriftChecker()

	// 12a: one extra ticket row for the tier.
	if _, err := pool.Exec(ctx,
		`INSERT INTO tickets (ticket_tier_id, status) VALUES ($1, 'available')`, tierID); err != nil {
		t.Fatal(err)
	}
	// 12b: a sold ticket under a held booking.
	held := holdBooking(t, pool, rdb, tierID, "drift-held", 2)
	if _, err := pool.Exec(ctx,
		`UPDATE tickets SET status = 'sold'
		 WHERE id = (SELECT id FROM tickets WHERE booking_id = $1 LIMIT 1)`, held.BookingID); err != nil {
		t.Fatal(err)
	}
	// 12c: a held ticket under a failed booking.
	dead := holdBooking(t, pool, rdb, tierID, "drift-dead", 1)
	if _, err := pool.Exec(ctx,
		`UPDATE booking SET status = 'failed', failed_at = NOW() WHERE id = $1`, dead.BookingID); err != nil {
		t.Fatal(err)
	}

	ticketsBefore := count(t, pool, `SELECT COUNT(*) FROM tickets WHERE ticket_tier_id = $1`, tierID)
	before := counter(t, metrics.InvariantViolation)

	if err := d.Cycle(ctx, rdb, pool); err != nil {
		t.Fatalf("cycle: %v", err)
	}

	// Three distinct violations: one tier count, one sold, one held.
	if got := counter(t, metrics.InvariantViolation); got != before+3 {
		t.Errorf("invariant counter = %v, want %v", got, before+3)
	}
	for _, check := range []string{
		"ticket_count_matches_quantity_total",
		"sold_ticket_on_unconfirmed_booking",
		"held_ticket_on_dead_booking",
	} {
		n := count(t, pool,
			`SELECT COUNT(*) FROM audit_log
			 WHERE action = 'invariant_violation' AND metadata->>'check' = $1`, check)
		if n != 1 {
			t.Errorf("audit rows for %s = %d, want 1", check, n)
		}
	}
	// Nothing repaired: not one row created or destroyed.
	if got := count(t, pool, `SELECT COUNT(*) FROM tickets WHERE ticket_tier_id = $1`, tierID); got != ticketsBefore {
		t.Errorf("ticket rows changed from %d to %d — the checker must never repair", ticketsBefore, got)
	}
	if got := bookingStatus(t, pool, dead.BookingID); got != "failed" {
		t.Errorf("booking status changed to %s — the checker must never repair", got)
	}
}

// §17 sweep B: people still waiting for an event that is no longer selling.
func TestDriftSweepsAbandonedQueueEntries(t *testing.T) {
	metrics.Register()
	pool, rdb := testutil.TestDB(t), testutil.TestRedis(t)
	ctx := context.Background()
	eventID := seedEvent(t, pool, 5, 300)
	users := seedUsers(t, pool, rdb, eventID, 3)
	d := NewDriftChecker()

	if n, err := rdb.ZCard(ctx, queue.Key(eventID)).Result(); err != nil || n != 3 {
		t.Fatalf("queue size = %d err=%v, want 3", n, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE events SET status = 'sold_out' WHERE id = $1`, eventID); err != nil {
		t.Fatal(err)
	}

	if err := d.Cycle(ctx, rdb, pool); err != nil {
		t.Fatalf("cycle: %v", err)
	}

	for _, u := range users {
		var status string
		var leftAt *string
		if err := pool.QueryRow(ctx,
			`SELECT status, left_at::text FROM queue_entries WHERE user_id = $1 AND event_id = $2`,
			u, eventID).Scan(&status, &leftAt); err != nil {
			t.Fatalf("read entry: %v", err)
		}
		if status != "expired" {
			t.Errorf("entry status = %s, want expired", status)
		}
		if leftAt == nil {
			t.Errorf("left_at not set for %s", u)
		}
	}
	if n, err := rdb.ZCard(ctx, queue.Key(eventID)).Result(); err != nil || n != 0 {
		t.Errorf("queue size = %d err=%v, want 0", n, err)
	}
	if n := count(t, pool,
		`SELECT COUNT(*) FROM audit_log WHERE entity_id = $1 AND action = 'queue_entry_abandoned'`,
		eventID); n != 3 {
		t.Errorf("abandoned audit rows = %d, want 3", n)
	}

	// Idempotent: a second cycle finds nothing left to do.
	if err := d.Cycle(ctx, rdb, pool); err != nil {
		t.Fatalf("second cycle: %v", err)
	}
	if n := count(t, pool,
		`SELECT COUNT(*) FROM audit_log WHERE entity_id = $1 AND action = 'queue_entry_abandoned'`,
		eventID); n != 3 {
		t.Errorf("abandoned audit rows after second cycle = %d, want 3", n)
	}
}

// An on-sale event's queue is left alone.
func TestDriftLeavesLiveQueueAlone(t *testing.T) {
	metrics.Register()
	pool, rdb := testutil.TestDB(t), testutil.TestRedis(t)
	ctx := context.Background()
	eventID := seedEvent(t, pool, 5, 300)
	users := seedUsers(t, pool, rdb, eventID, 2)
	d := NewDriftChecker()

	if err := d.Cycle(ctx, rdb, pool); err != nil {
		t.Fatalf("cycle: %v", err)
	}
	for _, u := range users {
		var status string
		if err := pool.QueryRow(ctx,
			`SELECT status FROM queue_entries WHERE user_id = $1 AND event_id = $2`,
			u, eventID).Scan(&status); err != nil {
			t.Fatalf("read entry: %v", err)
		}
		if status != "waiting" {
			t.Errorf("entry status = %s, want waiting (event is still on sale)", status)
		}
	}
	if n, _ := rdb.ZCard(ctx, queue.Key(eventID)).Result(); n != 2 {
		t.Errorf("queue size = %d, want 2", n)
	}
}

// One failing job must not cost the other two their turn.
func TestDriftJobFailureDoesNotSkipOtherJobs(t *testing.T) {
	metrics.Register()
	pool, rdb := testutil.TestDB(t), testutil.TestRedis(t)
	ctx := context.Background()
	tierID := seedTier(t, pool, rdb, 10)
	eventID := seedEvent(t, pool, 5, 300)
	seedUsers(t, pool, rdb, eventID, 2)
	if _, err := pool.Exec(ctx, `UPDATE events SET status = 'closed' WHERE id = $1`, eventID); err != nil {
		t.Fatal(err)
	}
	// An invariant violation for job B to find.
	if _, err := pool.Exec(ctx,
		`INSERT INTO tickets (ticket_tier_id, status) VALUES ($1, 'available')`, tierID); err != nil {
		t.Fatal(err)
	}
	violationsBefore := counter(t, metrics.InvariantViolation)

	// Job A talks to a dead Redis; jobs B and C use Postgres and must still run.
	deadRedis := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { deadRedis.Close() })

	d := NewDriftChecker()
	err := d.Cycle(ctx, deadRedis, pool)
	if err == nil {
		t.Fatal("cycle returned nil, want the inventory job's failure reported")
	}

	if got := counter(t, metrics.InvariantViolation); got <= violationsBefore {
		t.Errorf("job B did not run after job A failed (counter %v -> %v)", violationsBefore, got)
	}
	expired := count(t, pool,
		`SELECT COUNT(*) FROM queue_entries WHERE event_id = $1 AND status = 'expired'`, eventID)
	if expired != 2 {
		t.Errorf("job C expired %d entries after job A failed, want 2", expired)
	}
}

// A clean system must never trip an invariant.
func TestDriftCleanSystemStaysSilent(t *testing.T) {
	metrics.Register()
	pool, rdb := testutil.TestDB(t), testutil.TestRedis(t)
	ctx := context.Background()
	tierID := seedTier(t, pool, rdb, 10)
	holdBooking(t, pool, rdb, tierID, fmt.Sprintf("drift-clean-%d", userSeq), 2)
	d := NewDriftChecker()

	before := counter(t, metrics.InvariantViolation)
	for i := 0; i < 2; i++ {
		if err := d.Cycle(ctx, rdb, pool); err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
	}
	if got := counter(t, metrics.InvariantViolation); got != before {
		t.Errorf("invariant_violation_total moved on a clean system: %v -> %v", before, got)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM audit_log WHERE action = 'invariant_violation'`); n != 0 {
		t.Errorf("invariant audit rows = %d, want 0", n)
	}
	_ = ctx
}
