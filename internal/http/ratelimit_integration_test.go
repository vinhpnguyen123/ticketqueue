//go:build integration

package http_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// waitOutWindowBoundary sleeps into the next minute window if we are
// within 3s of a reset, so a test's requests never straddle two windows.
func waitOutWindowBoundary(t *testing.T) {
	t.Helper()
	until := time.Until(time.Now().Truncate(time.Minute).Add(time.Minute))
	if until < 3*time.Second {
		time.Sleep(until)
	}
}

// postLoginFrom posts a login attempt with a spoofed client IP.
func postLoginFrom(t *testing.T, url, ip string) *http.Response {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"email": "rl@example.com", "password": "supersecret1"})
	req, err := http.NewRequest(http.MethodPost, url+"/v1/auth/login", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", ip)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST login: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestLoginRateLimitExceeded(t *testing.T) {
	ts, _ := newTestServer(t)
	waitOutWindowBoundary(t)

	for i := 1; i <= 5; i++ {
		resp := postLoginFrom(t, ts.URL, "198.51.100.1")
		if resp.StatusCode == http.StatusTooManyRequests {
			t.Fatalf("request %d already rate limited", i)
		}
	}

	resp := postLoginFrom(t, ts.URL, "198.51.100.1")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("6th login: status = %d, want 429", resp.StatusCode)
	}
	retryAfter, err := strconv.Atoi(resp.Header.Get("Retry-After"))
	if err != nil || retryAfter < 1 || retryAfter > 60 {
		t.Errorf("Retry-After = %q, want 1..60 seconds", resp.Header.Get("Retry-After"))
	}
	var body struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	decode(t, resp, &body)
	if body.Error.Type != "rate_limit_exceeded" {
		t.Errorf("error type = %q, want rate_limit_exceeded", body.Error.Type)
	}
}

func TestRateLimitHeadersDecrement(t *testing.T) {
	ts, _ := newTestServer(t)
	waitOutWindowBoundary(t)

	for i, wantRemaining := range []string{"4", "3"} {
		resp := postLoginFrom(t, ts.URL, "198.51.100.2")
		if resp.Header.Get("X-RateLimit-Limit") != "5" {
			t.Errorf("call %d: X-RateLimit-Limit = %q, want 5", i+1, resp.Header.Get("X-RateLimit-Limit"))
		}
		if got := resp.Header.Get("X-RateLimit-Remaining"); got != wantRemaining {
			t.Errorf("call %d: X-RateLimit-Remaining = %q, want %q", i+1, got, wantRemaining)
		}
	}
}

func TestRateLimitBucketsAreIndependent(t *testing.T) {
	ts, _ := newTestServer(t)
	waitOutWindowBoundary(t)

	// Exhaust IP .3's login bucket.
	for i := 0; i < 6; i++ {
		postLoginFrom(t, ts.URL, "198.51.100.3")
	}
	if resp := postLoginFrom(t, ts.URL, "198.51.100.3"); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("exhausted IP: status = %d, want 429", resp.StatusCode)
	}
	// A different IP still gets through.
	if resp := postLoginFrom(t, ts.URL, "198.51.100.4"); resp.StatusCode == http.StatusTooManyRequests {
		t.Fatal("different IP shares the exhausted bucket")
	}
}
