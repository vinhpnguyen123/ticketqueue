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
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/vinhpnguyen123/ticketqueue/internal/auth"
	"github.com/vinhpnguyen123/ticketqueue/internal/booking"
	api "github.com/vinhpnguyen123/ticketqueue/internal/http"
	"github.com/vinhpnguyen123/ticketqueue/internal/paymentsim"
	"github.com/vinhpnguyen123/ticketqueue/internal/testutil"
	"github.com/vinhpnguyen123/ticketqueue/internal/worker"
)

// newPurchaseServer wires auth + purchase routes against a real paymentsim
// httptest server.
func newPurchaseServer(t *testing.T) (*httptest.Server, *pgxpool.Pool, *redis.Client) {
	t.Helper()
	pool := testutil.TestDB(t)
	rdb := testutil.TestRedis(t)

	pay := httptest.NewServer(paymentsim.NewServer("ps_test_localdev"))
	t.Cleanup(pay.Close)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	api.NewAuthHandler(pool, rdb, testSecret).Mount(r)
	api.NewPurchaseHandler(pool, rdb, testSecret,
		booking.NewPaymentClient(pay.URL, "ps_test_localdev")).Mount(r)
	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
	return ts, pool, rdb
}

// seedTier creates an on_sale event plus one tier with n available tickets,
// returning (eventID, tierID).
func seedTier(t *testing.T, pool *pgxpool.Pool, priceCents int64, n int) (string, string) {
	t.Helper()
	ctx := context.Background()
	var eventID, tierID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO events (name, venue, event_at, status)
		 VALUES ('Purchase Test', 'Venue', NOW() + INTERVAL '30 days', 'on_sale') RETURNING id`,
	).Scan(&eventID); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO ticket_tier (event_id, name, price_cents, quantity_total)
		 VALUES ($1, 'VIP', $2, $3) RETURNING id`, eventID, priceCents, n,
	).Scan(&tierID); err != nil {
		t.Fatalf("insert tier: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO tickets (ticket_tier_id, status) SELECT $1, 'available' FROM generate_series(1, $2)`,
		tierID, n); err != nil {
		t.Fatalf("insert tickets: %v", err)
	}
	return eventID, tierID
}

