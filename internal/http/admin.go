package http

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
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

// Defaults and caps for the admin list endpoints.
const (
	defaultStuckOlderThan = 90
	defaultStuckLimit     = 50
	defaultAuditLimit     = 100
	maxAdminLimit         = 500
)

// AdminHandler serves the operator endpoints: what is stuck, fix one by hand,
// and read the trail afterwards.
type AdminHandler struct {
	pool     *pgxpool.Pool
	rdb      *redis.Client
	secret   string
	payments *booking.PaymentClient
}

func NewAdminHandler(pool *pgxpool.Pool, rdb *redis.Client, secret string, payments *booking.PaymentClient) *AdminHandler {
	return &AdminHandler{pool: pool, rdb: rdb, secret: secret, payments: payments}
}

// Mount registers the admin routes behind JWT auth AND the is_admin check.
func (h *AdminHandler) Mount(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(auth.Middleware(h.secret))
		r.Use(auth.RequireAdmin)
		r.Get("/v1/admin/bookings/stuck", h.StuckBookings)
		r.Post("/v1/admin/bookings/{id}/reconcile", h.Reconcile)
		r.Get("/v1/admin/audit", h.Audit)
	})
}

type stuckBooking struct {
	ID        string `json:"id"`
	Reference string `json:"reference"`
	// Category is held_too_long or charged_not_delivered.
	Category      string    `json:"category"`
	UserID        string    `json:"user_id"`
	Status        string    `json:"status"`
	Quantity      int       `json:"quantity"`
	Total         money     `json:"total"`
	CreatedAt     time.Time `json:"created_at"`
	HoldExpiresAt time.Time `json:"hold_expires_at"`
	AgeSeconds    int       `json:"age_seconds"`
	PaymentStatus string    `json:"payment_status"`
	// FlaggedAt is when the booking was flagged reconciliation_needed; null for
	// held_too_long rows.
	FlaggedAt *time.Time `json:"flagged_at"`
	// ResolutionSource is which path last reconciled this booking
	// (reconciliation | hold_expiry | admin_manual), or null if none ever did.
	ResolutionSource *string `json:"resolution_source"`
}

// StuckBookings lists held bookings older than a threshold — the operator's
// view of what reconciliation has not resolved yet.
func (h *AdminHandler) StuckBookings(w http.ResponseWriter, r *http.Request) {
	olderThan, ok := intParam(w, r, "older_than_seconds", defaultStuckOlderThan)
	if !ok {
		return
	}
	limit, ok := intParam(w, r, "limit", defaultStuckLimit)
	if !ok {
		return
	}

	stuck, err := booking.ListStuck(r.Context(), h.pool, time.Duration(olderThan)*time.Second, limit)
	if err != nil {
		slog.Error("admin: stuck bookings query", "error", err)
		response.Error(w, r, http.StatusInternalServerError, "internal", "failed to list stuck bookings")
		return
	}

	out := make([]stuckBooking, 0, len(stuck))
	for _, b := range stuck {
		out = append(out, stuckBooking{
			ID:               b.ID,
			Reference:        reference(b.ID),
			Category:         b.Category,
			UserID:           b.UserID,
			Status:           b.Status,
			Quantity:         b.Quantity,
			Total:            money{Amount: b.TotalCents, Currency: b.Currency},
			CreatedAt:        b.CreatedAt,
			HoldExpiresAt:    b.HoldExpiresAt,
			AgeSeconds:       b.AgeSeconds,
			PaymentStatus:    b.PaymentStatus,
			FlaggedAt:        b.FlaggedAt,
			ResolutionSource: b.ResolutionSource,
		})
	}
	response.JSON(w, r, http.StatusOK, out)
}

// Reconcile resolves one booking by hand, through the SAME resolver the
// reconciliation worker uses. The only difference is the audit actor.
func (h *AdminHandler) Reconcile(w http.ResponseWriter, r *http.Request) {
	adminID, _ := auth.UserIDFromContext(r.Context())

	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		response.ErrorWithDetails(w, r, http.StatusUnprocessableEntity,
			"validation_failed", "invalid booking id", map[string]any{"id": "must be a valid UUID"})
		return
	}

	stuck, status, err := booking.LoadStuck(r.Context(), h.pool, id)
	if errors.Is(err, pgx.ErrNoRows) {
		response.Error(w, r, http.StatusNotFound, "booking_not_found", "booking not found")
		return
	}
	if err != nil {
		slog.Error("admin: load booking", "booking_id", id, "error", err)
		response.Error(w, r, http.StatusInternalServerError, "internal", "failed to reconcile booking")
		return
	}
	if status != "held" {
		response.ErrorWithDetails(w, r, http.StatusConflict, "booking_not_held",
			"only a held booking can be reconciled", map[string]any{"status": status})
		return
	}

	res, err := booking.ReconcileBooking(r.Context(), h.pool, h.rdb, h.payments, stuck,
		booking.Actor{Type: "admin", UserID: adminID, RequestID: middleware.GetReqID(r.Context()), Source: "admin_manual"})
	if err != nil {
		slog.Error("admin: reconcile", "booking_id", id, "error", err)
		response.Error(w, r, http.StatusInternalServerError, "internal", "failed to reconcile booking")
		return
	}

	if res.Provider == booking.ProviderUnreachable {
		// Nothing was changed and nothing was guessed. Say so plainly.
		response.ErrorWithDetails(w, r, http.StatusServiceUnavailable, "provider_unreachable",
			"payment provider did not answer; booking left held",
			map[string]any{"booking_id": id, "status": "held"})
		return
	}

	response.JSON(w, r, http.StatusOK, map[string]any{
		"booking_id":      id,
		"previous_status": status,
		"new_status":      res.Status,
		"provider_result": res.Provider,
	})
}

