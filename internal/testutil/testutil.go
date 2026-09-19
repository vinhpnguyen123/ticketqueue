// Package testutil provides helpers for integration tests that need a
// real Postgres (ticketqueue_test database) or Redis (DB index 1).
package testutil

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/redis/go-redis/v9"
	"github.com/vinhpnguyen123/ticketqueue/internal/db"
)

// tables lists every table in the schema, truncated between tests.
var tables = []string{
	"users", "events", "ticket_tier", "booking", "tickets",
	"queue_entries", "idempotency_keys", "payments", "audit_log",
	"webhook_events_seen",
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// TestDB connects to the ticketqueue_test database and registers a
// cleanup that truncates all tables so each test starts clean.
func TestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()

	url := getenv("TEST_DATABASE_URL",
		"postgres://ticketqueue:localdev@localhost:5434/ticketqueue_test?sslmode=disable")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// db.NewPool, not pgxpool.New: the suite must build its pool exactly the
	// way the binaries do, or it cannot catch a configuration that only breaks
	// in production. Specifically, PGBOUNCER=true switches pgx to simple
	// protocol; a suite that bypassed that would pass against PgBouncer while
	// the deployed code failed under concurrency.
	pool, err := db.NewPool(ctx, url)
	if err != nil {
		t.Fatalf("connect to test database (is docker compose up and migrate-test-up applied?): %v", err)
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, table := range tables {
			if _, err := pool.Exec(ctx, "TRUNCATE "+table+" RESTART IDENTITY CASCADE"); err != nil {
				t.Errorf("truncate %s: %v", table, err)
			}
		}
		pool.Close()
	})

	return pool
}

// TestRedis connects to local Redis on DB index 1 (never 0, which local
// dev uses) and registers a cleanup that flushes that index.
func TestRedis(t *testing.T) *redis.Client {
	t.Helper()

	client := redis.NewClient(&redis.Options{
		Addr: getenv("TEST_REDIS_ADDR", "localhost:6379"),
		DB:   1,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		client.Close()
		t.Fatalf("ping test redis (is docker compose up?): %v", err)
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := client.FlushDB(ctx).Err(); err != nil {
			t.Errorf("flush test redis db: %v", err)
		}
		client.Close()
	})

	return client
}
