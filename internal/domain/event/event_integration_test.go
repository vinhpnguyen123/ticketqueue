//go:build integration

package event_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/vinhpnguyen123/ticketqueue/internal/domain/event"
	"github.com/vinhpnguyen123/ticketqueue/internal/testutil"
)

// ExpireWaitingEntries is the primary mechanism for closing a queue; the drift
// checker's sweep is only the safety net. It takes a pgx.Tx precisely so it
// cannot be called outside the transaction that changes the event status.
func TestExpireWaitingEntries(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()

	var eventID, otherID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO events (name, venue, event_at, status)
		 VALUES ('Closing', 'Venue', NOW() + INTERVAL '30 days', 'on_sale') RETURNING id`).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO events (name, venue, event_at, status)
		 VALUES ('Untouched', 'Venue', NOW() + INTERVAL '30 days', 'on_sale') RETURNING id`).Scan(&otherID); err != nil {
		t.Fatal(err)
	}

	// Two waiting and one already admitted on the closing event, one waiting on
	// the other event.
	for i, spec := range []struct {
		eventID, status string
	}{
		{eventID, "waiting"}, {eventID, "waiting"}, {eventID, "admitted"}, {otherID, "waiting"},
	} {
		var userID string
		if err := pool.QueryRow(ctx,
			`INSERT INTO users (email, password_hash) VALUES ($1, 'x') RETURNING id`,
			"closer"+string(rune('a'+i))+"@test.dev").Scan(&userID); err != nil {
			t.Fatal(err)
		}
		admitted := "NULL"
		if spec.status == "admitted" {
			admitted = "NOW()"
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO queue_entries (user_id, event_id, status, admitted_at)
			 VALUES ($1, $2, $3, `+admitted+`)`, userID, spec.eventID, spec.status); err != nil {
			t.Fatal(err)
		}
	}

	var expired int
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE events SET status = 'sold_out' WHERE id = $1`, eventID); err != nil {
			return err
		}
		n, err := event.ExpireWaitingEntries(ctx, tx, eventID)
		expired = n
		return err
	}); err != nil {
		t.Fatalf("transaction: %v", err)
	}

	if expired != 2 {
		t.Errorf("expired = %d, want 2 (only the waiting ones)", expired)
	}

	var waiting, admittedCount int
	pool.QueryRow(ctx,
		`SELECT COUNT(*) FILTER (WHERE status = 'waiting'), COUNT(*) FILTER (WHERE status = 'admitted')
		 FROM queue_entries WHERE event_id = $1`, eventID).Scan(&waiting, &admittedCount)
	if waiting != 0 {
		t.Errorf("waiting entries left = %d, want 0", waiting)
	}
	if admittedCount != 1 {
		t.Errorf("admitted entries = %d, want 1 (an admitted user keeps their window)", admittedCount)
	}

	var otherWaiting int
	pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM queue_entries WHERE event_id = $1 AND status = 'waiting'`, otherID).Scan(&otherWaiting)
	if otherWaiting != 1 {
		t.Errorf("other event's queue was touched: %d waiting, want 1", otherWaiting)
	}

	var nullLeftAt int
	pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM queue_entries WHERE event_id = $1 AND status = 'expired' AND left_at IS NULL`,
		eventID).Scan(&nullLeftAt)
	if nullLeftAt != 0 {
		t.Errorf("%d expired entries have no left_at", nullLeftAt)
	}
}

// A rollback must take the queue cleanup with it: the whole point of taking a
// Tx is that the status change and the cleanup share one fate.
func TestExpireWaitingEntriesRollsBackWithItsTransaction(t *testing.T) {
	pool := testutil.TestDB(t)
	ctx := context.Background()

	var eventID, userID string
	pool.QueryRow(ctx,
		`INSERT INTO events (name, venue, event_at, status)
		 VALUES ('Rollback', 'Venue', NOW() + INTERVAL '30 days', 'on_sale') RETURNING id`).Scan(&eventID)
	pool.QueryRow(ctx,
		`INSERT INTO users (email, password_hash) VALUES ('rollback@test.dev', 'x') RETURNING id`).Scan(&userID)
	if _, err := pool.Exec(ctx,
		`INSERT INTO queue_entries (user_id, event_id, status) VALUES ($1, $2, 'waiting')`,
		userID, eventID); err != nil {
		t.Fatal(err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := event.ExpireWaitingEntries(ctx, tx, eventID); err != nil {
		t.Fatalf("expire: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	var status string
	pool.QueryRow(ctx,
		`SELECT status FROM queue_entries WHERE event_id = $1`, eventID).Scan(&status)
	if status != "waiting" {
		t.Errorf("entry status = %s after rollback, want waiting", status)
	}
}
