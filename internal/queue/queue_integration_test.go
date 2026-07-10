//go:build integration

package queue_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vinhpnguyen123/ticketqueue/internal/queue"
	"github.com/vinhpnguyen123/ticketqueue/internal/testutil"
)

func createUsers(t *testing.T, pool *pgxpool.Pool, n int) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`INSERT INTO users (email, password_hash)
		 SELECT 'queue-user-' || n || '@test.dev', 'not-a-real-hash'
		 FROM generate_series(1, $1) n
		 RETURNING id`, n)
	if err != nil {
		t.Fatalf("insert users: %v", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan user id: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("insert users: %v", err)
	}
	return ids
}

func createEvent(t *testing.T, pool *pgxpool.Pool, status string) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(),
		`INSERT INTO events (name, venue, event_at, status)
		 VALUES ('Queue Event', 'Test Venue', $1, $2) RETURNING id`,
		time.Now().Add(30*24*time.Hour), status,
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert event: %v", err)
	}
	return id
}

func TestRebuildFromPostgres(t *testing.T) {
	pool := testutil.TestDB(t)
	rdb := testutil.TestRedis(t)
	ctx := context.Background()

	eventID := createEvent(t, pool, "on_sale")
	users := createUsers(t, pool, 3)
	for i, u := range users {
		// Guarantee distinct joined_at micros so join order is unambiguous.
		time.Sleep(2 * time.Millisecond)
		pos, already, err := queue.Join(ctx, rdb, pool, u, eventID)
		if err != nil || already || pos != int64(i+1) {
			t.Fatalf("join user %d: pos=%d already=%v err=%v", i, pos, already, err)
		}
	}

	// Simulate a Redis crash losing everything.
	if err := rdb.FlushDB(ctx).Err(); err != nil {
		t.Fatalf("flushdb: %v", err)
	}
	if err := queue.RebuildFromPostgres(ctx, rdb, pool, eventID); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	size, err := rdb.ZCard(ctx, queue.Key(eventID)).Result()
	if err != nil || size != 3 {
		t.Fatalf("zcard after rebuild = %d, err = %v, want 3", size, err)
	}
	for i, u := range users {
		rank, err := rdb.ZRank(ctx, queue.Key(eventID), u).Result()
		if err != nil {
			t.Fatalf("zrank user %d: %v", i, err)
		}
		if rank != int64(i) {
			t.Errorf("user %d rank after rebuild = %d, want %d (order not preserved)", i, rank, i)
		}
	}
}

func TestConcurrentJoin(t *testing.T) {
	pool := testutil.TestDB(t)
	rdb := testutil.TestRedis(t)
	ctx := context.Background()

	const n = 100
	eventID := createEvent(t, pool, "on_sale")
	users := createUsers(t, pool, n)

	var wg sync.WaitGroup
	errs := make(chan error, n)
	start := make(chan struct{})
	for _, u := range users {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // maximize simultaneity
			if _, _, err := queue.Join(ctx, rdb, pool, u, eventID); err != nil {
				errs <- fmt.Errorf("user %s: %w", u, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("join error surfaced to caller: %v", err)
	}

	var rowCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM queue_entries WHERE event_id = $1`, eventID,
	).Scan(&rowCount); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rowCount != n {
		t.Errorf("queue_entries rows = %d, want %d", rowCount, n)
	}

	size, err := rdb.ZCard(ctx, queue.Key(eventID)).Result()
	if err != nil || size != n {
		t.Fatalf("zcard = %d, err = %v, want %d", size, err, n)
	}

	seen := make(map[int64]bool, n)
	for _, u := range users {
		rank, err := rdb.ZRank(ctx, queue.Key(eventID), u).Result()
		if err != nil {
			t.Fatalf("zrank %s: %v", u, err)
		}
		if rank < 0 || rank >= n {
			t.Errorf("rank %d out of range [0,%d)", rank, n)
		}
		if seen[rank] {
			t.Errorf("duplicate rank %d", rank)
		}
		seen[rank] = true
	}
}
