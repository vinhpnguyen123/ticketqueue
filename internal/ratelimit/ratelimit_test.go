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

	h := Limit(rdb, "test", 1, ByIP)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

func TestByIP(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.RemoteAddr = "10.1.2.3:5555"
	if got := ByIP(r); got != "10.1.2.3" {
		t.Errorf("RemoteAddr: got %q", got)
	}

	r.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	if got := ByIP(r); got != "203.0.113.7" {
		t.Errorf("X-Forwarded-For: got %q", got)
	}
}
