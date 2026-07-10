package http

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/vinhpnguyen123/ticketqueue/internal/auth"
	"github.com/vinhpnguyen123/ticketqueue/internal/domain/event"
	"github.com/vinhpnguyen123/ticketqueue/internal/http/response"
	"github.com/vinhpnguyen123/ticketqueue/internal/queue"
)

// QueueHandler serves the virtual-queue endpoints (all auth-required).
type QueueHandler struct {
	pool   *pgxpool.Pool
	rdb    *redis.Client
	events *event.Repo
	secret string
}

func NewQueueHandler(pool *pgxpool.Pool, rdb *redis.Client, secret string) *QueueHandler {
	return &QueueHandler{pool: pool, rdb: rdb, events: event.NewRepo(pool), secret: secret}
}

// Mount registers the queue routes on r behind the JWT middleware.
func (h *QueueHandler) Mount(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(auth.Middleware(h.secret))
		r.Post("/v1/events/{id}/queue/join", h.Join)
		r.Get("/v1/events/{id}/queue/position", h.Position)
		r.Post("/v1/events/{id}/queue/leave", h.Leave)
	})
}

// loadEvent parses the {id} param and loads the event, writing the error
// response itself when anything fails (returns ok=false in that case).
func (h *QueueHandler) loadEvent(w http.ResponseWriter, r *http.Request) (event.EventWithTiers, bool) {
	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		response.ErrorWithDetails(w, r, http.StatusUnprocessableEntity,
			"validation_failed", "invalid event id",
			map[string]any{"id": "must be a valid UUID"})
		return event.EventWithTiers{}, false
	}

	e, err := h.events.GetByID(r.Context(), id)
	if errors.Is(err, event.ErrNotFound) {
		response.Error(w, r, http.StatusNotFound, "event_not_found", "event not found")
		return event.EventWithTiers{}, false
	}
	if err != nil {
		slog.Error("queue: get event", "error", err)
		response.Error(w, r, http.StatusInternalServerError, "internal", "failed to load event")
		return event.EventWithTiers{}, false
	}
	return e, true
}

func (h *QueueHandler) Join(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		response.Error(w, r, http.StatusUnauthorized, "unauthorized", "missing authentication")
		return
	}
	e, ok := h.loadEvent(w, r)
	if !ok {
		return
	}
	if e.Status != "on_sale" {
		response.Error(w, r, http.StatusGone, "sales_closed", "ticket sales are not open for this event")
		return
	}

	position, alreadyInQueue, err := queue.Join(r.Context(), h.rdb, h.pool, userID, e.ID)
	if err != nil {
		slog.Error("queue: join", "error", err)
		response.Error(w, r, http.StatusInternalServerError, "internal", "failed to join queue")
		return
	}

	_, size, _, err := queue.Position(r.Context(), h.rdb, userID, e.ID)
	if err != nil {
		slog.Error("queue: size after join", "error", err)
		response.Error(w, r, http.StatusInternalServerError, "internal", "failed to join queue")
		return
	}

	var joinedAt time.Time
	if err := h.pool.QueryRow(r.Context(),
		`SELECT joined_at FROM queue_entries
		 WHERE user_id = $1 AND event_id = $2 AND status IN ('waiting', 'admitted')`,
		userID, e.ID,
	).Scan(&joinedAt); err != nil {
		slog.Error("queue: joined_at lookup", "error", err)
		response.Error(w, r, http.StatusInternalServerError, "internal", "failed to join queue")
		return
	}

	status := http.StatusCreated
	if alreadyInQueue {
		status = http.StatusOK // idempotent re-join is not an error
	}
	response.JSON(w, r, status, map[string]any{
		"position":   position,
		"queue_size": size,
		"joined_at":  joinedAt,
	})
}

func (h *QueueHandler) Position(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		response.Error(w, r, http.StatusUnauthorized, "unauthorized", "missing authentication")
		return
	}
	e, ok := h.loadEvent(w, r)
	if !ok {
		return
	}

	rank, size, found, err := queue.Position(r.Context(), h.rdb, userID, e.ID)
	if err != nil {
		slog.Error("queue: position", "error", err)
		response.Error(w, r, http.StatusInternalServerError, "internal", "failed to read queue position")
		return
	}
	if found {
		response.JSON(w, r, http.StatusOK, map[string]any{
			"position":   rank + 1,
			"queue_size": size,
			"status":     "waiting",
		})
		return
	}

	// ZRANK missed: the user may have been admitted (promotion removes the
	// member from the waiting set but keeps the durable row as 'admitted').
	var status string
	var admittedAt *time.Time
	err = h.pool.QueryRow(r.Context(),
		`SELECT status, admitted_at FROM queue_entries
		 WHERE user_id = $1 AND event_id = $2 AND status IN ('waiting', 'admitted')`,
		userID, e.ID,
	).Scan(&status, &admittedAt)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && status != "admitted") {
		response.Error(w, r, http.StatusNotFound, "not_in_queue", "you are not in this event's queue")
		return
	}
	if err != nil {
		slog.Error("queue: entry lookup", "error", err)
		response.Error(w, r, http.StatusInternalServerError, "internal", "failed to read queue position")
		return
	}
	if admittedAt == nil {
		slog.Error("queue: admitted entry missing admitted_at", "user_id", userID, "event_id", e.ID)
		response.Error(w, r, http.StatusInternalServerError, "internal", "failed to read queue position")
		return
	}

	token, err := auth.GeneratePurchaseToken(userID, e.ID, *admittedAt, e.PurchaseWindowSeconds, h.secret)
	if err != nil {
		slog.Error("queue: purchase token", "error", err)
		response.Error(w, r, http.StatusInternalServerError, "internal", "failed to read queue position")
		return
	}
	response.JSON(w, r, http.StatusOK, map[string]any{
		"status":            "admitted",
		"purchase_token":    token,
		"window_expires_at": admittedAt.Add(time.Duration(e.PurchaseWindowSeconds) * time.Second),
	})
}

func (h *QueueHandler) Leave(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		response.Error(w, r, http.StatusUnauthorized, "unauthorized", "missing authentication")
		return
	}
	e, ok := h.loadEvent(w, r)
	if !ok {
		return
	}

	err := queue.Leave(r.Context(), h.rdb, h.pool, userID, e.ID)
	if errors.Is(err, queue.ErrNotInQueue) {
		response.Error(w, r, http.StatusNotFound, "not_in_queue", "you are not in this event's queue")
		return
	}
	if err != nil {
		slog.Error("queue: leave", "error", err)
		response.Error(w, r, http.StatusInternalServerError, "internal", "failed to leave queue")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
