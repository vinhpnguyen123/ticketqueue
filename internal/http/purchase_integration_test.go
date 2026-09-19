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

// paySrvURL is the most recently wired paymentsim URL, for by-key lookups.
var paySrvURL string

// payClient is the client for that same server, for tests that drive the
// workers directly (hold expiry now calls the provider).
var payClient *booking.PaymentClient

// newPurchaseServer wires auth + purchase routes against a real paymentsim
// httptest server.
func newPurchaseServer(t *testing.T) (*httptest.Server, *pgxpool.Pool, *redis.Client) {
	t.Helper()
	pool := testutil.TestDB(t)
	rdb := testutil.TestRedis(t)

	pay := httptest.NewServer(paymentsim.NewServer("ps_test_localdev"))
	t.Cleanup(pay.Close)
	paySrvURL = pay.URL // integration tests run serially; safe to stash for lookupCharge

	pc := booking.NewPaymentClient(pay.URL, "ps_test_localdev")
	pc.HTTP.Timeout = 1 * time.Second // shorter than the shrunk slow-token delay
	payClient = pc

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	api.NewAuthHandler(pool, rdb, testSecret).Mount(r)
	api.NewPurchaseHandler(pool, rdb, testSecret, pc).Mount(r)
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
	return purchaseBodyTok(eventID, tierID, qty, purchaseTok, "tok_visa")
}

func purchaseBodyTok(eventID, tierID string, qty int, purchaseTok, paymentTok string) map[string]any {
	return map[string]any{
		"event_id": eventID, "tier_id": tierID, "quantity": qty,
		"payment_token": paymentTok, "purchase_token": purchaseTok,
	}
}

// bookingIDOf pulls the booking id from either a success envelope (data) or an
// error envelope (error.details), whichever the response carries.
func bookingIDOf(t *testing.T, out map[string]any) string {
	t.Helper()
	if data, ok := out["data"].(map[string]any); ok {
		return data["booking_id"].(string)
	}
	if errObj, ok := out["error"].(map[string]any); ok {
		if d, ok := errObj["details"].(map[string]any); ok {
			return d["booking_id"].(string)
		}
	}
	t.Fatalf("no booking_id in response: %v", out)
	return ""
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

// Test 1: tok_visa happy path → 201 confirmed; tickets sold; payments
// succeeded with provider_txn_id; audit rows present; Redis NOT given back.
func TestPurchaseConfirmed(t *testing.T) {
	ts, pool, rdb := newPurchaseServer(t)
	ctx := context.Background()
	eventID, tierID := seedTier(t, pool, 50000, 100)
	userID, jwtTok, purchaseTok := admittedUser(t, pool, eventID, 1)

	resp, out := purchase(t, ts, jwtTok, "buy-1", purchaseBody(eventID, tierID, 2, purchaseTok))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d body=%v, want 201", resp.StatusCode, out)
	}
	data := out["data"].(map[string]any)
	bookingID := data["booking_id"].(string)
	if data["status"] != "confirmed" {
		t.Errorf("status = %v, want confirmed", data["status"])
	}
	if tix, ok := data["tickets"].([]any); !ok || len(tix) != 2 {
		t.Errorf("tickets = %v, want 2", data["tickets"])
	}
	if ref, _ := data["reference"].(string); ref == "" {
		t.Errorf("missing reference")
	}

	var total int64
	var status string
	var version int
	if err := pool.QueryRow(ctx,
		`SELECT total_cents, status, version FROM booking WHERE id = $1 AND user_id = $2`,
		bookingID, userID,
	).Scan(&total, &status, &version); err != nil {
		t.Fatalf("booking row: %v", err)
	}
	if total != 100000 || status != "confirmed" || version != 1 {
		t.Errorf("booking total=%d status=%s version=%d, want 100000/confirmed/1", total, status, version)
	}

	var sold int
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM tickets WHERE booking_id = $1 AND status = 'sold'`, bookingID).Scan(&sold)
	if sold != 2 {
		t.Errorf("sold tickets = %d, want 2", sold)
	}

	// Confirmed sale keeps the inventory decremented — never incremented back.
	if got := redisInt(t, rdb, booking.InventoryKey(tierID)); got != 98 {
		t.Errorf("inventory = %d, want 98", got)
	}

	var payStatus string
	var txn *string
	if err := pool.QueryRow(ctx,
		`SELECT status, provider_txn_id FROM payments WHERE booking_id = $1`, bookingID,
	).Scan(&payStatus, &txn); err != nil {
		t.Fatalf("payments row: %v", err)
	}
	if payStatus != "succeeded" || txn == nil || *txn == "" {
		t.Errorf("payment status=%s txn=%v, want succeeded with a txn id", payStatus, txn)
	}

	var audits int
	pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM audit_log WHERE entity_id = $1
		 AND action IN ('booking_created','booking_confirmed','payment_succeeded')`,
		bookingID).Scan(&audits)
	// Three, not four: booking_created and tickets_held were merged, because
	// TX1 cannot produce one without the other.
	if audits != 3 {
		t.Errorf("audit rows = %d, want 3", audits)
	}
}