// admittedUser seeds a user directly (avoids the register rate limit) and
// mints them a session JWT plus a valid purchase token.
func admittedUser(t *testing.T, pool *pgxpool.Pool, eventID string, i int) (userID, jwtTok, purchaseTok string) {
	t.Helper()
	email := fmt.Sprintf("buyer%d@test.dev", i)
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO users (email, password_hash) VALUES ($1, 'x') RETURNING id`, email,
	).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	jwtTok, err := auth.GenerateToken(userID, email, false, testSecret, time.Hour)
	if err != nil {
		t.Fatalf("jwt: %v", err)
	}
	purchaseTok, err = auth.GeneratePurchaseToken(userID, eventID, time.Now(), 300, testSecret)
	if err != nil {
		t.Fatalf("purchase token: %v", err)
	}
	return userID, jwtTok, purchaseTok
}

func purchase(t *testing.T, ts *httptest.Server, jwtTok, idemKey string, body map[string]any) (*http.Response, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/purchases", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+jwtTok)
	req.Header.Set("Idempotency-Key", idemKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("purchase request: %v", err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	b, _ := io.ReadAll(resp.Body)
	if len(b) > 0 {
		json.Unmarshal(b, &out)
	}
	return resp, out
}

func purchaseBody(eventID, tierID string, qty int, purchaseTok string) map[string]any {
	return map[string]any{
		"event_id": eventID, "tier_id": tierID, "quantity": qty,
		"payment_token": "tok_visa", "purchase_token": purchaseTok,
	}
}

func redisInt(t *testing.T, rdb *redis.Client, key string) int {
	t.Helper()
	v, err := rdb.Get(context.Background(), key).Result()
	if err != nil {
		t.Fatalf("redis get %s: %v", key, err)
	}
	n, _ := strconv.Atoi(v)
	return n
}

func TestPurchaseHappyTX1(t *testing.T) {
	ts, pool, rdb := newPurchaseServer(t)
	ctx := context.Background()
	eventID, tierID := seedTier(t, pool, 50000, 100)
	userID, jwtTok, purchaseTok := admittedUser(t, pool, eventID, 1)

	resp, out := purchase(t, ts, jwtTok, "buy-1", purchaseBody(eventID, tierID, 2, purchaseTok))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d body=%v, want 202", resp.StatusCode, out)
	}
	data := out["data"].(map[string]any)
	bookingID := data["booking_id"].(string)
	if data["status"] != "held" {
		t.Errorf("status = %v, want held", data["status"])
	}

	var qty int
	var total int64
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT quantity, total_cents, status FROM booking WHERE id = $1 AND user_id = $2`,
		bookingID, userID,
	).Scan(&qty, &total, &status); err != nil {
		t.Fatalf("booking row: %v", err)
	}
	if qty != 2 || total != 100000 || status != "held" {
		t.Errorf("booking qty=%d total=%d status=%s, want 2/100000/held", qty, total, status)
	}

	var held int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM tickets WHERE booking_id = $1 AND status = 'held'`, bookingID,
	).Scan(&held); err != nil {
		t.Fatal(err)
	}
	if held != 2 {
		t.Errorf("held tickets = %d, want 2", held)
	}

	if got := redisInt(t, rdb, booking.InventoryKey(tierID)); got != 98 {
		t.Errorf("inventory = %d, want 98", got)
	}

	var audits int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM audit_log WHERE entity_id = $1 AND action IN ('booking_created','tickets_held')`,
		bookingID,
	).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 2 {
		t.Errorf("audit rows = %d, want 2", audits)
	}

	// The temporary seam recorded an initiated payment.
	var payStatus string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM payments WHERE booking_id = $1`, bookingID,
	).Scan(&payStatus); err != nil {
		t.Fatalf("payments row: %v", err)
	}
	if payStatus != "initiated" {
		t.Errorf("payment status = %s, want initiated", payStatus)
	}
}

func TestPurchaseDuplicateGuard(t *testing.T) {
	ts, pool, _ := newPurchaseServer(t)
	eventID, tierID := seedTier(t, pool, 50000, 10)
	_, jwtTok, purchaseTok := admittedUser(t, pool, eventID, 1)

	resp, _ := purchase(t, ts, jwtTok, "dup-1", purchaseBody(eventID, tierID, 1, purchaseTok))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first purchase = %d, want 202", resp.StatusCode)
	}
	resp, out := purchase(t, ts, jwtTok, "dup-2", purchaseBody(eventID, tierID, 1, purchaseTok))
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("second purchase = %d body=%v, want 410", resp.StatusCode, out)
	}
	errObj := out["error"].(map[string]any)
	if errObj["type"] != "active_booking_exists" {
		t.Errorf("error type = %v", errObj["type"])
	}
	if errObj["details"].(map[string]any)["booking_id"] == "" {
		t.Errorf("missing existing booking_id in details")
	}
}

func TestPurchaseSoldOutRedis(t *testing.T) {
	ts, pool, rdb := newPurchaseServer(t)
	eventID, tierID := seedTier(t, pool, 50000, 10)
	_, jwtTok, purchaseTok := admittedUser(t, pool, eventID, 1)

	rdb.Set(context.Background(), booking.InventoryKey(tierID), 0, 0)

	resp, out := purchase(t, ts, jwtTok, "so-1", purchaseBody(eventID, tierID, 1, purchaseTok))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if out["error"].(map[string]any)["type"] != "sold_out" {
		t.Errorf("error type = %v", out["error"])
	}
	var bookings int
	pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM booking`).Scan(&bookings)
	if bookings != 0 {
		t.Errorf("bookings created = %d, want 0", bookings)
	}
}

func TestPurchaseLuaRacedAheadOfTruth(t *testing.T) {
	ts, pool, rdb := newPurchaseServer(t)
	ctx := context.Background()
	eventID, tierID := seedTier(t, pool, 50000, 2)
	_, jwtTok, purchaseTok := admittedUser(t, pool, eventID, 1)

	// Test rig: Redis believes 5, Postgres has zero available rows.
	if _, err := pool.Exec(ctx, `DELETE FROM tickets WHERE ticket_tier_id = $1`, tierID); err != nil {
		t.Fatal(err)
	}
	rdb.Set(ctx, booking.InventoryKey(tierID), 5, 0)

	resp, out := purchase(t, ts, jwtTok, "race-1", purchaseBody(eventID, tierID, 1, purchaseTok))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d body=%v, want 409", resp.StatusCode, out)
	}
	if out["error"].(map[string]any)["type"] != "sold_out" {
		t.Errorf("error type = %v", out["error"])
	}
	// Postgres won; the Lua decrement was compensated back.
	if got := redisInt(t, rdb, booking.InventoryKey(tierID)); got != 5 {
		t.Errorf("inventory after compensation = %d, want 5", got)
	}
}