type auditRow struct {
	ID          string         `json:"id"`
	EntityType  string         `json:"entity_type"`
	EntityID    string         `json:"entity_id"`
	Action      string         `json:"action"`
	ActorType   string         `json:"actor_type"`
	ActorUserID *string        `json:"actor_user_id"`
	Metadata    map[string]any `json:"metadata"`
	RequestID   *string        `json:"request_id"`
	CreatedAt   time.Time      `json:"created_at"`
}

// auditSelect is the shared projection; created_at defaults to NOW()
// (transaction time), so rows written by one transaction tie exactly and
// action is the tiebreaker that keeps the trail in a stable order.
const auditSelect = `SELECT id, entity_type, entity_id, action, actor_type, actor_user_id,
                            metadata, request_id, created_at
                     FROM audit_log `

// Audit returns an entity's trail, or everything one request touched.
func (h *AdminHandler) Audit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	entityID, requestID := q.Get("entity_id"), q.Get("request_id")
	if (entityID == "") == (requestID == "") {
		response.ErrorWithDetails(w, r, http.StatusUnprocessableEntity,
			"validation_failed", "invalid audit query",
			map[string]any{"query": "exactly one of entity_id or request_id is required"})
		return
	}
	limit, ok := intParam(w, r, "limit", defaultAuditLimit)
	if !ok {
		return
	}

	var rows pgx.Rows
	var err error
	if entityID != "" {
		if _, perr := uuid.Parse(entityID); perr != nil {
			response.ErrorWithDetails(w, r, http.StatusUnprocessableEntity,
				"validation_failed", "invalid audit query",
				map[string]any{"entity_id": "must be a valid UUID"})
			return
		}
		entityType := q.Get("entity_type")
		if entityType == "" {
			entityType = "booking"
		}
		rows, err = h.pool.Query(r.Context(),
			auditSelect+`WHERE entity_type = $1 AND entity_id = $2
			             ORDER BY created_at ASC, action ASC LIMIT $3`,
			entityType, entityID, limit)
	} else {
		rows, err = h.pool.Query(r.Context(),
			auditSelect+`WHERE request_id = $1
			             ORDER BY created_at ASC, action ASC LIMIT $2`,
			requestID, limit)
	}
	h.writeAudit(w, r, rows, err)
}

func (h *AdminHandler) writeAudit(w http.ResponseWriter, r *http.Request, rows pgx.Rows, err error) {
	if err != nil {
		slog.Error("admin: audit query", "error", err)
		response.Error(w, r, http.StatusInternalServerError, "internal", "failed to read audit log")
		return
	}
	defer rows.Close()

	out := []auditRow{}
	for rows.Next() {
		var a auditRow
		if err := rows.Scan(&a.ID, &a.EntityType, &a.EntityID, &a.Action, &a.ActorType,
			&a.ActorUserID, &a.Metadata, &a.RequestID, &a.CreatedAt); err != nil {
			slog.Error("admin: scan audit row", "error", err)
			response.Error(w, r, http.StatusInternalServerError, "internal", "failed to read audit log")
			return
		}
		out = append(out, a)
	}
	if rows.Err() != nil {
		slog.Error("admin: iterate audit rows", "error", rows.Err())
		response.Error(w, r, http.StatusInternalServerError, "internal", "failed to read audit log")
		return
	}
	response.JSON(w, r, http.StatusOK, out)
}

// intParam reads a positive integer query param, writing a 422 and returning
// ok=false when it is present but not a sane number.
func intParam(w http.ResponseWriter, r *http.Request, name string, def int) (int, bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def, true
	}
	v, err := strconv.Atoi(raw)
	tooBig := name == "limit" && v > maxAdminLimit
	if err != nil || v < 0 || tooBig {
		response.ErrorWithDetails(w, r, http.StatusUnprocessableEntity,
			"validation_failed", "invalid query parameter",
			map[string]any{name: "must be a positive integer"})
		return 0, false
	}
	return v, true
}
