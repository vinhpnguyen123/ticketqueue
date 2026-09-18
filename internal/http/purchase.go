package http

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/vinhpnguyen123/ticketqueue/internal/auth"
	"github.com/vinhpnguyen123/ticketqueue/internal/booking"
	"github.com/vinhpnguyen123/ticketqueue/internal/http/response"
	"github.com/vinhpnguyen123/ticketqueue/internal/idempotency"
	"github.com/vinhpnguyen123/ticketqueue/internal/metrics"
)

// PurchaseHandler serves POST /v1/purchases (TX1 + the Week-4 payment seam).
type PurchaseHandler struct {
	pool     *pgxpool.Pool
	rdb      *redis.Client
	secret   string
	payments *booking.PaymentClient
}

func NewPurchaseHandler(pool *pgxpool.Pool, rdb *redis.Client, secret string, payments *booking.PaymentClient) *PurchaseHandler {
	return &PurchaseHandler{pool: pool, rdb: rdb, secret: secret, payments: payments}
}

func (h *PurchaseHandler) Mount(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(auth.Middleware(h.secret))
		r.With(idempotency.Middleware(h.pool)).Post("/v1/purchases", h.Create)
	})
}

type purchaseRequest struct {
	EventID       string `json:"event_id"`
	TierID        string `json:"tier_id"`
	Quantity      int    `json:"quantity"`
	PaymentToken  string `json:"payment_token"`
	PurchaseToken string `json:"purchase_token"`
}

func (req *purchaseRequest) validate() map[string]any {
	problems := map[string]any{}
	if _, err := uuid.Parse(req.EventID); err != nil {
		problems["event_id"] = "must be a valid UUID"
	}
	if _, err := uuid.Parse(req.TierID); err != nil {
		problems["tier_id"] = "must be a valid UUID"
	}
	if req.Quantity < 1 || req.Quantity > 10 {
		problems["quantity"] = "must be between 1 and 10"
	}
	if req.PaymentToken == "" {
		problems["payment_token"] = "is required"
	}
	if req.PurchaseToken == "" {
		problems["purchase_token"] = "is required"
	}
	if len(problems) == 0 {
		return nil
	}
	return problems
}