// Test 2: tok_declined → 402; booking failed; tickets released; Redis given
// back; payments 'failed'.
func TestPurchaseDeclined(t *testing.T) {
	ts, pool, rdb := newPurchaseServer(t)
	ctx := context.Background()
	eventID, tierID := seedTier(t, pool, 50000, 10)
	_, jwtTok, purchaseTok := admittedUser(t, pool, eventID, 1)

	resp, out := purchase(t, ts, jwtTok, "dec-1",
		purchaseBodyTok(eventID, tierID, 2, purchaseTok, "tok_declined"))
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("status = %d body=%v, want 402", resp.StatusCode, out)
	}
	if out["error"].(map[string]any)["type"] != "payment_declined" {
		t.Errorf("error type = %v", out["error"])
	}
	bookingID := bookingIDOf(t, out)

	var status string
	pool.QueryRow(ctx, `SELECT status FROM booking WHERE id = $1`, bookingID).Scan(&status)
	if status != "failed" {
		t.Errorf("booking status = %s, want failed", status)
	}
	var stillHeld int
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM tickets WHERE booking_id = $1`, bookingID).Scan(&stillHeld)
	if stillHeld != 0 {
		t.Errorf("tickets still on booking = %d, want 0 (released + nulled)", stillHeld)
	}
	var available int
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM tickets WHERE ticket_tier_id = $1 AND status = 'available'`, tierID).Scan(&available)
	if available != 10 {
		t.Errorf("available = %d, want 10", available)
	}
	if got := redisInt(t, rdb, booking.InventoryKey(tierID)); got != 10 {
		t.Errorf("inventory = %d, want 10 (given back)", got)
	}
	var payStatus string
	pool.QueryRow(ctx, `SELECT status FROM payments WHERE booking_id = $1`, bookingID).Scan(&payStatus)
	if payStatus != "failed" {
		t.Errorf("payment status = %s, want failed", payStatus)
	}
}

// Test 3: tok_slow_30s with a client timeout below the (shrunk) delay → 504
// payment_processing_unclear; booking STILL held; payments 'unknown'; Redis
// NOT given back.
func TestPurchaseAmbiguousTimeout(t *testing.T) {
	paymentsim.SetTestDelays(2 * time.Second) // > the 1s client timeout
	ts, pool, rdb := newPurchaseServer(t)
	ctx := context.Background()
	eventID, tierID := seedTier(t, pool, 50000, 10)
	_, jwtTok, purchaseTok := admittedUser(t, pool, eventID, 1)

	resp, out := purchase(t, ts, jwtTok, "amb-1",
		purchaseBodyTok(eventID, tierID, 2, purchaseTok, "tok_slow_30s"))
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status = %d body=%v, want 504", resp.StatusCode, out)
	}
	if out["error"].(map[string]any)["type"] != "payment_processing_unclear" {
		t.Errorf("error type = %v", out["error"])
	}
	bookingID := bookingIDOf(t, out)

	assertHeldUnknown(t, ctx, pool, bookingID)
	if got := redisInt(t, rdb, booking.InventoryKey(tierID)); got != 8 {
		t.Errorf("inventory = %d, want 8 (NOT given back)", got)
	}
}

