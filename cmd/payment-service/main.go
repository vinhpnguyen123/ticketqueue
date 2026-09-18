// Command payment-service runs the simulated payment provider
// (internal/paymentsim) as a standalone HTTP service — a separate company
// in our simulation, with no access to ticketqueue's Postgres.
package main

import (
	"log/slog"
	"net/http"
	"os"

	"github.com/vinhpnguyen123/ticketqueue/internal/metrics"
	"github.com/vinhpnguyen123/ticketqueue/internal/paymentsim"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	port := envOr("PAYMENT_PORT", "8081")
	apiKey := envOr("PAYMENT_API_KEY", "ps_test_localdev")

	metrics.Register()
	slog.Info("payment-service listening", "port", port)
	if err := http.ListenAndServe(":"+port, paymentsim.NewServer(apiKey)); err != nil {
		slog.Error("server exited", "error", err)
		os.Exit(1)
	}
}
