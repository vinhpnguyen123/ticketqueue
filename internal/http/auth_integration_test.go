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

	"github.com/vinhpnguyen123/ticketqueue/internal/auth"
	api "github.com/vinhpnguyen123/ticketqueue/internal/http"
	"github.com/vinhpnguyen123/ticketqueue/internal/testutil"
)

const testSecret = "integration-test-secret"

func newTestServer(t *testing.T) (*httptest.Server, *testServerDeps) {
	t.Helper()
	pool := testutil.TestDB(t)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	api.NewAuthHandler(pool, testSecret).Mount(r)

	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
	return ts, &testServerDeps{pool: pool}
}

type testServerDeps struct {
	pool *pgxpool.Pool
}

func postJSON(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func decode(t *testing.T, resp *http.Response, into any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

type authBody struct {
	Data struct {
		User struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		} `json:"user"`
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	} `json:"data"`
	RequestID string `json:"request_id"`
}

type errorBody struct {
	Error struct {
		Type      string         `json:"type"`
		Message   string         `json:"message"`
		RequestID string         `json:"request_id"`
		Details   map[string]any `json:"details"`
	} `json:"error"`
}

func register(t *testing.T, ts *httptest.Server, email, password string) authBody {
	t.Helper()
	resp := postJSON(t, ts.URL+"/v1/auth/register", map[string]string{"email": email, "password": password})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register status = %d, want 201", resp.StatusCode)
	}
	var body authBody
	decode(t, resp, &body)
	return body
}

func TestRegisterSuccess(t *testing.T) {
	ts, deps := newTestServer(t)
	body := register(t, ts, "alice@example.com", "supersecret1")

	if body.Data.User.Email != "alice@example.com" || body.Data.User.ID == "" {
		t.Errorf("user = %+v", body.Data.User)
	}

	claims, err := auth.ParseToken(body.Data.Token, testSecret)
	if err != nil {
		t.Fatalf("token does not parse: %v", err)
	}
	if claims.Subject != body.Data.User.ID || claims.Email != "alice@example.com" {
		t.Errorf("claims = %+v", claims)
	}

	var userCount, auditCount int
	if err := deps.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM users WHERE email = 'alice@example.com'`).Scan(&userCount); err != nil {
		t.Fatal(err)
	}
	if userCount != 1 {
		t.Errorf("user rows = %d", userCount)
	}
	if err := deps.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_log WHERE action = 'user_registered' AND entity_id = $1`,
		body.Data.User.ID).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 {
		t.Errorf("audit rows = %d", auditCount)
	}
}

func TestRegisterDuplicateEmail(t *testing.T) {
	ts, _ := newTestServer(t)
	register(t, ts, "dup@example.com", "supersecret1")

	resp := postJSON(t, ts.URL+"/v1/auth/register", map[string]string{"email": "dup@example.com", "password": "supersecret1"})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	var body errorBody
	decode(t, resp, &body)
	if body.Error.Type != "email_in_use" {
		t.Errorf("error type = %q", body.Error.Type)
	}
}

func TestRegisterValidation(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := postJSON(t, ts.URL+"/v1/auth/register", map[string]string{"email": "not-an-email", "password": "short"})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	var body errorBody
	decode(t, resp, &body)
	if body.Error.Type != "validation_failed" {
		t.Errorf("error type = %q", body.Error.Type)
	}
	if body.Error.Details["email"] == nil || body.Error.Details["password"] == nil {
		t.Errorf("details = %v, want both email and password flagged", body.Error.Details)
	}
}

func TestLoginSuccess(t *testing.T) {
	ts, _ := newTestServer(t)
	register(t, ts, "bob@example.com", "supersecret1")

	resp := postJSON(t, ts.URL+"/v1/auth/login", map[string]string{"email": "bob@example.com", "password": "supersecret1"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body authBody
	decode(t, resp, &body)
	if _, err := auth.ParseToken(body.Data.Token, testSecret); err != nil {
		t.Fatalf("token does not parse: %v", err)
	}
}

func TestLoginNoEnumeration(t *testing.T) {
	ts, _ := newTestServer(t)
	register(t, ts, "carol@example.com", "supersecret1")

	wrongPw := postJSON(t, ts.URL+"/v1/auth/login", map[string]string{"email": "carol@example.com", "password": "wrongpassword"})
	unknown := postJSON(t, ts.URL+"/v1/auth/login", map[string]string{"email": "nobody@example.com", "password": "wrongpassword"})

	for name, resp := range map[string]*http.Response{"wrong password": wrongPw, "unknown email": unknown} {
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s: status = %d, want 401", name, resp.StatusCode)
		}
	}

	var a, b errorBody
	decode(t, wrongPw, &a)
	decode(t, unknown, &b)
	if a.Error.Type != "invalid_credentials" || b.Error.Type != a.Error.Type || b.Error.Message != a.Error.Message {
		t.Errorf("bodies differ: %+v vs %+v", a.Error, b.Error)
	}
}

func TestMe(t *testing.T) {
	ts, _ := newTestServer(t)
	reg := register(t, ts, "dave@example.com", "supersecret1")

	get := func(token string) *http.Response {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/me", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET /v1/me: %v", err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}

	// Valid token.
	resp := get(reg.Data.Token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("with token: status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Data struct {
			User map[string]any `json:"user"`
		} `json:"data"`
	}
	decode(t, resp, &body)
	if body.Data.User["email"] != "dave@example.com" || body.Data.User["id"] != reg.Data.User.ID {
		t.Errorf("user = %v", body.Data.User)
	}
	if _, leaked := body.Data.User["password_hash"]; leaked {
		t.Error("password_hash leaked in /v1/me")
	}

	// No token.
	if resp := get(""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token: status = %d, want 401", resp.StatusCode)
	}

	// Expired token.
	expired, err := auth.GenerateToken(reg.Data.User.ID, "dave@example.com", false, testSecret, -time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	resp = get(expired)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expired token: status = %d, want 401", resp.StatusCode)
	}
	var errBody errorBody
	decode(t, resp, &errBody)
	if errBody.Error.Type != "unauthorized" {
		t.Errorf("error type = %q", errBody.Error.Type)
	}
}

func TestLoginAuditRows(t *testing.T) {
	ts, deps := newTestServer(t)
	reg := register(t, ts, "erin@example.com", "supersecret1")

	postJSON(t, ts.URL+"/v1/auth/login", map[string]string{"email": "erin@example.com", "password": "supersecret1"})
	postJSON(t, ts.URL+"/v1/auth/login", map[string]string{"email": "erin@example.com", "password": "wrongpassword"})
	postJSON(t, ts.URL+"/v1/auth/login", map[string]string{"email": "ghost@example.com", "password": "whatever12"})

	count := func(where string, args ...any) int {
		var n int
		if err := deps.pool.QueryRow(context.Background(),
			fmt.Sprintf(`SELECT count(*) FROM audit_log WHERE %s`, where), args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	if n := count(`action = 'login_succeeded' AND entity_id = $1`, reg.Data.User.ID); n != 1 {
		t.Errorf("login_succeeded rows = %d", n)
	}
	if n := count(`action = 'login_failed' AND metadata->>'attempted_email' = 'erin@example.com'`); n != 1 {
		t.Errorf("login_failed (wrong pw) rows = %d", n)
	}
	if n := count(`action = 'login_failed' AND metadata->>'attempted_email' = 'ghost@example.com'`); n != 1 {
		t.Errorf("login_failed (unknown email) rows = %d", n)
	}
}
