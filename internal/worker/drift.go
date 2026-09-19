package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/vinhpnguyen123/ticketqueue/internal/booking"
	"github.com/vinhpnguyen123/ticketqueue/internal/db"
	"github.com/vinhpnguyen123/ticketqueue/internal/metrics"
	"github.com/vinhpnguyen123/ticketqueue/internal/queue"
)

// DriftInterval is how often the drift checker runs.
const DriftInterval = 60 * time.Second

// observation is one tier's disagreement, remembered between cycles.
type observation struct {
	cached int64
	truth  int64
}

// DriftChecker is the only stateful worker: the two-cycle confirmation rule
// needs to remember what the previous cycle saw. The state is deliberately
// in-memory and per-instance — a restart just means the next correction waits
// one more cycle, which is the safe direction to be wrong in.
type DriftChecker struct {
	pending map[string]observation // tier_id → what the last cycle observed
}

func NewDriftChecker() *DriftChecker {
	return &DriftChecker{pending: map[string]observation{}}
}

// Cycle runs all three jobs. They are independent: a failure in one must not
// cost the other two their turn, so each is recovered and its error collected
// rather than returned immediately.
func (d *DriftChecker) Cycle(ctx context.Context, rdb *redis.Client, pool *pgxpool.Pool) error {
	jobs := []struct {
		name string
		run  func() error
	}{
		{"inventory", func() error { return d.checkInventory(ctx, rdb, pool) }},
		{"invariants", func() error { return checkInvariants(ctx, pool) }},
		{"abandoned_queue", func() error { return sweepAbandonedQueueEntries(ctx, rdb, pool) }},
	}

	var failed []string
	for _, job := range jobs {
		if err := runJob(job.name, job.run); err != nil {
			failed = append(failed, job.name)
			slog.Error("drift job failed", "job", job.name, "error", err)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("drift jobs failed: %v", failed)
	}
	return nil
}

// runJob turns a panic into an error so one broken job cannot take the cycle,
// or the process, down with it.
func runJob(name string, run func() error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("job %s panicked: %v", name, p)
		}
	}()
	return run()
}

// ---------------------------------------------------------------- JOB A

// checkInventory compares the Redis fast-path counter against Postgres truth
// for every on-sale tier, and corrects Redis only when the SAME disagreement
// shows up on two consecutive cycles.
//
// The two-cycle rule is the whole point. A TX1 in flight has already decremented
// Redis through the Lua script but has not yet committed its ticket claim, so a
// single snapshot legitimately disagrees with Postgres. Correcting on that first
// observation would write the counter back UP and let the fast path sell into a
// claim that is about to commit — the drift checker would cause the oversell it
// exists to prevent. Two identical consecutive observations mean the work has
// settled and the gap is real.
func (d *DriftChecker) checkInventory(ctx context.Context, rdb *redis.Client, pool *pgxpool.Pool) error {
	rows, err := pool.Query(ctx,
		`SELECT t.id, COUNT(tk.id) FILTER (WHERE tk.status = 'available')
		 FROM ticket_tier t
		 JOIN events e ON e.id = t.event_id
		 LEFT JOIN tickets tk ON tk.ticket_tier_id = t.id
		 WHERE e.status = 'on_sale'
		 GROUP BY t.id`)
	if err != nil {
		return fmt.Errorf("read tier truth: %w", err)
	}
	type tierTruth struct {
		id    string
		truth int64
	}
	var tiers []tierTruth
	for rows.Next() {
		var t tierTruth
		if err := rows.Scan(&t.id, &t.truth); err != nil {
			rows.Close()
			return fmt.Errorf("scan tier truth: %w", err)
		}
		tiers = append(tiers, t)
	}
	rows.Close()
	if rows.Err() != nil {
		return fmt.Errorf("iterate tier truth: %w", rows.Err())
	}

	seen := map[string]bool{}
	for _, t := range tiers {
		seen[t.id] = true
		key := booking.InventoryKey(t.id)

		cached, err := rdb.Get(ctx, key).Int64()
		if errors.Is(err, redis.Nil) {
			// Never initialised (or evicted). Seeding it from truth is not a
			// correction and must not be counted as drift.
			if err := rdb.Set(ctx, key, t.truth, 0).Err(); err != nil {
				return fmt.Errorf("lazy init %s: %w", key, err)
			}
			delete(d.pending, t.id)
			continue
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", key, err)
		}

		if cached == t.truth {
			delete(d.pending, t.id)
			continue
		}

		prev, pending := d.pending[t.id]
		if !pending || prev.cached != cached || prev.truth != t.truth {
			// First sighting, or the numbers moved: could still be in-flight
			// work. Remember and wait one cycle.
			d.pending[t.id] = observation{cached: cached, truth: t.truth}
			slog.Info("inventory disagreement observed, waiting for confirmation",
				"tier_id", t.id, "cached", cached, "truth", t.truth)
			continue
		}

		// Same numbers twice running: real drift. Postgres wins (§3 #1).
		if err := rdb.Set(ctx, key, t.truth, 0).Err(); err != nil {
			return fmt.Errorf("correct %s: %w", key, err)
		}
		delete(d.pending, t.id)
		metrics.InventoryDrift.Inc()
		slog.Warn("inventory drift corrected", "tier_id", t.id, "was", cached, "now", t.truth)

		if err := auditSystem(ctx, pool, "ticket_tier", t.id, "inventory_drift_corrected",
			map[string]any{"source": "drift", "before": cached, "after": t.truth}); err != nil {
			return err
		}
	}

	// Forget tiers that are no longer on sale, so the map cannot grow forever.
	for id := range d.pending {
		if !seen[id] {
			delete(d.pending, id)
		}
	}
	return nil
}

// ---------------------------------------------------------------- JOB B

