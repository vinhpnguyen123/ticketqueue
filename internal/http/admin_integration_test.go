//go:build integration

package http_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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

// newAdminServer wires the admin routes against a real paymentsim, returning
// the server plus an admin and a non-admin JWT.
func newAdminServer(t *testing.T) (ts *httptest.Server, pool *pgxpool.Pool, rdb *redis.Client, pc *booking.PaymentClient, adminTok, userTok string) {
	t.Helper()
	pool = testutil.TestDB(t)
	rdb = testutil.TestRedis(t)

	pay := httptest.NewServer(paymentsim.NewServer("ps_test_localdev"))
	t.Cleanup(pay.Close)
	pc = booking.NewPaymentClient(pay.URL, "ps_test_localdev")

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	api.NewAdminHandler(pool, rdb, testSecret, pc).Mount(r)
	ts = httptest.NewServer(r)
	t.Cleanup(ts.Close)

	_, adminTok = seedAdmin(t, pool, "admin@test.dev", true)
	_, userTok = seedAdmin(t, pool, "mortal@test.dev", false)
	return ts, pool, rdb, pc, adminTok, userTok
}

// seedAdmin creates a user with the given admin flag and returns its id + JWT.
func seedAdmin(t *testing.T, pool *pgxpool.Pool, email string, isAdmin bool) (string, string) {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO users (email, password_hash, is_admin) VALUES ($1, 'x', $2) RETURNING id`,
		email, isAdmin).Scan(&id); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	tok, err := auth.GenerateToken(id, email, isAdmin, testSecret, time.Hour)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	return id, tok
}

// adminReq performs a request with the given bearer token.
func adminReq(t *testing.T, ts *httptest.Server, method, path, token string) (*http.Response, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("decode body %q: %v", raw, err)
		}
	}
	return resp, out
}

// adminList unwraps a {"data": [...]} envelope.
func adminList(t *testing.T, out map[string]any) []any {
	t.Helper()
	data, ok := out["data"].([]any)
	if !ok {
		t.Fatalf("response has no data array: %v", out)
	}
	return data
}

// seedHeld runs a real TX1 and returns the held booking.
func seedHeld(t *testing.T, pool *pgxpool.Pool, rdb *redis.Client, tierID, idemKey string, qty int, ageSeconds int) booking.Held {
	t.Helper()
	ctx := context.Background()
	var userID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email, password_hash) VALUES ($1, 'x') RETURNING id`,
		fmt.Sprintf("holder-%s@test.dev", idemKey)).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	tier, err := booking.LoadTier(ctx, pool, tierID)
	if err != nil {
		t.Fatalf("load tier: %v", err)
	}
	if err := booking.ReserveInventory(ctx, rdb, pool, tierID, qty); err != nil {
		t.Fatalf("reserve inventory: %v", err)
	}
	held, err := booking.HoldTickets(ctx, pool, userID, tier, qty, idemKey, "req-admin")
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

// Test 1: every admin route is closed to a non-admin token.
func TestAdminForbiddenForNonAdmin(t *testing.T) {
	ts, pool, rdb, _, _, userTok := newAdminServer(t)
	_, tierID := seedTier(t, pool, 50000, 10)
	held := seedHeld(t, pool, rdb, tierID, "adm-forbidden", 1, 120)

	routes := []struct{ method, path string }{
		{http.MethodGet, "/v1/admin/bookings/stuck"},
		{http.MethodPost, "/v1/admin/bookings/" + held.BookingID + "/reconcile"},
		{http.MethodGet, "/v1/admin/audit?entity_id=" + held.BookingID},
	}
	for _, rt := range routes {
		resp, out := adminReq(t, ts, rt.method, rt.path, userTok)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s status = %d, want 403", rt.method, rt.path, resp.StatusCode)
			continue
		}
		if got := out["error"].(map[string]any)["type"]; got != "forbidden" {
			t.Errorf("%s %s error type = %v, want forbidden", rt.method, rt.path, got)
		}
	}
}

