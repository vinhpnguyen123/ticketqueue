package ratelimit

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/redis/go-redis/v9"
)

func TestFailOpenWhenRedisUnreachable(t *testing.T) {
	// Client pointed at a dead port: every call errors, requests must pass.
	rdb := redis.NewClient(&redis.Options{Addr: "localhost:1"})
	defer rdb.Close()

	h := Limit(rdb, "test", 1, ByClientIP(0))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200 (fail open)", i+1, w.Code)
		}
	}
}

func requestFrom(xff string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.RemoteAddr = "10.1.2.3:5555"
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	return r
}

func TestByClientIPNoProxyIgnoresHeader(t *testing.T) {
	if got := ByClientIP(0)(requestFrom("203.0.113.7")); got != "10.1.2.3" {
		t.Errorf("got %q, want the socket address", got)
	}
}

// The ALB appends the address it saw, so with one trusted proxy the rightmost
// entry is real and everything to its left is whatever the client sent.
func TestByClientIPOneHopTakesRightmost(t *testing.T) {
	if got := ByClientIP(1)(requestFrom("203.0.113.7, 198.51.100.9")); got != "198.51.100.9" {
		t.Errorf("got %q, want the proxy-appended entry", got)
	}
}

func TestByClientIPTwoHops(t *testing.T) {
	if got := ByClientIP(2)(requestFrom("203.0.113.7, 198.51.100.9, 10.0.0.1")); got != "198.51.100.9" {
		t.Errorf("got %q", got)
	}
}

func TestByClientIPReadsRepeatedHeaders(t *testing.T) {
	r := requestFrom("203.0.113.7")
	r.Header.Add("X-Forwarded-For", "198.51.100.9")
	if got := ByClientIP(1)(r); got != "198.51.100.9" {
		t.Errorf("got %q", got)
	}
}

func TestByClientIPMalformedFallsBack(t *testing.T) {
	for _, xff := range []string{"", "not-an-ip", "203.0.113.7, not-an-ip"} {
		if got := ByClientIP(1)(requestFrom(xff)); got != "10.1.2.3" {
			t.Errorf("xff %q: got %q, want the socket address", xff, got)
		}
	}
	if got := ByClientIP(3)(requestFrom("203.0.113.7")); got != "10.1.2.3" {
		t.Errorf("too few entries: got %q, want the socket address", got)
	}
}
