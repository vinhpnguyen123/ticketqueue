//go:build integration

package idempotency_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vinhpnguyen123/ticketqueue/internal/auth"
	"github.com/vinhpnguyen123/ticketqueue/internal/idempotency"
	"github.com/vinhpnguyen123/ticketqueue/internal/testutil"
)

const testSecret = "test-secret"

// newIdemServer builds auth → idempotency → dummy handler. The handler
// counts executions, optionally sleeps, and returns 201 with a JSON body.
func newIdemServer(t *testing.T, pool *pgxpool.Pool, sleep time.Duration) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		time.Sleep(sleep)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"data":{"created":true}}`))
	})
	ts := httptest.NewServer(auth.Middleware(testSecret)(idempotency.Middleware(pool)(handler)))
	t.Cleanup(ts.Close)
	return ts, &calls
}

func seedUser(t *testing.T, pool *pgxpool.Pool) (string, string) {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO users (email, password_hash) VALUES ('idem@test.dev', 'x') RETURNING id`,
	).Scan(&id); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	tok, err := auth.GenerateToken(id, "idem@test.dev", false, testSecret, time.Hour)
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	return id, tok
}

func post(t *testing.T, url, token, idemKey, body string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+token)
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, raw
}

func errType(t *testing.T, raw []byte) string {
	t.Helper()
	var e struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatalf("decode error body %q: %v", raw, err)
	}
	return e.Error.Type
}

func TestNewKeyRunsHandlerAndCompletesRow(t *testing.T) {
	pool := testutil.TestDB(t)
	userID, tok := seedUser(t, pool)
	ts, calls := newIdemServer(t, pool, 0)

	resp, raw := post(t, ts.URL, tok, "key-1", `{"qty":2}`)
	if resp.StatusCode != http.StatusCreated || calls.Load() != 1 {
		t.Fatalf("status=%d calls=%d, want 201 and 1", resp.StatusCode, calls.Load())
	}
	if string(raw) != `{"data":{"created":true}}` {
		t.Errorf("body = %s", raw)
	}

	var status int
	var body []byte
	var completed *time.Time
	var hash string
	if err := pool.QueryRow(context.Background(),
		`SELECT response_status, response_body, completed_at, request_hash
		 FROM idempotency_keys WHERE key = 'key-1' AND user_id = $1`, userID,
	).Scan(&status, &body, &completed, &hash); err != nil {
		t.Fatalf("row: %v", err)
	}
	if status != 201 || completed == nil {
		t.Errorf("row status=%d completed=%v, want 201 and non-nil", status, completed)
	}
	// SHA-256 of `{"qty":2}`
	if len(hash) != 64 {
		t.Errorf("request_hash = %q, want 64 hex chars", hash)
	}
	if !bytes.Contains(body, []byte(`"created"`)) {
		t.Errorf("stored body = %s", body)
	}
}

func TestReplaySameKeySameBody(t *testing.T) {
	pool := testutil.TestDB(t)
	_, tok := seedUser(t, pool)
	ts, calls := newIdemServer(t, pool, 0)

	post(t, ts.URL, tok, "key-r", `{"qty":2}`)
	resp, raw := post(t, ts.URL, tok, "key-r", `{"qty":2}`)

	if resp.StatusCode != http.StatusCreated {
		t.Errorf("replay status = %d, want 201", resp.StatusCode)
	}
	if resp.Header.Get("Idempotency-Replayed") != "true" {
		t.Errorf("missing Idempotency-Replayed header")
	}
	if calls.Load() != 1 {
		t.Errorf("handler ran %d times, want 1", calls.Load())
	}
	// JSONB storage normalizes whitespace, so compare parsed JSON, not bytes.
	var body struct {
		Data struct {
			Created bool `json:"created"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || !body.Data.Created {
		t.Errorf("replayed body = %s (err %v)", raw, err)
	}
}

func TestSameKeyDifferentBody(t *testing.T) {
	pool := testutil.TestDB(t)
	_, tok := seedUser(t, pool)
	ts, calls := newIdemServer(t, pool, 0)

	post(t, ts.URL, tok, "key-d", `{"qty":2}`)
	resp, raw := post(t, ts.URL, tok, "key-d", `{"qty":3}`)

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", resp.StatusCode)
	}
	if got := errType(t, raw); got != "idempotency_key_reuse" {
		t.Errorf("error type = %q, want idempotency_key_reuse", got)
	}
	if calls.Load() != 1 {
		t.Errorf("handler ran %d times, want 1", calls.Load())
	}
}

func TestConcurrentSameKeySerialized(t *testing.T) {
	pool := testutil.TestDB(t)
	_, tok := seedUser(t, pool)
	ts, calls := newIdemServer(t, pool, 200*time.Millisecond)

	const n = 10
	statuses := make([]int, n)
	bodies := make([][]byte, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			resp, raw := post(t, ts.URL, tok, "key-c", `{"qty":1}`)
			statuses[i] = resp.StatusCode
			bodies[i] = raw
		}(i)
	}
	close(start)
	wg.Wait()

	if calls.Load() != 1 {
		t.Fatalf("handler executed %d times, want exactly 1", calls.Load())
	}
	var created, conflict int
	for i, s := range statuses {
		switch s {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflict++
			if got := errType(t, bodies[i]); got != "request_in_progress" {
				t.Errorf("conflict %d error type = %q, want request_in_progress", i, got)
			}
		default:
			t.Errorf("request %d unexpected status %d", i, s)
		}
	}
	if created != 1 || conflict != n-1 {
		t.Errorf("created=%d conflict=%d, want 1 and %d", created, conflict, n-1)
	}
}

func TestStaleLockReturnsUnresolved(t *testing.T) {
	pool := testutil.TestDB(t)
	userID, tok := seedUser(t, pool)
	ts, calls := newIdemServer(t, pool, 0)

	// A previous attempt died mid-flight 5 minutes ago: claimed, never completed.
	body := `{"qty":2}`
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO idempotency_keys (key, user_id, request_hash, locked_at, expires_at)
		 VALUES ('key-s', $1, encode(sha256($2::bytea), 'hex'), NOW() - INTERVAL '5 minutes', NOW() + INTERVAL '24 hours')`,
		userID, body); err != nil {
		t.Fatalf("seed stale row: %v", err)
	}

	resp, raw := post(t, ts.URL, tok, "key-s", body)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", resp.StatusCode)
	}
	if got := errType(t, raw); got != "request_unresolved" {
		t.Errorf("error type = %q, want request_unresolved", got)
	}
	if calls.Load() != 0 {
		t.Errorf("handler must not re-execute a possibly-half-done request; ran %d times", calls.Load())
	}
}

func TestMissingHeader(t *testing.T) {
	pool := testutil.TestDB(t)
	_, tok := seedUser(t, pool)
	ts, calls := newIdemServer(t, pool, 0)

	resp, raw := post(t, ts.URL, tok, "", `{"qty":2}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	if got := errType(t, raw); got != "missing_idempotency_key" {
		t.Errorf("error type = %q, want missing_idempotency_key", got)
	}
	if calls.Load() != 0 {
		t.Errorf("handler ran %d times, want 0", calls.Load())
	}
}