type invariantCheck struct {
	name  string
	query string
}

// The three structural invariants. Every one of them should always return zero
// rows; a single row means a bug in the system, not a capacity problem.
var invariantChecks = []invariantCheck{
	{"ticket_count_matches_quantity_total", `
		SELECT t.id::text, t.quantity_total::text, COUNT(tk.id)::text
		FROM ticket_tier t
		LEFT JOIN tickets tk ON tk.ticket_tier_id = t.id
		GROUP BY t.id, t.quantity_total
		HAVING COUNT(tk.id) <> t.quantity_total`},

	{"sold_ticket_on_unconfirmed_booking", `
		SELECT tk.id::text, tk.booking_id::text, b.status
		FROM tickets tk
		JOIN booking b ON b.id = tk.booking_id
		WHERE tk.status = 'sold' AND b.status <> 'confirmed'`},

	{"held_ticket_on_dead_booking", `
		SELECT tk.id::text, tk.booking_id::text, b.status
		FROM tickets tk
		JOIN booking b ON b.id = tk.booking_id
		WHERE tk.status = 'held' AND b.status IN ('failed', 'cancelled')`},
}

// checkInvariants DETECTS and ALERTS. It never repairs anything: creating or
// deleting ticket rows is not a worker's job (§9 #5b), and a checker that
// "fixes" what it does not understand destroys the evidence of the real bug.
func checkInvariants(ctx context.Context, pool *pgxpool.Pool) error {
	for _, check := range invariantChecks {
		rows, err := pool.Query(ctx, check.query)
		if err != nil {
			return fmt.Errorf("invariant %s: %w", check.name, err)
		}
		// entityID is what is wrong; expected/actual describe how.
		type violation struct{ entityID, expected, actual string }
		var found []violation
		for rows.Next() {
			var v violation
			if err := rows.Scan(&v.entityID, &v.expected, &v.actual); err != nil {
				rows.Close()
				return fmt.Errorf("scan invariant %s: %w", check.name, err)
			}
			found = append(found, v)
		}
		rows.Close()
		if rows.Err() != nil {
			return fmt.Errorf("iterate invariant %s: %w", check.name, rows.Err())
		}

		for _, v := range found {
			metrics.InvariantViolation.Inc()
			slog.Error("INVARIANT VIOLATION — not auto-fixed, this is a bug",
				"check", check.name, "entity_id", v.entityID, "expected", v.expected, "actual", v.actual)

			entityType := "ticket"
			if check.name == "ticket_count_matches_quantity_total" {
				entityType = "ticket_tier"
			}
			if err := auditSystem(ctx, pool, entityType, v.entityID, "invariant_violation",
				map[string]any{
					"source": "drift", "check": check.name,
					"expected": v.expected, "actual": v.actual,
				}); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------- JOB C

// sweepAbandonedQueueEntries expires people still waiting for an event that is
// no longer selling. It is the safety net: the primary mechanism is
// event.ExpireWaitingEntries, called in the same transaction as the status
// change. This catches paths that forget and events that got stuck.
//
// Postgres first, Redis second (§3 #1): the durable record is the truth, and a
// ZREM lost to a crash leaves a stale member that Position already tolerates.
func sweepAbandonedQueueEntries(ctx context.Context, rdb *redis.Client, pool *pgxpool.Pool) error {
	rows, err := pool.Query(ctx,
		`UPDATE queue_entries qe SET status = 'expired', left_at = NOW()
		 FROM events e
		 WHERE qe.event_id = e.id AND qe.status = 'waiting'
		   AND e.status IN ('sold_out', 'closed', 'cancelled')
		 RETURNING qe.user_id, qe.event_id`)
	if err != nil {
		return fmt.Errorf("expire abandoned queue entries: %w", err)
	}
	type entry struct{ userID, eventID string }
	var expired []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.userID, &e.eventID); err != nil {
			rows.Close()
			return fmt.Errorf("scan abandoned entry: %w", err)
		}
		expired = append(expired, e)
	}
	rows.Close()
	if rows.Err() != nil {
		return fmt.Errorf("iterate abandoned entries: %w", rows.Err())
	}
	if len(expired) == 0 {
		return nil
	}

	for _, e := range expired {
		if err := auditSystem(ctx, pool, "event", e.eventID, "queue_entry_abandoned",
			map[string]any{"source": "drift", "user_id": e.userID}); err != nil {
			return err
		}
	}

	// After the write is durable, drop the members from the waiting set.
	byEvent := map[string][]any{}
	for _, e := range expired {
		byEvent[e.eventID] = append(byEvent[e.eventID], e.userID)
	}
	for eventID, members := range byEvent {
		if err := rdb.ZRem(ctx, queue.Key(eventID), members...).Err(); err != nil {
			slog.Error("failed to remove abandoned members from queue set",
				"event_id", eventID, "count", len(members), "error", err)
		}
	}

	metrics.WorkerItems.WithLabelValues(metrics.WorkerDrift).Add(float64(len(expired)))
	slog.Info("expired abandoned queue entries", "count", len(expired))
	return nil
}

// auditSystem writes one audit row for a worker action outside the booking
// lifecycle. actor_type is 'worker' per §9; metadata.source names the job, the
// same convention the finalize paths use via Actor.Source.
func auditSystem(ctx context.Context, pool *pgxpool.Pool, entityType, entityID, action string, metadata map[string]any) error {
	md, err := db.JSONB(metadata)
	if err != nil {
		return fmt.Errorf("audit %s: %w", action, err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO audit_log (entity_type, entity_id, action, actor_type, metadata)
		 VALUES ($1, $2, $3, 'worker', $4::jsonb)`,
		entityType, entityID, action, md); err != nil {
		return fmt.Errorf("audit %s: %w", action, err)
	}
	return nil
}