// Test 2: the stuck list shows only old held bookings, oldest first, with a
// sane age and the resolved payment status.
func TestAdminStuckBookings(t *testing.T) {
	ts, pool, rdb, _, adminTok, _ := newAdminServer(t)
	_, tierID := seedTier(t, pool, 50000, 20)

	oldest := seedHeld(t, pool, rdb, tierID, "adm-oldest", 2, 600)
	newer := seedHeld(t, pool, rdb, tierID, "adm-newer", 1, 200)
	seedHeld(t, pool, rdb, tierID, "adm-young", 1, 10) // inside the threshold

	// A confirmed booking must never appear, however old. confirmed_at comes
	// along because the schema's lifecycle CHECK requires it.
	confirmed := seedHeld(t, pool, rdb, tierID, "adm-confirmed", 1, 600)
	if _, err := pool.Exec(context.Background(),
		`UPDATE booking SET status = 'confirmed', confirmed_at = NOW() WHERE id = $1`, confirmed.BookingID); err != nil {
		t.Fatal(err)
	}

	resp, out := adminReq(t, ts, http.MethodGet, "/v1/admin/bookings/stuck?older_than_seconds=90", adminTok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%v, want 200", resp.StatusCode, out)
	}
	rows := adminList(t, out)
	if len(rows) != 2 {
		t.Fatalf("stuck rows = %d, want 2 (%v)", len(rows), rows)
	}

	first := rows[0].(map[string]any)
	second := rows[1].(map[string]any)
	if first["id"] != oldest.BookingID {
		t.Errorf("first row = %v, want the oldest booking %s", first["id"], oldest.BookingID)
	}
	if second["id"] != newer.BookingID {
		t.Errorf("second row = %v, want %s", second["id"], newer.BookingID)
	}
	if age := first["age_seconds"].(float64); age < 590 || age > 700 {
		t.Errorf("age_seconds = %v, want ~600", age)
	}
	if first["payment_status"] != "unknown" {
		t.Errorf("payment_status = %v, want unknown (nothing resolved yet)", first["payment_status"])
	}
	if first["reference"] == "" || first["reference"] == nil {
		t.Errorf("reference missing: %v", first)
	}
	total := first["total"].(map[string]any)
	if total["amount"].(float64) != 100000 || total["currency"] != "USD" {
		t.Errorf("total = %v, want money-as-object {100000, USD}", total)
	}
}

// Test 3: manual reconcile of a booking the provider did charge.
func TestAdminReconcileConfirms(t *testing.T) {
	ts, pool, rdb, pc, adminTok, _ := newAdminServer(t)
	ctx := context.Background()
	_, tierID := seedTier(t, pool, 50000, 10)
	held := seedHeld(t, pool, rdb, tierID, "adm-confirm-me", 2, 120)

	// The provider charged; the response never got back to us.
	if res := pc.Charge(ctx, "adm-confirm-me", held.TotalCents, held.Currency, "tok_visa"); res.Outcome != booking.Succeeded {
		t.Fatalf("setup charge outcome = %v", res.Outcome)
	}

	resp, out := adminReq(t, ts, http.MethodPost, "/v1/admin/bookings/"+held.BookingID+"/reconcile", adminTok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%v, want 200", resp.StatusCode, out)
	}
	data := out["data"].(map[string]any)
	if data["previous_status"] != "held" || data["new_status"] != "confirmed" || data["provider_result"] != "succeeded" {
		t.Errorf("response = %v, want held→confirmed/succeeded", data)
	}

	var status string
	pool.QueryRow(ctx, `SELECT status FROM booking WHERE id = $1`, held.BookingID).Scan(&status)
	if status != "confirmed" {
		t.Errorf("booking status = %s, want confirmed", status)
	}

	// The audit row must name the admin who did it.
	var adminID string
	pool.QueryRow(ctx, `SELECT id FROM users WHERE email = 'admin@test.dev'`).Scan(&adminID)
	var got string
	if err := pool.QueryRow(ctx,
		`SELECT actor_user_id::text FROM audit_log
		 WHERE entity_id = $1 AND action = 'booking_confirmed' AND actor_type = 'admin'`,
		held.BookingID).Scan(&got); err != nil {
		t.Fatalf("admin audit row: %v", err)
	}
	if got != adminID {
		t.Errorf("audit actor_user_id = %s, want the admin %s", got, adminID)
	}
}

// Test 4: a booking that is no longer held cannot be reconciled.
func TestAdminReconcileRejectsNonHeld(t *testing.T) {
	ts, pool, rdb, pc, adminTok, _ := newAdminServer(t)
	ctx := context.Background()
	_, tierID := seedTier(t, pool, 50000, 10)
	held := seedHeld(t, pool, rdb, tierID, "adm-twice", 1, 120)
	pc.Charge(ctx, "adm-twice", held.TotalCents, held.Currency, "tok_visa")

	if resp, out := adminReq(t, ts, http.MethodPost, "/v1/admin/bookings/"+held.BookingID+"/reconcile", adminTok); resp.StatusCode != http.StatusOK {
		t.Fatalf("first reconcile status = %d body=%v", resp.StatusCode, out)
	}
	resp, out := adminReq(t, ts, http.MethodPost, "/v1/admin/bookings/"+held.BookingID+"/reconcile", adminTok)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("second reconcile status = %d body=%v, want 409", resp.StatusCode, out)
	}
	if got := out["error"].(map[string]any)["type"]; got != "booking_not_held" {
		t.Errorf("error type = %v, want booking_not_held", got)
	}
}

