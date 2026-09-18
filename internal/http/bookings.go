package http

import (
	"errors"
	"log/slog"
	"net/http"
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
)

const (
	defaultBookingLimit = 20
	maxBookingLimit     = 50
)

// BookingsHandler serves a user's own bookings.
type BookingsHandler struct {
	pool     *pgxpool.Pool
	rdb      *redis.Client
	secret   string
	payments *booking.PaymentClient
}

func NewBookingsHandler(pool *pgxpool.Pool, rdb *redis.Client, secret string, payments *booking.PaymentClient) *BookingsHandler {
	return &BookingsHandler{pool: pool, rdb: rdb, secret: secret, payments: payments}
}

func (h *BookingsHandler) Mount(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(auth.Middleware(h.secret))
		r.Get("/v1/bookings", h.List)
		r.Get("/v1/bookings/{id}", h.Get)
	})
}

type eventRef struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	EventAt time.Time `json:"event_at"`
}

type tierRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// bookingSummary is the wire shape. Every field is listed explicitly, which is
// what keeps idempotency_key and provider_response from ever leaking into a
// response by accident (§11).
type bookingSummary struct {
	ID          string     `json:"id"`
	Reference   string     `json:"reference"`
	Status      string     `json:"status"`
	Quantity    int        `json:"quantity"`
	Total       money      `json:"total"`
	Event       eventRef   `json:"event"`
	Tier        tierRef    `json:"tier"`
	CreatedAt   time.Time  `json:"created_at"`
	ConfirmedAt *time.Time `json:"confirmed_at"`
	FailedAt    *time.Time `json:"failed_at"`
}

type bookingTicket struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type bookingPayment struct {
	Status    string    `json:"status"`
	TxnID     *string   `json:"provider_txn_id"`
	CardBrand *string   `json:"card_brand"`
	CardLast4 *string   `json:"card_last4"`
	CreatedAt time.Time `json:"created_at"`
}

type bookingDetail struct {
	bookingSummary
	PaymentStatus string           `json:"payment_status"`
	Tickets       []bookingTicket  `json:"tickets"`
	Payments      []bookingPayment `json:"payments"`
}

func toSummary(s booking.Summary) bookingSummary {
	return bookingSummary{
		ID:          s.ID,
		Reference:   reference(s.ID),
		Status:      s.Status,
		Quantity:    s.Quantity,
		Total:       money{Amount: s.TotalCents, Currency: s.Currency},
		Event:       eventRef{ID: s.EventID, Name: s.EventName, EventAt: s.EventAt},
		Tier:        tierRef{ID: s.TierID, Name: s.TierName},
		CreatedAt:   s.CreatedAt,
		ConfirmedAt: s.ConfirmedAt,
		FailedAt:    s.FailedAt,
	}
}

// List returns the caller's own bookings. The user id comes from the JWT, never
// from the URL or a query parameter (§11).
//
// This endpoint deliberately does NOT sync-reconcile: one provider call per row
// would turn a list request into a fan-out. Only the detail endpoint does that.
func (h *BookingsHandler) List(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		response.Error(w, r, http.StatusUnauthorized, "unauthorized", "missing authentication")
		return
	}

	limit, ok := intParam(w, r, "limit", defaultBookingLimit)
	if !ok {
		return
	}
	if limit == 0 || limit > maxBookingLimit {
		limit = maxBookingLimit
	}

	var cursor *string
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		if _, err := uuid.Parse(raw); err != nil {
			response.ErrorWithDetails(w, r, http.StatusUnprocessableEntity,
				"validation_failed", "invalid cursor",
				map[string]any{"cursor": "must be a valid UUID"})
			return
		}
		cursor = &raw
	}

	rows, next, err := booking.ListByUser(r.Context(), h.pool, userID, limit, cursor)
	if err != nil {
		slog.Error("bookings: list", "error", err)
		response.Error(w, r, http.StatusInternalServerError, "internal", "failed to list bookings")
		return
	}

	out := make([]bookingSummary, 0, len(rows))
	for _, s := range rows {
		out = append(out, toSummary(s))
	}
	response.List(w, r, http.StatusOK, out, response.Pagination{NextCursor: next, HasMore: next != nil})
}

