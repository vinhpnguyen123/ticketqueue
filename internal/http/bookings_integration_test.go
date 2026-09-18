//go:build integration

package http_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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
)

// providerCalls counts requests reaching the payment provider, so a test can
// assert that no provider call happened at all.
type providerCalls struct {
	n atomic.Int64
	h http.Handler
}

func (p *providerCalls) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.n.Add(1)
	p.h.ServeHTTP(w, r)
}

// newBookingsServer wires the bookings routes plus a call-counting provider.
func newBookingsServer(t *testing.T) (*httptest.Server, *pgxpool.Pool, *redis.Client, *booking.PaymentClient, *providerCalls) {
	t.Helper()
	pool := testutil.TestDB(t)
	rdb := testutil.TestRedis(t)

	counter := &providerCalls{h: paymentsim.NewServer("ps_test_localdev")}
	pay := httptest.NewServer(counter)
	t.Cleanup(pay.Close)
	pc := booking.NewPaymentClient(pay.URL, "ps_test_localdev")

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	api.NewBookingsHandler(pool, rdb, testSecret, pc).Mount(r)
	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
	return ts, pool, rdb, pc, counter
}

// bookingUser creates a user and returns its id plus a JWT.
func bookingUser(t *testing.T, pool *pgxpool.Pool, email string) (string, string) {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO users (email, password_hash) VALUES ($1, 'x') RETURNING id`, email).Scan(&id); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	tok, err := auth.GenerateToken(id, email, false, testSecret, time.Hour)
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	return id, tok
}

// hold runs a real TX1 for a given user, optionally ageing the booking.
func hold(t *testing.T, pool *pgxpool.Pool, rdb *redis.Client, userID, tierID, key string, qty, ageSeconds int) booking.Held {
	t.Helper()
	ctx := context.Background()
	tier, err := booking.LoadTier(ctx, pool, tierID)
	if err != nil {
		t.Fatalf("load tier: %v", err)
	}
	if err := booking.ReserveInventory(ctx, rdb, pool, tierID, qty); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	held, err := booking.HoldTickets(ctx, pool, userID, tier, qty, key, "req-b")
	if err != nil {
		t.Fatalf("TX1: %v", err)
	}
	if ageSeconds > 0 {
		if _, err := pool.Exec(ctx,
			`UPDATE booking SET created_at = NOW() - make_interval(secs => $2) WHERE id = $1`,
			held.BookingID, ageSeconds); err != nil {
			t.Fatalf("age booking: %v", err)
		}
	}
	return held
}

// authGet performs an authenticated GET, returning status, decoded body and raw bytes.
func authGet(t *testing.T, ts *httptest.Server, path, token string) (int, map[string]any, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("decode %q: %v", raw, err)
		}
	}
	return resp.StatusCode, out, string(raw)
}

// confirmedBooking drives a full happy purchase at the booking layer.
func confirmedBooking(t *testing.T, pool *pgxpool.Pool, rdb *redis.Client, pc *booking.PaymentClient, userID, tierID, key string) booking.Held {
	t.Helper()
	ctx := context.Background()
	held := hold(t, pool, rdb, userID, tierID, key, 2, 0)
	charge := pc.Charge(ctx, key, held.TotalCents, held.Currency, "tok_visa")
	if _, err := booking.Finalize(ctx, pool, held, tierID, userID, "req-b", charge); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	return held
}

// Test 1: the list shows a confirmed booking with a derived reference and
// money-as-object.
func TestBookingsListShape(t *testing.T) {
	ts, pool, rdb, pc, _ := newBookingsServer(t)
	_, tierID := seedTier(t, pool, 50000, 10)
	userID, tok := bookingUser(t, pool, "lister@test.dev")
	held := confirmedBooking(t, pool, rdb, pc, userID, tierID, "b-list-1")

	status, out, _ := authGet(t, ts, "/v1/bookings", tok)
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, out)
	}
	rows := out["data"].([]any)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	row := rows[0].(map[string]any)

	wantRef := "TQ-" + strings.ToUpper(held.BookingID[:8])
	if row["reference"] != wantRef {
		t.Errorf("reference = %v, want %s (derived, not stored)", row["reference"], wantRef)
	}
	if row["status"] != "confirmed" {
		t.Errorf("status = %v, want confirmed", row["status"])
	}
	total := row["total"].(map[string]any)
	if total["amount"].(float64) != 100000 || total["currency"] != "USD" {
		t.Errorf("total = %v, want money-as-object", total)
	}
	if row["event"].(map[string]any)["name"] == nil || row["tier"].(map[string]any)["name"] == nil {
		t.Errorf("event/tier missing: %v", row)
	}
	if row["confirmed_at"] == nil {
		t.Errorf("confirmed_at is null on a confirmed booking")
	}
}

// Test 2: cursor pagination walks every booking exactly once.
func TestBookingsListPagination(t *testing.T) {
	ts, pool, rdb, pc, _ := newBookingsServer(t)
	_, tierID := seedTier(t, pool, 50000, 20)
	userID, tok := bookingUser(t, pool, "pager@test.dev")
	for i := 0; i < 5; i++ {
		confirmedBooking(t, pool, rdb, pc, userID, tierID, fmt.Sprintf("b-page-%d", i))
	}

	seen := map[string]bool{}
	path := "/v1/bookings?limit=2"
	for page := 0; page < 5; page++ {
		status, out, _ := authGet(t, ts, path, tok)
		if status != http.StatusOK {
			t.Fatalf("page %d status = %d body=%v", page, status, out)
		}
		rows := out["data"].([]any)
		for _, raw := range rows {
			id := raw.(map[string]any)["id"].(string)
			if seen[id] {
				t.Errorf("booking %s returned twice", id)
			}
			seen[id] = true
		}
		pg := out["pagination"].(map[string]any)
		if !pg["has_more"].(bool) {
			if len(rows) != 1 {
				t.Errorf("last page rows = %d, want 1", len(rows))
			}
			break
		}
		if len(rows) != 2 {
			t.Errorf("page %d rows = %d, want 2", page, len(rows))
		}
		path = "/v1/bookings?limit=2&cursor=" + pg["next_cursor"].(string)
	}
	if len(seen) != 5 {
		t.Errorf("saw %d distinct bookings, want 5", len(seen))
	}

	if status, _, _ := authGet(t, ts, "/v1/bookings?cursor=not-a-uuid", tok); status != http.StatusUnprocessableEntity {
		t.Errorf("bad cursor status = %d, want 422", status)
	}
}

// Test 3: another user's booking is invisible, and answers 404 — not 403,
// which would confirm that the id is real.
func TestBookingsDetailHidesOtherUsers(t *testing.T) {
	ts, pool, rdb, pc, _ := newBookingsServer(t)
	_, tierID := seedTier(t, pool, 50000, 10)
	alice, _ := bookingUser(t, pool, "alice@test.dev")
	_, bobTok := bookingUser(t, pool, "bob@test.dev")
	held := confirmedBooking(t, pool, rdb, pc, alice, tierID, "b-alice-1")

	status, out, _ := authGet(t, ts, "/v1/bookings/"+held.BookingID, bobTok)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d body=%v, want 404", status, out)
	}
	if got := out["error"].(map[string]any)["type"]; got != "booking_not_found" {
		t.Errorf("error type = %v, want booking_not_found", got)
	}
}

// Test 4: the raw response must never carry internal fields.
func TestBookingsDetailLeaksNothing(t *testing.T) {
	ts, pool, rdb, pc, _ := newBookingsServer(t)
	_, tierID := seedTier(t, pool, 50000, 10)
	userID, tok := bookingUser(t, pool, "privacy@test.dev")
	held := confirmedBooking(t, pool, rdb, pc, userID, tierID, "b-secret-key-1")

	status, out, raw := authGet(t, ts, "/v1/bookings/"+held.BookingID, tok)
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, out)
	}
	for _, forbidden := range []string{"idempotency_key", "provider_response", "b-secret-key-1"} {
		if strings.Contains(raw, forbidden) {
			t.Errorf("response leaks %q:\n%s", forbidden, raw)
		}
	}
	if len(out["data"].(map[string]any)["tickets"].([]any)) != 2 {
		t.Errorf("tickets missing from detail: %v", out)
	}
}

// Test 5: a stuck booking resolves on read, through the shared resolver.
func TestBookingsDetailSyncReconciles(t *testing.T) {
	ts, pool, rdb, pc, calls := newBookingsServer(t)
	ctx := context.Background()
	_, tierID := seedTier(t, pool, 50000, 10)
	userID, tok := bookingUser(t, pool, "stuck@test.dev")

	held := hold(t, pool, rdb, userID, tierID, "b-stuck-1", 2, int(booking.StuckAfter.Seconds())+60)
	// The provider charged; the response was lost.
	if res := pc.Charge(ctx, "b-stuck-1", held.TotalCents, held.Currency, "tok_visa"); res.Outcome != booking.Succeeded {
		t.Fatalf("setup charge = %v", res.Outcome)
	}
	before := calls.n.Load()

	status, out, _ := authGet(t, ts, "/v1/bookings/"+held.BookingID, tok)
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, out)
	}
	data := out["data"].(map[string]any)
	if data["status"] != "confirmed" {
		t.Errorf("status = %v, want confirmed (resolved on read)", data["status"])
	}
	if data["payment_status"] != "succeeded" {
		t.Errorf("payment_status = %v, want succeeded", data["payment_status"])
	}
	if calls.n.Load() == before {
		t.Errorf("no provider call was made for a stuck booking")
	}

	// Charged exactly once, and the trail names the path that did it.
	var payments int
	pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM payments WHERE booking_id = $1 AND status = 'succeeded'`,
		held.BookingID).Scan(&payments)
	if payments != 1 {
		t.Errorf("succeeded payments rows = %d, want 1", payments)
	}
	var source string
	if err := pool.QueryRow(ctx,
		`SELECT metadata->>'source' FROM audit_log
		 WHERE entity_id = $1 AND action = 'booking_confirmed'`, held.BookingID).Scan(&source); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if source != "sync_retry" {
		t.Errorf("audit source = %q, want sync_retry", source)
	}
}