// THE RACE: 3 tickets, 5 concurrent buyers, exactly 3 succeed, zero
// double-assignment.
func TestPurchaseConcurrencyRace(t *testing.T) {
	ts, pool, _ := newPurchaseServer(t)
	ctx := context.Background()
	eventID, tierID := seedTier(t, pool, 50000, 3)

	const n = 5
	type buyer struct{ jwtTok, purchaseTok string }
	buyers := make([]buyer, n)
	for i := range buyers {
		_, jwtTok, purchaseTok := admittedUser(t, pool, eventID, i)
		buyers[i] = buyer{jwtTok, purchaseTok}
	}

	statuses := make([]int, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range buyers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			resp, _ := purchase(t, ts, buyers[i].jwtTok, fmt.Sprintf("race-%d", i),
				purchaseBody(eventID, tierID, 1, buyers[i].purchaseTok))
			statuses[i] = resp.StatusCode
		}(i)
	}
	close(start)
	wg.Wait()

	var accepted, soldOut int
	for _, s := range statuses {
		switch s {
		case http.StatusAccepted:
			accepted++
		case http.StatusConflict:
			soldOut++
		default:
			t.Errorf("unexpected status %d", s)
		}
	}
	if accepted != 3 || soldOut != 2 {
		t.Fatalf("accepted=%d soldOut=%d, want 3 and 2", accepted, soldOut)
	}

	// Zero double-assignment: 3 held tickets, each on a distinct booking.
	var held, bookings int
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM tickets WHERE status = 'held'`).Scan(&held)
	pool.QueryRow(ctx, `SELECT COUNT(DISTINCT booking_id) FROM tickets WHERE status = 'held'`).Scan(&bookings)
	if held != 3 || bookings != 3 {
		t.Errorf("held=%d distinct bookings=%d, want 3 and 3", held, bookings)
	}
	var available int
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM tickets WHERE status = 'available'`).Scan(&available)
	if available != 0 {
		t.Errorf("available = %d, want 0", available)
	}
}

func TestHoldExpiryReleasesTickets(t *testing.T) {
	ts, pool, rdb := newPurchaseServer(t)
	ctx := context.Background()
	eventID, tierID := seedTier(t, pool, 50000, 10)
	_, jwtTok, purchaseTok := admittedUser(t, pool, eventID, 1)

	resp, out := purchase(t, ts, jwtTok, "exp-1", purchaseBody(eventID, tierID, 2, purchaseTok))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("purchase = %d, want 202", resp.StatusCode)
	}
	bookingID := out["data"].(map[string]any)["booking_id"].(string)
	invBefore := redisInt(t, rdb, booking.InventoryKey(tierID)) // 8

	if _, err := pool.Exec(ctx,
		`UPDATE booking SET hold_expires_at = NOW() - INTERVAL '1 second' WHERE id = $1`, bookingID); err != nil {
		t.Fatal(err)
	}
	if err := worker.HoldExpiryCycle(ctx, rdb, pool); err != nil {
		t.Fatalf("cycle: %v", err)
	}

	var status string
	pool.QueryRow(ctx, `SELECT status FROM booking WHERE id = $1`, bookingID).Scan(&status)
	if status != "failed" {
		t.Errorf("booking status = %s, want failed", status)
	}
	var released int
	pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM tickets WHERE ticket_tier_id = $1 AND status = 'available' AND booking_id IS NULL`,
		tierID).Scan(&released)
	if released != 10 {
		t.Errorf("available tickets = %d, want 10", released)
	}
	if got := redisInt(t, rdb, booking.InventoryKey(tierID)); got != invBefore+2 {
		t.Errorf("inventory = %d, want %d", got, invBefore+2)
	}
	var audits int
	pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM audit_log WHERE entity_id = $1 AND action IN ('booking_expired','tickets_released')`,
		bookingID).Scan(&audits)
	if audits != 2 {
		t.Errorf("audit rows = %d, want 2", audits)
	}

	// Second cycle: idempotent no-op.
	if err := worker.HoldExpiryCycle(ctx, rdb, pool); err != nil {
		t.Fatalf("second cycle: %v", err)
	}
	if got := redisInt(t, rdb, booking.InventoryKey(tierID)); got != invBefore+2 {
		t.Errorf("inventory after no-op cycle = %d, want %d", got, invBefore+2)
	}
}

func TestHoldExpiryNeverReleasesSold(t *testing.T) {
	ts, pool, rdb := newPurchaseServer(t)
	ctx := context.Background()
	eventID, tierID := seedTier(t, pool, 50000, 10)
	_, jwtTok, purchaseTok := admittedUser(t, pool, eventID, 1)

	resp, out := purchase(t, ts, jwtTok, "sold-1", purchaseBody(eventID, tierID, 2, purchaseTok))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("purchase = %d, want 202", resp.StatusCode)
	}
	bookingID := out["data"].(map[string]any)["booking_id"].(string)

	// Hand-set one of the booking's tickets to sold (as TX2 would).
	if _, err := pool.Exec(ctx,
		`UPDATE tickets SET status = 'sold'
		 WHERE id = (SELECT id FROM tickets WHERE booking_id = $1 LIMIT 1)`, bookingID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE booking SET hold_expires_at = NOW() - INTERVAL '1 second' WHERE id = $1`, bookingID); err != nil {
		t.Fatal(err)
	}
	if err := worker.HoldExpiryCycle(ctx, rdb, pool); err != nil {
		t.Fatalf("cycle: %v", err)
	}

	var sold, availableOfBooking int
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM tickets WHERE booking_id = $1 AND status = 'sold'`, bookingID).Scan(&sold)
	if sold != 1 {
		t.Errorf("sold tickets touched: count = %d, want 1 untouched", sold)
	}
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM tickets WHERE ticket_tier_id = $1 AND status = 'available'`, tierID).Scan(&availableOfBooking)
	if availableOfBooking != 9 {
		t.Errorf("available = %d, want 9 (only the held one released)", availableOfBooking)
	}
}
