//go:build integration

package http_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/vinhpnguyen123/ticketqueue/internal/auth"
	api "github.com/vinhpnguyen123/ticketqueue/internal/http"
	"github.com/vinhpnguyen123/ticketqueue/internal/queue"
	"github.com/vinhpnguyen123/ticketqueue/internal/testutil"
)

func newQueueServer(t *testing.T) (*httptest.Server, *pgxpool.Pool, *redis.Client) {
	t.Helper()
	pool := testutil.TestDB(t)
	rdb := testutil.TestRedis(t)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	api.NewAuthHandler(pool, testSecret).Mount(r)
	api.NewQueueHandler(pool, rdb, testSecret).Mount(r)

	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
	return ts, pool, rdb
}

// registerUser registers via the API and returns (userID, bearer token).
func registerUser(t *testing.T, ts *httptest.Server, email string) (string, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"email": email, "password": "password123"})
	resp, err := http.Post(ts.URL+"/v1/auth/register", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register status = %d", resp.StatusCode)
	}
	var out struct {
		Data struct {
			User  struct{ ID string }
			Token string
		}
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode register: %v", err)
	}
	return out.Data.User.ID, out.Data.Token
}

// doQueue sends an authed request and decodes the JSON body (if into != nil).
func doQueue(t *testing.T, method, url, token string, into any) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if into != nil {
		if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
			t.Fatalf("decode %s %s: %v", method, url, err)
		}
	}
	return resp
}

type queueJoinBody struct {
	Data struct {
		Position  int64     `json:"position"`
		QueueSize int64     `json:"queue_size"`
		JoinedAt  time.Time `json:"joined_at"`
	} `json:"data"`
}

type queuePositionBody struct {
	Data struct {
		Position        int64     `json:"position"`
		QueueSize       int64     `json:"queue_size"`
		Status          string    `json:"status"`
		PurchaseToken   string    `json:"purchase_token"`
		WindowExpiresAt time.Time `json:"window_expires_at"`
	} `json:"data"`
}

func TestQueueJoinIdempotent(t *testing.T) {
	ts, pool, _ := newQueueServer(t)
	eventID := seedEvent(t, pool, "Queue Concert", "on_sale", nil)
	_, tok1 := registerUser(t, ts, "q1@test.dev")
	_, tok2 := registerUser(t, ts, "q2@test.dev")
	joinURL := fmt.Sprintf("%s/v1/events/%s/queue/join", ts.URL, eventID)

	var body queueJoinBody
	resp := doQueue(t, http.MethodPost, joinURL, tok1, &body)
	if resp.StatusCode != http.StatusCreated || body.Data.Position != 1 {
		t.Fatalf("first join: status=%d position=%d, want 201/1", resp.StatusCode, body.Data.Position)
	}
	if body.Data.JoinedAt.IsZero() {
		t.Error("joined_at missing")
	}

	resp = doQueue(t, http.MethodPost, joinURL, tok2, &body)
	if resp.StatusCode != http.StatusCreated || body.Data.Position != 2 {
		t.Fatalf("second user join: status=%d position=%d, want 201/2", resp.StatusCode, body.Data.Position)
	}

	// Idempotent re-join: 200, not error, original position.
	resp = doQueue(t, http.MethodPost, joinURL, tok1, &body)
	if resp.StatusCode != http.StatusOK || body.Data.Position != 1 {
		t.Fatalf("re-join: status=%d position=%d, want 200/1", resp.StatusCode, body.Data.Position)
	}
	if body.Data.QueueSize != 2 {
		t.Errorf("queue_size = %d, want 2", body.Data.QueueSize)
	}
}

func TestQueuePositionReflectsLeaves(t *testing.T) {
	ts, pool, _ := newQueueServer(t)
	eventID := seedEvent(t, pool, "Queue Concert", "on_sale", nil)
	_, tok1 := registerUser(t, ts, "q1@test.dev")
	_, tok2 := registerUser(t, ts, "q2@test.dev")
	base := fmt.Sprintf("%s/v1/events/%s/queue", ts.URL, eventID)

	doQueue(t, http.MethodPost, base+"/join", tok1, nil)
	doQueue(t, http.MethodPost, base+"/join", tok2, nil)

	var pos queuePositionBody
	resp := doQueue(t, http.MethodGet, base+"/position", tok2, &pos)
	if resp.StatusCode != http.StatusOK || pos.Data.Position != 2 || pos.Data.Status != "waiting" {
		t.Fatalf("user2 position = %+v (status %d), want position 2 waiting", pos.Data, resp.StatusCode)
	}

	if resp := doQueue(t, http.MethodPost, base+"/leave", tok1, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("leave status = %d, want 204", resp.StatusCode)
	}

	resp = doQueue(t, http.MethodGet, base+"/position", tok2, &pos)
	if pos.Data.Position != 1 || pos.Data.QueueSize != 1 {
		t.Errorf("after leave: position=%d size=%d, want 1/1", pos.Data.Position, pos.Data.QueueSize)
	}
}

