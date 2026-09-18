//go:build integration

package http_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/vinhpnguyen123/ticketqueue/internal/booking"
	api "github.com/vinhpnguyen123/ticketqueue/internal/http"
	"github.com/vinhpnguyen123/ticketqueue/internal/paymentsim"
	"github.com/vinhpnguyen123/ticketqueue/internal/testutil"
	"github.com/vinhpnguyen123/ticketqueue/internal/worker"
)

const webhookSecret = "whsec_test"

// newWebhookServer wires the receiver against a real paymentsim.
func newWebhookServer(t *testing.T) (*httptest.Server, *pgxpool.Pool, *redis.Client, *booking.PaymentClient) {
	t.Helper()
	pool := testutil.TestDB(t)
	rdb := testutil.TestRedis(t)

	pay := httptest.NewServer(paymentsim.NewServer("ps_test_localdev"))
	t.Cleanup(pay.Close)
	pc := booking.NewPaymentClient(pay.URL, "ps_test_localdev")

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	api.NewWebhookHandler(pool, rdb, webhookSecret, pc).Mount(r)
	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
	return ts, pool, rdb, pc
}

// deliver posts a webhook, signing it the way the provider does. Signature and
// timestamp are overridable so tests can send deliberately bad ones.
func deliver(t *testing.T, ts *httptest.Server, body []byte, sig string, ts64 int64) (int, map[string]any) {
	t.Helper()
	if sig == "" {
		sig = paymentsim.Sign(body, webhookSecret)
	}
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/webhooks/payment-service", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Webhook-Signature", sig)
	req.Header.Set("X-Webhook-Timestamp", strconv.FormatInt(ts64, 10))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return resp.StatusCode, out
}

func webhookBody(t *testing.T, eventID, idemKey, typ string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"event_id":        eventID,
		"type":            typ,
		"idempotency_key": idemKey,
		"charge_id":       "ch_" + idemKey,
		"amount":          100000,
		"currency":        "USD",
		"created_at":      time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

var webhookSeq int

// stuckBookingFor creates a held booking whose charge the provider has settled.
func stuckBookingFor(t *testing.T, pool *pgxpool.Pool, rdb *redis.Client, pc *booking.PaymentClient, tierID, key, token string) booking.Held {
	t.Helper()
	ctx := context.Background()
	webhookSeq++
	var userID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email, password_hash) VALUES ($1, 'x') RETURNING id`,
		fmt.Sprintf("hook%d@test.dev", webhookSeq)).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	tier, err := booking.LoadTier(ctx, pool, tierID)
	if err != nil {
		t.Fatal(err)
	}
	if err := booking.ReserveInventory(ctx, rdb, pool, tierID, 2); err != nil {
		t.Fatal(err)
	}
	held, err := booking.HoldTickets(ctx, pool, userID, tier, 2, key, "req-w")
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		pc.Charge(ctx, key, held.TotalCents, held.Currency, token)
	}
	return held
}

// Test 1: a valid webhook resolves a stuck booking, and the trail names it.
func TestWebhookResolvesHeldBooking(t *testing.T) {
	ts, pool, rdb, pc := newWebhookServer(t)
	ctx := context.Background()
	_, tierID := seedTier(t, pool, 50000, 10)
	held := stuckBookingFor(t, pool, rdb, pc, tierID, "wh-ok-1", "tok_visa")

	status, out := deliver(t, ts, webhookBody(t, uuid.NewString(), "wh-ok-1", "charge.succeeded"),
		"", time.Now().Unix())
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v, want 200", status, out)
	}

	var bookingStatus string
	pool.QueryRow(ctx, `SELECT status FROM booking WHERE id = $1`, held.BookingID).Scan(&bookingStatus)
	if bookingStatus != "confirmed" {
		t.Errorf("booking status = %s, want confirmed", bookingStatus)
	}
	var sold int
	pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM tickets WHERE booking_id = $1 AND status = 'sold'`, held.BookingID).Scan(&sold)
	if sold != 2 {
		t.Errorf("sold tickets = %d, want 2", sold)
	}
	var source string
	if err := pool.QueryRow(ctx,
		`SELECT metadata->>'source' FROM audit_log
		 WHERE entity_id = $1 AND action = 'booking_confirmed'`, held.BookingID).Scan(&source); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if source != "webhook" {
		t.Errorf("audit source = %q, want webhook", source)
	}
}