// Test 4: tok_drops_response → 504; same held/unknown state; AND the provider
// actually holds a succeeded charge (money moved, response lost).
func TestPurchaseDroppedResponse(t *testing.T) {
	ts, pool, rdb := newPurchaseServer(t)
	ctx := context.Background()
	eventID, tierID := seedTier(t, pool, 50000, 10)
	_, jwtTok, purchaseTok := admittedUser(t, pool, eventID, 1)

	resp, out := purchase(t, ts, jwtTok, "drop-key-1",
		purchaseBodyTok(eventID, tierID, 2, purchaseTok, "tok_drops_response"))
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status = %d body=%v, want 504", resp.StatusCode, out)
	}
	bookingID := bookingIDOf(t, out)
	assertHeldUnknown(t, ctx, pool, bookingID)
	if got := redisInt(t, rdb, booking.InventoryKey(tierID)); got != 8 {
		t.Errorf("inventory = %d, want 8 (NOT given back)", got)
	}

	// The money/response split: provider has a succeeded charge under the key.
	var bookingKey string
	pool.QueryRow(ctx, `SELECT idempotency_key FROM booking WHERE id = $1`, bookingID).Scan(&bookingKey)
	ch := lookupCharge(t, bookingKey)
	if ch["status"] != "succeeded" {
		t.Errorf("provider charge status = %v, want succeeded", ch["status"])
	}
}

// Test 5: idempotent replay — same key, second call replays the 201 verbatim,
// no second charge, no extra tickets sold.
func TestPurchaseIdempotentReplay(t *testing.T) {
	ts, pool, _ := newPurchaseServer(t)
	ctx := context.Background()
	eventID, tierID := seedTier(t, pool, 50000, 10)
	_, jwtTok, purchaseTok := admittedUser(t, pool, eventID, 1)

	body := purchaseBody(eventID, tierID, 2, purchaseTok)
	resp1, out1 := purchase(t, ts, jwtTok, "rep-1", body)
	if resp1.StatusCode != http.StatusCreated {
		t.Fatalf("first = %d body=%v, want 201", resp1.StatusCode, out1)
	}
	resp2, out2 := purchase(t, ts, jwtTok, "rep-1", body)
	if resp2.StatusCode != http.StatusCreated {
		t.Fatalf("replay = %d body=%v, want 201", resp2.StatusCode, out2)
	}
	if resp2.Header.Get("Idempotency-Replayed") != "true" {
		t.Errorf("missing Idempotency-Replayed header on replay")
	}
	bookingID := out1["data"].(map[string]any)["booking_id"].(string)
	if out2["data"].(map[string]any)["booking_id"] != bookingID {
		t.Errorf("replay returned a different booking_id")
	}

	var payments, sold int
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM payments WHERE booking_id = $1`, bookingID).Scan(&payments)
	if payments != 1 {
		t.Errorf("payments rows = %d, want 1 (no second charge)", payments)
	}
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM tickets WHERE status = 'sold'`).Scan(&sold)
	if sold != 2 {
		t.Errorf("sold tickets = %d, want 2 (no extra)", sold)
	}
}