// Test 6: the provider is down. The user sees 'held', not an error, and nothing
// is guessed.
func TestBookingsDetailSyncReconcileProviderDown(t *testing.T) {
	pool, rdb := testutil.TestDB(t), testutil.TestRedis(t)
	dead := booking.NewPaymentClient("http://127.0.0.1:1", "ps_test_localdev")
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	api.NewBookingsHandler(pool, rdb, testSecret, dead).Mount(r)
	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)

	_, tierID := seedTier(t, pool, 50000, 10)
	userID, tok := bookingUser(t, pool, "down@test.dev")
	held := hold(t, pool, rdb, userID, tierID, "b-down-1", 2, int(booking.StuckAfter.Seconds())+60)

	status, out, _ := authGet(t, ts, "/v1/bookings/"+held.BookingID, tok)
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v, want 200", status, out)
	}
	if got := out["data"].(map[string]any)["status"]; got != "held" {
		t.Errorf("status = %v, want held (never guess)", got)
	}
	var dbStatus string
	pool.QueryRow(context.Background(), `SELECT status FROM booking WHERE id = $1`, held.BookingID).Scan(&dbStatus)
	if dbStatus != "held" {
		t.Errorf("stored status = %s, want held", dbStatus)
	}
}

// Tests 7+8: no provider call for a young booking, and none from the list
// endpoint at all — a list must never fan out to the provider.
func TestBookingsNoProviderCallWhenNotStuck(t *testing.T) {
	ts, pool, rdb, _, calls := newBookingsServer(t)
	_, tierID := seedTier(t, pool, 50000, 10)
	userID, tok := bookingUser(t, pool, "young@test.dev")
	held := hold(t, pool, rdb, userID, tierID, "b-young-1", 2, 0) // fresh

	before := calls.n.Load()
	status, out, _ := authGet(t, ts, "/v1/bookings/"+held.BookingID, tok)
	if status != http.StatusOK {
		t.Fatalf("detail status = %d body=%v", status, out)
	}
	if got := out["data"].(map[string]any)["status"]; got != "held" {
		t.Errorf("status = %v, want held", got)
	}
	if n := calls.n.Load() - before; n != 0 {
		t.Errorf("provider calls for a young booking = %d, want 0", n)
	}

	// Age it so it WOULD be reconciled, then hit the list endpoint.
	if _, err := pool.Exec(context.Background(),
		`UPDATE booking SET created_at = NOW() - make_interval(secs => $2) WHERE id = $1`,
		held.BookingID, int(booking.StuckAfter.Seconds())+60); err != nil {
		t.Fatal(err)
	}
	before = calls.n.Load()
	if status, out, _ := authGet(t, ts, "/v1/bookings", tok); status != http.StatusOK {
		t.Fatalf("list status = %d body=%v", status, out)
	}
	if n := calls.n.Load() - before; n != 0 {
		t.Errorf("provider calls from the list endpoint = %d, want 0 (no fan-out)", n)
	}
}

