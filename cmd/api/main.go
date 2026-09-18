package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/vinhpnguyen123/ticketqueue/internal/booking"
	"github.com/vinhpnguyen123/ticketqueue/internal/config"
	"github.com/vinhpnguyen123/ticketqueue/internal/db"
	api "github.com/vinhpnguyen123/ticketqueue/internal/http"
	apimw "github.com/vinhpnguyen123/ticketqueue/internal/http/middleware"
	"github.com/vinhpnguyen123/ticketqueue/internal/metrics"
	"github.com/vinhpnguyen123/ticketqueue/internal/redis"
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

	// Pool saturation is the most likely first bottleneck under load, and it is
	// invisible in request latency alone.
	metrics.RegisterAPI()
	go metrics.SamplePool(ctx, pool)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(apimw.RequestLogger(logger))
	r.Use(apimw.Metrics)
	r.Use(middleware.Recoverer)
	// VPC-only in a real deploy: /metrics describes internal capacity and
	// failure modes and must never be public (§11a).
	r.Handle("/metrics", metrics.Handler())
	r.Get("/v1/health", api.HealthHandler)
	r.Get("/v1/health/ready", api.ReadyHandler(
		pool.Ping,
		func(ctx context.Context) error { return rdb.Ping(ctx).Err() },
	))
	api.NewAuthHandler(pool, rdb, cfg.JWTSecret).Mount(r)
	api.NewEventsHandler(pool).Mount(r)
	api.NewQueueHandler(pool, rdb, cfg.JWTSecret).Mount(r)
	payments := booking.NewPaymentClient(cfg.PaymentURL, cfg.PaymentKey)
	api.NewPurchaseHandler(pool, rdb, cfg.JWTSecret, payments).Mount(r)
	api.NewBookingsHandler(pool, rdb, cfg.JWTSecret, payments).Mount(r)
	api.NewAdminHandler(pool, rdb, cfg.JWTSecret, payments).Mount(r)

	srv := &http.Server{Addr: ":" + cfg.Port, Handler: r}

	go func() {
		logger.Info("api server listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server error", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
		os.Exit(1)
	}
	logger.Info("shutdown complete")
}
