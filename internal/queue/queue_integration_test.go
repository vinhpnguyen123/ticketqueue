//go:build integration

package queue_test

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

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

// closedPool returns a pool on the same database that fails every query.
func closedPool(t *testing.T, pool *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	dead, err := pgxpool.New(context.Background(), pool.Config().ConnString())
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	dead.Close()
	return dead
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

func joinAll(t *testing.T, pool *pgxpool.Pool, rdb *redis.Client, eventID string, users []string) {
	t.Helper()
	for i, u := range users {
		// Guarantee distinct joined_at micros so join order is unambiguous.
		time.Sleep(2 * time.Millisecond)
		pos, already, err := queue.Join(context.Background(), rdb, pool, u, eventID)
		if err != nil || already || pos != int64(i+1) {
			t.Fatalf("join user %d: pos=%d already=%v err=%v", i, pos, already, err)
		}
	}
}

func TestSyncRestoresOrderAfterFlush(t *testing.T) {
	pool := testutil.TestDB(t)
	rdb := testutil.TestRedis(t)
	ctx := context.Background()
	eventID := createEvent(t, pool, "on_sale")
	users := createUsers(t, pool, 3)
	joinAll(t, pool, rdb, eventID, users)

	// Simulate a Redis crash losing everything.
	if err := rdb.FlushDB(ctx).Err(); err != nil {
		t.Fatalf("flushdb: %v", err)
	}
	if err := queue.SyncFromPostgres(ctx, rdb, pool, eventID); err != nil {
		t.Fatalf("sync: %v", err)
	}

	got, err := rdb.ZRange(ctx, queue.Key(eventID), 0, -1).Result()
	if err != nil {
		t.Fatalf("zrange: %v", err)
	}
	if !slices.Equal(got, users) {
		t.Errorf("order after sync = %v, want %v", got, users)
	}
}

// The old rebuild deleted the key first, so this emptied a healthy queue.
func TestSyncQueryFailureKeepsMembers(t *testing.T) {
	pool := testutil.TestDB(t)
	rdb := testutil.TestRedis(t)
	ctx := context.Background()
	eventID := createEvent(t, pool, "on_sale")
	joinAll(t, pool, rdb, eventID, createUsers(t, pool, 3))

	if err := queue.SyncFromPostgres(ctx, rdb, closedPool(t, pool), eventID); err == nil {
		t.Fatal("sync with a dead database returned nil")
	}
	if size, err := rdb.ZCard(ctx, queue.Key(eventID)).Result(); err != nil || size != 3 {
		t.Errorf("zcard = %d, err = %v, want 3 untouched", size, err)
	}
}

func TestSyncRemovesDepartedMember(t *testing.T) {
	pool := testutil.TestDB(t)
	rdb := testutil.TestRedis(t)
	ctx := context.Background()
	eventID := createEvent(t, pool, "on_sale")
	users := createUsers(t, pool, 2)
	joinAll(t, pool, rdb, eventID, users)
	if _, err := pool.Exec(ctx,
		`UPDATE queue_entries SET status = 'left', left_at = NOW() WHERE user_id = $1`, users[0]); err != nil {
		t.Fatal(err)
	}

	if err := queue.SyncFromPostgres(ctx, rdb, pool, eventID); err != nil {
		t.Fatalf("sync: %v", err)
	}
	got, err := rdb.ZRange(ctx, queue.Key(eventID), 0, -1).Result()
	if err != nil {
		t.Fatalf("zrange: %v", err)
	}
	if !slices.Equal(got, users[1:]) {
		t.Errorf("members = %v, want only %v", got, users[1:])
	}
}

func TestSyncKeepsWaitingMembersAndScores(t *testing.T) {
	pool := testutil.TestDB(t)
	rdb := testutil.TestRedis(t)
	ctx := context.Background()
	eventID := createEvent(t, pool, "on_sale")
	users := createUsers(t, pool, 2)
	joinAll(t, pool, rdb, eventID, users)
	before, err := rdb.ZScore(ctx, queue.Key(eventID), users[0]).Result()
	if err != nil {
		t.Fatal(err)
	}

	if err := queue.SyncFromPostgres(ctx, rdb, pool, eventID); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if after, err := rdb.ZScore(ctx, queue.Key(eventID), users[0]).Result(); err != nil || after != before {
		t.Errorf("score = %v, err = %v, want unchanged %v", after, err, before)
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

// Postgres first: a failed leave must leave both stores saying "still in the
// queue", so a retry works and nothing disagrees.
func TestLeaveDBFailureKeepsRedisMember(t *testing.T) {
	pool := testutil.TestDB(t)
	rdb := testutil.TestRedis(t)
	ctx := context.Background()
	eventID := createEvent(t, pool, "on_sale")
	users := createUsers(t, pool, 1)
	joinAll(t, pool, rdb, eventID, users)

	if err := queue.Leave(ctx, rdb, closedPool(t, pool), users[0], eventID); err == nil {
		t.Fatal("leave with a dead database returned nil")
	}
	if _, err := rdb.ZScore(ctx, queue.Key(eventID), users[0]).Result(); err != nil {
		t.Fatalf("member gone after a failed leave: %v", err)
	}

	if err := queue.Leave(ctx, rdb, pool, users[0], eventID); err != nil {
		t.Fatalf("retry: %v", err)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM queue_entries WHERE user_id = $1`, users[0]).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "left" {
		t.Errorf("status = %s, want left", status)
	}
	if n, err := rdb.ZCard(ctx, queue.Key(eventID)).Result(); err != nil || n != 0 {
		t.Errorf("zcard = %d, err = %v, want 0", n, err)
	}
}

func auditCount(t *testing.T, pool *pgxpool.Pool, eventID, action string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM audit_log WHERE entity_id = $1 AND action = $2`, eventID, action).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Promotion reads Postgres, so once the row exists the user IS queued and can be
// admitted. Reporting failure, or losing the audit row, would both be false.
func TestJoinSucceedsAndAuditsWithRedisDown(t *testing.T) {
	pool := testutil.TestDB(t)
	rdb := testutil.TestRedis(t)
	ctx := context.Background()
	eventID := createEvent(t, pool, "on_sale")
	userID := createUsers(t, pool, 1)[0]
	dead := redis.NewClient(&redis.Options{Addr: "localhost:1"})
	defer dead.Close()

	pos, already, err := queue.Join(ctx, dead, pool, userID, eventID)
	if err != nil || already || pos != 0 {
		t.Fatalf("join = pos %d already %v err %v, want 0, false, nil", pos, already, err)
	}
	if got := auditCount(t, pool, eventID, "queue_joined"); got != 1 {
		t.Errorf("queue_joined rows = %d, want 1", got)
	}

	pos, already, err = queue.Join(ctx, rdb, pool, userID, eventID)
	if err != nil || !already || pos != 1 {
		t.Fatalf("retry = pos %d already %v err %v, want 1, true, nil", pos, already, err)
	}
	if got := auditCount(t, pool, eventID, "queue_joined"); got != 1 {
		t.Errorf("queue_joined rows after retry = %d, want 1", got)
	}
}

func TestLeaveAuditsWithRedisDown(t *testing.T) {
	pool := testutil.TestDB(t)
	rdb := testutil.TestRedis(t)
	ctx := context.Background()
	eventID := createEvent(t, pool, "on_sale")
	users := createUsers(t, pool, 1)
	joinAll(t, pool, rdb, eventID, users)
	dead := redis.NewClient(&redis.Options{Addr: "localhost:1"})
	defer dead.Close()

	if err := queue.Leave(ctx, dead, pool, users[0], eventID); err != nil {
		t.Fatalf("leave: %v", err)
	}
	if got := auditCount(t, pool, eventID, "queue_left"); got != 1 {
		t.Errorf("queue_left rows = %d, want 1", got)
	}
}
