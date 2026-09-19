package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/vinhpnguyen123/ticketqueue/internal/booking"
	"github.com/vinhpnguyen123/ticketqueue/internal/config"
	"github.com/vinhpnguyen123/ticketqueue/internal/db"
	"github.com/vinhpnguyen123/ticketqueue/internal/metrics"
	"github.com/vinhpnguyen123/ticketqueue/internal/redis"
	"github.com/vinhpnguyen123/ticketqueue/internal/version"
	"github.com/vinhpnguyen123/ticketqueue/internal/worker"
)

func main() {
	if probe := flag.String("healthcheck", "", "probe this URL, exit 0 if healthy (used by the container HEALTHCHECK)"); true {
		flag.Parse()
		if *probe != "" {
			os.Exit(version.Healthcheck(*probe))
		}
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	logger.Info("worker starting", "version", version.SHA)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		logger.Error("postgres unreachable", "error", err)
		os.Exit(1)
	}
	defer pool.Close()
	logger.Info("connected to postgres")

	rdb, err := redis.NewClient(ctx, cfg.RedisAddr)
	if err != nil {
		logger.Error("redis unreachable", "error", err)
		os.Exit(1)
	}
	defer rdb.Close()
	logger.Info("connected to redis")

	payments := booking.NewPaymentClient(cfg.PaymentURL, cfg.PaymentKey)

	// The worker owns the money-at-risk gauges, sampled on their own schedule
	// rather than inside any worker cycle.
	metrics.RegisterWorker()
	go worker.SampleGauges(ctx, pool)

	// VPC-only in a real deploy: never expose /metrics publicly (§11a).
	metricsSrv := &http.Server{Addr: ":" + cfg.MetricsPort, Handler: metricsMux()}
	go func() {
		logger.Info("worker metrics listening", "port", cfg.MetricsPort)
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("metrics server failed", "error", err)
		}
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = metricsSrv.Shutdown(shutdownCtx)
	}()

	// The drift checker is the one worker that carries state between cycles
	// (the two-cycle inventory confirmation rule).
	drift := worker.NewDriftChecker()

	// Each Runner blocks until SIGINT/SIGTERM cancels ctx.
	var wg sync.WaitGroup
	runners := []*worker.Runner{
		worker.NewRunner("promotion", worker.PromotionInterval, func(ctx context.Context) error {
			return worker.PromoteCycle(ctx, rdb, pool)
		}),
		worker.NewRunner("idem-cleanup", worker.IdemCleanupInterval, func(ctx context.Context) error {
			return worker.IdemCleanupCycle(ctx, pool)
		}),
		worker.NewRunner("hold-expiry", worker.HoldExpiryInterval, func(ctx context.Context) error {
			return worker.HoldExpiryCycle(ctx, rdb, pool, payments)
		}),
		worker.NewRunner("reconciliation", worker.ReconcileInterval, func(ctx context.Context) error {
			return worker.ReconcileCycle(ctx, rdb, pool, payments)
		}),
		worker.NewRunner("drift", worker.DriftInterval, func(ctx context.Context) error {
			return drift.Cycle(ctx, rdb, pool)
		}),
	}
	for _, r := range runners {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Start(ctx)
		}()
	}
	wg.Wait()

	logger.Info("shutdown complete")
}

// metricsMux serves /metrics and /health on the worker's own port.
//
// The worker had no HTTP health endpoint: it is not a server, so nothing ever
// needed one. A container does. Both Docker's HEALTHCHECK and an ECS health
// check need something to ask, and "the metrics port answers" is a genuine
// liveness signal for this process — the scrape handler runs on the same
// goroutine pool as everything else, so if it answers, the process is alive.
func metricsMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics.Handler())
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status":  "ok",
			"version": version.SHA,
		})
	})
	return mux
}
