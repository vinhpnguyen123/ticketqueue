// Package queue implements the virtual waiting queue: a Redis sorted set
// is the hot path for position lookups, and the queue_entries table in
// Postgres is the durable mirror used to rebuild Redis after a crash.
//
// Redis key conventions:
//
//	queue:{event_id} — sorted set; member = user_id,
//	                   score = join timestamp as float64 unix micros
package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// ErrNotInQueue is returned by Leave when the user has no active queue entry.
var ErrNotInQueue = errors.New("not in queue")

// Key returns the Redis sorted-set key for an event's queue.
func Key(eventID string) string {
	return "queue:" + eventID
}

// Join adds a user to an event's queue and returns their 1-based position.
// alreadyInQueue is true when the user had an active entry before this call
// (the join is idempotent — callers use this to pick 200 vs 201).
//
// Postgres is written FIRST, then Redis: the durable record must exist
// before the fast path. If the Redis write fails after the Postgres insert,
// RebuildFromPostgres will restore the member; if Postgres fails, nothing
// entered Redis, so there is no ghost queue member that would vanish on the
// next rebuild.
func Join(ctx context.Context, rdb *redis.Client, db *pgxpool.Pool, userID, eventID string) (position int64, alreadyInQueue bool, err error) {
	var joinedAt time.Time
	err = db.QueryRow(ctx,
		`INSERT INTO queue_entries (user_id, event_id, status)
		 VALUES ($1, $2, 'waiting')
		 RETURNING joined_at`,
		userID, eventID,
	).Scan(&joinedAt)

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		// idx_queue_active unique violation: user already has an active
		// entry. Fetch its original joined_at so the ZADD NX below can
		// self-heal a missing Redis member with the correct score.
		alreadyInQueue = true
		err = db.QueryRow(ctx,
			`SELECT joined_at FROM queue_entries
			 WHERE user_id = $1 AND event_id = $2 AND status IN ('waiting', 'admitted')`,
			userID, eventID,
		).Scan(&joinedAt)
	}
	if err != nil {
		return 0, false, fmt.Errorf("insert queue entry: %w", err)
	}

	// Score is the Postgres joined_at as unix micros so the hot path and
	// RebuildFromPostgres produce identical orderings. NX keeps an existing
	// member's score untouched on idempotent re-joins.
	if err := rdb.ZAddNX(ctx, Key(eventID), redis.Z{
		Score:  float64(joinedAt.UnixMicro()),
		Member: userID,
	}).Err(); err != nil {
		return 0, false, fmt.Errorf("zadd queue member: %w", err)
	}

	rank, err := rdb.ZRank(ctx, Key(eventID), userID).Result()
	if err != nil {
		return 0, false, fmt.Errorf("zrank queue member: %w", err)
	}

	if !alreadyInQueue {
		audit(ctx, db, userID, eventID, "queue_joined")
	}
	return rank + 1, alreadyInQueue, nil
}

// Position returns the user's 0-based rank and the queue size from Redis
// only (no Postgres). found is false when the user is not in the sorted set.
func Position(ctx context.Context, rdb *redis.Client, userID, eventID string) (rank int64, size int64, found bool, err error) {
	rank, err = rdb.ZRank(ctx, Key(eventID), userID).Result()
	if errors.Is(err, redis.Nil) {
		found = false
	} else if err != nil {
		return 0, 0, false, fmt.Errorf("zrank: %w", err)
	} else {
		found = true
	}

	size, err = rdb.ZCard(ctx, Key(eventID)).Result()
	if err != nil {
		return 0, 0, false, fmt.Errorf("zcard: %w", err)
	}
	return rank, size, found, nil
}

// Leave removes the user from the Redis set and marks their active
// queue_entries row as left. Returns ErrNotInQueue when no active row exists.
func Leave(ctx context.Context, rdb *redis.Client, db *pgxpool.Pool, userID, eventID string) error {
	if err := rdb.ZRem(ctx, Key(eventID), userID).Err(); err != nil {
		return fmt.Errorf("zrem queue member: %w", err)
	}

	tag, err := db.Exec(ctx,
		`UPDATE queue_entries SET status = 'left', left_at = NOW()
		 WHERE user_id = $1 AND event_id = $2 AND status IN ('waiting', 'admitted')`,
		userID, eventID,
	)
	if err != nil {
		return fmt.Errorf("update queue entry: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotInQueue
	}

	audit(ctx, db, userID, eventID, "queue_left")
	return nil
}

// RebuildFromPostgres wipes the event's Redis queue and repopulates it from
// the durable queue_entries mirror, preserving the original join order.
// This is the Redis-crash recovery path.
func RebuildFromPostgres(ctx context.Context, rdb *redis.Client, db *pgxpool.Pool, eventID string) error {
	if err := rdb.Del(ctx, Key(eventID)).Err(); err != nil {
		return fmt.Errorf("del queue key: %w", err)
	}

	rows, err := db.Query(ctx,
		`SELECT user_id, joined_at FROM queue_entries
		 WHERE event_id = $1 AND status = 'waiting'
		 ORDER BY joined_at`,
		eventID,
	)
	if err != nil {
		return fmt.Errorf("select waiting entries: %w", err)
	}
	defer rows.Close()

	var members []redis.Z
	for rows.Next() {
		var userID string
		var joinedAt time.Time
		if err := rows.Scan(&userID, &joinedAt); err != nil {
			return fmt.Errorf("scan waiting entry: %w", err)
		}
		members = append(members, redis.Z{
			Score:  float64(joinedAt.UnixMicro()),
			Member: userID,
		})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("select waiting entries: %w", err)
	}
	if len(members) == 0 {
		return nil
	}

	if err := rdb.ZAdd(ctx, Key(eventID), members...).Err(); err != nil {
		return fmt.Errorf("zadd rebuilt members: %w", err)
	}
	return nil
}

func audit(ctx context.Context, db *pgxpool.Pool, userID, eventID, action string) {
	if _, err := db.Exec(ctx,
		`INSERT INTO audit_log (entity_type, entity_id, action, actor_type, actor_user_id, request_id)
		 VALUES ('event', $1, $2, 'user', $3, $4)`,
		eventID, action, userID, middleware.GetReqID(ctx),
	); err != nil {
		// Audit failures never fail the queue operation.
		slog.Error("audit log insert failed", "action", action, "error", err)
	}
}