// Test 2: at-least-once delivery must not mean at-least-once effects.
func TestWebhookReplayIsIgnored(t *testing.T) {
	ts, pool, rdb, pc := newWebhookServer(t)
	ctx := context.Background()
	_, tierID := seedTier(t, pool, 50000, 10)
	held := stuckBookingFor(t, pool, rdb, pc, tierID, "wh-replay-1", "tok_visa")

	eventID := uuid.NewString()
	body := webhookBody(t, eventID, "wh-replay-1", "charge.succeeded")
	for i := 0; i < 3; i++ {
		if status, out := deliver(t, ts, body, "", time.Now().Unix()); status != http.StatusOK {
			t.Fatalf("delivery %d status = %d body=%v", i, status, out)
		}
	}

	var payments, confirmations int
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM payments WHERE booking_id = $1`, held.BookingID).Scan(&payments)
	if payments != 1 {
		t.Errorf("payments rows = %d, want 1", payments)
	}
	pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM audit_log WHERE entity_id = $1 AND action = 'booking_confirmed'`,
		held.BookingID).Scan(&confirmations)
	if confirmations != 1 {
		t.Errorf("booking_confirmed audit rows = %d, want 1", confirmations)
	}
	var seen int
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM webhook_events_seen WHERE event_id = $1`, eventID).Scan(&seen)
	if seen != 1 {
		t.Errorf("dedup rows = %d, want 1", seen)
	}
}

// Test 3: a body that does not match its signature is refused.
func TestWebhookRejectsTamperedBody(t *testing.T) {
	ts, pool, rdb, pc := newWebhookServer(t)
	_, tierID := seedTier(t, pool, 50000, 10)
	held := stuckBookingFor(t, pool, rdb, pc, tierID, "wh-tamper-1", "tok_visa")

	original := webhookBody(t, uuid.NewString(), "wh-tamper-1", "charge.succeeded")
	sig := paymentsim.Sign(original, webhookSecret)
	tampered := webhookBody(t, uuid.NewString(), "wh-tamper-1", "charge.failed")

	status, out := deliver(t, ts, tampered, sig, time.Now().Unix())
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d body=%v, want 401", status, out)
	}
	if got := out["error"].(map[string]any)["type"]; got != "invalid_signature" {
		t.Errorf("error type = %v, want invalid_signature", got)
	}
	if got := bookingStatusOf(t, pool, held.BookingID); got != "held" {
		t.Errorf("booking status = %s, want held (nothing applied)", got)
	}
}

// Test 4: a captured delivery replayed hours later is refused even though its
// signature is still perfectly valid — that is what the timestamp is for.
func TestWebhookRejectsStaleTimestamp(t *testing.T) {
	ts, pool, rdb, pc := newWebhookServer(t)
	_, tierID := seedTier(t, pool, 50000, 10)
	held := stuckBookingFor(t, pool, rdb, pc, tierID, "wh-stale-1", "tok_visa")

	body := webhookBody(t, uuid.NewString(), "wh-stale-1", "charge.succeeded")
	status, out := deliver(t, ts, body, "", time.Now().Add(-10*time.Minute).Unix())
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d body=%v, want 401", status, out)
	}
	if got := out["error"].(map[string]any)["type"]; got != "timestamp_expired" {
		t.Errorf("error type = %v, want timestamp_expired", got)
	}
	if got := bookingStatusOf(t, pool, held.BookingID); got != "held" {
		t.Errorf("booking status = %s, want held", got)
	}
}

// Test 5: a key we have no booking for answers 200, not 404 — a 404 would only
// make the provider retry something that can never succeed.
func TestWebhookUnknownKeyReturns200(t *testing.T) {
	ts, pool, _, _ := newWebhookServer(t)
	_, _ = seedTier(t, pool, 50000, 10)

	status, out := deliver(t, ts, webhookBody(t, uuid.NewString(), "wh-nobody-knows", "charge.succeeded"),
		"", time.Now().Unix())
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v, want 200", status, out)
	}
	if got := out["data"].(map[string]any)["status"]; got != "unknown_key_ignored" {
		t.Errorf("status = %v, want unknown_key_ignored", got)
	}
}

// Test 6: the worker got there first. The webhook must change nothing and leave
// exactly one confirmation in the trail.
func TestWebhookAfterWorkerAlreadyConfirmed(t *testing.T) {
	ts, pool, rdb, pc := newWebhookServer(t)
	ctx := context.Background()
	_, tierID := seedTier(t, pool, 50000, 10)
	held := stuckBookingFor(t, pool, rdb, pc, tierID, "wh-late-1", "tok_visa")

	if _, err := pool.Exec(ctx,
		`UPDATE booking SET created_at = NOW() - INTERVAL '5 minutes' WHERE id = $1`, held.BookingID); err != nil {
		t.Fatal(err)
	}
	if err := worker.ReconcileCycle(ctx, rdb, pool, pc); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	status, out := deliver(t, ts, webhookBody(t, uuid.NewString(), "wh-late-1", "charge.succeeded"),
		"", time.Now().Unix())
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v, want 200", status, out)
	}
	if got := out["data"].(map[string]any)["status"]; got != "already_resolved" {
		t.Errorf("status = %v, want already_resolved", got)
	}

	var confirmations, payments int
	pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM audit_log WHERE entity_id = $1 AND action = 'booking_confirmed'`,
		held.BookingID).Scan(&confirmations)
	if confirmations != 1 {
		t.Errorf("booking_confirmed audit rows = %d, want exactly 1", confirmations)
	}
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM payments WHERE booking_id = $1`, held.BookingID).Scan(&payments)
	if payments != 1 {
		t.Errorf("payments rows = %d, want 1", payments)
	}
	var source string
	pool.QueryRow(ctx,
		`SELECT metadata->>'source' FROM audit_log WHERE entity_id = $1 AND action = 'booking_confirmed'`,
		held.BookingID).Scan(&source)
	if source != "reconciliation" {
		t.Errorf("audit source = %q, want reconciliation (the worker did the work)", source)
	}
}

func bookingStatusOf(t *testing.T, pool *pgxpool.Pool, id string) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(), `SELECT status FROM booking WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatalf("booking status: %v", err)
	}
	return s
}

