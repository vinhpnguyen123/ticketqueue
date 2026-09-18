// Package worker provides the background workers (promotion, and later
// reconciliation / hold-expiration) plus the shared Runner loop they all use.
package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/vinhpnguyen123/ticketqueue/internal/metrics"
)

// Runner runs fn every interval until ctx is cancelled. A panic inside fn
// is recovered and logged — one bad cycle must never kill the process.
type Runner struct {
	name     string
	interval time.Duration
	fn       func(context.Context) error
}

func NewRunner(name string, interval time.Duration, fn func(context.Context) error) *Runner {
	return &Runner{name: name, interval: interval, fn: fn}
}

// Start blocks, running one cycle immediately and then one per interval,
// until ctx is cancelled.
func (r *Runner) Start(ctx context.Context) {
	slog.Info("worker started", "worker", r.name, "interval", r.interval)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		r.runCycle(ctx)
		select {
		case <-ctx.Done():
			slog.Info("worker stopped", "worker", r.name)
			return
		case <-ticker.C:
		}
	}
}

func (r *Runner) runCycle(ctx context.Context) {
	defer func() {
		if p := recover(); p != nil {
			slog.Error("worker cycle panicked", "worker", r.name, "panic", p)
		}
	}()

	start := time.Now()
	err := r.fn(ctx)
	elapsed := time.Since(start)
	metrics.WorkerCycleDuration.WithLabelValues(r.name).Observe(elapsed.Seconds())
	if err != nil && ctx.Err() == nil {
		// A deferral is not an error and is not counted here; only genuine
		// failures reach this branch.
		metrics.WorkerErrors.WithLabelValues(r.name).Inc()
		slog.Error("worker cycle failed", "worker", r.name, "duration", elapsed, "error", err)
		return
	}
	slog.Info("worker cycle done", "worker", r.name, "duration", elapsed)
}
