//go:build integration

package worker

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/vinhpnguyen123/ticketqueue/internal/queue"
	"github.com/vinhpnguyen123/ticketqueue/internal/testutil"
)

func init() {
	jitter = func() {} // no sleeps in tests
}

// seedEvent inserts an on_sale event with the given window capacity.
func seedEvent(t *testing.T, pool *pgxpool.Pool, maxWindows, windowSeconds int) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO events (name, venue, event_at, status, max_concurrent_windows, purchase_window_seconds)
		 VALUES ('Promo Test', 'Venue', NOW() + INTERVAL '30 days', 'on_sale', $1, $2) RETURNING id`,
		maxWindows, windowSeconds,
	).Scan(&id); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	return id
}

// seedUsers creates n users and joins them to the event in order,
// returning their IDs in join order.
func seedUsers(t *testing.T, pool *pgxpool.Pool, rdb *redis.Client, eventID string, n int) []string {
	t.Helper()
	ctx := context.Background()
	ids := make([]string, n)
	for i := range ids {
		if err := pool.QueryRow(ctx,
			`INSERT INTO users (email, password_hash) VALUES ($1, 'x') RETURNING id`,
			fmt.Sprintf("promo%d@test.dev", i),
		).Scan(&ids[i]); err != nil {
			t.Fatalf("insert user: %v", err)
		}
		if _, _, err := queue.Join(ctx, rdb, pool, ids[i], eventID); err != nil {
			t.Fatalf("join queue: %v", err)
		}
		time.Sleep(2 * time.Millisecond) // distinct joined_at ordering
	}
	return ids
}

func entryStatus(t *testing.T, pool *pgxpool.Pool, userID, eventID string) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM queue_entries WHERE user_id = $1 AND event_id = $2`,
		userID, eventID,
	).Scan(&s); err != nil {
		t.Fatalf("entry status: %v", err)
	}
	return s
}

func TestPromoteFIFOUpToCapacity(t *testing.T) {
	pool := testutil.TestDB(t)
	rdb := testutil.TestRedis(t)
	ctx := context.Background()

	eventID := seedEvent(t, pool, 2, 300)
	users := seedUsers(t, pool, rdb, eventID, 3)

	if err := PromoteCycle(ctx, rdb, pool); err != nil {
		t.Fatalf("cycle: %v", err)
	}

	for i, want := range []string{"admitted", "admitted", "waiting"} {
		if got := entryStatus(t, pool, users[i], eventID); got != want {
			t.Errorf("user %d status = %q, want %q", i+1, got, want)
		}
	}
	if card := rdb.ZCard(ctx, queue.Key(eventID)).Val(); card != 1 {
		t.Errorf("ZCARD = %d, want 1", card)
	}

	var audits int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM audit_log WHERE action = 'queue_admitted' AND actor_type = 'worker'`,
	).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 2 {
		t.Errorf("queue_admitted audit rows = %d, want 2", audits)
	}

	// Second cycle: both windows occupied, nobody admitted.
	if err := PromoteCycle(ctx, rdb, pool); err != nil {
		t.Fatalf("second cycle: %v", err)
	}
	if got := entryStatus(t, pool, users[2], eventID); got != "waiting" {
		t.Errorf("user 3 after full-capacity cycle = %q, want waiting", got)
	}
}

func TestExpiredWindowFreesSlot(t *testing.T) {
	pool := testutil.TestDB(t)
	rdb := testutil.TestRedis(t)
	ctx := context.Background()

	eventID := seedEvent(t, pool, 2, 300)
	users := seedUsers(t, pool, rdb, eventID, 3)

	if err := PromoteCycle(ctx, rdb, pool); err != nil {
		t.Fatalf("cycle: %v", err)
	}

	// Age user 1's window past purchase_window_seconds.
	if _, err := pool.Exec(ctx,
		`UPDATE queue_entries SET admitted_at = NOW() - INTERVAL '301 seconds'
		 WHERE user_id = $1 AND event_id = $2`, users[0], eventID); err != nil {
		t.Fatalf("age window: %v", err)
	}

	// This cycle sweeps the expired window; the next admits user 3 into
	// the freed slot (sweep runs after admission within a cycle).
	for i := 0; i < 2; i++ {
		if err := PromoteCycle(ctx, rdb, pool); err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
	}

	for i, want := range []string{"expired", "admitted", "admitted"} {
		if got := entryStatus(t, pool, users[i], eventID); got != want {
			t.Errorf("user %d status = %q, want %q", i+1, got, want)
		}
	}

	var audits int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM audit_log WHERE action = 'queue_window_expired' AND actor_type = 'worker'`,
	).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 1 {
		t.Errorf("queue_window_expired audit rows = %d, want 1", audits)
	}
}

func TestPromoteSkipsUserWhoLeft(t *testing.T) {
	pool := testutil.TestDB(t)
	rdb := testutil.TestRedis(t)
	ctx := context.Background()

	eventID := seedEvent(t, pool, 2, 300)
	users := seedUsers(t, pool, rdb, eventID, 2)

	// User 1's durable row flips to 'left' but the Redis member remains
	// (simulates leaving between ZPOPMIN and the guarded update).
	if _, err := pool.Exec(ctx,
		`UPDATE queue_entries SET status = 'left', left_at = NOW()
		 WHERE user_id = $1 AND event_id = $2`, users[0], eventID); err != nil {
		t.Fatal(err)
	}

	if err := PromoteCycle(ctx, rdb, pool); err != nil {
		t.Fatalf("cycle: %v", err)
	}

	if got := entryStatus(t, pool, users[0], eventID); got != "left" {
		t.Errorf("left user status = %q, want left (must not be admitted)", got)
	}
	if got := entryStatus(t, pool, users[1], eventID); got != "admitted" {
		t.Errorf("user 2 status = %q, want admitted (batch must continue)", got)
	}
}
