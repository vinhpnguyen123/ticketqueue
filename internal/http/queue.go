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
	"github.com/vinhpnguyen123/ticketqueue/internal/ratelimit"
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
		r.With(ratelimit.Limit(h.rdb, "queue_join", 10, ratelimit.ByUserID)).
			Post("/v1/events/{id}/queue/join", h.Join)
		r.With(ratelimit.Limit(h.rdb, "queue_position", 30, ratelimit.ByUserID)).
			Get("/v1/events/{id}/queue/position", h.Position)
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
	if errors.Is(err, queue.ErrAlreadyAdmitted) {
		h.respondFromEntry(w, r, e, userID, nil)
		return
	}
	if err != nil {
		slog.Error("queue: join", "error", err)
		response.Error(w, r, http.StatusInternalServerError, "internal", "failed to join queue")
		return
	}

	// Null, not 0, when Redis cannot say: the join itself has succeeded.
	var positionOut, sizeOut any
	if position > 0 {
		positionOut = position
		if _, size, _, err := queue.Position(r.Context(), h.rdb, userID, e.ID); err != nil {
			slog.Warn("queue: size after join unavailable", "error", err)
		} else {
			sizeOut = size
		}
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
		"position":   positionOut,
		"queue_size": sizeOut,
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

	// Redis being down is not the user's problem: promotion keeps admitting from
	// Postgres, so a token must stay reachable. Fall through to the durable row.
	rank, size, found, err := queue.Position(r.Context(), h.rdb, userID, e.ID)
	if err != nil {
		slog.Warn("queue: position unavailable, answering from postgres", "error", err)
		found = false
	}
	waiting := map[string]any{"status": "waiting", "position": nil, "queue_size": nil}
	if found {
		waiting["position"], waiting["queue_size"] = rank+1, size
	}

	// Redis can only be behind Postgres about who was admitted: a worker crash
	// or a failed ZREM leaves a committed admission still in the set. Admission
	// is FIFO, so such a member sits at the front. Only the front therefore needs
	// the durable check, which keeps the polling crowd behind it off Postgres.
	if found && rank >= int64(e.MaxConcurrentWindows) {
		response.JSON(w, r, http.StatusOK, waiting)
		return
	}
	h.respondFromEntry(w, r, e, userID, waiting)
}

// respondFromEntry answers from the durable row: the purchase token and the
// original window for an admitted user, the waiting body for a waiting one, and
// 404 for anyone else. A nil waiting body also answers 404.
func (h *QueueHandler) respondFromEntry(w http.ResponseWriter, r *http.Request, e event.EventWithTiers, userID string, waiting map[string]any) {
	var status string
	var admittedAt *time.Time
	err := h.pool.QueryRow(r.Context(),
		`SELECT status, admitted_at FROM queue_entries
		 WHERE user_id = $1 AND event_id = $2 AND status IN ('waiting', 'admitted')`,
		userID, e.ID,
	).Scan(&status, &admittedAt)
	if err == nil && status == "waiting" && waiting != nil {
		response.JSON(w, r, http.StatusOK, waiting)
		return
	}
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
