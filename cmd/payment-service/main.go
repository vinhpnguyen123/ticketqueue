// Command payment-service is a standalone simulated payment provider with a
// Stripe-like contract. It is a separate company in our simulation: its own
// in-memory store, its own auth key, no access to ticketqueue's Postgres.
// The payment_token matrix injects failures (declines, slowness, dropped
// responses) that a real provider can't produce on demand — which is exactly
// what the reconciliation worker exists to handle.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
)

// Vars so tests can shrink the delays instead of sleeping 35s.
var (
	slow5s  = 5 * time.Second
	slow30s = 30 * time.Second
)

type card struct {
	Brand string `json:"brand"`
	Last4 string `json:"last4"`
}

type charge struct {
	ID             string    `json:"id"`
	Status         string    `json:"status"` // succeeded | failed
	Amount         int64     `json:"amount"`
	Currency       string    `json:"currency"`
	Card           card      `json:"card"`
	FailureCode    string    `json:"failure_code,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	IdempotencyKey string    `json:"idempotency_key"`
}

type store struct {
	mu      sync.RWMutex
	charges map[string]charge // by idempotency key
	// keys that already burned their one 500 for tok_500_then_succeed
	failedOnce map[string]bool
}

type server struct {
	apiKey string
	st     *store
}

func newServer(apiKey string) http.Handler {
	s := &server{apiKey: apiKey, st: &store{
		charges:    make(map[string]charge),
		failedOnce: make(map[string]bool),
	}}
	r := chi.NewRouter()
	r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	r.Route("/v1", func(r chi.Router) {
		r.Use(s.requireAuth)
		r.Post("/charges", s.createCharge)
		// The reconciliation bridge: after a dropped response, ticketqueue
		// looks the charge up by the idempotency key it sent, to learn
		// whether money actually moved.
		r.Get("/charges/by-idempotency-key/{key}", s.getByKey)
	})
	return r
}

func (s *server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+s.apiKey {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "invalid or missing API key")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type chargeRequest struct {
	Amount       int64             `json:"amount"`
	Currency     string            `json:"currency"`
	PaymentToken string            `json:"payment_token"`
	Description  string            `json:"description"`
	Metadata     map[string]string `json:"metadata"`
}

func (s *server) createCharge(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		writeErr(w, http.StatusBadRequest, "missing_idempotency_key", "Idempotency-Key header is required")
		return
	}
	var req chargeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", "malformed JSON body")
		return
	}

	// Provider-side idempotency, mirroring Stripe: a key we've seen returns
	// the stored outcome verbatim and never re-processes the charge.
	s.st.mu.RLock()
	if ch, ok := s.st.charges[key]; ok {
		s.st.mu.RUnlock()
		slog.Info("charge replayed from idempotency store", "key", key, "status", ch.Status)
		writeJSON(w, http.StatusOK, ch)
		return
	}
	s.st.mu.RUnlock()

	switch req.PaymentToken {
	case "tok_visa":
		s.succeed(w, req, key, card{"visa", "4242"})
	case "tok_mastercard":
		s.succeed(w, req, key, card{"mastercard", "4444"})
	case "tok_declined":
		s.fail(w, req, key, "card_declined")
	case "tok_insufficient_funds":
		s.fail(w, req, key, "insufficient_funds")
	case "tok_slow_5s":
		time.Sleep(slow5s)
		s.succeed(w, req, key, card{"visa", "4242"})
	case "tok_slow_30s":
		time.Sleep(slow30s)
		s.succeed(w, req, key, card{"visa", "4242"})
	case "tok_drops_response":
		ch := s.record(req, key, "succeeded", card{"visa", "4242"}, "")
		slog.Info("charge processed, dropping response", "token", req.PaymentToken, "key", key, "charge_id", ch.ID)
		// Simulates the response being lost on the network AFTER money moved:
		// the charge is stored (retrievable by key), but the client gets a
		// dead connection instead of an HTTP response. This is the scenario
		// the reconciliation worker exists for.
		hj, ok := w.(http.Hijacker)
		if !ok {
			writeErr(w, http.StatusInternalServerError, "internal_error", "cannot hijack connection")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			slog.Error("hijack failed", "error", err)
			return
		}
		conn.Close()
	case "tok_500_then_succeed":
		s.st.mu.Lock()
		burned := s.st.failedOnce[key]
		s.st.failedOnce[key] = true
		s.st.mu.Unlock()
		if !burned {
			// Nothing stored: the provider genuinely failed before charging.
			slog.Info("charge attempt", "token", req.PaymentToken, "key", key, "outcome", "transient_500")
			writeErr(w, http.StatusInternalServerError, "internal_error", "transient provider error, retry")
			return
		}
		s.succeed(w, req, key, card{"visa", "4242"})
	default:
		slog.Info("charge attempt", "token", req.PaymentToken, "key", key, "outcome", "invalid_token")
		writeErr(w, http.StatusUnprocessableEntity, "invalid_payment_token", "unknown payment_token")
	}
}

// record stores the charge outcome under its idempotency key.
func (s *server) record(req chargeRequest, key, status string, c card, failureCode string) charge {
	ch := charge{
		ID:             "ch_" + randHex(8),
		Status:         status,
		Amount:         req.Amount,
		Currency:       req.Currency,
		Card:           c,
		FailureCode:    failureCode,
		CreatedAt:      time.Now().UTC(),
		IdempotencyKey: key,
	}
	s.st.mu.Lock()
	s.st.charges[key] = ch
	s.st.mu.Unlock()
	return ch
}

func (s *server) succeed(w http.ResponseWriter, req chargeRequest, key string, c card) {
	ch := s.record(req, key, "succeeded", c, "")
	slog.Info("charge attempt", "token", req.PaymentToken, "key", key, "outcome", "succeeded", "charge_id", ch.ID)
	writeJSON(w, http.StatusOK, ch)
}

func (s *server) fail(w http.ResponseWriter, req chargeRequest, key, failureCode string) {
	ch := s.record(req, key, "failed", card{}, failureCode)
	slog.Info("charge attempt", "token", req.PaymentToken, "key", key, "outcome", "failed", "failure_code", failureCode)
	writeJSON(w, http.StatusPaymentRequired, ch)
}

func (s *server) getByKey(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	s.st.mu.RLock()
	ch, ok := s.st.charges[key]
	s.st.mu.RUnlock()
	if !ok {
		writeErr(w, http.StatusNotFound, "charge_not_found", "no charge for that idempotency key")
		return
	}
	writeJSON(w, http.StatusOK, ch)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("encode response", "error", err)
	}
}

func writeErr(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"type": typ, "message": msg}})
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failing means the host is broken
	}
	return hex.EncodeToString(b)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	port := envOr("PAYMENT_PORT", "8081")
	apiKey := envOr("PAYMENT_API_KEY", "ps_test_localdev")

	slog.Info("payment-service listening", "port", port)
	if err := http.ListenAndServe(":"+port, newServer(apiKey)); err != nil {
		slog.Error("server exited", "error", err)
		os.Exit(1)
	}
}