// Test 6: payment-vs-expiry race — the hold expires (and the worker releases
// the tickets) before TX2 confirms a succeeded charge. Driven at the booking
// layer so the interleaving is deterministic.
func TestPurchaseExpiryRaceAfterCharge(t *testing.T) {
	_, pool, rdb := newPurchaseServer(t)
	ctx := context.Background()
	eventID, tierID := seedTier(t, pool, 50000, 10)
	userID, _, _ := admittedUser(t, pool, eventID, 1)
	rdb.Set(ctx, booking.InventoryKey(tierID), 10, 0)

	tier, err := booking.LoadTier(ctx, pool, tierID)
	if err != nil {
		t.Fatalf("load tier: %v", err)
	}
	_ = eventID
	held, err := booking.HoldTickets(ctx, pool, userID, tier, 2, "race-key-1", "req-1")
	if err != nil {
		t.Fatalf("TX1: %v", err)
	}

	// Force the hold to expire, then let the expiry worker win the race.
	if _, err := pool.Exec(ctx,
		`UPDATE booking SET hold_expires_at = NOW() - INTERVAL '1 second' WHERE id = $1`, held.BookingID); err != nil {
		t.Fatal(err)
	}
	if err := worker.HoldExpiryCycle(ctx, rdb, pool, payClient); err != nil {
		t.Fatalf("expiry cycle: %v", err)
	}

	// Now TX2 finalizes a SUCCEEDED charge against the already-expired booking.
	fin, err := booking.Finalize(ctx, pool, held, tierID, userID, "req-1",
		booking.ChargeResult{Outcome: booking.Succeeded, TxnID: "ch_race", CardBrand: "visa", CardLast4: "4242"})
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if fin.Status != "expired_after_charge" {
		t.Fatalf("finalize status = %s, want expired_after_charge", fin.Status)
	}

	var bookingStatus, payStatus string
	pool.QueryRow(ctx, `SELECT status FROM booking WHERE id = $1`, held.BookingID).Scan(&bookingStatus)
	if bookingStatus != "failed" {
		t.Errorf("booking status = %s, want failed (expiry won)", bookingStatus)
	}
	pool.QueryRow(ctx, `SELECT status FROM payments WHERE booking_id = $1`, held.BookingID).Scan(&payStatus)
	if payStatus != "succeeded" {
		t.Errorf("payment status = %s, want succeeded (money moved, recorded)", payStatus)
	}
	var recon int
	pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM audit_log WHERE entity_id = $1 AND action = 'reconciliation_needed' AND actor_type = 'system'`,
		held.BookingID).Scan(&recon)
	if recon != 1 {
		t.Errorf("reconciliation_needed audit rows = %d, want 1", recon)
	}
}

func assertHeldUnknown(t *testing.T, ctx context.Context, pool *pgxpool.Pool, bookingID string) {
	t.Helper()
	var status string
	pool.QueryRow(ctx, `SELECT status FROM booking WHERE id = $1`, bookingID).Scan(&status)
	if status != "held" {
		t.Errorf("booking status = %s, want held", status)
	}
	var held int
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM tickets WHERE booking_id = $1 AND status = 'held'`, bookingID).Scan(&held)
	if held != 2 {
		t.Errorf("held tickets = %d, want 2 (still held)", held)
	}
	var payStatus string
	pool.QueryRow(ctx, `SELECT status FROM payments WHERE booking_id = $1`, bookingID).Scan(&payStatus)
	if payStatus != "unknown" {
		t.Errorf("payment status = %s, want unknown", payStatus)
	}
}

// lookupCharge queries the paymentsim by idempotency key via a fresh client
// against the same in-process server URL captured by the test harness.
func lookupCharge(t *testing.T, key string) map[string]any {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, paySrvURL+"/v1/charges/by-idempotency-key/"+key, nil)
	req.Header.Set("Authorization", "Bearer ps_test_localdev")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("lookup charge: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("lookup charge status = %d, want 200", resp.StatusCode)
	}
	out := map[string]any{}
	b, _ := io.ReadAll(resp.Body)
	json.Unmarshal(b, &out)
	return out
}