// Get returns one of the caller's bookings, resolving it against the provider
// first if it has been sitting 'held' long enough to be considered stuck.
func (h *BookingsHandler) Get(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		response.Error(w, r, http.StatusUnauthorized, "unauthorized", "missing authentication")
		return
	}

	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		response.ErrorWithDetails(w, r, http.StatusUnprocessableEntity,
			"validation_failed", "invalid booking id",
			map[string]any{"id": "must be a valid UUID"})
		return
	}

	detail, err := booking.GetByIDForUser(r.Context(), h.pool, id, userID)
	if errors.Is(err, booking.ErrNotFound) {
		// Someone else's booking answers the same way a nonexistent one does.
		response.Error(w, r, http.StatusNotFound, "booking_not_found", "booking not found")
		return
	}
	if err != nil {
		slog.Error("bookings: get", "booking_id", id, "error", err)
		response.Error(w, r, http.StatusInternalServerError, "internal", "failed to load booking")
		return
	}

	if detail.Status == "held" && time.Since(detail.CreatedAt) > booking.StuckAfter {
		detail = h.syncReconcile(r, detail, userID)
	}

	resp := bookingDetail{
		bookingSummary: toSummary(detail.Summary),
		PaymentStatus:  detail.PaymentStatus(),
		Tickets:        make([]bookingTicket, 0, len(detail.Tickets)),
		Payments:       make([]bookingPayment, 0, len(detail.Payments)),
	}
	for _, t := range detail.Tickets {
		resp.Tickets = append(resp.Tickets, bookingTicket{ID: t.ID, Status: t.Status})
	}
	for _, p := range detail.Payments {
		resp.Payments = append(resp.Payments, bookingPayment{
			Status: p.Status, TxnID: p.TxnID, CardBrand: p.CardBrand,
			CardLast4: p.CardLast4, CreatedAt: p.CreatedAt,
		})
	}
	response.JSON(w, r, http.StatusOK, resp)
}

// syncReconcile resolves a stuck booking on read, so a user who retries sees
// the real outcome instead of a stale 'held'. It is the fourth and last caller
// of the shared resolver; the branch logic lives there, not here.
//
// It is best-effort by design: anything that goes wrong returns the booking as
// it stands. An unreachable provider in particular is NOT an error — the user
// sees 'held' (processing), and the worker will resolve it later. Never guess.
func (h *BookingsHandler) syncReconcile(r *http.Request, current booking.Detail, userID string) booking.Detail {
	stuck, status, err := booking.LoadStuck(r.Context(), h.pool, current.ID)
	if err != nil || status != "held" {
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			slog.Error("bookings: load stuck", "booking_id", current.ID, "error", err)
		}
		return current
	}

	res, err := booking.ReconcileBooking(r.Context(), h.pool, h.rdb, h.payments, stuck,
		booking.Actor{
			Type:      "user",
			UserID:    userID,
			RequestID: middleware.GetReqID(r.Context()),
			Source:    "sync_retry",
		})
	if err != nil {
		slog.Error("bookings: sync reconcile", "booking_id", current.ID, "error", err)
		return current
	}
	if res.Provider == booking.ProviderUnreachable {
		return current // still 'held'; the user sees "processing"
	}

	// Either we resolved it or a sibling did a moment earlier (applied=false).
	// Both mean the stored state moved on, so re-read rather than infer.
	fresh, err := booking.GetByIDForUser(r.Context(), h.pool, current.ID, userID)
	if err != nil {
		slog.Error("bookings: re-read after reconcile", "booking_id", current.ID, "error", err)
		return current
	}
	return fresh
}
