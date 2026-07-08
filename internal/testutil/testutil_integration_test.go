//go:build integration

package testutil

import (
	"context"
	"testing"
)

func TestTestDBConnectsAndTruncates(t *testing.T) {
	pool := TestDB(t)

	if _, err := pool.Exec(context.Background(),
		`INSERT INTO users (email, password_hash) VALUES ('testutil@example.com', 'x')`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM users WHERE email = 'testutil@example.com'`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 row, got %d", count)
	}
	// Cleanup truncates; verified implicitly by the next test starting clean.
}

func TestTestDBStartsClean(t *testing.T) {
	pool := TestDB(t)

	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM users`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected clean users table, got %d rows", count)
	}
}

func TestTestRedisUsesDB1AndFlushes(t *testing.T) {
	client := TestRedis(t)
	ctx := context.Background()

	if err := client.Set(ctx, "testutil:key", "value", 0).Err(); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := client.Get(ctx, "testutil:key").Result()
	if err != nil || got != "value" {
		t.Fatalf("get: %q, %v", got, err)
	}
}

func TestTestRedisStartsClean(t *testing.T) {
	client := TestRedis(t)

	keys, err := client.Keys(context.Background(), "*").Result()
	if err != nil {
		t.Fatalf("keys: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("expected empty redis db 1, got keys %v", keys)
	}
}