func TestPurchaseDuplicateGuard(t *testing.T) {
	ts, pool, _ := newPurchaseServer(t)
	eventID, tierID := seedTier(t, pool, 50000, 10)
	_, jwtTok, purchaseTok := admittedUser(t, pool, eventID, 1)

	// First purchase must stay 'held' for the duplicate guard to fire; a
	// confirmed booking is no longer "in progress". tok_drops_response holds it.
	resp, _ := purchase(t, ts, jwtTok, "dup-1",
		purchaseBodyTok(eventID, tierID, 1, purchaseTok, "tok_drops_response"))
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("first purchase = %d, want 504", resp.StatusCode)
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

	var confirmed, soldOut int
	for _, s := range statuses {
		switch s {
		case http.StatusCreated:
			confirmed++
		case http.StatusConflict:
			soldOut++
		default:
			t.Errorf("unexpected status %d", s)
		}
	}
	if confirmed != 3 || soldOut != 2 {
		t.Fatalf("confirmed=%d soldOut=%d, want 3 and 2", confirmed, soldOut)
	}

	// Zero double-assignment: 3 sold tickets, each on a distinct booking.
	var sold, bookings int
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM tickets WHERE status = 'sold'`).Scan(&sold)
	pool.QueryRow(ctx, `SELECT COUNT(DISTINCT booking_id) FROM tickets WHERE status = 'sold'`).Scan(&bookings)
	if sold != 3 || bookings != 3 {
		t.Errorf("sold=%d distinct bookings=%d, want 3 and 3", sold, bookings)
	}
	var available int
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM tickets WHERE status = 'available'`).Scan(&available)
	if available != 0 {
		t.Errorf("available = %d, want 0", available)
	}
}