// Test 9: payment_status follows the §6 rule when both rows exist.
func TestBookingsDetailPaymentStatusSkipsUnknownRow(t *testing.T) {
	ts, pool, rdb, pc, _ := newBookingsServer(t)
	ctx := context.Background()
	_, tierID := seedTier(t, pool, 50000, 10)
	userID, tok := bookingUser(t, pool, "healed@test.dev")

	held := hold(t, pool, rdb, userID, tierID, "b-healed-1", 2, int(booking.StuckAfter.Seconds())+60)
	// Ambiguous first: TX2 writes the 'unknown' row and leaves it held.
	if _, err := booking.Finalize(ctx, pool, held, tierID, userID, "req-b",
		booking.ChargeResult{Outcome: booking.Ambiguous}); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if res := pc.Charge(ctx, "b-healed-1", held.TotalCents, held.Currency, "tok_visa"); res.Outcome != booking.Succeeded {
		t.Fatalf("setup charge = %v", res.Outcome)
	}

	status, out, _ := authGet(t, ts, "/v1/bookings/"+held.BookingID, tok)
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, out)
	}
	data := out["data"].(map[string]any)
	if data["payment_status"] != "succeeded" {
		t.Errorf("payment_status = %v, want succeeded — the 'unknown' row is history", data["payment_status"])
	}
	if len(data["payments"].([]any)) != 2 {
		t.Errorf("payments rows in response = %d, want 2 (both kept, as history)", len(data["payments"].([]any)))
	}
}