func TestQueueLeave(t *testing.T) {
	ts, pool, _ := newQueueServer(t)
	eventID := seedEvent(t, pool, "Queue Concert", "on_sale", nil)
	userID, tok := registerUser(t, ts, "q1@test.dev")
	base := fmt.Sprintf("%s/v1/events/%s/queue", ts.URL, eventID)

	doQueue(t, http.MethodPost, base+"/join", tok, nil)
	if resp := doQueue(t, http.MethodPost, base+"/leave", tok, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("leave status = %d, want 204", resp.StatusCode)
	}

	var status string
	var leftAt *time.Time
	err := pool.QueryRow(context.Background(),
		`SELECT status, left_at FROM queue_entries WHERE user_id = $1 AND event_id = $2`,
		userID, eventID,
	).Scan(&status, &leftAt)
	if err != nil {
		t.Fatalf("queue_entries lookup: %v", err)
	}
	if status != "left" || leftAt == nil {
		t.Errorf("row = status %q left_at %v, want 'left' with left_at set", status, leftAt)
	}

	var errResp errorBody
	resp := doQueue(t, http.MethodPost, base+"/leave", tok, &errResp)
	if resp.StatusCode != http.StatusNotFound || errResp.Error.Type != "not_in_queue" {
		t.Errorf("second leave: status=%d type=%q, want 404 not_in_queue", resp.StatusCode, errResp.Error.Type)
	}

	resp = doQueue(t, http.MethodGet, base+"/position", tok, &errResp)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("position after leave: status=%d, want 404", resp.StatusCode)
	}
}

func TestQueueJoinDraftEvent(t *testing.T) {
	ts, pool, _ := newQueueServer(t)
	eventID := seedEvent(t, pool, "Draft Event", "draft", nil)
	_, tok := registerUser(t, ts, "q1@test.dev")

	var errResp errorBody
	resp := doQueue(t, http.MethodPost,
		fmt.Sprintf("%s/v1/events/%s/queue/join", ts.URL, eventID), tok, &errResp)
	if resp.StatusCode != http.StatusGone || errResp.Error.Type != "sales_closed" {
		t.Errorf("status=%d type=%q, want 410 sales_closed", resp.StatusCode, errResp.Error.Type)
	}
}

func TestQueuePositionAdmitted(t *testing.T) {
	ts, pool, rdb := newQueueServer(t)
	eventID := seedEvent(t, pool, "Queue Concert", "on_sale", nil)
	userID, tok := registerUser(t, ts, "q1@test.dev")
	base := fmt.Sprintf("%s/v1/events/%s/queue", ts.URL, eventID)
	ctx := context.Background()

	doQueue(t, http.MethodPost, base+"/join", tok, nil)

	// Simulate the Week 3 promotion worker: mark admitted in Postgres and
	// remove from the waiting sorted set.
	if _, err := pool.Exec(ctx,
		`UPDATE queue_entries SET status = 'admitted', admitted_at = NOW()
		 WHERE user_id = $1 AND event_id = $2`, userID, eventID); err != nil {
		t.Fatalf("admit: %v", err)
	}
	if err := rdb.ZRem(ctx, queue.Key(eventID), userID).Err(); err != nil {
		t.Fatalf("zrem: %v", err)
	}

	var pos queuePositionBody
	resp := doQueue(t, http.MethodGet, base+"/position", tok, &pos)
	if resp.StatusCode != http.StatusOK || pos.Data.Status != "admitted" {
		t.Fatalf("status=%d body=%+v, want 200 admitted", resp.StatusCode, pos.Data)
	}
	if pos.Data.WindowExpiresAt.IsZero() {
		t.Error("window_expires_at missing")
	}
	claims, err := auth.ParseToken(pos.Data.PurchaseToken, testSecret)
	if err != nil {
		t.Fatalf("purchase token does not parse: %v", err)
	}
	if claims.Subject != userID {
		t.Errorf("purchase token sub = %q, want %q", claims.Subject, userID)
	}
}