// Test 5: provider unreachable → 503, and nothing changed. Never guess.
func TestAdminReconcileProviderUnreachable(t *testing.T) {
	pool, rdb := testutil.TestDB(t), testutil.TestRedis(t)
	dead := booking.NewPaymentClient("http://127.0.0.1:1", "ps_test_localdev")

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	api.NewAdminHandler(pool, rdb, testSecret, dead).Mount(r)
	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
	_, adminTok := seedAdmin(t, pool, "admin@test.dev", true)

	_, tierID := seedTier(t, pool, 50000, 10)
	held := seedHeld(t, pool, rdb, tierID, "adm-down", 2, 120)

	resp, out := adminReq(t, ts, http.MethodPost, "/v1/admin/bookings/"+held.BookingID+"/reconcile", adminTok)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body=%v, want 503", resp.StatusCode, out)
	}
	if got := out["error"].(map[string]any)["type"]; got != "provider_unreachable" {
		t.Errorf("error type = %v, want provider_unreachable", got)
	}

	var status string
	pool.QueryRow(context.Background(), `SELECT status FROM booking WHERE id = $1`, held.BookingID).Scan(&status)
	if status != "held" {
		t.Errorf("booking status = %s, want held (nothing guessed)", status)
	}
	var tickets int
	pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tickets WHERE booking_id = $1 AND status = 'held'`, held.BookingID).Scan(&tickets)
	if tickets != 2 {
		t.Errorf("held tickets = %d, want 2 (untouched)", tickets)
	}
}

// Test 6: the audit endpoint returns the whole story, in order.
func TestAdminAuditTimeline(t *testing.T) {
	ts, pool, rdb, pc, adminTok, _ := newAdminServer(t)
	ctx := context.Background()
	_, tierID := seedTier(t, pool, 50000, 10)
	held := seedHeld(t, pool, rdb, tierID, "adm-timeline", 1, 120)
	pc.Charge(ctx, "adm-timeline", held.TotalCents, held.Currency, "tok_visa")

	if resp, out := adminReq(t, ts, http.MethodPost, "/v1/admin/bookings/"+held.BookingID+"/reconcile", adminTok); resp.StatusCode != http.StatusOK {
		t.Fatalf("reconcile status = %d body=%v", resp.StatusCode, out)
	}

	resp, out := adminReq(t, ts, http.MethodGet,
		"/v1/admin/audit?entity_type=booking&entity_id="+held.BookingID, adminTok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%v, want 200", resp.StatusCode, out)
	}
	rows := adminList(t, out)

	var timeline []string
	var last time.Time
	for _, raw := range rows {
		row := raw.(map[string]any)
		timeline = append(timeline, row["action"].(string)+":"+row["actor_type"].(string))
		at, err := time.Parse(time.RFC3339Nano, row["created_at"].(string))
		if err != nil {
			t.Fatalf("parse created_at: %v", err)
		}
		if at.Before(last) {
			t.Errorf("audit rows out of order: %v before %v", at, last)
		}
		last = at
	}
	want := []string{
		"booking_created:user",
		"tickets_held:user",
		"booking_confirmed:admin",
		"payment_succeeded:admin",
	}
	if fmt.Sprint(timeline) != fmt.Sprint(want) {
		t.Errorf("timeline = %v, want %v", timeline, want)
	}
}

// Test 7: the audit endpoint needs exactly one selector.
func TestAdminAuditRequiresOneSelector(t *testing.T) {
	ts, _, _, _, adminTok, _ := newAdminServer(t)

	for _, q := range []string{"", "?entity_id=" + uuidZero + "&request_id=req-1"} {
		resp, out := adminReq(t, ts, http.MethodGet, "/v1/admin/audit"+q, adminTok)
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("query %q status = %d, want 422", q, resp.StatusCode)
			continue
		}
		if got := out["error"].(map[string]any)["type"]; got != "validation_failed" {
			t.Errorf("query %q error type = %v, want validation_failed", q, got)
		}
	}
}

const uuidZero = "00000000-0000-0000-0000-000000000000"
