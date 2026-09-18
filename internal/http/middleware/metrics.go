package middleware

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/vinhpnguyen123/ticketqueue/internal/metrics"
)

// Metrics records request latency. The route label is read AFTER the handler
// runs, because chi only knows the matched pattern by then — and the pattern is
// what keeps ids out of the label set.
func Metrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		start := time.Now()
		next.ServeHTTP(ww, r)
		metrics.HTTPDuration.WithLabelValues(
			metrics.RouteLabel(r), r.Method, strconv.Itoa(ww.Status()),
		).Observe(time.Since(start).Seconds())
	})
}