// newLiveStack wires a paymentsim that actually delivers webhooks to a running
// API, so the whole loop is exercised: purchase → charge → webhook → resolve.
func newLiveStack(t *testing.T) (*httptest.Server, *pgxpool.Pool, *redis.Client) {
	t.Helper()
	pool := testutil.TestDB(t)
	rdb := testutil.TestRedis(t)

	// The API server and the provider each need the other's URL, so the API
	// mux is built first and its server created before the provider that will
	// call it.
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)

	pay := httptest.NewServer(paymentsim.NewServer("ps_test_localdev",
		paymentsim.WithWebhook(ts.URL+"/webhooks/payment-service", webhookSecret)))
	t.Cleanup(pay.Close)

	pc := booking.NewPaymentClient(pay.URL, "ps_test_localdev")
	pc.HTTP.Timeout = 1 * time.Second
	payClient = pc

	api.NewAuthHandler(pool, rdb, testSecret).Mount(r)
	api.NewPurchaseHandler(pool, rdb, testSecret, pc).Mount(r)
	api.NewWebhookHandler(pool, rdb, webhookSecret, pc).Mount(r)
	return ts, pool, rdb
}

// waitFor polls until cond holds or the deadline passes. Webhook delivery is
// asynchronous by design, so the test waits for it rather than assuming timing.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Test 7: with webhooks enabled, an ordinary successful purchase is unaffected.
// TX2 confirms it synchronously and the webhook that arrives afterwards is a
// no-op — it must not duplicate the payment or add a second confirmation.
func TestWebhookDoesNotDisturbHappyPath(t *testing.T) {
	ts, pool, _ := newLiveStack(t)
	ctx := context.Background()
	eventID, tierID := seedTier(t, pool, 50000, 10)
	_, jwtTok, purchaseTok := admittedUser(t, pool, eventID, 1)

	resp, out := purchase(t, ts, jwtTok, "wh-happy-1",
		purchaseBodyTok(eventID, tierID, 2, purchaseTok, "tok_visa"))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("purchase = %d body=%v, want 201", resp.StatusCode, out)
	}
	bookingID := bookingIDOf(t, out)

	// Let the webhook land before asserting it changed nothing.
	waitFor(t, "the webhook to be recorded", func() bool {
		var n int
		pool.QueryRow(ctx, `SELECT COUNT(*) FROM webhook_events_seen`).Scan(&n)
		return n > 0
	})

	var confirmations, payments int
	pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM audit_log WHERE entity_id = $1 AND action = 'booking_confirmed'`,
		bookingID).Scan(&confirmations)
	if confirmations != 1 {
		t.Errorf("booking_confirmed audit rows = %d, want exactly 1", confirmations)
	}
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM payments WHERE booking_id = $1`, bookingID).Scan(&payments)
	if payments != 1 {
		t.Errorf("payments rows = %d, want 1", payments)
	}
	var source string
	pool.QueryRow(ctx,
		`SELECT metadata->>'source' FROM audit_log WHERE entity_id = $1 AND action = 'booking_confirmed'`,
		bookingID).Scan(&source)
	if source != "purchase" {
		t.Errorf("audit source = %q, want purchase (the synchronous path did the work)", source)
	}
	if got := bookingStatusOf(t, pool, bookingID); got != "confirmed" {
		t.Errorf("booking status = %s, want confirmed", got)
	}
}

