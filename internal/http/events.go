package http

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vinhpnguyen123/ticketqueue/internal/domain/event"
	"github.com/vinhpnguyen123/ticketqueue/internal/http/response"
)

const (
	defaultEventLimit = 20
	maxEventLimit     = 50
)

// EventsHandler serves the public event-browsing endpoints.
type EventsHandler struct {
	events *event.Repo
}

func NewEventsHandler(pool *pgxpool.Pool) *EventsHandler {
	return &EventsHandler{events: event.NewRepo(pool)}
}

// Mount registers the public event routes on r.
func (h *EventsHandler) Mount(r chi.Router) {
	r.Get("/v1/events", h.List)
	r.Get("/v1/events/{id}", h.Get)
}

// money is the mandatory object form for all monetary values:
// {"amount": <cents int>, "currency": "USD"} — never a bare number.
type money struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

type tierResponse struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Price          money  `json:"price"`
	AvailableCount int    `json:"available_count"`
}

type eventResponse struct {
	ID      string         `json:"id"`
	Name    string         `json:"name"`
	Venue   string         `json:"venue"`
	EventAt time.Time      `json:"event_at"`
	Status  string         `json:"status"`
	Tiers   []tierResponse `json:"tiers"`
}

func toEventResponse(e event.EventWithTiers) eventResponse {
	tiers := make([]tierResponse, 0, len(e.Tiers))
	for _, t := range e.Tiers {
		tiers = append(tiers, tierResponse{
			ID:             t.ID,
			Name:           t.Name,
			Price:          money{Amount: t.PriceCents, Currency: t.Currency},
			AvailableCount: t.AvailableCount,
		})
	}
	return eventResponse{
		ID: e.ID, Name: e.Name, Venue: e.Venue,
		EventAt: e.EventAt, Status: e.Status, Tiers: tiers,
	}
}

func (h *EventsHandler) List(w http.ResponseWriter, r *http.Request) {
	limit := defaultEventLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			response.ErrorWithDetails(w, r, http.StatusUnprocessableEntity,
				"validation_failed", "invalid query parameters",
				map[string]any{"limit": "must be a positive integer"})
			return
		}
		limit = min(n, maxEventLimit)
	}

	var cursor *uuid.UUID
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			response.ErrorWithDetails(w, r, http.StatusUnprocessableEntity,
				"validation_failed", "invalid query parameters",
				map[string]any{"cursor": "must be a valid UUID"})
			return
		}
		cursor = &id
	}

	events, nextCursor, err := h.events.ListOnSale(r.Context(), limit, cursor)
	if err != nil {
		slog.Error("list events", "error", err)
		response.Error(w, r, http.StatusInternalServerError, "internal", "failed to list events")
		return
	}

	data := make([]eventResponse, 0, len(events))
	for _, e := range events {
		data = append(data, toEventResponse(e))
	}

	pagination := response.Pagination{HasMore: nextCursor != nil}
	if nextCursor != nil {
		s := nextCursor.String()
		pagination.NextCursor = &s
	}
	response.List(w, r, http.StatusOK, data, pagination)
}

func (h *EventsHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		response.ErrorWithDetails(w, r, http.StatusUnprocessableEntity,
			"validation_failed", "invalid event id",
			map[string]any{"id": "must be a valid UUID"})
		return
	}

	e, err := h.events.GetByID(r.Context(), id)
	if errors.Is(err, event.ErrNotFound) {
		response.Error(w, r, http.StatusNotFound, "event_not_found", "event not found")
		return
	}
	if err != nil {
		slog.Error("get event", "error", err)
		response.Error(w, r, http.StatusInternalServerError, "internal", "failed to load event")
		return
	}

	// TODO(week 2): replace the placeholder with the real Redis queue size.
	response.JSON(w, r, http.StatusOK, struct {
		eventResponse
		Queue map[string]int `json:"queue"`
	}{
		eventResponse: toEventResponse(e),
		Queue:         map[string]int{"size": 0},
	})
}