// The hold expired on a booking the provider had already charged
// (tok_drops_response). Before the §17 fix this worker released the tickets and
// resold the customer's seat. It now asks the provider first and confirms.
func TestHoldExpiryConfirmsChargedBooking(t *testing.T) {
	ts, pool, rdb := newPurchaseServer(t)
	ctx := context.Background()
	eventID, tierID := seedTier(t, pool, 50000, 10)
	_, jwtTok, purchaseTok := admittedUser(t, pool, eventID, 1)

	resp, out := purchase(t, ts, jwtTok, "exp-1",
		purchaseBodyTok(eventID, tierID, 2, purchaseTok, "tok_drops_response"))
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("purchase = %d, want 504", resp.StatusCode)
	}
	bookingID := bookingIDOf(t, out)
	invBefore := redisInt(t, rdb, booking.InventoryKey(tierID)) // 8

	if _, err := pool.Exec(ctx,
		`UPDATE booking SET hold_expires_at = NOW() - INTERVAL '1 second' WHERE id = $1`, bookingID); err != nil {
		t.Fatal(err)
	}
	if err := worker.HoldExpiryCycle(ctx, rdb, pool, payClient); err != nil {
		t.Fatalf("cycle: %v", err)
	}

	var status string
	pool.QueryRow(ctx, `SELECT status FROM booking WHERE id = $1`, bookingID).Scan(&status)
	if status != "confirmed" {
		t.Errorf("booking status = %s, want confirmed (the card was charged)", status)
	}
	var sold int
	pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM tickets WHERE booking_id = $1 AND status = 'sold'`, bookingID).Scan(&sold)
	if sold != 2 {
		t.Errorf("sold tickets = %d, want 2 (never resold under a paying customer)", sold)
	}
	if got := redisInt(t, rdb, booking.InventoryKey(tierID)); got != invBefore {
		t.Errorf("inventory = %d, want %d (NOT given back)", got, invBefore)
	}
	var audits int
	pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM audit_log WHERE entity_id = $1 AND actor_type = 'worker'
		 AND metadata->>'source' = 'hold_expiry'`, bookingID).Scan(&audits)
	if audits != 2 {
		t.Errorf("hold_expiry audit rows = %d, want 2 (confirmed + payment_succeeded)", audits)
	}

	// Second cycle: idempotent no-op.
	if err := worker.HoldExpiryCycle(ctx, rdb, pool, payClient); err != nil {
		t.Fatalf("second cycle: %v", err)
	}
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM payments WHERE booking_id = $1`, bookingID).Scan(&sold)
	if sold != 2 {
		t.Errorf("payments rows = %d, want 2 (the unknown attempt + the succeeded resolution)", sold)
	}
}

func TestHoldExpiryNeverReleasesSold(t *testing.T) {
	ts, pool, rdb := newPurchaseServer(t)
	ctx := context.Background()
	eventID, tierID := seedTier(t, pool, 50000, 10)
	_, jwtTok, purchaseTok := admittedUser(t, pool, eventID, 1)

	resp, out := purchase(t, ts, jwtTok, "sold-1",
		purchaseBodyTok(eventID, tierID, 2, purchaseTok, "tok_drops_response"))
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("purchase = %d, want 504", resp.StatusCode)
	}
	bookingID := bookingIDOf(t, out)

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
	if err := worker.HoldExpiryCycle(ctx, rdb, pool, payClient); err != nil {
		t.Fatalf("cycle: %v", err)
	}

	// The provider charged this booking, so both tickets end up sold: the one
	// already flipped, and the one still held. Nothing is released.
	var sold, available int
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM tickets WHERE booking_id = $1 AND status = 'sold'`, bookingID).Scan(&sold)
	if sold != 2 {
		t.Errorf("sold tickets = %d, want 2", sold)
	}
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM tickets WHERE ticket_tier_id = $1 AND status = 'available'`, tierID).Scan(&available)
	if available != 8 {
		t.Errorf("available = %d, want 8 (nothing released)", available)
	}
}

// The benign twin of the expiry race: hold-expiry asked the provider, got the
// same 'succeeded' answer TX2 has, and confirmed the booking first. TX2's
// guarded UPDATE loses — but the customer HAS their tickets, so this must not
// duplicate the payment or raise a refund flag.
func TestPurchaseSiblingConfirmedFirst(t *testing.T) {
	_, pool, rdb := newPurchaseServer(t)
	ctx := context.Background()
	eventID, tierID := seedTier(t, pool, 50000, 10)
	userID, _, _ := admittedUser(t, pool, eventID, 1)
	rdb.Set(ctx, booking.InventoryKey(tierID), 10, 0)

	tier, err := booking.LoadTier(ctx, pool, tierID)
	if err != nil {
		t.Fatalf("load tier: %v", err)
	}
	held, err := booking.HoldTickets(ctx, pool, userID, tier, 2, "sibling-1", "req-s")
	if err != nil {
		t.Fatalf("TX1: %v", err)
	}
	// The provider really did charge this key.
	if res := payClient.Charge(ctx, "sibling-1", held.TotalCents, held.Currency, "tok_visa"); res.Outcome != booking.Succeeded {
		t.Fatalf("setup charge outcome = %v", res.Outcome)
	}

	// Hold expires; the worker asks, learns it was charged, and confirms.
	if _, err := pool.Exec(ctx,
		`UPDATE booking SET hold_expires_at = NOW() - INTERVAL '1 second' WHERE id = $1`, held.BookingID); err != nil {
		t.Fatal(err)
	}
	if err := worker.HoldExpiryCycle(ctx, rdb, pool, payClient); err != nil {
		t.Fatalf("expiry cycle: %v", err)
	}

	// Only now does TX2 finalize the same succeeded charge.
	fin, err := booking.Finalize(ctx, pool, held, tierID, userID, "req-s",
		booking.ChargeResult{Outcome: booking.Succeeded, TxnID: "ch_sibling", CardBrand: "visa", CardLast4: "4242"})
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if fin.Status != "confirmed" {
		t.Errorf("finalize status = %s, want confirmed (the customer has their tickets)", fin.Status)
	}
	if len(fin.Tickets) != 2 {
		t.Errorf("tickets returned = %d, want 2", len(fin.Tickets))
	}

	var payments, flags int
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM payments WHERE booking_id = $1`, held.BookingID).Scan(&payments)
	if payments != 1 {
		t.Errorf("payments rows = %d, want exactly 1 (no duplicate for one charge)", payments)
	}
	pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM audit_log WHERE entity_id = $1 AND action = 'reconciliation_needed'`,
		held.BookingID).Scan(&flags)
	if flags != 0 {
		t.Errorf("reconciliation_needed rows = %d, want 0 (nothing to refund)", flags)
	}
}
