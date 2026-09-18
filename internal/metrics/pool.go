package metrics

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolSampleInterval is how often connection-pool stats are read.
const PoolSampleInterval = 5 * time.Second

// SamplePool keeps the connection-pool gauges current until ctx is cancelled.
// Pool saturation is the most likely first bottleneck at 10k VU, and it is
// invisible in request latency alone: the query is fast, the wait for a
// connection is not.
//
// pgxpool only exposes cumulative totals, so the two counters are fed by deltas
// between samples rather than by per-acquire observations.
func SamplePool(ctx context.Context, pool *pgxpool.Pool) {
	var lastWait time.Duration
	var lastEmpty int64

	ticker := time.NewTicker(PoolSampleInterval)
	defer ticker.Stop()
	for {
		stat := pool.Stat()
		DBPoolInUse.Set(float64(stat.AcquiredConns()))
		DBPoolIdle.Set(float64(stat.IdleConns()))
		DBPoolMax.Set(float64(stat.MaxConns()))

		// Counters only move forward; a pool reset would make a delta negative,
		// so clamp rather than corrupt the series.
		if wait := stat.AcquireDuration(); wait > lastWait {
			DBPoolAcquireWaitSeconds.Add((wait - lastWait).Seconds())
			lastWait = wait
		}
		if empty := stat.EmptyAcquireCount(); empty > lastEmpty {
			DBPoolEmptyAcquire.Add(float64(empty - lastEmpty))
			lastEmpty = empty
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