func (h *PurchaseHandler) Create(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		response.Error(w, r, http.StatusUnauthorized, "unauthorized", "missing authentication")
		return
	}

	var req purchaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.Error(w, r, http.StatusUnprocessableEntity, "validation_failed", "malformed JSON body")
		return
	}
	if problems := req.validate(); problems != nil {
		response.ErrorWithDetails(w, r, http.StatusUnprocessableEntity,
			"validation_failed", "invalid purchase request", problems)
		return
	}

	// The purchase token proves this user was admitted from the queue for
	// this event and their window is still open.
	claims, err := auth.ParsePurchaseToken(req.PurchaseToken, h.secret)
	if errors.Is(err, auth.ErrTokenExpired) {
		metrics.PurchaseOutcome.WithLabelValues(metrics.OutcomeWindowExpired).Inc()
		response.Error(w, r, http.StatusGone, "purchase_window_expired", "your purchase window has closed")
		return
	}
	if err != nil || claims.Subject != userID || claims.EventID != req.EventID {
		response.Error(w, r, http.StatusUnauthorized, "invalid_purchase_token", "purchase token is invalid for this user/event")
		return
	}

	// Duplicate-charge guard: one held booking per user at a time.
	if existing, found, err := booking.ActiveBookingID(r.Context(), h.pool, userID); err != nil {
		slog.Error("purchase: active booking lookup", "error", err)
		response.Error(w, r, http.StatusInternalServerError, "internal", "failed to process purchase")
		return
	} else if found {
		metrics.PurchaseOutcome.WithLabelValues(metrics.OutcomeDuplicateBlocked).Inc()
		response.ErrorWithDetails(w, r, http.StatusGone, "active_booking_exists",
			"you already have a booking in progress",
			map[string]any{"booking_id": existing})
		return
	}

	tier, err := booking.LoadTier(r.Context(), h.pool, req.TierID)
	if errors.Is(err, pgx.ErrNoRows) {
		response.Error(w, r, http.StatusNotFound, "tier_not_found", "ticket tier not found")
		return
	}
	if err != nil {
		slog.Error("purchase: load tier", "error", err)
		response.Error(w, r, http.StatusInternalServerError, "internal", "failed to process purchase")
		return
	}
	if tier.EventID != req.EventID {
		response.ErrorWithDetails(w, r, http.StatusUnprocessableEntity,
			"validation_failed", "invalid purchase request",
			map[string]any{"tier_id": "does not belong to event_id"})
		return
	}

	// Layer 1: Redis fast reject.
	if err := booking.ReserveInventory(r.Context(), h.rdb, h.pool, tier.ID, req.Quantity); errors.Is(err, booking.ErrSoldOut) {
		metrics.PurchaseOutcome.WithLabelValues(metrics.OutcomeSoldOut).Inc()
		response.Error(w, r, http.StatusConflict, "sold_out", "not enough tickets available")
		return
	} else if err != nil {
		slog.Error("purchase: reserve inventory", "error", err)
		response.Error(w, r, http.StatusInternalServerError, "internal", "failed to process purchase")
		return
	}

	// TX1: Layers 2 and 3.
	idemKey := r.Header.Get("Idempotency-Key")
	// Timings wrap the call and are recorded after it returns, so no
	// instrumentation ever runs while a row lock is held (§11a).
	tx1Start := time.Now()
	held, err := booking.HoldTickets(r.Context(), h.pool, userID, tier, req.Quantity,
		idemKey, middleware.GetReqID(r.Context()))
	metrics.PurchasePhase.WithLabelValues(metrics.PhaseTX1).Observe(time.Since(tx1Start).Seconds())
	if err != nil {
		// TX1 rolled back but Layer 1 already decremented: give it back.
		// After commit/rollback only, never inside the transaction.
		if cerr := booking.CompensateInventory(r.Context(), h.rdb, tier.ID, req.Quantity); cerr != nil {
			slog.Error("purchase: inventory compensation failed (drift checker will heal)",
				"tier_id", tier.ID, "qty", req.Quantity, "error", cerr)
		}
		if errors.Is(err, booking.ErrSoldOut) {
			metrics.PurchaseOutcome.WithLabelValues(metrics.OutcomeSoldOut).Inc()
			response.Error(w, r, http.StatusConflict, "sold_out", "not enough tickets available")
			return
		}
		slog.Error("purchase: TX1 failed", "error", err)
		response.Error(w, r, http.StatusInternalServerError, "internal", "failed to process purchase")
		return
	}
	idempotency.SetBookingID(r, held.BookingID)
	// --- End PHASE 1 (TX1). All row locks released at COMMIT above. ---

	// --- PHASE 2: the payment call. NO DB transaction is open here — we never
	// hold locks across a network call. Charge classifies every outcome
	// (succeeded / declined / ambiguous) so PHASE 3 branches in one place.
	payStart := time.Now()
	charge := h.payments.Charge(r.Context(), idemKey, held.TotalCents, held.Currency, req.PaymentToken)
	metrics.PurchasePhase.WithLabelValues(metrics.PhasePaymentCall).Observe(time.Since(payStart).Seconds())

	// --- PHASE 3: TX2 finalization based on the payment outcome.
	reqID := middleware.GetReqID(r.Context())
	tx2Start := time.Now()
	fin, err := booking.Finalize(r.Context(), h.pool, held, tier.ID, userID, reqID, charge)
	metrics.PurchasePhase.WithLabelValues(metrics.PhaseTX2).Observe(time.Since(tx2Start).Seconds())
	if err != nil {
		slog.Error("purchase: TX2 finalize failed", "booking_id", held.BookingID, "error", err)
		response.Error(w, r, http.StatusInternalServerError, "internal", "failed to finalize purchase")
		return
	}

	switch fin.Status {
	case "confirmed":
		metrics.PurchaseOutcome.WithLabelValues(metrics.OutcomeConfirmed).Inc()
		response.JSON(w, r, http.StatusCreated, map[string]any{
			"booking_id": held.BookingID,
			"status":     "confirmed",
			"tickets":    fin.Tickets,
			"reference":  reference(held.BookingID),
		})
	case "failed":
		metrics.PurchaseOutcome.WithLabelValues(metrics.OutcomeDeclined).Inc()
		// Tickets released inside TX2; give the inventory back to Redis AFTER
		// commit, never inside the transaction. ReleaseQty is 0 when the expiry
		// worker already compensated (the lost-guard race).
		if fin.ReleaseQty > 0 {
			if cerr := booking.CompensateInventory(r.Context(), h.rdb, tier.ID, fin.ReleaseQty); cerr != nil {
				slog.Error("purchase: decline inventory compensation failed (drift checker will heal)",
					"tier_id", tier.ID, "qty", fin.ReleaseQty, "error", cerr)
			}
		}
		response.ErrorWithDetails(w, r, http.StatusPaymentRequired, "payment_declined",
			"your card was declined", map[string]any{"booking_id": held.BookingID})
	case "held_unknown":
		metrics.PurchaseOutcome.WithLabelValues(metrics.OutcomeAmbiguous).Inc()
		// Booking stays 'held' as durable evidence; the reconciliation worker
		// resolves it by querying the provider by idempotency key. We NEVER guess.
		response.ErrorWithDetails(w, r, http.StatusGatewayTimeout, "payment_processing_unclear",
			"payment is being verified, check booking status", map[string]any{"booking_id": held.BookingID})
	case "expired_after_charge":
		metrics.PurchaseOutcome.WithLabelValues(metrics.OutcomeExpiredAfterCharge).Inc()
		// Money moved but the hold had already expired. Recorded for refund.
		response.ErrorWithDetails(w, r, http.StatusConflict, "booking_expired_after_charge",
			"your hold expired while payment was processing; a refund will be issued",
			map[string]any{"booking_id": held.BookingID})
	default:
		slog.Error("purchase: unknown finalize status", "status", fin.Status, "booking_id", held.BookingID)
		response.Error(w, r, http.StatusInternalServerError, "internal", "failed to finalize purchase")
	}
}

// reference is the human-facing booking reference derived from the booking id.
func reference(bookingID string) string {
	return "TQ-" + strings.ToUpper(bookingID[:8])
}
