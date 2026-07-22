//go:build integration

package worker

import (
	"context"
	"testing"

	"github.com/vinhpnguyen123/ticketqueue/internal/testutil"
)

func TestIdemCleanupDeletesOnlyExpired(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()

	var userID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email, password_hash) VALUES ('cleanup@test.dev', 'x') RETURNING id`,
	).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct{ key, offset string }{
		{"expired-1", "- INTERVAL '1 hour'"},
		{"expired-2", "- INTERVAL '25 hours'"},
		{"fresh-1", "+ INTERVAL '23 hours'"},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO idempotency_keys (key, user_id, request_hash, expires_at)
			 VALUES ($1, $2, 'h', NOW() `+row.offset+`)`, row.key, userID); err != nil {
			t.Fatal(err)
		}
	}

	if err := IdemCleanupCycle(ctx, pool); err != nil {
		t.Fatalf("cycle: %v", err)
	}

	var keys []string
	rows, err := pool.Query(ctx, `SELECT key FROM idempotency_keys ORDER BY key`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		rows.Scan(&k)
		keys = append(keys, k)
	}
	if len(keys) != 1 || keys[0] != "fresh-1" {
		t.Errorf("remaining keys = %v, want [fresh-1]", keys)
	}
}
