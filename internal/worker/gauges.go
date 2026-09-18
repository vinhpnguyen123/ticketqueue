package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vinhpnguyen123/ticketqueue/internal/booking"
	"github.com/vinhpnguyen123/ticketqueue/internal/metrics"
)

// GaugeInterval is how often the money-at-risk gauges are sampled.
//
// Sampling is deliberately independent of every worker cycle. Reconciliation
// runs every 30s and hold-expiry every 5s, so a gauge updated inside a cycle
// would draw a sawtooth that reads as a problem during load when it is only an
// artefact of the sampling schedule.
const GaugeInterval = 5 * time.Second

// SampleGauges keeps the money-at-risk gauges current until ctx is cancelled.
//
// A failed query never takes the binary down and never zeroes a gauge: it logs
// and leaves the last known value, because a metrics goroutine that can kill a
// worker is worse than a stale number on a dashboard.
func SampleGauges(ctx context.Context, pool *pgxpool.Pool) {
	ticker := time.NewTicker(GaugeInterval)
	defer ticker.Stop()
	for {
		SampleGaugesOnce(ctx, pool)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// SampleGaugesOnce runs one sampling pass. Exported so tests can drive it
// deterministically instead of waiting on the ticker.
func SampleGaugesOnce(ctx context.Context, pool *pgxpool.Pool) {
	if n, err := booking.BookingsHeldCount(ctx, pool); err != nil {
		if ctx.Err() == nil {
			slog.Error("gauge sample failed, keeping last value", "gauge", "bookings_held_current", "error", err)
		}
	} else {
		metrics.BookingsHeld.Set(float64(n))
	}

	// Same query definitions the admin stuck-list endpoint uses, so the
	// dashboard and the API can never disagree about what is stuck.
	counts, err := booking.CountStuck(ctx, pool, booking.StuckAfter)
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("gauge sample failed, keeping last value", "gauge", "admin_stuck_bookings_current", "error", err)
		}
		return
	}
	for category, n := range counts {
		metrics.AdminStuckBookings.WithLabelValues(category).Set(float64(n))
	}
}
