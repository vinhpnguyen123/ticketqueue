// Package http contains the router, handlers, and middleware for the API server.
package http

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// Pinger checks that a dependency is reachable.
type Pinger func(ctx context.Context) error

// HealthHandler reports service liveness. It must not check dependencies.
func HealthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`))
}

// ReadyHandler returns a readiness probe that pings Postgres and Redis,
// each with a 2-second timeout.
func ReadyHandler(pingDB, pingRedis Pinger) http.HandlerFunc {
	check := func(ctx context.Context, ping Pinger) string {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if err := ping(ctx); err != nil {
			return "unreachable"
		}
		return "ok"
	}

	return func(w http.ResponseWriter, r *http.Request) {
		checks := map[string]string{
			"database": check(r.Context(), pingDB),
			"redis":    check(r.Context(), pingRedis),
		}
		status := "ready"
		code := http.StatusOK
		if checks["database"] != "ok" || checks["redis"] != "ok" {
			status = "unavailable"
			code = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(map[string]any{"status": status, "checks": checks})
	}
}
