// Package middleware holds ticketqueue-specific HTTP middleware that
// complements the chi built-ins (RequestID, Recoverer).
package middleware

import (
	"log/slog"
	"net/http"
	"time"

	chimw "github.com/go-chi/chi/v5/middleware"
)

// RequestLogger emits one slog line per request with method, path,
// status, duration_ms, and request_id.
func RequestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := chimw.NewWrapResponseWriter(w, r.ProtoMajor)

			next.ServeHTTP(ww, r)

			logger.Info("request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", ww.Status(),
				"duration_ms", float64(time.Since(start).Microseconds())/1000.0,
				"request_id", chimw.GetReqID(r.Context()),
			)
		})
	}
}
