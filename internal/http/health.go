// Package http contains the router, handlers, and middleware for the API server.
package http

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/vinhpnguyen123/ticketqueue/internal/version"
)

// Pinger checks that a dependency is reachable.
type Pinger func(ctx context.Context) error

// HealthHandler reports service liveness. It must not check dependencies.
//
// It also returns the commit the binary was built from. That is the cheapest
// possible answer to "which version is actually running?", and in ECS it is
// often the only one available: an image tag can be re-pushed under the same
// name, so the tag cannot be trusted to identify the code.
func HealthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":  "ok",
		"version": version.SHA,
	})
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