// Test 8: the case the webhook actually earns its place. The client's response
// is dropped, so the purchase ends ambiguous — and the webhook resolves the
// booking in about a second, without the worker and without a user retry.
func TestWebhookResolvesDroppedResponse(t *testing.T) {
	ts, pool, _ := newLiveStack(t)
	ctx := context.Background()
	eventID, tierID := seedTier(t, pool, 50000, 10)
	_, jwtTok, purchaseTok := admittedUser(t, pool, eventID, 1)

	// The client still gets nothing: webhooks must not change what it sees.
	resp, out := purchase(t, ts, jwtTok, "wh-drop-1",
		purchaseBodyTok(eventID, tierID, 2, purchaseTok, "tok_drops_response"))
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("purchase = %d body=%v, want 504", resp.StatusCode, out)
	}
	bookingID := bookingIDOf(t, out)

	// No worker runs in this test: if the booking resolves, the webhook did it.
	waitFor(t, "the webhook to resolve the booking", func() bool {
		return bookingStatusOf(t, pool, bookingID) == "confirmed"
	})

	var source string
	if err := pool.QueryRow(ctx,
		`SELECT metadata->>'source' FROM audit_log
		 WHERE entity_id = $1 AND action = 'booking_confirmed'`, bookingID).Scan(&source); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if source != "webhook" {
		t.Errorf("audit source = %q, want webhook", source)
	}
	var sold, payments int
	pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM tickets WHERE booking_id = $1 AND status = 'sold'`, bookingID).Scan(&sold)
	if sold != 2 {
		t.Errorf("sold tickets = %d, want 2", sold)
	}
	// The 'unknown' row TX2 wrote plus the 'succeeded' row the webhook wrote.
	pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM payments WHERE booking_id = $1 AND status = 'succeeded'`, bookingID).Scan(&payments)
	if payments != 1 {
		t.Errorf("succeeded payments rows = %d, want exactly 1 (charged once)", payments)
	}
}
