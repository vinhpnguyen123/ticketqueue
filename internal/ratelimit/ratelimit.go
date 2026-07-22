// Package ratelimit implements a per-minute fixed-window rate limiter
// backed by Redis. Windows are minute-aligned; each (scope, identifier,
// window) gets its own counter key:
//
//	ratelimit:{scope}:{identifier}:{window_start_unix}
//
// FAIL OPEN: if Redis is unreachable the request is allowed with a
// warning — availability over strictness for rate limiting; the queue
// and DB constraints are the real protection against abuse.
package ratelimit

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/vinhpnguyen123/ticketqueue/internal/auth"
	"github.com/vinhpnguyen123/ticketqueue/internal/http/response"
)

// incrScript atomically increments the window counter and sets its TTL
// on first hit (2 minutes: the 1-minute window plus slack so keys
// self-expire without a cleanup job).
var incrScript = redis.NewScript(`
local count = redis.call('INCR', KEYS[1])
if count == 1 then
	redis.call('EXPIRE', KEYS[1], ARGV[1])
end
return count
`)

// ByIP keys requests by client IP: first X-Forwarded-For entry when
// present (we sit behind an ALB in deployment), else RemoteAddr host.
func ByIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ByUserID keys requests by the authenticated user ID. Returns "" when
// there is none (route not behind auth middleware) — the request is
// then not rate limited; the 401 from auth is the real gate.
func ByUserID(r *http.Request) string {
	id, _ := auth.UserIDFromContext(r.Context())
	return id
}

// Limit returns chi middleware enforcing perMinute requests per
// keyFn(r) identifier within scope. Allowed responses carry
// X-RateLimit-Limit / X-RateLimit-Remaining; rejected ones add
// Retry-After and get 429 rate_limit_exceeded.
func Limit(rdb *redis.Client, scope string, perMinute int, keyFn func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := keyFn(r)
			if id == "" {
				next.ServeHTTP(w, r)
				return
			}

			windowStart := time.Now().Truncate(time.Minute)
			key := fmt.Sprintf("ratelimit:%s:%s:%d", scope, id, windowStart.Unix())

			count, err := incrScript.Run(r.Context(), rdb, []string{key}, 120).Int()
			if err != nil {
				// ponytail: fail open — availability over strictness for rate
				// limiting; the queue and DB constraints are the real protection.
				slog.Warn("rate limit check failed, allowing request",
					"scope", scope, "error", err)
				next.ServeHTTP(w, r)
				return
			}

			remaining := perMinute - count
			if remaining < 0 {
				remaining = 0
			}
			w.Header().Set("X-RateLimit-Limit", strconv.Itoa(perMinute))
			w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))

			if count > perMinute {
				retryAfter := int(time.Until(windowStart.Add(time.Minute)).Seconds())
				if retryAfter < 1 {
					retryAfter = 1
				}
				w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
				response.Error(w, r, http.StatusTooManyRequests,
					"rate_limit_exceeded", "too many requests, slow down")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
