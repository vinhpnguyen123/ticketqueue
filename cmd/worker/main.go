package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/vinhpnguyen123/ticketqueue/internal/booking"
	"github.com/vinhpnguyen123/ticketqueue/internal/config"
	"github.com/vinhpnguyen123/ticketqueue/internal/db"
	"github.com/vinhpnguyen123/ticketqueue/internal/redis"
	"github.com/vinhpnguyen123/ticketqueue/internal/worker"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

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
