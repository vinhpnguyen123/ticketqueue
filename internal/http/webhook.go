package http

import (
	"crypto/hmac"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/vinhpnguyen123/ticketqueue/internal/booking"
	"github.com/vinhpnguyen123/ticketqueue/internal/http/response"
	"github.com/vinhpnguyen123/ticketqueue/internal/paymentsim"
)

// webhookMaxSkew is how old a delivery may be before it is refused. A captured
// request replayed later is the attack this closes; the signature alone would
// happily validate forever.
const webhookMaxSkew = 5 * time.Minute

// webhookMaxBody caps what we will read before verifying anything, so an
// unauthenticated endpoint cannot be used to make us allocate.
const webhookMaxBody = 64 << 10

// WebhookHandler receives settlement notifications from the payment provider.
//
// It is a LATENCY OPTIMISATION, not a correctness mechanism: reconciliation
// resolves every one of these bookings anyway, within a cycle. That is why
// every failure path here is allowed to give up quietly — losing a webhook
// costs seconds, never money.
type WebhookHandler struct {
	pool     *pgxpool.Pool
	rdb      *redis.Client
	secret   string
	payments *booking.PaymentClient
}

func NewWebhookHandler(pool *pgxpool.Pool, rdb *redis.Client, secret string, payments *booking.PaymentClient) *WebhookHandler {
	return &WebhookHandler{pool: pool, rdb: rdb, secret: secret, payments: payments}
}

// Mount registers the receiver. Deliberately NOT under /v1 and NOT behind the
// JWT middleware: the caller is another system, and the HMAC signature is its
// credential.
func (h *WebhookHandler) Mount(r chi.Router) {
	r.Post("/webhooks/payment-service", h.Receive)
}

type webhookEvent struct {
	EventID        string `json:"event_id"`
	Type           string `json:"type"`
	IdempotencyKey string `json:"idempotency_key"`
	ChargeID       string `json:"charge_id"`
	Amount         int64  `json:"amount"`
	Currency       string `json:"currency"`
}

func (h *WebhookHandler) Receive(w http.ResponseWriter, r *http.Request) {
	// The signature covers the RAW bytes, so they must be read before any
	// decode. Re-encoding the parsed struct would produce different bytes and
	// every signature would fail.
	raw, err := io.ReadAll(io.LimitReader(r.Body, webhookMaxBody))
	if err != nil {
		response.Error(w, r, http.StatusBadRequest, "invalid_request", "cannot read body")
		return
	}

	if !h.verify(raw, r.Header.Get("X-Webhook-Signature")) {
		slog.Warn("webhook rejected: bad signature")
		response.Error(w, r, http.StatusUnauthorized, "invalid_signature", "signature does not match")
		return
	}
	if !freshTimestamp(r.Header.Get("X-Webhook-Timestamp")) {
		slog.Warn("webhook rejected: stale timestamp")
		response.Error(w, r, http.StatusUnauthorized, "timestamp_expired", "delivery is too old")
		return
	}

	var ev webhookEvent
	if err := json.Unmarshal(raw, &ev); err != nil || ev.EventID == "" || ev.IdempotencyKey == "" {
		response.Error(w, r, http.StatusBadRequest, "invalid_request", "malformed webhook body")
		return
	}

	// Delivery is at-least-once, so the first thing we do with a valid event is
	// claim it. Losing this race means someone else already did the work.
	tag, err := h.pool.Exec(r.Context(),
		`INSERT INTO webhook_events_seen (event_id) VALUES ($1) ON CONFLICT DO NOTHING`, ev.EventID)
	if err != nil {
		slog.Error("webhook dedup insert failed", "event_id", ev.EventID, "error", err)
		response.Error(w, r, http.StatusInternalServerError, "internal", "failed to record webhook")
		return
	}
	if tag.RowsAffected() == 0 {
		// Already handled. 200 so the provider stops retrying.
		response.JSON(w, r, http.StatusOK, map[string]any{"status": "duplicate_ignored"})
		return
	}

	h.resolve(w, r, ev)
}

// resolve finds the booking this event belongs to and puts it through the same
// resolver every other path uses. Anything unexpected still answers 200: a
// non-2xx makes the provider retry, and retrying will not make an unknown key
// known or a resolved booking unresolved.
func (h *WebhookHandler) resolve(w http.ResponseWriter, r *http.Request, ev webhookEvent) {
	stuck, status, err := booking.LoadStuckByKey(r.Context(), h.pool, ev.IdempotencyKey)
	if errors.Is(err, pgx.ErrNoRows) {
		// A charge we have no booking for: someone else's key, or a test. Not
		// an error on our side, and a 404 would only make the provider retry.
		slog.Warn("webhook for unknown idempotency key", "event_id", ev.EventID)
		h.markProcessed(r, ev.EventID)
		response.JSON(w, r, http.StatusOK, map[string]any{"status": "unknown_key_ignored"})
		return
	}
	if err != nil {
		slog.Error("webhook booking lookup failed", "event_id", ev.EventID, "error", err)
		response.Error(w, r, http.StatusInternalServerError, "internal", "failed to process webhook")
		return
	}

	if status != "held" {
		// The synchronous path, the worker, or a sibling already finished it.
		// Nothing to do, and nothing to record.
		h.markProcessed(r, ev.EventID)
		response.JSON(w, r, http.StatusOK, map[string]any{"status": "already_resolved"})
		return
	}

	res, err := booking.ReconcileBooking(r.Context(), h.pool, h.rdb, h.payments, stuck,
		booking.Actor{
			Type:      "system",
			RequestID: middleware.GetReqID(r.Context()),
			Source:    "webhook",
		})
	if err != nil {
		slog.Error("webhook resolve failed", "booking_id", stuck.BookingID, "error", err)
		response.Error(w, r, http.StatusInternalServerError, "internal", "failed to process webhook")
		return
	}

	h.markProcessed(r, ev.EventID)
	slog.Info("webhook resolved booking",
		"booking_id", stuck.BookingID, "provider", res.Provider, "status", res.Status, "applied", res.Applied)
	response.JSON(w, r, http.StatusOK, map[string]any{
		"status":     res.Status,
		"booking_id": stuck.BookingID,
	})
}

// markProcessed is best-effort bookkeeping: the dedup claim is what matters,
// and a missing processed_at costs nothing.
func (h *WebhookHandler) markProcessed(r *http.Request, eventID string) {
	if _, err := h.pool.Exec(r.Context(),
		`UPDATE webhook_events_seen SET processed_at = NOW() WHERE event_id = $1`, eventID); err != nil {
		slog.Error("webhook mark processed failed", "event_id", eventID, "error", err)
	}
}

// verify checks the HMAC in constant time. An early return on a length
// mismatch is fine — the length is not secret.
func (h *WebhookHandler) verify(raw []byte, got string) bool {
	if h.secret == "" || got == "" {
		return false
	}
	return hmac.Equal([]byte(got), []byte(paymentsim.Sign(raw, h.secret)))
}

func freshTimestamp(header string) bool {
	secs, err := strconv.ParseInt(header, 10, 64)
	if err != nil {
		return false
	}
	// Future timestamps are refused as firmly as old ones: a clock far ahead
	// would otherwise extend the replay window indefinitely.
	age := time.Since(time.Unix(secs, 0))
	return age < webhookMaxSkew && age > -webhookMaxSkew
}
